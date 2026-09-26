package testplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shengyu/edgelink/internal/dataplane"
	"github.com/shengyu/edgelink/internal/haproxy"
	"github.com/shengyu/edgelink/internal/model"
	"github.com/shengyu/edgelink/internal/sni"
)

// GoRelayName 是 gorelay 实现的标识名。
const GoRelayName = "gorelay"

// GoRelay 纯 Go 的 SNI/TCP 转发内核。
//
// 它**不做** TLS 终止：SNI 模式只解析 ClientHello 取域名，随后把原始字节原样转发，
// 客户端与源站之间的 TLS 仍是端到端加密的（与 HAProxy 的 tcp 模式语义一致）。
type GoRelay struct {
	nodeID string
	// inspectDelay 是读取 ClientHello 的观察窗口。
	inspectDelay time.Duration
	sink         dataplane.LogSink
	clock        func() time.Time

	// state 是当前生效的路由快照。用原子指针替换实现"无锁读、原子换"——
	// 正在转发中的连接持有旧快照的引用，配置更新不会影响它们的路由决策。
	state atomic.Pointer[relayState]

	mu     sync.Mutex
	active map[string]*boundListener
	ver    int

	// 指标
	totalConns  atomic.Int64
	activeConns atomic.Int64
	bytesIn     atomic.Int64
	bytesOut    atomic.Int64
	connErrors  atomic.Int64
	noMatch     atomic.Int64
	seq         atomic.Uint64

	// 源站健康（按 target 维度记录最近一次拨号结果）
	healthMu sync.Mutex
	health   map[string]bool

	closed atomic.Bool
}

// NewGoRelay 创建转发内核。sink 为 nil 时日志被丢弃（仅测试用）。
func NewGoRelay(nodeID string, sink dataplane.LogSink) *GoRelay {
	r := &GoRelay{
		nodeID:       nodeID,
		inspectDelay: 5 * time.Second,
		sink:         sink,
		clock:        time.Now,
		active:       map[string]*boundListener{},
		health:       map[string]bool{},
	}
	r.state.Store(&relayState{
		sniRoutes: map[string]routeTarget{},
		tcpRoutes: map[string]routeTarget{},
	})
	return r
}

// SetClock 注入时钟（测试用）。
func (r *GoRelay) SetClock(f func() time.Time) {
	if f != nil {
		r.clock = f
	}
}

// SetInspectDelay 调整 ClientHello 观察窗口。
func (r *GoRelay) SetInspectDelay(d time.Duration) {
	if d > 0 {
		r.inspectDelay = d
	}
}

// SetSink 替换日志出口。
//
// 存在的必要性：启动顺序上，数据面要先于日志摄入器创建（日志摄入器需要节点信息），
// 但日志出口又必须在数据面开始接受连接之前挂好。
// 允许后挂避免了"必须把创建顺序拧成某个特定形状"——那种约束日后一定会被人改坏。
func (r *GoRelay) SetSink(s dataplane.LogSink) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sink = s
}

func (r *GoRelay) Name() string { return GoRelayName }

func (r *GoRelay) Capabilities(context.Context) (model.Capabilities, error) {
	return model.Capabilities{
		Version:           "gorelay/1.0.0",
		Supported:         true,
		MasterWorker:      true,
		ExposeFDListeners: false,
		SNICapture:        true, // 握手期解析，不依赖"会话结束还能重读"
		JSONEscape:        true,
		UnixDgramLog:      false,
		ProbedAt:          r.clock(),
	}, nil
}

// ActiveVersion 当前生效版本。
func (r *GoRelay) ActiveVersion(context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ver, nil
}

// ============================== 应用配置 ==============================

type routeTarget struct {
	businessID string
	routeID    string
	backend    string
	server     string
	originAddr string
	originPort int

	connectTimeout time.Duration
	idleClientSrv  time.Duration // 客户端方向空闲超时
	idleOriginSrv  time.Duration // 源站方向空闲超时

	maxConn    int
	queueLimit int
}

