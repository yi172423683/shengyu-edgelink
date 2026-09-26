// shengyu-edgelink-server 是管理平台主进程。
//
// 部署形态（需求 §二.5）：管理平台与节点**允许同机部署**。
// 本进程同时承担两个角色：
//
//	管理面：HTTP API + 元数据库 + 日志库 + 发布流水线
//	节点面：本机 HAProxy + 日志接收
//
// 之所以把"同机节点"做成内嵌而不是"起两个进程再互相调 API"：
//   - 少一层网络与认证，也就少一类"自己调自己失败"的故障；
//   - 日志不需要绕一圈 HTTP，落盘延迟更低。
//
// 远程节点仍由独立的 shengyu-agent 通过 /api/agent/* 接入，两条路径共用 internal/agent。
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shengyu/edgelink/internal/agent"
	"github.com/shengyu/edgelink/internal/api"
	"github.com/shengyu/edgelink/internal/auth"
	"github.com/shengyu/edgelink/internal/dataplane"
	"github.com/shengyu/edgelink/internal/haproxy"
	"github.com/shengyu/edgelink/internal/id"
	"github.com/shengyu/edgelink/internal/logparse"
	"github.com/shengyu/edgelink/internal/logstore"
	"github.com/shengyu/edgelink/internal/model"
	"github.com/shengyu/edgelink/internal/publish"
	"github.com/shengyu/edgelink/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("启动失败: %v", err)
	}
}

type options struct {
	listen          string
	dataDir         string
	configRoot      string
	dataplaneKind   string
	nodeName        string
	nodeID          string
	adminUser       string
	adminPass       string
	adminIPAllow    string
	ipAllow         string
	logSocket       string
	statsSocket     string
	statsSocketDir  string
	statsSocketGr   string
	haproxyUser     string
	haproxyGroup    string
	haproxyNBThread int
	binary          string
	tlsCert         string
	tlsKey          string
	sessionTTL      time.Duration
	secureCookie    bool
	checkOnly       bool
	writeBaseline   bool
	allowLoopback   bool
	showVersion     bool
	profile         string
	maxConn         int
	batchSize       int
	batchEvery      time.Duration
	batchMaxBuffer  int
	maxOpenShards   int
	logKeepAlive    time.Duration
	retention       logstore.RetentionPolicy
}

type resourceProfile struct {
	Name           string
	MaxConn        int
	BatchSize      int
	BatchEvery     time.Duration
	BatchMaxBuffer int
	MaxOpenShards  int
	LogKeepAlive   time.Duration
	Retention      logstore.RetentionPolicy
}

func liteResourceProfile() resourceProfile {
	return resourceProfile{Name: "lite", MaxConn: 4000, BatchSize: 256, BatchEvery: time.Second,
		BatchMaxBuffer: 3000, MaxOpenShards: 2, LogKeepAlive: 15 * time.Second,
		Retention: logstore.RetentionPolicy{RetainDays: 1, QuotaBytes: 1 << 30, CompressAfterHours: 0, MinFreeBytes: 512 << 20}}
}

func standardResourceProfile() resourceProfile {
	return resourceProfile{Name: "standard", MaxConn: 20000, BatchSize: 512, BatchEvery: time.Second,
		BatchMaxBuffer: 20000, MaxOpenShards: 8, LogKeepAlive: 30 * time.Second,
		Retention: logstore.DefaultRetentionPolicy()}
}

func hostMemoryBytes() (int64, bool) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "MemTotal:" {
			kb, err := strconv.ParseInt(f[1], 10, 64)
			if err == nil && kb > 0 {
				return kb * 1024, true
			}
		}
	}
	return 0, false
}

func resolveResourceProfile(requested string) (resourceProfile, error) {
	switch strings.ToLower(strings.TrimSpace(requested)) {
	case "", "auto":
		if bytes, ok := hostMemoryBytes(); ok && bytes <= 1536<<20 {
			return liteResourceProfile(), nil
		}
		return standardResourceProfile(), nil
	case "lite":
		return liteResourceProfile(), nil
	case "standard":
		return standardResourceProfile(), nil
	default:
		return resourceProfile{}, fmt.Errorf("-profile=%q 无效：允许 auto、lite、standard", requested)
	}
}

func applyResourceProfile(o *options) error {
	p, err := resolveResourceProfile(o.profile)
	if err != nil {
		return err
	}
	o.profile, o.maxConn, o.batchSize, o.batchEvery = p.Name, p.MaxConn, p.BatchSize, p.BatchEvery
	o.batchMaxBuffer, o.maxOpenShards, o.logKeepAlive, o.retention = p.BatchMaxBuffer, p.MaxOpenShards, p.LogKeepAlive, p.Retention
	return nil
}

