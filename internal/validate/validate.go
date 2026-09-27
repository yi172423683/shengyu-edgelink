// Package validate 做发布前的静态校验（需求 §四「严格校验所有输入，检测重复规则、监听冲突、
// 目标回环和不允许的目标地址」+ §五「语法与冲突检查」）。
//
// 设计原则：
//   - 校验是纯函数：只在内存里算，不碰文件、不碰网络、不碰数据库；
//   - 输出是可枚举的 Issue（带稳定 Code），便于界面按 Code 展示中文说明与修复建议；
//   - 有 error 就不发布；warning 只提示不阻断。
package validate

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/shengyu/edgelink/internal/id"
	"github.com/shengyu/edgelink/internal/model"
)

// Severity 问题级别。
type Severity string

const (
	// SevError 阻断发布。
	SevError Severity = "error"
	// SevWarning 只提示。
	SevWarning Severity = "warning"
)

// Issue 一条校验结论。
type Issue struct {
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Field    string   `json:"field"`
	Message  string   `json:"message"`
	Hint     string   `json:"hint,omitempty"` // 下一步怎么办
}

// Result 一次校验的全部结论。
type Result struct {
	Issues []Issue `json:"issues"`
}

// OK 表示没有 error（warning 允许通过）。
func (r Result) OK() bool {
	for _, i := range r.Issues {
		if i.Severity == SevError {
			return false
		}
	}
	return true
}

// Errors 只返回 error 级问题。
func (r Result) Errors() []Issue {
	var out []Issue
	for _, i := range r.Issues {
		if i.Severity == SevError {
			out = append(out, i)
		}
	}
	return out
}

func (r *Result) errf(code, field, format string, a ...any) {
	r.Issues = append(r.Issues, Issue{Severity: SevError, Code: code, Field: field, Message: fmt.Sprintf(format, a...)})
}

func (r *Result) warnf(code, field, format string, a ...any) {
	r.Issues = append(r.Issues, Issue{Severity: SevWarning, Code: code, Field: field, Message: fmt.Sprintf(format, a...)})
}

func (r *Result) hint(sel string, h string) {
	for i := range r.Issues {
		if r.Issues[i].Code == sel && r.Issues[i].Hint == "" {
			r.Issues[i].Hint = h
		}
	}
}

// 问题码（界面按码展示「为什么」和「怎么办」）。
const (
	CodeDomainSyntax        = "DOMAIN_SYNTAX"
	CodeDomainWildcard      = "DOMAIN_WILDCARD_UNSUPPORTED"
	CodeDomainNotIDN        = "DOMAIN_NEEDS_PUNYCODE"
	CodeDomainDuplicate     = "DOMAIN_DUPLICATE"
	CodeDomainRequired      = "DOMAIN_REQUIRED"
	CodeDomainNotAllowed    = "DOMAIN_NOT_ALLOWED_IN_MODE"
	CodeEntryRequired       = "ENTRY_REQUIRED"
	CodeEntryNotAllowed     = "ENTRY_NOT_ALLOWED_IN_MODE"
	CodeEntryPortRange      = "ENTRY_PORT_RANGE"
	CodeEntryPrivilegedPort = "ENTRY_PRIVILEGED_PORT"
	CodeListenConflict      = "LISTEN_CONFLICT"
	CodeSNIEntryMissing     = "SNI_ENTRY_MISSING"
	CodeSNIEntryDisabled    = "SNI_ENTRY_DISABLED"
	CodeSNIPortCollision    = "SNI_PORT_COLLISION"
	CodeOriginNotIP         = "ORIGIN_NOT_IP"
	CodeOriginHostSyntax    = "ORIGIN_HOST_SYNTAX"
	CodeOriginHostnameDNS   = "ORIGIN_HOSTNAME_DNS"
	CodeOriginPortRange     = "ORIGIN_PORT_RANGE"
	CodeOriginLoopback      = "ORIGIN_LOOPBACK"
	CodeOriginUnspecified   = "ORIGIN_UNSPECIFIED"
	CodeOriginMulticast     = "ORIGIN_MULTICAST"
	CodeOriginLinkLocal     = "ORIGIN_LINK_LOCAL"
	CodeOriginDenied        = "ORIGIN_DENIED"
	CodeOriginSelfReference = "ORIGIN_SELF_REFERENCE"
	CodeTimeoutTooSmall     = "TIMEOUT_TOO_SMALL"
	CodeTimeoutTooLarge     = "TIMEOUT_TOO_LARGE"
	CodeTimeoutWarnSmall    = "TIMEOUT_WARN_SMALL"
	CodeMaxConnInvalid      = "MAXCONN_INVALID"
	CodeQueueInvalid        = "QUEUE_INVALID"
	CodeHealthCheckInvalid  = "HEALTHCHECK_INVALID"
	CodeCapabilityMissing   = "CAPABILITY_MISSING"
	CodeHandleCollision     = "HANDLE_COLLISION"
	CodeModeMismatch        = "MODE_MISMATCH"
	CodeRoleInvalid         = "ROLE_INVALID"
	CodeNodeDisabled        = "NODE_DISABLED"
	CodePrimaryMissing      = "PRIMARY_MODE_DATA_MISSING"
)