type relayState struct {
	version int
	// sniRoutes 以**小写域名**为键。域名大小写不敏感，但路由表必须规范化为小写，
	// 否则 "A.example.com" 与 "a.example.com" 会得到不同结果（真实事故来源）。
	sniRoutes map[string]routeTarget
	// tcpRoutes 以监听地址 host:port 为键。
	tcpRoutes map[string]routeTarget
	// sniPorts 记录哪些端口是 SNI 入口（用于日志里的 frontend 名）。
	sniPorts map[int]bool
}

// Validate 对候选配置做检查。
//
// gorelay 不消费 HAProxy 配置文件（它直接吃结构化状态），所以这里**不解析 cfg**，
// 而是检查配置文件是否可读、非空 —— 目的是保持与 HAProxy 实现相同的失败语义：
// 候选配置有问题时，必须在 Validate 阶段就被挡住，而不是等 Apply 才发现。
//
// 真正的语义校验（域名重复、端口冲突、源站回环等）在 validate 包里做，
// 且是**发布前**对结构化状态的检查，两个数据面共享同一套。
func (r *GoRelay) Validate(_ context.Context, cfgPath string) error {
	if cfgPath == "" {
		return errors.New("gorelay: 候选配置路径为空")
	}
	st, err := os.Stat(cfgPath)
	if err != nil {
		return fmt.Errorf("gorelay: 读取候选配置失败: %w", err)
	}
	if st.Size() == 0 {
		return errors.New("gorelay: 候选配置为空文件")
	}
	return nil
}

// Apply 应用目标状态。
//
// 关键顺序（需求 §五"错误配置和端口冲突不破坏现有服务"）：
//
//	① 先绑定所有**新增**监听；任一失败 → 关掉刚绑的、原状态不变，返回错误
//	② 全部成功后才原子替换路由快照并关闭**移除**的监听
//
// 绝不允许"先关旧再开新"：那样一次端口冲突就会把正在服务的业务打断。
func (r *GoRelay) Apply(ctx context.Context, req dataplane.ApplyRequest) error {
	if r.closed.Load() {
		return errors.New("gorelay: 已关闭")
	}
	desired := dataplane.ExpectedListeners(req.State)

	// 先算出目标快照：监听器的「种类」（SNI 入口 / TCP 独立入口）必须由**目标状态**决定，
	// 而不是当前状态 —— 否则新增的 SNI 入口会被当成 TCP 入口处理（真实 bug，已修）。
	ns := buildRelayState(req.State, req.Version)

	r.mu.Lock()
	defer r.mu.Unlock()

	want := map[string]model.Listener{}
	for _, l := range desired {
		want[l.Key()] = l
	}

	// ① 绑定新增监听
	added := make([]*boundListener, 0, len(want))
	for k, l := range want {
		if _, ok := r.active[k]; ok {
			continue
		}
		kind := "tcp"
		if ns.sniPorts[l.Port] {
			kind = "sni"
		}
		bl, err := r.bind(l, kind)
		if err != nil {
			for _, a := range added {
				a.close()
			}
			return fmt.Errorf("gorelay: 绑定 %s 失败（现有转发未受影响）: %w", k, err)
		}
		added = append(added, bl)
	}

	// ② 原子替换快照
	r.state.Store(ns)

	for _, bl := range added {
		r.active[bl.key] = bl
		bl.start()
	}
	// 关闭被移除的监听（在服务中的连接由 boundListener 自己等它自然结束）
	for k, bl := range r.active {
		if _, ok := want[k]; !ok {
			bl.stopAccepting()
			delete(r.active, k)
		}
	}
	r.ver = req.Version
	return nil
}