func parseFlags() options {
	var o options
	flag.StringVar(&o.listen, "listen", "127.0.0.1:8081",
		"管理 API 监听地址。默认只绑本机 —— 管理入口不应直接对公网暴露（需求 §九）")
	flag.StringVar(&o.dataDir, "data", "/var/lib/shengyu-edgelink",
		"状态目录（元数据库与日志分片）。注意：**配置不在这里**，见 -config-root")
	flag.StringVar(&o.configRoot, "config-root", "/etc/shengyu-edgelink/haproxy",
		"配置根目录。HAProxy 只读这个目录下的 current/，"+
			"管理面把每个版本写进 versions/<节点>/vN/ 并按需原子替换到 current/。"+
			"该值必须与 shengyu-edgelink-haproxy.service 里 -f 指向的路径一致，安装脚本按此约定创建目录")
	flag.StringVar(&o.dataplaneKind, "dataplane", dataplane.HAProxyName,
		"转发内核。**只支持 haproxy**：生产统一使用 HAProxy 社区版（版本范围见 docs/04-haproxy-baseline.md）。"+
			"仓库内另有一个纯 Go 的测试辅助数据面（internal/testplane），它仅供自动化测试使用，"+
			"不随本程序提供，也不能作为生产降级内核")
	flag.StringVar(&o.nodeName, "node-name", "", "本机节点名称（用于同机部署）")
	flag.StringVar(&o.nodeID, "node-id", "", "本机节点 ID（留空则自动创建/复用同名节点）")
	flag.StringVar(&o.adminUser, "admin-user", "admin", "初始管理员用户名")
	flag.StringVar(&o.adminPass, "admin-pass", "", "初始管理员口令（仅首次创建时使用）")
	flag.StringVar(&o.adminIPAllow, "admin-ip-allow", "", "限制管理员登录来源（逗号分隔 IP/CIDR，空=不限制）")
	flag.StringVar(&o.ipAllow, "ip-allow", "", "（已废弃，等价于 admin-ip-allow）")
	flag.StringVar(&o.logSocket, "log-socket", "/run/shengyu-edgelink/log.sock", "HAProxy 日志套接字路径")
	flag.StringVar(&o.statsSocket, "stats-socket", "/run/shengyu-edgelink/haproxy.sock",
		"HAProxy 统计套接字路径（旧模式：所有版本共用一个）")
	flag.StringVar(&o.statsSocketDir, "stats-socket-dir", "/run/shengyu-edgelink",
		"按版本区分的统计套接字目录（推荐）。每个版本用 stats-v<版本>.sock，"+
			"发布验证就是靠连它来确认「vN 的 worker 真的在跑」，而不再只读我们自己写的版本标记")
	flag.StringVar(&o.statsSocketGr, "stats-socket-group", "shengyu",
		"统计套接字的属组。管理面以非 root 运行时必须能读它，否则统计与发布验证都会因权限失败")
	// HAProxy 自身的运行账号。**默认不是 root**：master-worker 模式下 worker 会切到它，
	// 而 unit 侧再叠加一层（单元直接 User=shengyu），两层都不依赖对方正确。
	// 传空串表示"不在配置里写降权" —— 只有在 unit 已经保证非 root 时才该这么做，
	// 所以启动自检会在"传空 + 以 root 运行"时给出明确告警（见 checkRunIdentity）。
	flag.StringVar(&o.haproxyUser, "haproxy-user", haproxy.DefaultRunUser,
		"HAProxy worker 进程降权到的用户（global: user）。留空=不在配置里降权")
	flag.StringVar(&o.haproxyGroup, "haproxy-group", haproxy.DefaultRunGroup,
		"HAProxy worker 进程降权到的用户组（global: group）。留空=不在配置里降权")
	// 线程数。默认取**进程可见的 CPU 数**（与 HAProxy 自身默认行为一致，见
	// haproxy.DefaultNBThread 的注释），上限 64（HAProxy 编译期 MAX_THREADS）。
	//
	// 为什么必须做成参数而不是继续写死：写死 4 在 2 vCPU 的验收机上会让 HAProxy
	// 打印 nbthread 相关告警，吞吐数据不可信；而"该用几个线程"本质上是部署机
	// 的属性，不是产品的常量。
	flag.IntVar(&o.haproxyNBThread, "haproxy-nbthread", haproxy.DefaultNBThread(),
		fmt.Sprintf("HAProxy worker 线程数（global: nbthread）。默认=进程可见 CPU 数；"+
			"0 也表示自动取 CPU 数；取值范围 %d..%d，超出会在启动自检阶段直接拒绝",
			haproxy.MinNBThread, haproxy.MaxNBThread))
	flag.StringVar(&o.binary, "haproxy", "/usr/sbin/haproxy", "haproxy 可执行文件路径")
	flag.StringVar(&o.tlsCert, "tls-cert", "", "TLS 证书（留空则用 HTTP，仅限本机调试）")
	flag.StringVar(&o.tlsKey, "tls-key", "", "TLS 私钥")
	flag.DurationVar(&o.sessionTTL, "session-ttl", 12*time.Hour, "登录会话有效期")
	flag.BoolVar(&o.secureCookie, "secure-cookie", false, "Cookie 仅在 HTTPS 下发送（生产必须为 true）")
	flag.BoolVar(&o.checkOnly, "check", false, "只做启动自检并退出（不监听端口）")
	flag.BoolVar(&o.writeBaseline, "write-baseline", false,
		"写入一份不监听任何端口的基线配置到 -config-root/current 后退出。"+
			"安装时先跑它，HAProxy 的 unit 才有配置可读（否则首次 start 会因文件不存在失败）")
	flag.BoolVar(&o.allowLoopback, "allow-loopback-origin", false,
		"允许把回环地址（127.0.0.1）作为源站。仅用于本地开发/验收；生产必须保持 false")
	flag.BoolVar(&o.showVersion, "version", false, "打印本构建的版本号并退出（发布构建由 -ldflags -X main.Version= 注入）")
	flag.StringVar(&o.profile, "profile", "auto", "资源档位：auto（1C1G 自动 lite）、lite 或 standard")
	flag.Parse()
	if o.adminIPAllow == "" {
		o.adminIPAllow = o.ipAllow
	}
	// 线程数合法性：0 归一为 CPU 数；负数与超上限是明确的错误输入，直接拒绝启动。
	// 放在这里（而不是等渲染报错）是因为这是**部署参数写错**，越早炸越好 ——
	// 一个跑着的服务不会因此半路失败。
	if o.haproxyNBThread < 0 || o.haproxyNBThread > haproxy.MaxNBThread {
		log.Fatalf("-haproxy-nbthread=%d 非法：允许范围 0..%d（0 表示自动取 CPU 数 %d）",
			o.haproxyNBThread, haproxy.MaxNBThread, haproxy.DefaultNBThread())
	}
	o.haproxyNBThread = haproxy.NormalizeNBThread(o.haproxyNBThread)
	// 生产告警：不开 TLS 又不绑本机等于把管理后台裸奔在网络上。
	if o.tlsCert == "" && !strings.HasPrefix(o.listen, "127.") && !strings.HasPrefix(o.listen, "localhost") {
		log.Printf("警告：未配置 TLS 且监听地址不是本机（%s）。管理面必须由外层反代提供 HTTPS，"+
			"否则口令与会话令牌会以明文经过网络（需求 §九）。", o.listen)
	}
	if o.tlsCert != "" && !o.secureCookie {
		log.Printf("提示：已启用 TLS，建议同时加 -secure-cookie=true")
	}
	return o
}