// Options 校验参数。所有"可调策略"都在这里，不写死在规则里。
type Options struct {
	// DeniedOriginCIDRs 明确禁止的源站网段（额外于内置的硬禁网段）。
	DeniedOriginCIDRs []*net.IPNet
	// AllowPrivateOrigin 是否允许 RFC1918/ULA 作为源站。
	// 默认 true（国内源站常在内网），但会产出 warning。
	AllowPrivateOrigin bool
	// AllowLoopbackOrigin 是否允许回环地址（127.0.0.0/8、::1）作为源站。
	//
	// 生产**必须为 false**：回环源站意味着"中转节点自己转发给自己"，
	// 会形成自环，把节点打满。
	//
	// 允许显式打开的唯一场景是本地开发与端到端验收 ——
	// 那时源站与中转就在同一台机器上，用回环是自然的做法。
	// 之所以做成**显式开关**而不是"检测到测试环境就自动放行"：
	// 自动放行意味着生产上只要判断逻辑出一点偏差，自环就会重新变成可能。
	AllowLoopbackOrigin bool
	// MaxListenerInspect 由渲染层提供：本次渲染会占用的所有 (addr,port)，用于与系统既有监听做对比。
	// 为空表示不做这一步。
	MinClientTimeoutMS int // 默认 30000
	MaxClientTimeoutMS int // 默认 24h
}

// DefaultOptions 返回保守默认值。
func DefaultOptions() Options {
	return Options{
		AllowPrivateOrigin: true,
		MinClientTimeoutMS: 30000,
		MaxClientTimeoutMS: 24 * 60 * 60 * 1000,
	}
}