func buildRelayState(st model.DesiredState, version int) *relayState {
	ns := &relayState{
		version:   version,
		sniRoutes: map[string]routeTarget{},
		tcpRoutes: map[string]routeTarget{},
		sniPorts:  map[int]bool{},
	}
	entryByID := map[string]model.SNIEntry{}
	for _, e := range st.SNIEntries {
		entryByID[e.ID] = e
		if e.Enabled {
			ns.sniPorts[e.BindPort] = true
		}
	}
	for _, rv := range st.Routes {
		if !rv.Enabled {
			continue
		}
		t := routeTarget{
			businessID: rv.Route.BusinessID,
			routeID:    rv.Route.ID,
			backend:    haproxy.BackendName(rv),
			server:     haproxy.ServerName(rv),
			originAddr: rv.Route.OriginHost,
			originPort: rv.Route.OriginPort,
			maxConn:    rv.EffectiveMaxConn,
		}
		if rv.ConnectTimeoutMS > 0 {
			t.connectTimeout = time.Duration(rv.ConnectTimeoutMS) * time.Millisecond
		}
		if rv.ClientTimeoutMS > 0 {
			t.idleClientSrv = time.Duration(rv.ClientTimeoutMS) * time.Millisecond
		}
		if rv.ServerTimeoutMS > 0 {
			t.idleOriginSrv = time.Duration(rv.ServerTimeoutMS) * time.Millisecond
		}
		switch rv.Mode {
		case model.ModeSNITLS:
			for _, d := range rv.Domains {
				key := strings.ToLower(strings.TrimSpace(d))
				if key == "" {
					continue
				}
				ns.sniRoutes[key] = t
			}
		case model.ModeTCPPort:
			l := model.Listener{Addr: dataplane.NormalizeBindAddr(rv.Route.EntryAddr), Port: rv.Route.EntryPort}
			ns.tcpRoutes[l.Key()] = t
		}
	}
	// tcpRoutes 里已经按监听键落好了，直接给到监听器用。
	return ns
}

