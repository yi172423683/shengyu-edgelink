// Package model 定义全平台领域对象。
//
// 约定：本包只有数据结构与常量，没有任何业务逻辑与 I/O；所有校验在 validate 包，
// 所有渲染在 haproxy 包。这样模型可以同时被 mgr、agent、CLI 复用而不引入依赖。
package model

import (
	"net"
	"strconv"
	"time"
)

// Mode 转发模式（需求 §三）。
type Mode string

const (
	// ModeSNITLS：TLS SNI 透传，多域名共享一个入口 IP:443，不终止业务 TLS。
	ModeSNITLS Mode = "sni_tls"
	// ModeTCPPort：TCP 端口转发，入口 IP:端口 固定映射到源站 IP:端口。
	ModeTCPPort Mode = "tcp_port"
)

// Valid 判断模式是否受支持。
func (m Mode) Valid() bool { return m == ModeSNITLS || m == ModeTCPPort }

// Role 业务在某节点上的角色（需求 §十二.1）。
type Role string

const (
	RolePrimary Role = "primary"
	RoleBackup  Role = "backup"
)

// Valid 判断角色是否合法。
func (r Role) Valid() bool { return r == RolePrimary || r == RoleBackup }

// NodeHealth 节点健康（管理面视角，聚合 agent 心跳与能力探针结果）。
type NodeHealth string

const (
	NodeUnknown  NodeHealth = "unknown"
	NodeOnline   NodeHealth = "online"
	NodeOffline  NodeHealth = "offline"
	NodeDegraded NodeHealth = "degraded" // agent 在线但能力不全（如不支持平滑发布）
)

// NodeHealthDetail 把"节点健康"拆成三个**互相独立**的维度（评审 F10）。
//
// 为什么不能只留一个 Health 字段：一次故障里，"agent 进程还活着吗""HAProxy 在跑吗"
// "业务路径（监听 + 源站）通吗"这三件事的答案经常不一致，而它们的处置方式完全不同：
//
//	Agent offline + Proxy online  → 管理面失联，但转发正常（不需要叫醒运维去救业务）
//	Agent online  + Proxy offline → 真正的事故：转发可能已经断了
//	Path missing                  → 期望端口没在监听：配置没生效或被别的进程占了
//
// 只报一个聚合值的话，运维看到"离线"会以为业务挂了，而实际可能只是心跳没上来。
type NodeHealthDetail struct {
	// Agent 本机运行时/agent 进程维度。
	Agent NodeHealth `json:"agent"`
	// Proxy 转发内核维度（HAProxy 进程与统计接口是否可达）。
	Proxy NodeHealth `json:"proxy"`
	// Path 业务路径维度（期望监听是否都在、配置版本是否与期望一致）。
	Path NodeHealth `json:"path"`
	// Note 一句话说明判定的依据，避免界面只给颜色不给理由。
	Note string `json:"note,omitempty"`
	// At 本次采样的时刻。
	At time.Time `json:"at"`
}

// Aggregate 把三个维度聚合成一个总状态。
//
// 规则刻意保守：**只要 Proxy 离线就一定不是 online**（转发是这套系统的存在意义）；
// Proxy 活着而 Path 有问题时报 degraded 而不是 offline —— 转发本身还行，
// 但配置没完全生效，需要人看一眼。
//
// 空字符串一律按 unknown 处理：记录还没被写过、或某个维度暂时采不到时，
// 界面应该显示"未知"，而不是被当成"降级"（那会让人以为已经出事了）。
func (d NodeHealthDetail) Aggregate() NodeHealth {
	a, p, path := norm(d.Agent), norm(d.Proxy), norm(d.Path)
	switch {
	case a == NodeUnknown && p == NodeUnknown && path == NodeUnknown:
		return NodeUnknown
	case a == NodeOffline && p == NodeOnline:
		// 管理面失联但转发在跑：这**不是**故障，标 degraded 提醒去看一眼。
		return NodeDegraded
	case p == NodeOffline:
		return NodeOffline
	case p == NodeOnline && path == NodeOnline:
		return NodeOnline
	default:
		return NodeDegraded
	}
}

func norm(h NodeHealth) NodeHealth {
	if h == "" {
		return NodeUnknown
	}
	return h
}

// Describe 生成人话描述（界面与诊断包共用）。
func (d NodeHealthDetail) Describe() string {
	if d.Note != "" {
		return d.Note
	}
	return "agent=" + string(d.Agent) + "，转发内核=" + string(d.Proxy) + "，业务路径=" + string(d.Path)
}