// Business 校验单条业务的基础字段（不含跨对象冲突）。
func Business(b model.Business, domains []string, opt Options) Result {
	var r Result
	if !b.Mode.Valid() {
		r.errf(CodeModeMismatch, "mode", "不支持的转发模式 %q（仅支持 %s / %s）", b.Mode, model.ModeSNITLS, model.ModeTCPPort)
	}

	// 域名
	switch b.Mode {
	case model.ModeSNITLS:
		if len(domains) == 0 {
			r.errf(CodeDomainRequired, "domains", "SNI 透传模式下必须至少填写一个域名")
			r.hint(CodeDomainRequired, "域名用于握手阶段的 SNI 精确匹配；未配置的 SNI 会被直接拒绝。")
		}
	case model.ModeTCPPort:
		if len(domains) > 0 {
			r.warnf(CodeDomainNotAllowed, "domains", "TCP 端口转发模式不使用域名，已填写的 %d 个域名会被忽略", len(domains))
		}
	}
	seen := map[string]bool{}
	for _, d := range domains {
		if err := Domain(d); err != nil {
			switch err.Code {
			case CodeDomainWildcard:
				r.errf(err.Code, "domains", "%s", err.Msg)
				r.hint(err.Code, "第一版只支持精确域名匹配。若确需泛域名，请在业务侧为每个子域各建一条业务。")
			case CodeDomainNotIDN:
				r.errf(err.Code, "domains", "%s", err.Msg)
				r.hint(err.Code, "请把中文域名转成 punycode（xn-- 前缀）后再填写。")
			default:
				r.errf(CodeDomainSyntax, "domains", "%s", err.Msg)
			}
			continue
		}
		if seen[d] {
			r.errf(CodeDomainDuplicate, "domains", "同一业务内域名重复：%s", d)
		}
		seen[d] = true
	}

	// 超时
	if b.ConnectTimeoutMS < 0 {
		r.errf(CodeTimeoutTooSmall, "connect_timeout_ms", "连接超时不能为负")
	}
	minT := opt.MinClientTimeoutMS
	if minT == 0 {
		minT = 30000
	}
	maxT := opt.MaxClientTimeoutMS
	if maxT == 0 {
		maxT = 24 * 60 * 60 * 1000
	}
	for _, f := range []struct {
		name string
		v    int
	}{{"client_timeout_ms", b.ClientTimeoutMS}, {"server_timeout_ms", b.ServerTimeoutMS}} {
		if f.v != 0 && f.v < minT {
			r.errf(CodeTimeoutTooSmall, f.name, "%s=%dms 小于下限 %dms", f.name, f.v, minT)
			r.hint(CodeTimeoutTooSmall, "TCP 透传下超时过小会掐断长连接，日志里会看到 timeout 但业务其实没有故障，极易误判。")
		}
		if f.v > maxT {
			r.errf(CodeTimeoutTooLarge, f.name, "%s=%dms 超过上限 %dms", f.name, f.v, maxT)
		}
		if f.v >= minT && f.v < 60000 {
			r.warnf(CodeTimeoutWarnSmall, f.name, "%s=%dms 偏小（<60s），可能影响长空闲连接", f.name, f.v)
			r.hint(CodeTimeoutWarnSmall, "建议 ≥ 60s，除非你确认业务侧全是短连接。")
		}
	}

	if b.MaxConn < 0 {
		r.errf(CodeMaxConnInvalid, "maxconn", "最大连接数不能为负")
	}
	if b.QueueLimit < 0 {
		r.errf(CodeQueueInvalid, "queue_limit", "排队上限不能为负")
	}
	r.HealthCheck(b.HealthCheck, "healthcheck")

	if b.PrimaryNodeID == "" {
		r.warnf(CodePrimaryMissing, "primary_node_id", "未选择主节点，这条业务不会被发布到任何节点")
	}
	if b.PrimaryNodeID != "" && b.BackupNodeID != "" && b.PrimaryNodeID == b.BackupNodeID {
		r.errf(CodeListenConflict, "backup_node_id", "主节点与备用节点不能是同一台")
	}
	return r
}

// HealthCheck 校验健康探测配置。
//
// 类型语义（评审 F09 之后明确下来，界面文案要与之一致）：
//
//	tcp   纯 TCP 连接探测
//	tls   TLS 握手探测（渲染成 check-ssl，**只加密探测**）
//	http  HTTP 探测，明文到源站
//	https HTTP 探测，且探测本身走 TLS（option httpchk + check-ssl）
//
// 不设 "tcp+tls" 这种模糊值：一个探测要么走 TLS、要么不走，没有第三种。
func (r *Result) HealthCheck(h model.HealthCheck, field string) {
	if !h.Enabled {
		return
	}
	if !h.ValidKind() || h.Kind == "" {
		r.errf(CodeHealthCheckInvalid, field+".kind",
			"探测类型 %q 不支持（tcp/tls/http/https）", h.Kind)
	}
	if h.IntervalMS < 1000 {
		r.errf(CodeHealthCheckInvalid, field+".interval_ms", "探测间隔 %dms 太短（下限 1000ms）", h.IntervalMS)
		r.hint(CodeHealthCheckInvalid, "过密的探测会被源站当成攻击，也会增加节点负载。")
	}
	if h.TimeoutMS <= 0 || h.TimeoutMS > h.IntervalMS {
		r.errf(CodeHealthCheckInvalid, field+".timeout_ms", "探测超时 %dms 必须 >0 且不超过间隔 %dms", h.TimeoutMS, h.IntervalMS)
	}
	if h.Rise < 1 || h.Fall < 1 {
		r.errf(CodeHealthCheckInvalid, field, "rise/fall 必须 ≥1")
	}
	if h.IsHTTP() && h.HTTPPath != "" && !strings.HasPrefix(h.HTTPPath, "/") {
		r.errf(CodeHealthCheckInvalid, field+".http_path", "HTTP 探测路径必须以 / 开头")
	}
	if h.ExpectStatus != 0 && (h.ExpectStatus < 100 || h.ExpectStatus > 599) {
		r.errf(CodeHealthCheckInvalid, field+".expect_status",
			"期望状态码 %d 不是合法 HTTP 状态码", h.ExpectStatus)
	}
	// 证书校验的提示方向与旧版相反，这是刻意的：透传场景下证书由**客户端**校验，
	// 中转节点只做活性探测。对自签/内网证书强行 verify 会把健康源站判死，
	// 所以默认（跳过校验）不需要警告，**打开校验**才需要提示可能的误判。
	if h.UsesTLS() && !h.SkipCertVerify {
		r.warnf(CodeHealthCheckInvalid, field+".skip_cert_verify",
			"已开启源站证书校验：若源站用的是自签或内网证书，探测会失败并把源站判为不可用（业务转发不受影响）")
	}
}