func (r *GoRelay) bind(l model.Listener, kind string) (*boundListener, error) {
	addr := net.JoinHostPort(l.Addr, strconv.Itoa(l.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &boundListener{relay: r, key: l.Key(), ln: ln, kind: kind, port: l.Port, listenAddr: l.Addr}, nil
}

// ============================== 监听与转发 ==============================

type boundListener struct {
	relay      *GoRelay
	key        string
	ln         net.Listener
	kind       string
	port       int
	listenAddr string

	stopOnce sync.Once
	wg       sync.WaitGroup
}

func (b *boundListener) start() {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for {
			c, err := b.ln.Accept()
			if err != nil {
				return
			}
			b.wg.Add(1)
			go func() {
				defer b.wg.Done()
				b.handle(c)
			}()
		}
	}()
}

// stopAccepting 停止接受新连接，并等待已建立的连接自然结束。
// 注意：**不主动掐断已建立的连接** —— 这正是"常规规则更新不主动中断现有连接"。
func (b *boundListener) stopAccepting() {
	b.stopOnce.Do(func() { _ = b.ln.Close() })
}

func (b *boundListener) close() {
	b.stopAccepting()
	b.wg.Wait()
}

func (b *boundListener) handle(c net.Conn) {
	r := b.relay
	acceptTS := r.clock()
	connID := fmt.Sprintf("%s-%s-%d", r.nodeID, acceptTS.Format("20060102T150405.000"), r.seq.Add(1))

	r.totalConns.Add(1)
	r.activeConns.Add(1)
	defer r.activeConns.Add(-1)
	defer c.Close()

	clientIP, clientPort := splitHostPort(c.RemoteAddr())
	entryAddr, entryPort := b.listenAddr, b.port
	state := r.state.Load()

	var (
		sniName string
		prefix  []byte
		target  routeTarget
		found   bool
		failMsg string
	)

	if b.kind == "sni" {
		name, pf, err := sni.PeekSNI(c, r.inspectDelay)
		prefix = pf
		switch {
		case err == nil:
			sniName = name
			target, found = state.sniRoutes[strings.ToLower(name)]
			if !found {
				failMsg = "未匹配 SNI：" + name + " 不在本节点的路由表中（按策略拒绝）"
			}
		case errors.Is(err, sni.ErrNotTLS):
			failMsg = "入口收到非 TLS 流量，无法提取 SNI（SNI 入口只接受 TLS）"
		case errors.Is(err, sni.ErrNoSNI):
			failMsg = "客户端未发送 SNI（例如用 IP 直连），SNI 入口无法路由"
		default:
			failMsg = "读取 ClientHello 失败或超时：" + err.Error()
		}
	} else {
		target, found = state.tcpRoutes[b.key]
		if !found {
			failMsg = "该监听已无对应业务规则"
		}
	}

	if !found {
		r.noMatch.Add(1)
		b.emit(logRecord{
			acceptTS: acceptTS, connID: connID, ver: state.version,
			clientIP: clientIP, clientPort: clientPort,
			entryAddr: entryAddr, entryPort: entryPort,
			sni: sniName, be: haproxy.UnmatchedBackendName(b.port), srv: "-",
			termCode: "SC", termReason: failMsg,
			endTS: r.clock(),
		})
		return
	}

	// 拨号源站
	dialer := net.Dialer{}
	if target.connectTimeout > 0 {
		dialer.Timeout = target.connectTimeout
	} else {
		dialer.Timeout = 5 * time.Second
	}
	tDialStart := r.clock()
	up, err := dialer.Dial("tcp", net.JoinHostPort(target.originAddr, strconv.Itoa(target.originPort)))
	tConnected := r.clock()
	r.markHealth(target, err == nil)
	if err != nil {
		r.connErrors.Add(1)
		b.emit(logRecord{
			acceptTS: acceptTS, connID: connID, ver: state.version,
			clientIP: clientIP, clientPort: clientPort,
			entryAddr: entryAddr, entryPort: entryPort,
			sni: sniName, be: target.backend, srv: target.server,
			origin:    net.JoinHostPort(target.originAddr, strconv.Itoa(target.originPort)),
			connectMS: ms(tConnected.Sub(tDialStart)),
			termCode:  "SC", termReason: "源站连接失败：" + err.Error(),
			endTS: r.clock(),
		})
		return
	}
	defer up.Close()

	// 把已经读走的 ClientHello 前缀原样补发，否则源站收到的握手是残缺的。
	if len(prefix) > 0 {
		if _, werr := up.Write(prefix); werr != nil {
			r.connErrors.Add(1)
			b.emit(logRecord{
				acceptTS: acceptTS, connID: connID, ver: state.version,
				clientIP: clientIP, clientPort: clientPort,
				entryAddr: entryAddr, entryPort: entryPort,
				sni: sniName, be: target.backend, srv: target.server,
				origin:    net.JoinHostPort(target.originAddr, strconv.Itoa(target.originPort)),
				connectMS: ms(tConnected.Sub(tDialStart)),
				termCode:  "SD", termReason: "回放握手前缀失败：" + werr.Error(),
				endTS: r.clock(),
			})
			return
		}
	}

	// 双向转发。两个方向各一个 goroutine，任一方先结束就半关另一方的写方向，
	// 等两边都结束才算这个会话结束（TCP 日志本就在会话结束时才完整）。
	//
	// 字节数**边转发边累加到全局计数器**，而不是等会话结束再加一次：
	// 需求 §六.D 明确要求"实时流量和在线连接不得依赖结束日志计算"，
	// 一条挂了一天的长连接如果在结束时才计数，那它一整天都不会出现在吞吐曲线里。
	var bIn, bOut int64
	done := make(chan struct{}, 2)
	go func() {
		bIn = copyIdle(up, c, target.idleClientSrv, &r.bytesIn)
		closeWrite(up)
		done <- struct{}{}
	}()
	go func() {
		bOut = copyIdle(c, up, target.idleOriginSrv, &r.bytesOut)
		closeWrite(c)
		done <- struct{}{}
	}()
	<-done
	<-done

	b.emit(logRecord{
		acceptTS: acceptTS, connID: connID, ver: state.version,
		clientIP: clientIP, clientPort: clientPort,
		entryAddr: entryAddr, entryPort: entryPort,
		sni: sniName, be: target.backend, srv: target.server,
		origin:    net.JoinHostPort(target.originAddr, strconv.Itoa(target.originPort)),
		waitMS:    ms(tDialStart.Sub(acceptTS)),
		connectMS: ms(tConnected.Sub(tDialStart)),
		sessionMS: ms(r.clock().Sub(acceptTS)),
		bytesIn:   bIn, bytesOut: bOut,
		termCode: "CD", // 会话正常结束（HAProxy 语义：client done）
		endTS:    r.clock(),
	})
}

func (r *GoRelay) markHealth(t routeTarget, ok bool) {
	key := net.JoinHostPort(t.originAddr, strconv.Itoa(t.originPort))
	r.healthMu.Lock()
	r.health[key] = ok
	r.healthMu.Unlock()
}

func (r *GoRelay) Listeners(context.Context) ([]model.Listener, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]model.Listener, 0, len(r.active))
	for _, bl := range r.active {
		out = append(out, model.Listener{Addr: bl.listenAddr, Port: bl.port})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out, nil
}