func run() error {
	o := parseFlags()

	if o.showVersion {
		fmt.Printf("shengyu-edgelink-server %s (agent=%s, os=%s/%s)\n", Version, AgentVersion, runtime.GOOS, runtime.GOARCH)
		return nil
	}
	if err := applyResourceProfile(&o); err != nil {
		return err
	}
	log.Printf("资源档位: %s（HAProxy maxconn=%d，日志批量=%d/%s，缓冲上限=%d，常驻分片=%d，日志保留=%d天）",
		o.profile, o.maxConn, o.batchSize, o.batchEvery, o.batchMaxBuffer, o.maxOpenShards, o.retention.RetainDays)

	if err := os.MkdirAll(o.dataDir, 0o750); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}
	metaPath := filepath.Join(o.dataDir, "meta.db")
	logsRoot := filepath.Join(o.dataDir, "logs")

	// 配置根与状态目录**必须分开**：状态（元数据库、日志分片）归服务自己所有，
	// 配置归 HAProxy 读、管理面写。两者混在同一个目录下会让 systemd 的
	// 读写权限规划无从下手 —— 要么给多了（配置可被随便改），要么给少了（写不进去）。
	// 评审 F02 指出的正是"程序写 /var/lib/...，unit 读 /etc/..."这种不一致。
	configRoot, err := filepath.Abs(o.configRoot)
	if err != nil {
		return fmt.Errorf("解析配置根目录失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(configRoot, "current"), 0o750); err != nil {
		return fmt.Errorf("创建配置根目录 %s 失败: %w", configRoot, err)
	}
	log.Printf("配置根目录: %s（HAProxy 读 %s/current）", configRoot, configRoot)
	// 把**实际生效**的线程数打进启动日志：验收时这一行是证据，
	// 用来确认"配置里写的 nbthread"与"这台机器的 CPU 数"对得上（docs/08 第 3/17 项）。
	log.Printf("HAProxy 线程数 nbthread=%d（进程可见 CPU=%d，上限 %d，0=自动；用 -haproxy-nbthread 调整）",
		haproxy.NormalizeNBThread(o.haproxyNBThread), runtime.NumCPU(), haproxy.MaxNBThread)

	// -write-baseline：安装期用。写一份不监听任何端口的基线配置后退出，
	// 让 HAProxy 的 unit 在"还没有任何业务"时也有配置可读。
	if o.writeBaseline {
		if err := writeBaseline(configRoot, o); err != nil {
			return err
		}
		log.Printf("已写入基线配置（不监听任何端口），路径 %s", filepath.Join(configRoot, "current"))
		return nil
	}

	st, err := store.Open(metaPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// 生产进程开启分片连接保活：日志是持续高频写入的，每次 flush 都新建/关闭
	// 一次 SQLite 连接会随日志量线性放大开销。保活之所以安全，是因为读侧
	// 不再依赖 immutable（见 db.Open 的说明）—— 写连接开着，读方也能看到已提交数据。
	//
	// 保活时长取 30 秒：跨小时的边界上最多同时按住 2 个分片（当前小时 + 刚过的小时），
	// 30 秒足够让"跨点那一批补传日志"复用同一条连接。
	logs, err := logstore.New(logsRoot, logstore.WithKeepAlive(o.logKeepAlive), logstore.WithMaxOpenShards(o.maxOpenShards))
	if err != nil {
		return err
	}
	defer func() {
		// 退出前把 WAL 合并回主文件：不合并也不会丢数据，但合并后每个分片是一个
		// 干净的单文件，备份/拷走/用别的工具打开都省事。
		if err := logs.CheckpointAll(); err != nil {
			log.Printf("退出前合并 WAL 失败（不影响已落盘数据）: %v", err)
		}
		if err := logs.Close(); err != nil {
			log.Printf("关闭日志库失败: %v", err)
		}
	}()

	// ---- 启动自检：把"能提前发现"的问题在启动时暴露，而不是等第一条业务进来才炸 ----
	if v, err := st.SchemaVersion(); err != nil || v != "1" {
		return fmt.Errorf("元数据库 schema 版本异常: %q (%v)", v, err)
	}
	if err := assertSecurityDefaults(); err != nil {
		return err
	}
	log.Printf("元数据库就绪: %s", metaPath)
	log.Printf("日志分片根目录: %s", logsRoot)

	// 首次启动时补齐基线配置：不能假设安装脚本一定跑过 -write-baseline。
	// 少了这一步，服务能起来但 HAProxy 起不来，而报错会出现在另一个服务上，很难查。
	if err := ensureBaseline(configRoot, o); err != nil {
		return err
	}

	// ---- 管理员引导 ----
	nUsers, err := st.CountUsers()
	if err != nil {
		return err
	}
	if nUsers == 0 {
		if o.adminPass == "" {
			return errors.New("尚未创建任何管理员账号，请用 -admin-pass 指定初始口令后重试" +
				"（首次启动引导；口令不会写入日志，也不会被保存到磁盘之外的任何地方）")
		}
		u := &store.User{ID: id.New("usr"), Username: o.adminUser, Enabled: true, Role: "admin",
			IPAllowlist: o.adminIPAllow}
		if err := st.CreateUser(u, o.adminPass, assertIterations()); err != nil {
			return fmt.Errorf("创建初始管理员失败: %w", err)
		}
		log.Printf("已创建初始管理员 %q（来源限制: %s）", o.adminUser,
			orNone(o.adminIPAllow))
	}
	// ---- 数据面 ----
	//
	// 放在 -check 之前：自检的意义就是"把安装问题在启动前暴露"。
	// 如果自检不碰数据面，那它检查完之后启动仍然可能因为 HAProxy 版本不符、
	// 或没有 reload 权限而失败 —— 那自检就没起到作用。
	dp, nodeID, err := buildDataplane(st, o)
	if err != nil {
		return err
	}
	defer dp.Close()

	// reload 权限预检（评审 F03）：非 root 时先验一次，别等第一次发布才发现没授权。
	if h, ok := dp.(*dataplane.HAProxy); ok {
		checked, perr := h.PreflightReload(context.Background())
		switch {
		case perr != nil && o.checkOnly:
			return perr
		case perr != nil:
			// 不阻断启动：管理面可用、转发不受影响，只是"发布"会失败。
			// 但要说清楚，避免运维在第一次发布时才撞上。
			log.Printf("【发布能力告警】%v", perr)
		case checked:
			log.Printf("已确认可对 %s 执行平滑 reload（当前身份 ✓）", h.ServiceName)
		default:
			log.Printf("%s 尚未运行，跳过 reload 权限预检（首次安装时属正常）", h.ServiceName)
		}
	}

	if o.checkOnly {
		log.Printf("自检通过（未启动监听）")
		return nil
	}

	pipeline := publish.New(st, dp, configRoot, renderDefaults(o))
	// 绑定本机节点：发布/回滚只允许作用于这台机器（见 publish.Pipeline 的说明）。
	pipeline.SetLocalNode(nodeID)
	if err := refreshLocalNode(st, nodeID, dp, o); err != nil {
		return err
	}

	// ---- 日志摄入：数据面产出的每一行都走同一条解析入库路径 ----
	resolver := newDynamicResolver(st, nodeID)
	ing := agent.NewIngestor(logs, resolver, nodeID)
	// 攒批器：一次写入一批，而不是一行一个事务（见 agent.Batcher 的说明）。
	// 它仍然"不阻塞转发、不静默丢弃"：缓冲区满时丢最旧的并计数，管理面能看到。
	batcher := agent.NewBatcher(ing)
	batcher.Size, batcher.Every, batcher.MaxBuffer = o.batchSize, o.batchEvery, o.batchMaxBuffer

	switch dp.Name() {
	case dataplane.HAProxyName:
		// HAProxy 通过 unix 数据报套接字投递日志；本进程在同一台机器上接收。
		recv := &agent.UnixDgramReceiver{Path: o.logSocket}
		if err := recv.Listen(); err != nil {
			return fmt.Errorf("绑定日志套接字失败（HAProxy 的 log 指令要指向它）: %w", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			if serr := recv.Serve(ctx, batcher.Add); serr != nil {
				log.Printf("日志接收停止: %v", serr)
			}
		}()
		// 定时冲刷 + 退出前冲刷（退出前那一次在 Batcher.Run 里做）。
		go batcher.Run(ctx)
		log.Printf("日志通道：Unix 套接字 %s（攒批写入：%d 行或 %s 触发一次提交）",
			o.logSocket, batcher.Size, batcher.Every)
	default:
		// 走到这里说明有人给 selectDataplane 加了新实现却忘了接日志通道。
		// 与其静默地"转发正常但一条日志都没有"，不如启动即失败。
		return fmt.Errorf("数据面 %q 未接入日志通道，拒绝启动（否则会静默丢日志）", dp.Name())
	}

	// ---- API ----
	srv := api.New(st, logs, pipeline, api.Config{
		SessionTTL:          o.sessionTTL,
		SecureCookies:       o.secureCookie,
		LoginRateLimit:      10,
		AllowLoopbackOrigin: o.allowLoopback,
		// 版本号与监听地址原样透给管理台：向导欢迎页要显示"当前版本/管理端口"，
		// 这两项只能来自进程自身，不能让前端猜（猜出来的版本号会和实际运行的二进制不符）。
		Version:    Version,
		ListenAddr: o.listen,
	})
	if o.allowLoopback {
		log.Printf("警告：已允许回环地址作为源站（-allow-loopback-origin）。这只适合本机开发/验收，不要在生产开启。")
	}
	srv.NodeID = nodeID
	srv.DataplaneName = dp.Name()
	// 把日志摄入链路挂到管理面上：总览页要显示「有没有日志被静默丢弃」。
	// 这条链路失败既不影响转发也不报错，是排查时最容易误判的一环。
	srv.Ingest = ing
	srv.Batch = batcher
	// 发布成功后立刻刷新日志解析器：新接入的业务从"下一行日志"起就能被正确归属，
	// 不用等缓存自然过期。这个钩子是"业务刚接入时日志归属为空"那类问题的根治办法。
	srv.OnPublished = func(_ string, _ int) { resolver.Refresh() }

	httpSrv := &http.Server{
		Addr:              o.listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if o.tlsCert != "" {
		cert, cerr := tls.LoadX509KeyPair(o.tlsCert, o.tlsKey)
		if cerr != nil {
			return fmt.Errorf("加载 TLS 证书失败: %w", cerr)
		}
		httpSrv.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	// ---- 后台维护 ----
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		maintenanceLoop(ctx, st, logs, ing, batcher, o.retention)
	}()

	// ---- 本机心跳（评审 F10）----
	//
	// 没有它，本机节点在启动约 90 秒后会被自己的维护任务标成离线。
	// 心跳带三个维度的健康（agent / 转发内核 / 业务路径），而不是一个聚合值。
	if dp.Name() == dataplane.HAProxyName {
		hb := agent.NewHeartbeater(nodeID,
			newLocalProber(dp, st, nodeID, o),
			func(rec agent.HeartbeatRecord) error {
				return st.RecordNodeHealth(store.NodeHealthInput{
					NodeID:              rec.NodeID,
					AgentVersion:        rec.AgentVersion,
					HAProxyVersion:      rec.HAProxyVersion,
					AppliedVersion:      rec.AppliedVersion,
					AppliedVersionKnown: rec.AppliedVersionKnown,
					Capabilities:        rec.Capabilities,
					Detail:              rec.Detail,
				})
			}, 20*time.Second)
		wg.Add(1)
		go func() {
			defer wg.Done()
			hb.Run(ctx)
		}()
	}

	errCh := make(chan error, 1)
	go func() {
		if o.tlsCert != "" {
			log.Printf("管理面已启动: https://%s（数据面 %s，节点 %s）", o.listen, dp.Name(), nodeID)
			errCh <- httpSrv.ListenAndServeTLS("", "")
			return
		}
		log.Printf("管理面已启动: http://%s（数据面 %s，节点 %s）", o.listen, dp.Name(), nodeID)
		errCh <- httpSrv.ListenAndServe()
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case s := <-sig:
		log.Printf("收到信号 %s，开始优雅退出", s)
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	_ = httpSrv.Shutdown(shutCtx)
	wg.Wait()
	log.Printf("已退出")
	return nil
}

// euid 返回当前有效用户 ID。
//
// 单独包一层是为了让"是否以 root 运行"这件事在 Windows 上也可判定：
// os.Geteuid 在 Windows 返回 -1（既不是 0），于是会走非 root 分支，
// 而 Windows 本来也不是生产环境，这个结果不会造成误导。
func euid() int { return os.Geteuid() }

// renderDefaults 把命令行选项翻译成渲染器的默认值。
//
// 集中在一处，保证"服务启动时用的渲染参数"与"命令行写下来的部署参数"不会各说各话：
// 统计套接字路径、日志套接字路径都必须与 unit 里声明的一致，否则
// 配置能渲染出来、HAProxy 也能起，但管理面读不到统计 —— 这种"半通"最难查。
func renderDefaults(o options) haproxy.Defaults {
	d := haproxy.DefaultDefaults()
	d.ConfigDir = haproxy.ConfigDirToken
	d.LogSocketPath = o.logSocket
	d.StatsSocketDir = o.statsSocketDir
	d.StatsSocketPath = o.statsSocket
	d.StatsSocketGroup = o.statsSocketGr
	d.RunUser = o.haproxyUser
	d.RunGroup = o.haproxyGroup
	// 渲染器内部还会再归一/校验一次（零值与超上限分别处理），这里只做赋值。
	d.NBThread = o.haproxyNBThread
	if o.maxConn > 0 {
		d.MaxConn = o.maxConn
	}
	return d
}

// writeBaseline 写一份"不监听任何端口"的基线配置，供安装期使用。
func writeBaseline(configRoot string, o options) error {
	r, err := haproxy.RenderBaseline(renderDefaults(o))
	if err != nil {
		return fmt.Errorf("渲染基线配置失败: %w", err)
	}
	cur := filepath.Join(configRoot, "current")
	if err := os.MkdirAll(cur, 0o750); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", cur, err)
	}
	// 与发布流程一致：先写内容文件，最后写 VERSION 作为提交标记。
	cfg := strings.ReplaceAll(string(r.Config), haproxy.ConfigDirToken, filepath.ToSlash(cur))
	if err := os.WriteFile(filepath.Join(cur, "haproxy.cfg"), []byte(cfg), 0o640); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cur, dataplane.VersionFileName),
		[]byte(strconv.Itoa(haproxy.BaselineVersion)+"\n"), 0o640)
}

// ensureBaseline 在 current/haproxy.cfg 不存在时补一份基线配置。
//
// 不覆盖已存在的配置：那是上一次发布的成果，哪怕它是坏的也要先报出来，
// 而不是用一个"看起来正常"的基线把它悄悄替换掉。
func ensureBaseline(configRoot string, o options) error {
	cfg := filepath.Join(configRoot, "current", "haproxy.cfg")
	if _, err := os.Stat(cfg); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	log.Printf("current/haproxy.cfg 不存在，写入基线配置（不监听任何端口）以便 HAProxy 能启动")
	return writeBaseline(configRoot, o)
}

// assertSecurityDefaults 在启动时守住几个"一旦放宽就会静默变弱"的安全默认值。//
// 为什么要在启动时断言而不是靠代码注释：PBKDF2 迭代次数是个变量（测试会调低它）。
// 如果有人误在生产把默认值改小，一切功能照常，只有口令强度悄悄下降 —— 这种退化必须主动拦。
func assertSecurityDefaults() error {
	if assertIterations() < 100_000 {
		return fmt.Errorf("PBKDF2 迭代次数过低（%d）。生产必须 ≥100000，请检查是否有人改过默认值。",
			assertIterations())
	}
	return nil
}

// assertIterations 返回当前生效的迭代次数，便于日志与断言。
func assertIterations() int { return auth.DefaultIterations }

// checkDataplaneKind 判断 -dataplane 的取值是否被允许。
//
// 单独抽成纯函数是为了让"生产只允许 HAProxy"这条约束**可被自动化测试钉住**：
// 一旦有人把这个测试辅助数据面重新接回 CLI，测试会立刻失败，
// 而不是等到某天有人在生产上用它转发客户业务。
func checkDataplaneKind(kind string) error {
	switch kind {
	case dataplane.HAProxyName, "":
		return nil
	case "gorelay":
		return fmt.Errorf("不支持的数据面 %q：internal/testplane（纯 Go 转发实现）仅供自动化测试使用，"+
			"不随本程序提供，也不是生产降级方案；生产必须使用 %s", kind, dataplane.HAProxyName)
	default:
		return fmt.Errorf("不支持的数据面 %q：本程序只支持 %s（HAProxy 社区版，版本范围见 docs/04-haproxy-baseline.md）",
			kind, dataplane.HAProxyName)
	}
}

// buildDataplane 依据配置构造数据面，并保证同机节点记录存在。
func buildDataplane(st *store.Store, o options) (dataplane.Applier, string, error) {
	if err := checkDataplaneKind(o.dataplaneKind); err != nil {
		return nil, "", err
	}
	// 同机节点优先复用：先按 -node-id，再按 -node-name 找，最后新建。
	nodeID := o.nodeID
	if nodeID == "" {
		nodes, err := st.ListNodes()
		if err != nil {
			return nil, "", err
		}
		name := o.nodeName
		if name == "" {
			host, _ := os.Hostname()
			name = "本机-" + host
		}
		for _, n := range nodes {
			if n.Name == name {
				nodeID = n.ID
				break
			}
		}
	}
	if nodeID == "" {
		name := o.nodeName
		if name == "" {
			host, _ := os.Hostname()
			name = "本机-" + host
		}
		n := &model.Node{ID: id.New("node"), Name: name, Enabled: true, Health: model.NodeOnline}
		if err := st.CreateNode(n); err != nil {
			return nil, "", err
		}
		nodeID = n.ID
		log.Printf("已创建同机节点 %s（%s）", n.Name, nodeID)
	}

	switch o.dataplaneKind {
	case dataplane.HAProxyName, "":
		// 先确认"转发进程不会长期以 root 运行"，再往下走（见 checkRunIdentity 的说明）。
		if err := checkRunIdentity(o); err != nil {
			return nil, "", err
		}
		root, aerr := filepath.Abs(o.configRoot)
		if aerr != nil {
			return nil, "", fmt.Errorf("解析配置根目录失败: %w", aerr)
		}
		h := dataplane.NewHAProxy()
		h.Binary = o.binary
		h.ConfigRoot = root
		h.CurrentDir = filepath.Join(root, "current")
		h.StatsSocket = o.statsSocket
		h.StatsSocketDir = o.statsSocketDir
		// 非 root 运行时，reload 走 **systemd + polkit 授权**，不走 sudo。
		//
		// 为什么不能用 sudo（评审"NoNewPrivileges 与提权方案冲突"）：
		//   server 单元的沙箱里有 NoNewPrivileges=true，它的语义是"本进程及其子进程
		//   不允许通过 setuid/setgid 提升权限"。而 sudo 恰恰依赖 setuid ——
		//   两者放在一起，*sudo 永远不可能成功*，而且失败信息只有一句
		//   "sudo: effective uid is not 0"，与"授权没配好"看起来一模一样，极难定位。
		//
		// 正确做法是让**进程不提权**，把授权交给 systemd 的 D-Bus + polkit：
		//   进程仍以 shengyu 身份调用 `systemctl reload shengyu-edgelink-haproxy`，
		//   systemd 通过 D-Bus 收到请求，由 polkit 规则判断"这个 uid 能不能管这个 unit"。
		//   规则文件只放行**一个单元的一个动作**（见 deploy/polkit-50-shengyu-edgelink.rules），
		//   不是"给这个用户开个万能 helper"。
		h.ReloadCmd = []string{"systemctl", "reload", h.ServiceName}
		if euid() == 0 {
			log.Printf("以 root 身份运行：平滑生效直接执行 %s", strings.Join(h.ReloadCmd, " "))
		} else {
			log.Printf("以非 root 身份运行：平滑生效将通过 systemd+polkit 执行 %s"+
				"（需安装 deploy/polkit-50-shengyu-edgelink.rules；启动时会先验证一次权限）",
				strings.Join(h.ReloadCmd, " "))
		}
		caps, err := h.Capabilities(context.Background())
		if err != nil {
			return nil, "", err
		}
		log.Printf("HAProxy 能力探测: version=%q supported=%v master_worker=%v",
			caps.Version, caps.Supported, caps.MasterWorker)
		if !caps.Supported {
			return nil, "", fmt.Errorf("HAProxy 版本 %q 不在受支持范围内（见 docs/04-haproxy-baseline.md）。"+
				"请先安装受支持的 HAProxy 社区版再启动本程序", caps.Version)
		}
		if !caps.MasterWorker {
			return nil, "", fmt.Errorf("HAProxy 未启用 master-worker 模式，无法平滑发布" +
				"（需求 §五：常规规则更新不得中断现有连接）。请在 global 段启用 master-worker")
		}
		return h, nodeID, nil
	default:
		// 明确拒绝，并且说清楚为什么 —— 避免有人把测试辅助实现当成"备用内核"在生产上开起来。
		return nil, "", checkDataplaneKind(o.dataplaneKind)
	}
}

// refreshLocalNode 把本机数据面的实际能力与版本写回节点记录，
// 让界面上"期望版本 vs 实际版本"从一开始就是真实的，而不是等第一次心跳才填。
//
// 同时写入三维健康（Agent/Proxy/Path）：启动完成本身就是一个可观测的采样点，
// 在这里留空会让界面在第一个心跳周期（20 秒）内显示"健康未知"。
func refreshLocalNode(st *store.Store, nodeID string, dp dataplane.Applier, o options) error {
	n, err := st.GetNode(nodeID)
	if err != nil {
		return err
	}
	caps, capsErr := dp.Capabilities(context.Background())
	ver, verr := dp.ActiveVersion(context.Background())
	n.Capabilities = caps
	n.HAProxyVersion = caps.Version
	n.AgentVersion = AgentVersion

	det := model.NodeHealthDetail{Agent: model.NodeOnline, At: time.Now().UTC()}
	var notes []string
	if capsErr != nil {
		det.Proxy = model.NodeOffline
		notes = append(notes, "转发内核探测失败："+capsErr.Error())
	} else {
		det.Proxy = model.NodeOnline
	}
	if verr != nil {
		notes = append(notes, "运行版本读取失败："+verr.Error())
	} else if ver <= 0 {
		det.Path = model.NodeUnknown
		notes = append(notes, "尚未发布任何配置")
	} else {
		n.AppliedVersion = ver
		det.Path = model.NodeOnline
	}
	det.Note = strings.Join(notes, "；")
	n.HealthDetail = det
	n.Health = det.Aggregate()
	n.LastHeartbeat = time.Now().UTC()
	// 入口地址：把本机公网 IP 作为默认值，避免界面上一片空白。
	if n.PublicIPv4 == "" {
		if ip := detectOutboundIPv4(); ip != "" {
			n.PublicIPv4 = ip
		}
	}
	return st.UpdateNode(n)
}

// AgentVersion 本进程内嵌节点运行时的版本。
const AgentVersion = "shengyu-edgelink-embedded/0.1.0"

// Version 本构建的版本号，由发布脚本注入：
//
//	go build -ldflags "-X main.Version=v0.1.0"
//
// 为什么要它：验收报告必须能回答"这一轮跑的是哪个二进制"。
// 靠文件名或构建时间都不行（文件会被拷来拷去、时间会被覆盖），
// 只有内嵌在二进制里的版本号能在 `systemctl status` 的日志里被原样读出来。
// 未注入时的默认值故意叫 "dev"：看到它就说明这不是发布构建，不该上生产。
var Version = "dev"

// detectOutboundIPv4 通过一次 UDP 拨号探测本机出口 IPv4。
// 用 UDP 而不是 HTTP：UDP 只做路由选择、不实际发包，
// 因此不会因为外网不通而卡住启动，也不会产生任何外部请求。
func detectOutboundIPv4() string {
	c, err := net.DialTimeout("udp", "198.51.100.1:9", time.Second)
	if err != nil {
		return ""
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok && a.IP != nil && a.IP.To4() != nil {
		return a.IP.String()
	}
	return ""
}

// checkRunIdentity 在启动阶段确认"HAProxy 的转发进程不会长期以 root 运行"。
//
// 这条检查是复核意见的落地（"HAProxy worker 必须明确降权到 shengyu，
// 不能长期以 root 运行"）。它必须**在启动时就失败**，而不是等到现场发现
// ps 里全是 root —— 那种问题在验收时才会被追问，而那时已经装完、发过版了。
//
// 判据与依据：
//   - master-worker 模式下，HAProxy 只有 **worker 进程**切到 global 的 user/group
//     （源码 src/haproxy.c：`if ((global.mode & (MODE_MWORKER | MODE_DAEMON)) == 0) set_identity()`，
//     mworker 时 master 不切）；因此"配置里没有 user/group" + "以 root 启动"
//     ⇒ 转发进程就是 root。这正是要拦住的组合。
//   - 非 root 启动 + 配了 user/group 是安全的：src/linuxcap.c 的
//     prepare_caps_for_setuid / finalize_caps_after_setuid 都是 `if (from_uid != 0) return 0;`，
//     随后的 setuid(自己)/setgid(自己) 必然成功。
func checkRunIdentity(o options) error {
	if runtime.GOOS == "windows" {
		// Windows 上没有 HAProxy（本项目的数据面在 Windows 上本来也起不来），
		// 这里只跳过检查并说明，不伪造结论。
		log.Printf("当前为 Windows：跳过 HAProxy 运行身份检查（生产目标是 Linux）")
		return nil
	}
	user := strings.TrimSpace(o.haproxyUser)
	group := strings.TrimSpace(o.haproxyGroup)
	if user == "" {
		if euid() == 0 {
			return fmt.Errorf("拒绝启动：当前以 root 运行，且没有配置 HAProxy 降权目标（-haproxy-user 为空）。" +
				"这会让**所有**转发进程长期以 root 运行。请用 -haproxy-user shengyu（默认值）" +
				"或在 systemd 单元里用 User=shengyu 启动")
		}
		log.Printf("注意：未配置 HAProxy 降权目标，当前进程 uid=%d 非 root，转发进程不会以 root 运行", euid())
		return nil
	}
	if _, err := userLookup(user); err != nil {
		return fmt.Errorf("HAProxy 降权目标用户 %q 不存在（%v）。"+
			"配置里写了它，HAProxy 启动时会直接失败。请先创建该账号（install.sh 会自动建），"+
			"或用 -haproxy-user 指定正确的用户", user, err)
	}
	if group != "" {
		if _, err := groupLookup(group); err != nil {
			return fmt.Errorf("HAProxy 降权目标用户组 %q 不存在（%v）。配置里写了它，HAProxy 启动时会直接失败", group, err)
		}
	}
	log.Printf("HAProxy 转发进程将降权到 %s:%s（worker 进程；master 在 unit 里也已非 root）",
		user, orNone(group))
	return nil
}

// userLookup / groupLookup 抽成变量，便于测试注入（真实实现读 /etc/passwd、/etc/group）。
var (
	userLookup = func(name string) (string, error) {
		u, err := user.Lookup(name)
		if err != nil {
			return "", err
		}
		return u.Uid, nil
	}
	groupLookup = func(name string) (string, error) {
		g, err := user.LookupGroup(name)
		if err != nil {
			return "", err
		}
		return g.Gid, nil
	}
)

// maintenanceLoop 周期性维护：节点离线判定、会话清理、日志保留与容量控制。
//
// 全部做成"失败只记日志、不退出进程"：维护任务是辅助能力，
// 它挂掉不应该影响转发与管理入口。
func maintenanceLoop(ctx context.Context, st *store.Store, logs *logstore.Store, ing *agent.Ingestor, batcher *agent.Batcher, pol logstore.RetentionPolicy) {
	stale := time.NewTicker(30 * time.Second)
	retention := time.NewTicker(10 * time.Minute)
	defer stale.Stop()
	defer retention.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stale.C:
			if n, err := st.MarkStaleNodes(90 * time.Second); err != nil {
				log.Printf("标记离线节点失败: %v", err)
			} else if n > 0 {
				log.Printf("已将 %d 台超过 90 秒未心跳的节点标记为离线", n)
			}
			if _, err := st.PurgeExpiredSessions(); err != nil {
				log.Printf("清理过期会话失败: %v", err)
			}
			if _, err := st.PurgeAuditBefore(time.Now().Add(-90 * 24 * time.Hour)); err != nil {
				log.Printf("清理过期审计失败: %v", err)
			}
		case <-retention.C:
			// 保留期 + 容量上限 + 磁盘水位 + 压缩，一次算清（评审 F12）。
			//
			// 关键是**策略里不再只有保留天数**：只按天数删，等于把"高流量节点在
			// 7 天内把盘写满"的风险留给现场 —— 而日志盘写满会连带打挂登录与发布。
			for _, k := range logstore.AllKinds() {
				res, err := logs.ApplyMaintenance(k, pol)
				if err != nil {
					log.Printf("执行 %s 日志维护失败: %v", k, err)
					continue
				}
				if res.Deleted > 0 || res.Compressed > 0 {
					log.Printf("%s 日志维护：删除 %d 个分片（释放 %.1f MiB）、压缩 %d 个分片（%.1f MiB → %.1f MiB）",
						k, res.Deleted, float64(res.ReleasedBytes)/(1<<20),
						res.Compressed, float64(res.CompressedIn)/(1<<20), float64(res.CompressedOut)/(1<<20))
				}
				for _, e := range res.Errors {
					log.Printf("%s 日志维护告警: %s", k, e)
				}
				if pol.MinFreeBytes > 0 && res.FreeBytes >= 0 && res.FreeBytes < pol.MinFreeBytes {
					log.Printf("【磁盘告警】日志根目录可用空间仅 %.1f MiB，低于水位 %.1f MiB。"+
						"已按策略删除最旧分片；如果仍不够，请扩容或调小保留期",
						float64(res.FreeBytes)/(1<<20), float64(pol.MinFreeBytes)/(1<<20))
				}
			}
			s := ing.IngestStats()
			if s.Dropped > 0 || s.ParseErrs > 0 {
				log.Printf("日志摄入异常：解析失败 %d 条、丢弃 %d 条，最后错误: %s",
					s.ParseErrs, s.Dropped, s.LastError)
			}
			if batcher != nil {
				bs := batcher.Stats()
				if bs.BufferDrops > 0 {
					log.Printf("日志攒批缓冲溢出丢弃 %d 条（写入速度跟不上，建议降低采集量或检查磁盘）", bs.BufferDrops)
				}
			}
		}
	}
}