// DomainError 域名校验错误。
type DomainError struct {
	Code string
	Msg  string
}

func (e *DomainError) Error() string { return e.Msg }

// Domain 校验一个域名是否可用作 SNI 精确匹配。
//
// 规则：
//   - 总长 ≤253，label 1..63；
//   - 只允许 [a-z0-9-]，不能以 - 开头/结尾；
//   - 不接受通配符（第一版明确不支持，需求 §三）；
//   - 不接受 IP 字面量（SNI 是 hostname，不是 IP）；
//   - 不接受端口/协议/路径/大写（SNI 大小写不敏感，但我们统一归一化为小写后再存）；
//   - 非 ASCII（中文域名）要求先转 punycode。
func Domain(d string) *DomainError {
	if d == "" {
		return &DomainError{Code: CodeDomainSyntax, Msg: "域名不能为空"}
	}
	if strings.Contains(d, "*") {
		return &DomainError{Code: CodeDomainWildcard, Msg: fmt.Sprintf("域名 %q 含通配符，第一版不支持通配符域名规则", d)}
	}
	if strings.ContainsAny(d, ":/?#@") {
		return &DomainError{Code: CodeDomainSyntax, Msg: fmt.Sprintf("域名 %q 含非法字符（不能包含端口、协议或路径）", d)}
	}
	if d != strings.ToLower(d) {
		return &DomainError{Code: CodeDomainSyntax, Msg: fmt.Sprintf("域名 %q 必须使用小写", d)}
	}
	for _, r := range d {
		if r > 127 {
			return &DomainError{Code: CodeDomainNotIDN, Msg: fmt.Sprintf("域名 %q 含非 ASCII 字符", d)}
		}
	}
	if net.ParseIP(d) != nil {
		return &DomainError{Code: CodeDomainSyntax, Msg: fmt.Sprintf("%q 是 IP 字面量，SNI 必须是域名", d)}
	}
	if len(d) > 253 {
		return &DomainError{Code: CodeDomainSyntax, Msg: fmt.Sprintf("域名 %q 超过 253 字符", d)}
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return &DomainError{Code: CodeDomainSyntax, Msg: fmt.Sprintf("域名 %q 至少要有一个点（如 a.example.com）", d)}
	}
	for _, l := range labels {
		if l == "" {
			return &DomainError{Code: CodeDomainSyntax, Msg: fmt.Sprintf("域名 %q 含有空的标签（连续的点）", d)}
		}
		if len(l) > 63 {
			return &DomainError{Code: CodeDomainSyntax, Msg: fmt.Sprintf("域名 %q 的标签 %q 超过 63 字符", d, l)}
		}
		if strings.HasPrefix(l, "-") || strings.HasSuffix(l, "-") {
			return &DomainError{Code: CodeDomainSyntax, Msg: fmt.Sprintf("域名 %q 的标签 %q 不能以 - 开头或结尾", d, l)}
		}
		for _, r := range l {
			if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' {
				return &DomainError{Code: CodeDomainSyntax, Msg: fmt.Sprintf("域名 %q 的标签 %q 含非法字符 %q", d, l, string(r))}
			}
		}
	}
	return nil
}