func (r *GoRelay) Stats(context.Context) (dataplane.Stats, error) {
	up, down := 0, 0
	r.healthMu.Lock()
	for _, ok := range r.health {
		if ok {
			up++
		} else {
			down++
		}
	}
	r.healthMu.Unlock()
	r.mu.Lock()
	ver := r.ver
	r.mu.Unlock()
	return dataplane.Stats{
		At:              r.clock(),
		ActiveConns:     r.activeConns.Load(),
		TotalConns:      r.totalConns.Load(),
		BytesIn:         r.bytesIn.Load(),
		BytesOut:        r.bytesOut.Load(),
		ConnErrors:      r.connErrors.Load(),
		NoMatch:         r.noMatch.Load(),
		BackendUp:       int64(up),
		BackendDown:     int64(down),
		DataplaneVer:    ver,
		DataplaneDetail: "gorelay（纯 Go 转发内核）",
	}, nil
}

// Verify 验证应用结果：逐条确认期望监听确实在本进程内绑定。
func (r *GoRelay) Verify(ctx context.Context, req dataplane.VerifyRequest) (dataplane.VerifyResult, error) {
	r.mu.Lock()
	ver := r.ver
	active := make(map[string]bool, len(r.active))
	for k := range r.active {
		active[k] = true
	}
	r.mu.Unlock()

	res := dataplane.VerifyResult{DataplaneVersion: ver, ConfigHealthy: true}
	for _, l := range dataplane.ExpectedListeners(req.State) {
		st := dataplane.ListenerStatus{Expected: l, Bound: active[l.Key()], OwnedByUs: active[l.Key()]}
		if !st.Bound {
			st.Note = "期望监听但未绑定"
			res.Problems = append(res.Problems, l.Key()+" 未监听")
		}
		res.Listeners = append(res.Listeners, st)
	}
	res.OK = len(res.Problems) == 0
	res.Detail = dataplane.DescribeVerify(res)
	return res, nil
}

func (r *GoRelay) Close() error {
	r.closed.Store(true)
	r.mu.Lock()
	listeners := make([]*boundListener, 0, len(r.active))
	for _, bl := range r.active {
		listeners = append(listeners, bl)
	}
	r.active = map[string]*boundListener{}
	r.mu.Unlock()
	for _, bl := range listeners {
		bl.close()
	}
	return nil
}

// ============================== 日志输出 ==============================

// logRecord 是连接日志的内部表示。字段名与 HAProxy log-format 的 JSON 键一一对应，
// 取不到的字段输出 "-"（与 HAProxy 的不可用标记一致），这样下游解析只有一条代码路径。
type logRecord struct {
	acceptTS time.Time
	endTS    time.Time
	ver      int
	connID   string

	clientIP   string
	clientPort int
	entryAddr  string
	entryPort  int
	sni        string
	be         string
	srv        string
	origin     string

	waitMS    *int64
	connectMS *int64
	sessionMS *int64
	bytesIn   int64
	bytesOut  int64

	termCode   string
	termReason string
}

type wireLog struct {
	TS   string `json:"ts"`
	Node string `json:"node"`
	Ver  int    `json:"ver"`
	CID  string `json:"cid"`

	CI  string `json:"ci"`
	CP  string `json:"cp"`
	FI  string `json:"fi"`
	FP  string `json:"fp"`
	SNI string `json:"sni"`
	BE  string `json:"be"`
	SRV string `json:"srv"`

	TQ string `json:"tq"`
	TC string `json:"tc"`
	TT string `json:"tt"`
	// 两个方向分开写，与 haproxy.Registry 的 up/down 一致（评审 F11）：
	//   up   = bytes_uploaded = 客户端→源站
	//   down = bytes_read     = 源站→客户端
	UP   string `json:"up"`
	DOWN string `json:"down"`

	// 源站地址：HAProxy 对应 %si / %sp（target address），表示**实际连到了哪里**。
	OIP   string `json:"oip"`
	OPORT string `json:"oport"`

	TSC string `json:"tsc"`
	ERR string `json:"err"`
	RC  string `json:"rc"`
	BQ  string `json:"bq"`

	UID string `json:"uid"`
}

