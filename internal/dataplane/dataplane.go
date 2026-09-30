// Package dataplane 抽象"转发内核"。
//
// 本包只提供**一个**生产实现：haproxy.go（HAProxy 社区版）。
// 需求 §二.1 规定转发内核统一使用 HAProxy 社区版，这条约束在代码层面有三重落实：
//
//  1. 本包不再包含任何其它转发实现；
//  2. 主程序（cmd/shengyu-edgelink-server）的 -dataplane 只接受 haproxy，
//     并且有专门的测试（main_test.go）钉住这条约束；
//  3. 那个纯 Go 的转发实现被放在**独立包** internal/testplane 里，
//     只被自动化测试 import。Go 只链接被 import 的包，
//     因此生产二进制里**根本不存在**这段代码，也就无所谓"被误开成降级内核"。
//
// 接口之所以仍然存在（而不是让 publish 直接依赖 HAProxy），是为了让发布流水线、
// 日志链路、诊断、版本回滚这些**与内核无关**的逻辑可以被测试覆盖 ——
// 测试用 testplane 这个辅助实现替身，生产用 haproxy。
package dataplane

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shengyu/edgelink/internal/haproxy"
	"github.com/shengyu/edgelink/internal/model"
)

// LogSink 接收一行连接日志。格式与 HAProxy 的 log-format 输出**同构**（同一个 JSON 键集合），
// 这样 logparse 不需要知道这行是哪个内核产生的。
//
// **刻意不返回 error**：需求 §八 明确要求"日志系统故障不得阻塞业务转发"。
// 如果 sink 能返回错误，调用点就必然要写出"失败了怎么办"的分支，
// 而唯一正确的处理就是"记下并继续转发"。与其留一个会被误用的返回值，
// 不如把这条纪律固化在类型里：sink 自己负责计数与上报，绝不向转发路径回抛。
type LogSink func(line string)

// ApplyRequest 一次配置应用请求。
type ApplyRequest struct {
	// State 是目标期望状态（含全部启用路由与 SNI 入口）。
	State model.DesiredState
	// Version 目标版本号。
	Version int
	// ConfigDir 该版本的目录（已渲染好配置文件）。
	ConfigDir string
	// ConfigPath 配置文件路径。
	ConfigPath string
	// PreviousVersion 上一生效版本，0 表示没有。
	PreviousVersion int
}

// VerifyRequest 应用后验证请求。
type VerifyRequest struct {
	State   model.DesiredState
	Version int
	// ProbeTimeout 单个监听端口的探测超时。
	ProbeTimeout time.Duration
}

// ListenerStatus 期望监听与实际监听的比对结果。
type ListenerStatus struct {
	// 地址与端口在**嵌套**的 Expected 里（它不是"这一段监听的属性"，而是"期望长什么样"）。
	//
	// 这一组 json 标签是补上的：原先只有 Frontend 有标签，其余字段按 Go 默认规则
	// 序列化成 `Expected` / `Bound` / `OwnedByUs` / `Note` —— 于是界面里
	// `l.addr` 永远读到 undefined，发布成功的气泡显示成"监听：undefined:undefined"。
	// 真机 dump 里 `(None, None, None, None)` 就是这个问题的症状（见 docs/08 §9.3）。
	Expected model.Listener `json:"expected"`
	// Frontend 该监听在配置里对应的 frontend 名。
	// 验证"这个端口是不是**我们的**进程在听"就靠它 —— 端口有人听 ≠ 我们的服务在听。
	Frontend string `json:"frontend,omitempty"`
	// Bound 表示当前确有进程在监听该 地址:端口。
	Bound bool `json:"bound"`
	// OwnedByUs 表示已**确证**监听者就是本数据面里该版本的 frontend。
	// 无法确证时必须是 false —— 宁可报"未通过验证"，也不能报一个漂亮的假成功。
	OwnedByUs bool   `json:"owned_by_us"`
	Note      string `json:"note,omitempty"`
}