// OriginHostname 校验"源站域名"这一写法是否合法。
//
// 与 Domain（SNI 域名）刻意分开：源站域名是给 HAProxy 去连的主机名，
// 语义上不要求是公网可解析的 SNI，规则相同但报错文案不同 ——
// 混用同一个校验会让"源站填错"报成"SNI 域名不合法"，排查方向直接跑偏。
func OriginHostname(h string) *DomainError {
	if h == "" {
		return &DomainError{Code: CodeOriginHostSyntax, Msg: "源站地址不能为空"}
	}
	if strings.ContainsAny(h, "*:/?#@ ") {
		return &DomainError{Code: CodeOriginHostSyntax,
			Msg: fmt.Sprintf("源站 %q 含非法字符（不能带通配符、协议、端口、路径或空格）", h)}
	}
	if h != strings.ToLower(h) {
		return &DomainError{Code: CodeOriginHostSyntax, Msg: fmt.Sprintf("源站 %q 必须使用小写", h)}
	}
	for _, r := range h {
		if r > 127 {
			return &DomainError{Code: CodeOriginHostSyntax, Msg: fmt.Sprintf("源站 %q 含非 ASCII 字符", h)}
		}
	}
	if len(h) > 253 {
		return &DomainError{Code: CodeOriginHostSyntax, Msg: fmt.Sprintf("源站 %q 超过 253 字符", h)}
	}
	if strings.HasSuffix(h, ".") {
		return &DomainError{Code: CodeOriginHostSyntax, Msg: fmt.Sprintf("源站 %q 不能以点结尾", h)}
	}
	labels := strings.Split(h, ".")
	for _, l := range labels {
		if l == "" {
			return &DomainError{Code: CodeOriginHostSyntax, Msg: fmt.Sprintf("源站 %q 含有空的标签（连续的点）", h)}
		}
		if len(l) > 63 {
			return &DomainError{Code: CodeOriginHostSyntax, Msg: fmt.Sprintf("源站 %q 的标签 %q 超过 63 字符", h, l)}
		}
		if strings.HasPrefix(l, "-") || strings.HasSuffix(l, "-") {
			return &DomainError{Code: CodeOriginHostSyntax, Msg: fmt.Sprintf("源站 %q 的标签 %q 不能以 - 开头或结尾", h, l)}
		}
		for _, r := range l {
			if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' {
				return &DomainError{Code: CodeOriginHostSyntax,
					Msg: fmt.Sprintf("源站 %q 的标签 %q 含非法字符 %q", h, l, string(r))}
			}
		}
	}
	return nil
}