// dynamicResolver 是带缓存的日志解析器。
//
// 为什么需要它：backend 名 → 业务 ID 的映射来自数据库，而日志是持续高速到达的，
// 每来一行都查两次库会把日志摄入变成数据库压力源。
//
// 但**只在启动时构建一次是不够的**，而且这是一个很隐蔽的坑：
// 如果解析器固化，那么"刚接入的业务"在缓存过期前产生的日志会全部解析不出归属 ——
// 表现为「业务明明在转发、日志里业务 ID 却是空的」，排查时极易被误判成转发问题。
// 所以策略是：**未命中就刷新一次再试**，缓存只用于"命中"这条快路径。
type dynamicResolver struct {
	st     *store.Store
	nodeID string
	mu     sync.RWMutex
	cur    logparse.Resolver
	at     time.Time
}

func newDynamicResolver(st *store.Store, nodeID string) *dynamicResolver {
	d := &dynamicResolver{st: st, nodeID: nodeID}
	d.Refresh()
	return d
}

// Refresh 重建映射。可由"发布成功"等事件主动触发。
func (d *dynamicResolver) Refresh() {
	r, err := d.st.ResolverForNode(d.nodeID)
	if err != nil {
		log.Printf("刷新日志解析器失败（继续沿用上一份）: %v", err)
		return
	}
	d.mu.Lock()
	d.cur, d.at = r, time.Now()
	d.mu.Unlock()
}