// VerifyResult 应用结果验证（需求 §五：验证全部预期监听端口和对应服务）。
type VerifyResult struct {
	OK               bool             `json:"ok"`
	DataplaneVersion int              `json:"dataplane_version"`
	Listeners        []ListenerStatus `json:"listeners"`
	Problems         []string         `json:"problems"`
	Detail           string           `json:"detail"`
	// ConfigHealthy 表示"配置本身应用成功"。
	// 与 OK 的区别：OK=false 且 ConfigHealthy=true 意味着
	// 「配置发布成功，但源站业务本来就不可用」——需求明确要求区分这两种情况。
	ConfigHealthy bool `json:"config_healthy"`

	// 以下三条是"验证通过"的**三个独立判据**，逐条暴露出来。
	//
	// 为什么要暴露而不是让调用方从 Problems 那段中文里猜：上层（发布流水线）
	// 必须能回答"这次回滚到底验证了什么、哪一条没成立"。把结论塞进一段散文里，
	// 界面就只能二值化地显示"成功/失败"，而运维真正需要的是"哪一条判据没成立"。
	// 三条全部为 true 才可能有 OK=true；任何一条无法确认都必须是 false（fail-closed）。
	VersionMarkerOK bool   `json:"version_marker_ok"`
	StatsSocketOK   bool   `json:"stats_socket_ok"`
	StatsSocketPath string `json:"stats_socket_path,omitempty"`
	ListenersOK     bool   `json:"listeners_ok"`
}

// FileSetDiff 描述 current/ 与某个版本目录之间的差异。
//
// 这是"回滚到底恢复完整了没有"的判据：只看 VERSION 是不够的 ——
// 最危险的一种"看起来回滚成功"就是 VERSION 写回了旧版号，而配置正文仍是新的。
type FileSetDiff struct {
	VersionDir string `json:"version_dir"`
	// Same 逐字节一致的文件数（用于报告"几个文件都对上了"）。
	Same int `json:"same"`
	// Missing 版本目录有、current/ 没有。
	Missing []string `json:"missing,omitempty"`
	// Extra current/ 有、版本目录没有（陈旧残留）。发布时会按目标版本清单清掉。
	Extra []string `json:"extra,omitempty"`
	// Mismatched 两边都有但内容不同。
	Mismatched []string `json:"mismatched,omitempty"`
	// ManifestMissing 表示该版本目录没有文件清单（升级前发布的旧版本）。
	// 这不是错误，但它意味着"完整文件集合"只能靠目录列举来推断。
	ManifestMissing bool `json:"manifest_missing,omitempty"`
	// VersionDirExtra 版本目录里有、但清单没列的普通文件（"多余文件"）。
	//
	// 与 Extra 的区别是**在哪一侧多出来**：
	//   - Extra：current/ 多出来的陈旧残留 ⇒ 必须清掉（否则 current/ 会显示
	//     一个看起来生效、实际无人引用的允许清单）；
	//   - VersionDirExtra：版本目录多出来的（清单生成后目录被人为改过，例如运维
	//     手工 `cp haproxy.cfg haproxy.cfg.orig`）⇒ **不阻断**发布与回滚。
	//
	// 为什么 VersionDirExtra 不阻断（评审决定）：
	// 这类文件根本不会被写进 current/（写入以清单为准），对正确性没有影响；
	// 而把它当硬错误会**在事故中挡住回滚** —— 回滚恰好是最不能拖延的时候。
	// 但它必须被看得见：它是"这个目录被改过"的唯一信号，所以会进 warning 与审计。
	// 唯一例外是"配置真的引用了它"，那时由 publishFiles 直接拒绝（那是另一回事：
	// 未纳入清单的配置会影响运行）。
	VersionDirExtra []string `json:"version_dir_extra,omitempty"`
}

// OK 表示 current/ 与版本目录逐字节一致（含"没有多余文件"）。
func (d FileSetDiff) OK() bool {
	return len(d.Missing) == 0 && len(d.Extra) == 0 && len(d.Mismatched) == 0
}

// Describe 生成人可读的差异说明，直接进失败面板。
func (d FileSetDiff) Describe() string {
	if d.OK() {
		return fmt.Sprintf("%d 个文件逐字节一致", d.Same)
	}
	var parts []string
	if len(d.Missing) > 0 {
		parts = append(parts, "缺少 "+strings.Join(d.Missing, ", "))
	}
	if len(d.Mismatched) > 0 {
		parts = append(parts, "内容不一致 "+strings.Join(d.Mismatched, ", "))
	}
	if len(d.Extra) > 0 {
		parts = append(parts, "多余残留 "+strings.Join(d.Extra, ", "))
	}
	return strings.Join(parts, "；")
}