// Origin 校验源站地址。
//
// 接受三种写法：**IPv4、IPv6、域名**。
//
// 硬禁（任何策略下都不允许）：未指定地址、回环、组播、链路本地、广播。
// 策略禁：Options.DeniedOriginCIDRs；以及源站等于本节点入口 IP（会形成自环转发）。
func Origin(host string, port int, opt Options, nodeIPs ...string) Result {
	var r Result
	if host == "" {
		r.errf(CodeOriginNotIP, "origin_host", "源站地址不能为空")
		return r
	}
	if port < 1 || port > 65535 {
		r.errf(CodeOriginPortRange, "origin_port", "源站端口 %d 超出 1..65535", port)
	}

	host = strings.TrimSpace(host)
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		// 不是 IP —— 按域名源站处理。
		//
		// 这里刻意**不**把域名当成错误。中转节点确实不做二次 DNS 解析，
		// 但"填域名"是运维最自然的表达，把它一律拒掉只会逼用户在源站 IP 变化后
		// 手工维护一份影子记录。代价是：HAProxy 只在**启动时**解析一次主机名，
		// 所以必须把这个语义明确讲出来（见下面的 warning），而不是让用户事后踩坑。
		if err := OriginHostname(host); err != nil {
			r.errf(CodeOriginHostSyntax, "origin_host", "%s", err.Msg)
			r.hint(CodeOriginHostSyntax, "源站可填 IPv4、IPv6 或域名；域名请写完整主机名（如 origin.example.com），不要带 http:// 或端口。")
			return r
		}
		r.warnf(CodeOriginHostnameDNS, "origin_host",
			"源站 %s 是域名：HAProxy 只在启动/重载时解析一次，DNS 记录变更后必须重新发布才会生效", host)
		r.hint(CodeOriginHostnameDNS,
			"若源站 IP 经常变动，要么改用固定 IP，要么在 DNS 记录变更后手动执行一次「发布」让解析重新生效。")
		// 域名无法做网段/自环判定（解析结果未知），到此为止。
		return r
	}

	switch {
	case ip.IsUnspecified():
		r.errf(CodeOriginUnspecified, "origin_host", "源站不能是未指定地址（0.0.0.0 / ::）")
	case ip.IsLoopback():
		if opt.AllowLoopbackOrigin {
			// 只有显式开启时才放行，并且**必须留下 warning** ——
			// 即使放行，这也是"源站与中转同机"的信号，运维应当知道。
			r.warnf(CodeOriginLoopback, "origin_host",
				"源站是回环地址（%s）：仅当源站与中转节点在同一台机器上时才可接受", ip)
		} else {
			r.errf(CodeOriginLoopback, "origin_host", "源站不能是回环地址（%s）", ip)
			r.hint(CodeOriginLoopback, "回环地址意味着「源站=中转节点自己」，会造成自环转发。"+
				"（本地开发/验收场景可用 -allow-loopback-origin 显式放行）")
		}
	case ip.IsMulticast():
		r.errf(CodeOriginMulticast, "origin_host", "源站不能是组播地址（%s）", ip)
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
		r.errf(CodeOriginLinkLocal, "origin_host", "源站不能是链路本地地址（%s）", ip)
	}

	for _, n := range opt.DeniedOriginCIDRs {
		if n != nil && n.Contains(ip) {
			r.errf(CodeOriginDenied, "origin_host", "源站 %s 命中禁止网段 %s", ip, n.String())
		}
	}

	if opt.AllowPrivateOrigin {
		for _, cidr := range privateOriginCIDRs {
			if _, n, err := net.ParseCIDR(cidr); err == nil && n.Contains(ip) {
				r.warnf(CodeOriginDenied, "origin_host",
					"源站 %s 属于内网网段 %s：若这不是客户内网回源，请确认不是填错", ip, cidr)
				break
			}
		}
	} else {
		for _, cidr := range privateOriginCIDRs {
			if _, n, err := net.ParseCIDR(cidr); err == nil && n.Contains(ip) {
				r.errf(CodeOriginDenied, "origin_host", "当前策略不允许内网源站：%s 属于 %s", ip, cidr)
			}
		}
	}

	for _, nip := range nodeIPs {
		if nip == "" {
			continue
		}
		// 显式允许回环源站的场景（本地开发/验收）下，回环地址必然等于节点 IP，
		// 此时自环判定无意义 —— 但只对**回环**豁免，公网地址的自环检查照旧。
		if opt.AllowLoopbackOrigin && ip.IsLoopback() {
			continue
		}
		if p := net.ParseIP(strings.Trim(nip, "[]")); p != nil && p.Equal(ip) {
			r.errf(CodeOriginSelfReference, "origin_host",
				"源站 %s 与所在节点的公网 IP 相同，会造成自环转发", ip)
			r.hint(CodeOriginSelfReference, "请确认客户源站是不是误填了中转节点的地址。")
		}
	}
	return r
}

var privateOriginCIDRs = []string{
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7", "100.64.0.0/10",
}