func (b *boundListener) emit(rec logRecord) {
	if b.relay.sink == nil {
		return
	}
	w := wireLog{
		TS:   rec.acceptTS.UTC().Format(haproxyDateLayout),
		Node: b.relay.nodeID,
		Ver:  rec.ver,
		CID:  rec.connID,
		CI:   rec.clientIP,
		CP:   numOrDash(rec.clientPort),
		FI:   rec.entryAddr,
		FP:   numOrDash(rec.entryPort),
		SNI:  orDash(rec.sni),
		BE:   orDash(rec.be),
		SRV:  orDash(rec.srv),
		TQ:   ptrOrDash(rec.waitMS),
		TC:   ptrOrDash(rec.connectMS),
		TT:   ptrOrDash(rec.sessionMS),
		// 字节数是**真实测量值**：0 字节是真实结果，不是"取不到"。
		// 所以这里始终输出数字，不能用 orDash 语义（那是 HAProxy 的不可用标记）。
		UP:   strconv.FormatInt(rec.bytesIn, 10),
		DOWN: strconv.FormatInt(rec.bytesOut, 10),
		TSC:  orDash(rec.termCode),
		ERR:  orDash(rec.termReason),
		// 重试与队列峰值：gorelay 不做重试也不排队，是**真实的 0**，不是"不可用"。
		RC: "0",
		BQ: "0",
		// 目标地址：对应 HAProxy 的 %si/%sp。gorelay 直接知道自己连到了哪里，
		// 所以这个字段是"实际观测值"而不是"按配置反查"。
		OIP:   orDash(hostOf(rec.origin)),
		OPORT: strconv.Itoa(portOf(rec.origin)),
		UID:   rec.connID,
	}
	line, err := json.Marshal(w)
	if err != nil {
		return
	}
	b.relay.sink(string(line) + "\n")
}

// haproxyDateLayout 必须与 logparse 的解析格式完全一致，否则 gorelay 产出的日志
// 在平台侧会因为时间解析失败而丢字段。
const haproxyDateLayout = "02/Jan/2006:15:04:05.000"

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func numOrDash(n int) string {
	if n == 0 {
		return "-"
	}
	return strconv.Itoa(n)
}

func ptrOrDash(p *int64) string {
	if p == nil {
		return "-"
	}
	return strconv.FormatInt(*p, 10)
}

func ms(d time.Duration) *int64 {
	v := d.Milliseconds()
	if v < 0 {
		v = 0
	}
	return &v
}

func splitHostPort(a net.Addr) (string, int) {
	if a == nil {
		return "", 0
	}
	h, p, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String(), 0
	}
	n, _ := strconv.Atoi(p)
	return h, n
}

func closeWrite(c net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

// copyIdle 转发数据，并在每个读操作前刷新读超时实现"空闲超时"。
// idle<=0 表示不设空闲超时（长连接业务常常长时间静默，误设会掐断正常连接）。
//
// total 参数是全局字节计数器，函数**每转发一块就累加一次**，
// 这样"还在进行中的长连接"也能立刻体现在实时指标里。
func copyIdle(dst net.Conn, src net.Conn, idle time.Duration, total *atomic.Int64) int64 {
	buf := make([]byte, 32*1024)
	var n64 int64
	for {
		if idle > 0 {
			_ = src.SetReadDeadline(time.Now().Add(idle))
		} else {
			_ = src.SetReadDeadline(time.Time{})
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return n64
			}
			n64 += int64(n)
			if total != nil {
				total.Add(int64(n))
			}
		}
		if err != nil {
			return n64
		}
	}
}

// hostOf / portOf 拆 "host:port"；拆不开时返回空与 0（调用方会写成 `-`）。
func hostOf(hostport string) string {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return ""
	}
	return h
}

func portOf(hostport string) int {
	_, p, err := net.SplitHostPort(hostport)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return 0
	}
	return n
}