// FileSetInspector 是"以文件为准"的数据面提供的**可选**能力。
//
// 它回答的问题是：`current/` 是不是已经**完整**变成了某一版的样子。
// 为什么必须单独有这条判据：判断"回滚是否成功"不能只看 reload 命令返回码，
// 也不能只看 VERSION 文件 —— 还必须确认配置正文与它引用的允许清单整套都换回来了。
// 只恢复了 VERSION 而正文没换，是"看起来回滚成功"里最危险的一种
// （磁盘标记说是旧版，进程重启后加载的却是没获批准的新正文）。
//
// 不实现它的数据面（例如纯 Go 的测试替身）不适用这条判据 ——
// 那要由调用方明确记为"不适用"，而不是静默当作通过。
type FileSetInspector interface {
	CompareCurrentToVersion(ctx context.Context, versionDir string) (FileSetDiff, error)
}

// Stats 实时指标（需求 §六.D：实时流量与在线连接不得依赖结束日志计算）。
type Stats struct {
	At              time.Time `json:"at"`
	ActiveConns     int64     `json:"active_conns"`
	TotalConns      int64     `json:"total_conns"`
	BytesIn         int64     `json:"bytes_in"`
	BytesOut        int64     `json:"bytes_out"`
	ConnErrors      int64     `json:"conn_errors"`
	NoMatch         int64     `json:"no_match"`
	QueueDepth      int64     `json:"queue_depth"`
	BackendUp       int64     `json:"backend_up"`
	BackendDown     int64     `json:"backend_down"`
	DataplaneVer    int       `json:"dataplane_version"`
	DataplaneDetail string    `json:"dataplane_detail"`
}

// ClientTraffic 是 HAProxy stick table 中某个客户端 IP 的实时计数。
type ClientTraffic struct {
	ClientIP          string `json:"client_ip"`
	ActiveConnections int64  `json:"active_connections"`
	BytesUpRate       int64  `json:"bytes_up_rate"`
	BytesDownRate     int64  `json:"bytes_down_rate"`
}

// ClientTrafficReporter 是可选的运行态能力，供管理页面展示每个客户端 IP 的实时流量。
type ClientTrafficReporter interface {
	ClientTraffic(context.Context) ([]ClientTraffic, error)
}

// Applier 转发内核的统一接口。
//
// 契约（发布流水线依赖这些不变量，实现方必须遵守）：
//   - Apply 在**任何**失败路径上都不得破坏当前已生效的转发；
//     先起新监听、成功了再拆旧监听，不允许先关后开；
//   - Verify 只做只读探测，不修改任何状态；
//   - Stats 必须能反映"尚未结束的长连接"，不能只看已收尾的会话。
type Applier interface {
	// Name 实现名。生产恒为 haproxy；测试替身用别的名字。
	Name() string
	// Capabilities 探测能力矩阵，发布前据此做前置校验。
	Capabilities(ctx context.Context) (model.Capabilities, error)
	// Validate 对**候选**配置做语法与冲突检查，不改变任何运行状态。
	// 这是"错误配置不破坏现有服务"的第一道闸门：检查不过就根本不会走到 Apply。
	Validate(ctx context.Context, cfgPath string) error
	// Apply 应用目标状态。
	Apply(ctx context.Context, req ApplyRequest) error
	// Verify 验证应用结果。
	Verify(ctx context.Context, req VerifyRequest) (VerifyResult, error)
	// Listeners 返回当前实际监听。
	Listeners(ctx context.Context) ([]model.Listener, error)
	// Stats 返回实时指标。
	Stats(ctx context.Context) (Stats, error)
	// ActiveVersion 返回当前生效的配置版本。
	ActiveVersion(ctx context.Context) (int, error)
	// Close 释放资源（关监听等）。实现方保证优雅关闭，不掐断在途连接。
	Close() error
}

// ListenerKey 监听地址的规范化键。统一成 host:port 便于集合比较。
func ListenerKey(l model.Listener) string {
	return l.Key()
}

// sniEntriesInUse 返回"确实会产出监听"的 SNI 入口 ID 集合。
//
// **这条口径必须与渲染器严格一致**（haproxy.Render 里 `if len(rs) == 0 { continue }`，
// 注释原文："空入口不下发：避免占用端口却没有规则（未配置的 SNI 会被拒，
// 但没有必要占用资源）"）。
//
// 为什么不一致会造成真事故（真机实测，见 docs/08 第 6 节）：
// 先用 API 建了一个 9443 入口、但业务没建成功 ⇒ 该入口是"空"的 ⇒
// 渲染出的配置里**故意没有** 9443 的 frontend（这是设计行为），
// 而这里却把 9443 算进"应当监听"，于是 Verify 判"0.0.0.0:9443 未监听"，
// 发布白等 30.1 秒（重试 61 次）后失败，并触发一次**完全没有必要**的回滚。
// 后果不是"更严格更安全"，而是：一次假失败 + 一次假回滚 + 运维去查一个不存在的故障。
//
// 同一个集合也要喂给 expectedFrontends，否则"该监听哪些端口"与"这些端口该是哪个
// frontend 在听"会来自两个不同的口径。
func sniEntriesInUse(st model.DesiredState) map[string]bool {
	used := map[string]bool{}
	for _, rv := range st.Routes {
		if !rv.Enabled || rv.Mode != model.ModeSNITLS || rv.Route.SNIEntryID == "" {
			continue
		}
		used[rv.Route.SNIEntryID] = true
	}
	return used
}