// State 对一台节点的完整期望状态做校验（含跨路由冲突）。
//
// 这一步是「监听冲突」「重复规则」「对象名碰撞」的唯一真理来源。
func State(st model.DesiredState, opt Options) Result {
	var r Result

	if !st.Node.Enabled {
		r.errf(CodeNodeDisabled, "node", "节点 %s 已停用，不能发布", st.Node.Name)
	}

	sniEntries := map[string]model.SNIEntry{}
	for _, e := range st.SNIEntries {
		sniEntries[e.ID] = e
	}

	// 监听占用表：key = addr:port -> 占用者描述
	type owner struct {
		what string
		key  string
	}
	taken := map[string]owner{}

	// 1) 先登记 SNI 共享入口
	for _, e := range st.SNIEntries {
		if !e.Enabled {
			continue
		}
		k := model.Listener{Addr: e.BindAddr, Port: e.BindPort}.Key()
		if o, ok := taken[k]; ok {
			r.errf(CodeListenConflict, "node_sni_entries",
				"SNI 入口 %s 与 %s 占用同一监听 %s", e.ID, o.key, k)
			continue
		}
		taken[k] = owner{"sni_entry", e.ID}
	}

	// 2) 域名按入口唯一：同一节点、同一 SNI 入口上的同一域名不能重复。
	// 同一个域名可以挂在不同入口端口，例如 443 与 8444。
	domainOwner := map[string]string{}
	handleOwner := map[string]string{}

	for j, v := range st.Routes {
		prefix := fmt.Sprintf("routes[%d]", j)
		rt := v.Route

		if !v.Enabled || !rt.Enabled {
			continue
		}
		if !rt.Role.Valid() {
			r.errf(CodeRoleInvalid, prefix+".role", "角色 %q 非法（primary/backup）", rt.Role)
		}
		if v.Mode != "" && rt.Mode != "" && v.Mode != rt.Mode {
			r.errf(CodeModeMismatch, prefix+".mode", "路由模式 %q 与业务模式 %q 不一致", rt.Mode, v.Mode)
		}

		// 对象名手柄碰撞：两条业务若手柄相同会生成同名 backend，HAProxy 直接报重复。
		h := id.Handle(rt.BusinessID)
		if !id.HandleValid(h) {
			r.errf(CodeHandleCollision, prefix, "业务 %s 生成的对象名手柄非法：%q", rt.BusinessID, h)
		}
		if prev, ok := handleOwner[h]; ok && prev != rt.BusinessID {
			r.errf(CodeHandleCollision, prefix,
				"业务 %s 与 %s 生成相同对象名手柄 %q，会导致 HAProxy backend 重名", rt.BusinessID, prev, h)
		}
		handleOwner[h] = rt.BusinessID

		switch rt.Mode {
		case model.ModeSNITLS:
			if rt.SNIEntryID == "" {
				r.errf(CodeSNIEntryMissing, prefix+".sni_entry_id", "SNI 模式必须指定该节点的共享 SNI 入口")
				break
			}
			e, ok := sniEntries[rt.SNIEntryID]
			if !ok {
				r.errf(CodeSNIEntryMissing, prefix+".sni_entry_id", "引用的 SNI 入口 %s 不存在", rt.SNIEntryID)
				break
			}
			if e.NodeID != st.Node.ID {
				r.errf(CodeSNIEntryMissing, prefix+".sni_entry_id", "SNI 入口 %s 不属于节点 %s", e.ID, st.Node.ID)
			}
			if !e.Enabled {
				r.errf(CodeSNIEntryDisabled, prefix+".sni_entry_id", "SNI 入口 %s 已停用", e.ID)
			}
			if rt.EntryAddr != "" || rt.EntryPort != 0 {
				r.errf(CodeEntryNotAllowed, prefix+".entry_port",
					"SNI 模式的入口来自共享 SNI 入口（%s），不能单独指定入口地址/端口", e.ID)
				r.hint(CodeEntryNotAllowed, "多域名共享 443 是设计约束；单独指定入口端口会让 SNI 分流失去意义。")
			}
			if len(v.Domains) == 0 {
				r.errf(CodeDomainRequired, prefix+".domains", "SNI 模式必须至少一个域名")
			}
			for _, d := range v.Domains {
				if err := Domain(d); err != nil {
					r.errf(CodeDomainSyntax, prefix+".domains", "%s", err.Msg)
					continue
				}
				ownerKey := rt.SNIEntryID + "\x00" + d
				if prev, ok := domainOwner[ownerKey]; ok {
					r.errf(CodeDomainDuplicate, prefix+".domains",
						"域名 %s 已被 %s 使用：同一 SNI 入口上同一域名只能属于一条业务", d, prev)
					continue
				}
				domainOwner[ownerKey] = rt.BusinessID
			}

		case model.ModeTCPPort:
			if rt.EntryAddr == "" || rt.EntryPort == 0 {
				r.errf(CodeEntryRequired, prefix+".entry_port", "TCP 端口转发模式必须指定入口地址与端口")
				break
			}
			if rt.EntryPort < 1 || rt.EntryPort > 65535 {
				r.errf(CodeEntryPortRange, prefix+".entry_port", "入口端口 %d 超出 1..65535", rt.EntryPort)
				break
			}
			if rt.EntryPort < 1024 {
				r.warnf(CodeEntryPrivilegedPort, prefix+".entry_port",
					"入口端口 %d 是特权端口（<1024），需要 haproxy 拥有 CAP_NET_BIND_SERVICE", rt.EntryPort)
				r.hint(CodeEntryPrivilegedPort, "要么改用 ≥1024 的端口，要么给 unit 加 AmbientCapabilities=CAP_NET_BIND_SERVICE。")
			}
			k := model.Listener{Addr: rt.EntryAddr, Port: rt.EntryPort}.Key()
			if o, ok := taken[k]; ok {
				r.errf(CodeListenConflict, prefix+".entry_port",
					"监听 %s 已被 %s 占用", k, o.key)
				r.hint(CodeListenConflict, "同一个 (地址, 端口) 只能被一条启用的 TCP 业务占用；SNI 入口端口也不能被复用。")
			} else {
				taken[k] = owner{"tcp_route", rt.ID}
			}
			if len(v.Domains) > 0 {
				r.warnf(CodeDomainNotAllowed, prefix+".domains", "TCP 模式不使用域名，已忽略 %d 个域名", len(v.Domains))
			}

		default:
			r.errf(CodeModeMismatch, prefix+".mode", "不支持的模式 %q", rt.Mode)
		}

		// 源站
		r.merge(Origin(rt.OriginHost, rt.OriginPort, opt, st.Node.PublicIPv4, st.Node.PublicIPv6))

		// 端口撞车：TCP 模式入口与源站相同不算错，但几乎肯定是填错
		if rt.Mode == model.ModeTCPPort && rt.EntryPort == rt.OriginPort &&
			rt.EntryAddr == rt.OriginHost {
			r.warnf(CodeOriginSelfReference, prefix+".origin_port",
				"入口与源站完全相同（%s:%d），这会形成自环转发", rt.EntryAddr, rt.EntryPort)
		}

		if v.EffectiveMaxConn < 0 {
			r.errf(CodeMaxConnInvalid, prefix+".maxconn", "maxconn 不能为负")
		}
		if v.EffectiveQueueLimit < 0 {
			r.errf(CodeQueueInvalid, prefix+".queue_limit", "queue_limit 不能为负")
		}
		r.HealthCheck(v.HealthCheck, prefix+".healthcheck")

		// 能力矩阵前置校验（需求 §十二.1：备用节点缺有效配置/能力不得切流）
		if ok, why := st.Node.Capabilities.Supports(rt.Mode); !ok {
			r.errf(CodeCapabilityMissing, "node.capabilities",
				"节点 %s 不能承载该业务：%s", st.Node.Name, why)
			r.hint(CodeCapabilityMissing, "在节点管理页跑一次 HAProxy 能力探针，确认版本与所需特性；不支持就不要把这类业务发到这台节点上。")
		}
	}

	// 端口冲突提示排序稳定，便于测试与人工阅读
	sort.SliceStable(r.Issues, func(i, j int) bool {
		if r.Issues[i].Severity != r.Issues[j].Severity {
			return r.Issues[i].Severity == SevError
		}
		return r.Issues[i].Code < r.Issues[j].Code
	})
	return r
}