func (d *dynamicResolver) snapshot() (logparse.Resolver, time.Time) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.cur, d.at
}

// lookup 在**不刷新**的前提下查一次。
func (d *dynamicResolver) lookup(be string) (string, string, bool) {
	cur, _ := d.snapshot()
	if cur == nil {
		return "", "", false
	}
	return cur.BusinessForBackend(be)
}

// resolve 先走缓存快路径；未命中且缓存已超过 minAge 就刷新一次再试。
//
// minAge 不是 0：未匹配 SNI 的兜底 backend 会**永远**查不到，若每次都刷新，
// 攻击者只要持续发未知 SNI 就能把这里变成数据库压力源。
func (d *dynamicResolver) resolve(be string) (string, string, bool) {
	if bid, rid, ok := d.lookup(be); ok {
		return bid, rid, true
	}
	_, at := d.snapshot()
	if time.Since(at) >= 2*time.Second {
		d.Refresh()
		return d.lookup(be)
	}
	return "", "", false
}

func (d *dynamicResolver) BusinessForBackend(be string) (string, string, bool) {
	return d.resolve(be)
}

func (d *dynamicResolver) OriginForBackend(be string) (string, int, bool) {
	bid, _, ok := d.resolve(be)
	if !ok {
		return "", 0, false
	}
	cur, _ := d.snapshot()
	if cur == nil {
		return "", 0, false
	}
	_ = bid
	return cur.OriginForBackend(be)
}