// ExpectedListeners 从期望状态推导出该节点应当监听的 地址:端口 集合（需求 §五）。
//
// 这是"验证全部预期监听端口"的输入，也是"端口冲突不破坏现有服务"的检测依据。
// 规则（与渲染器逐条对齐，见 sniEntriesInUse）：
//   - SNI 入口：每条启用且**至少有一条启用路由**的 SNIEntry 一个监听（通常是 *:443，多业务共享）；
//     空入口不监听（渲染器就不下发它）；
//   - TCP 模式：每条启用的路由一个独立监听（入口地址:端口）。
func ExpectedListeners(st model.DesiredState) []model.Listener {
	set := map[string]model.Listener{}
	inUse := sniEntriesInUse(st)
	for _, e := range st.SNIEntries {
		if !e.Enabled || !inUse[e.ID] {
			continue
		}
		l := model.Listener{Addr: NormalizeBindAddr(e.BindAddr), Port: e.BindPort}
		set[l.Key()] = l
	}
	for _, rv := range st.Routes {
		if !rv.Enabled || rv.Mode != model.ModeTCPPort {
			continue
		}
		l := model.Listener{Addr: NormalizeBindAddr(rv.Route.EntryAddr), Port: rv.Route.EntryPort}
		set[l.Key()] = l
	}
	return sortedListeners(set)
}

// NormalizeBindAddr 把绑定地址规范化。
//
// 导出是必要的：测试辅助数据面（internal/testplane）需要与生产数据面用**同一套**
// 地址规范化规则，否则同一份配置在两个实现下会得出不同的监听集合。
func NormalizeBindAddr(a string) string {
	if a == "" || a == "*" {
		return "0.0.0.0"
	}
	return a
}

// expectedFrontends 返回「监听键 → 该监听在配置里对应的 frontend 名」。
//
// 存在的意义：验证不能只问"这个端口有没有人在听"，而要问"听的是不是我们这一版配置的 frontend"。
// 端口冲突（别人的程序先占了 443）与"新 worker 没起来"这两种情况，
// 在只看端口时都表现为"端口在监听"，只有按 frontend 名核对才区分得出来。
func expectedFrontends(st model.DesiredState) map[string]string {
	out := map[string]string{}
	inUse := sniEntriesInUse(st)
	for _, e := range st.SNIEntries {
		if !e.Enabled || !inUse[e.ID] {
			continue
		}
		l := model.Listener{Addr: NormalizeBindAddr(e.BindAddr), Port: e.BindPort}
		out[l.Key()] = haproxy.SNIFrontendName(e.BindPort)
	}
	for _, rv := range st.Routes {
		if !rv.Enabled || rv.Mode != model.ModeTCPPort {
			continue
		}
		l := model.Listener{Addr: NormalizeBindAddr(rv.Route.EntryAddr), Port: rv.Route.EntryPort}
		out[l.Key()] = haproxy.TCPFrontendName(rv.Route.BusinessID)
	}
	return out
}

// DefaultVerifyTimeout 单端口探测默认超时。
const DefaultVerifyTimeout = 2 * time.Second

// Describeverify 生成人类可读的验证摘要，直接给诊断界面用。
func DescribeVerify(v VerifyResult) string {
	if v.OK {
		return fmt.Sprintf("版本 %d 已生效，%d 个监听全部就绪", v.DataplaneVersion, len(v.Listeners))
	}
	if v.ConfigHealthy {
		return fmt.Sprintf("版本 %d 配置已生效，但存在以下问题：%s", v.DataplaneVersion, joinProblems(v.Problems))
	}
	return fmt.Sprintf("版本 %d 未通过验证：%s", v.DataplaneVersion, joinProblems(v.Problems))
}

func joinProblems(p []string) string {
	if len(p) == 0 {
		return "（无细节）"
	}
	out := p[0]
	for _, s := range p[1:] {
		out += "；" + s
	}
	return out
}