func (r *Result) merge(o Result) { r.Issues = append(r.Issues, o.Issues...) }

// Merge 把另一组校验结论并入本结果（导出给 API 层跨业务校验使用）。
func (r *Result) Merge(o Result) { r.merge(o) }

// DomainsAcrossBusinesses 检查域名是否被其他业务占用（跨业务的全局唯一性）。
// 数据库上用 UNIQUE(domain) 兜底，这里给出可读的报错。
func DomainsAcrossBusinesses(businessID string, domains []string, ownerOf map[string]string) Result {
	var r Result
	for _, d := range domains {
		if owner, ok := ownerOf[d]; ok && owner != businessID {
			r.errf(CodeDomainDuplicate, "domains",
				"域名 %s 已被业务 %s 占用；同一域名不能同时挂两条业务", d, owner)
			r.hint(CodeDomainDuplicate,
				"如果确实是同一业务的 A/B 双节点，请不要新建业务，而是给现有业务添加备用节点。")
		}
	}
	return r
}

// DomainsAcrossBusinessesForEntry 检查同一节点上的 SNI 入口域名冲突。
// ownerOf 的 key 为 sniEntryID + NUL + domain。
func DomainsAcrossBusinessesForEntry(businessID, sniEntryID string, domains []string, ownerOf map[string]string) Result {
	var r Result
	for _, d := range domains {
		key := sniEntryID + "\x00" + d
		if owner, ok := ownerOf[key]; ok && owner != businessID {
			r.errf(CodeDomainDuplicate, "domains",
				"域名 %s 已被业务 %s 占用：同一入口上同一域名不能同时挂两条业务", d, owner)
			r.hint(CodeDomainDuplicate,
				"如果要让同一域名使用多个源站端口，请为每个端口创建不同的数据面入口（例如 443、8444）。")
		}
	}
	return r
}