// ---- 按配置版本解析（评审 F11）----
//
// 必须显式实现，否则外层拿到的是 logparse.Resolver 接口，
// 类型断言 `res.(logparse.VersionedResolver)` 会失败，版本快照富化永远不会生效 ——
// 而"永远不会生效"的表现和历史日志源站写空是一样的，很难发现。
//
// 缓存策略与 resolve 一致：命中就直接返回；未命中且缓存过期才刷新一次，
// 避免攻击者用一堆不存在的 backend 名把这里变成数据库压力源。
func (d *dynamicResolver) BusinessForBackendAt(cfgVersion int, be string) (string, string, bool) {
	if vr, ok := d.versioned(); ok {
		if bid, rid, found := vr.BusinessForBackendAt(cfgVersion, be); found {
			return bid, rid, true
		}
	}
	return "", "", false
}

func (d *dynamicResolver) OriginForBackendAt(cfgVersion int, be string) (string, int, bool) {
	if vr, ok := d.versioned(); ok {
		if h, p, found := vr.OriginForBackendAt(cfgVersion, be); found {
			return h, p, true
		}
	}
	return "", 0, false
}

func (d *dynamicResolver) versioned() (logparse.VersionedResolver, bool) {
	cur, at := d.snapshot()
	if cur == nil {
		d.Refresh()
		cur, at = d.snapshot()
	}
	if cur == nil {
		return nil, false
	}
	if vr, ok := cur.(logparse.VersionedResolver); ok {
		return vr, true
	}
	// 底层解析器不支持按版本解析时，刷新一次再试（例如实现被替换）。
	if time.Since(at) >= 2*time.Second {
		d.Refresh()
		if cur2, _ := d.snapshot(); cur2 != nil {
			if vr, ok := cur2.(logparse.VersionedResolver); ok {
				return vr, true
			}
		}
	}
	return nil, false
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "不限制"
	}
	return s
}