// Customer 客户（需求 §四）。
type Customer struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Contact   string    `json:"contact"`
	Remark    string    `json:"remark"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// HealthCheck 可选的健康探测配置（需求 §四）。
//
// 注意：这是**中转节点 → 源站**的探测，结论不能冒充真实用户请求日志，
// 也不能冒充终端用户所在地区的体验（需求 §六.B）。
//
// ⚠️ 语义边界（评审 F09）：这里的配置**只影响探测**，绝不允许影响业务传输。
// 这个类型被渲染成 HAProxy 的 `check` / `check-ssl` / `check-sni` / `option httpchk`，
// 而**不会**被渲染成 server 行的 `ssl` / `sni` / `verify` —— 后三者管的是真实代理连接，
// 写在透传场景里等于给客户的 TLS 流量再套一层 TLS。详见 haproxy.renderBackend 的注释。
type HealthCheck struct {
	Enabled      bool   `json:"enabled"`
	Kind         string `json:"kind"`          // tcp | tls | http | https
	IntervalMS   int    `json:"interval_ms"`   // HAProxy inter
	TimeoutMS    int    `json:"timeout_ms"`    // 渲染成 backend 的 timeout check（server 行没有 timeout 参数）
	Rise         int    `json:"rise"`          // 连续成功多少次标记为 up
	Fall         int    `json:"fall"`          // 连续失败多少次标记为 down
	SNI          string `json:"sni,omitempty"` // tls/https 探测使用的 SNI；为空则用业务域名
	HTTPPath     string `json:"http_path,omitempty"`
	ExpectStatus int    `json:"expect_status,omitempty"` // 为空表示任意 2xx/3xx
	// SkipCertVerify 是否跳过**探测**时的证书校验。
	//
	// 默认 true（跳过）。理由：本平台是 TLS 透传，证书信任的判定权在客户端；
	// 中转节点做的事只是"源站还活着吗"。对自签/内网证书强行 verify 会把健康源站
	// 判死，反而制造故障。运维确需校验证书时才把它设为 false。
	SkipCertVerify bool `json:"skip_cert_verify"`
}

// IsHTTP 该探测是否需要走 HTTP 请求（option httpchk）。
func (h HealthCheck) IsHTTP() bool { return h.Kind == "http" || h.Kind == "https" }

// UsesTLS 该探测本身是否需要 TLS（check-ssl）。**只作用于探测**。
func (h HealthCheck) UsesTLS() bool { return h.Kind == "tls" || h.Kind == "https" }

// ValidKind 探测类型是否受支持。
func (h HealthCheck) ValidKind() bool {
	switch h.Kind {
	case "", "tcp", "tls", "http", "https":
		return true
	}
	return false
}

// DefaultHealthCheck 返回保守默认值。
//
// 默认关闭、且默认跳过证书校验 —— 理由见 SkipCertVerify 的说明。
func DefaultHealthCheck() HealthCheck {
	return HealthCheck{
		Kind: "tcp", IntervalMS: 5000, TimeoutMS: 3000, Rise: 2, Fall: 3,
		SkipCertVerify: true,
	}
}

// Business 一条业务（需求 §四 + §十二.1 的双节点绑定）。
type Business struct {
	ID         string `json:"id"`
	CustomerID string `json:"customer_id"`
	Name       string `json:"name"`
	Remark     string `json:"remark"`
	Mode       Mode   `json:"mode"`
	Enabled    bool   `json:"enabled"`

	// 超时（毫秒）。0 表示使用节点默认值（见 haproxy.Defaults）。
	ConnectTimeoutMS int `json:"connect_timeout_ms"`
	ClientTimeoutMS  int `json:"client_timeout_ms"`
	ServerTimeoutMS  int `json:"server_timeout_ms"`

	// 可选连接数及排队限制。0 表示不限。
	MaxConn    int `json:"maxconn"`
	QueueLimit int `json:"queue_limit"`

	HealthCheck HealthCheck `json:"healthcheck"`

	// 双节点绑定（需求 §十二.1）。BackupNodeID 为空表示未配置备用节点。
	PrimaryNodeID string `json:"primary_node_id"`
	BackupNodeID  string `json:"backup_node_id"`

	Domains   []string  `json:"domains"` // SNI 模式必填（精确匹配，不支持通配符）
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Route 一条业务在一台节点上的转发规则 —— 这是渲染 HAProxy 配置的输入单元。
type Route struct {
	ID         string `json:"id"`
	BusinessID string `json:"business_id"`
	NodeID     string `json:"node_id"`
	Role       Role   `json:"role"`
	// Mode 与所属业务的 Mode 冗余存储：渲染与校验都要用它，
	// 冗余一份可以少一次查业务表，且能在校验时检出"路由模式与业务模式不一致"这种脏数据。
	Mode Mode `json:"mode"`

	// SNI 模式：指向该节点的共享 SNI 入口（默认 *:443）。
	SNIEntryID string `json:"sni_entry_id,omitempty"`

	// TCP 模式：显式入口。
	EntryAddr string `json:"entry_addr,omitempty"`
	EntryPort int    `json:"entry_port,omitempty"`

	OriginHost string `json:"origin_host"` // 源站 IP（不接受域名）
	OriginPort int    `json:"origin_port"`
	OriginSNI  string `json:"origin_sni,omitempty"` // 可选：向源站发起的 SNI 覆盖

	QueueLimit int `json:"queue_limit"`
	MaxConn    int `json:"maxconn"`

	Enabled            bool `json:"enabled"`
	LastAppliedVersion int  `json:"last_applied_version"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SNIEntry 节点上的一处共享 SNI 入口（需求 §三 模式 A：多域名共享一个入口 IP:443）。
type SNIEntry struct {
	ID        string    `json:"id"`
	NodeID    string    `json:"node_id"`
	BindAddr  string    `json:"bind_addr"`
	BindPort  int       `json:"bind_port"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// Capabilities 节点的 HAProxy 能力矩阵（由 agent 的功能探针产出，见 docs/04 §5）。
//
// 发布前必须据此做前置校验：某节点不支持 SNI 捕获，就永远不允许把 sni_tls 业务发上去。
type Capabilities struct {
	Version              string    `json:"version"`       // haproxy -vv 解析出的版本
	Supported            bool      `json:"supported"`     // 版本在受支持范围内
	MasterWorker         bool      `json:"master_worker"` // 支持平滑 reload
	ExposeFDListeners    bool      `json:"expose_fd_listeners"`
	SNICapture           bool      `json:"sni_capture"` // set-var(txn.sni) + req_ssl_sni
	JSONEscape           bool      `json:"json_escape"` // 结构化日志转义
	UnixDgramLog         bool      `json:"unix_dgram_log"`
	UnixDgramLogFallback bool      `json:"unix_dgram_log_fallback"` // 退化到 UDP
	ProbeError           string    `json:"probe_error,omitempty"`
	ProbedAt             time.Time `json:"probed_at"`
}

// Supports 判断该节点能否承载给定模式。
func (c Capabilities) Supports(m Mode) (bool, string) {
	if !c.Supported {
		return false, "HAProxy 版本不在受支持范围内（" + c.Version + "）"
	}
	if !c.MasterWorker {
		return false, "HAProxy 不支持 master-worker，无法平滑发布"
	}
	if m == ModeSNITLS && !c.SNICapture {
		return false, "HAProxy 不支持握手期 SNI 捕获（set-var + req_ssl_sni），不能发布 SNI 业务"
	}
	return true, ""
}

// Node 一台中转节点。
type Node struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	GroupName       string     `json:"group_name"`
	AgentEndpoint   string     `json:"agent_endpoint"`
	PublicIPv4      string     `json:"public_ipv4"`
	PublicIPv6      string     `json:"public_ipv6"`
	Region          string     `json:"region"`
	AgentVersion    string     `json:"agent_version"`
	HAProxyVersion  string     `json:"haproxy_version"`
	AppliedVersion  int        `json:"applied_version"`  // 节点实际生效版本（agent 上报）
	ExpectedVersion int        `json:"expected_version"` // 管理面期望版本（发布记录推出）
	LastHeartbeat   time.Time  `json:"last_heartbeat"`
	Health          NodeHealth `json:"health"`
	// HealthDetail 三个维度的健康明细（评审 F10）。
	HealthDetail NodeHealthDetail `json:"health_detail"`
	Capabilities Capabilities     `json:"capabilities"`
	Enabled      bool             `json:"enabled"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`
}

// VersionSkewed 表示期望版本与实际版本不一致（需求 §五：后台显示两者）。
func (n Node) VersionSkewed() bool { return n.ExpectedVersion != n.AppliedVersion }

// Listener 一个期望监听的地址:端口。
type Listener struct {
	Addr string `json:"addr"`
	Port int    `json:"port"`
}

// Key 用于集合比较。
func (l Listener) Key() string {
	return net.JoinHostPort(l.Addr, strconv.Itoa(l.Port))
}

// DesiredState 一次渲染的完整输入：一台节点 + 它上面所有启用的路由。
type DesiredState struct {
	Node       Node
	Routes     []RouteView
	SNIEntries []SNIEntry
	Version    int // 本次要发布的版本号（>=1）
	// PublishedAt 用于写入 meta.json；零值表示由调用方填充。
	PublishedAt time.Time
}

// RouteView 是 Route 的渲染视图：把业务级字段与路由级字段合并后的结果，
// 避免渲染器再去查一遍业务表。
type RouteView struct {
	Route               Route
	BusinessName        string
	Mode                Mode
	Domains             []string
	ConnectTimeoutMS    int
	ClientTimeoutMS     int
	ServerTimeoutMS     int
	EffectiveMaxConn    int
	EffectiveQueueLimit int
	HealthCheck         HealthCheck
	Enabled             bool
}
