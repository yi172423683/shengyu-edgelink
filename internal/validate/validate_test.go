package validate

import (
	"net"
	"strings"
	"testing"

	"github.com/shengyu/edgelink/internal/model"
)

func codes(r Result) []string {
	var out []string
	for _, i := range r.Issues {
		out = append(out, string(i.Severity)+":"+i.Code)
	}
	return out
}

func hasCode(r Result, sev Severity, code string) bool {
	for _, i := range r.Issues {
		if i.Severity == sev && i.Code == code {
			return true
		}
	}
	return false
}

func TestDomainRules(t *testing.T) {
	cases := []struct {
		name string
		in   string
		code string
		ok   bool
	}{
		{"正常域名", "a.example.com", "", true},
		{"多级域名", "edge.cdn.example.co.jp", "", true},
		{"含数字与短横", "cdn-01.example.com", "", true},
		{"通配符被拒", "*.example.com", CodeDomainWildcard, false},
		{"大写被拒", "A.example.com", CodeDomainSyntax, false},
		{"带端口被拒", "a.example.com:443", CodeDomainSyntax, false},
		{"带协议被拒", "https://a.example.com", CodeDomainSyntax, false},
		{"IP 字面量被拒", "203.0.113.5", CodeDomainSyntax, false},
		{"中文域名要 punycode", "中文.example.com", CodeDomainNotIDN, false},
		{"单标签被拒", "localhost", CodeDomainSyntax, false},
		{"连续点被拒", "a..example.com", CodeDomainSyntax, false},
		{"标签以横线开头被拒", "-a.example.com", CodeDomainSyntax, false},
		{"空被拒", "", CodeDomainSyntax, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Domain(c.in)
			if c.ok {
				if err != nil {
					t.Fatalf("应当通过，实际报错 %s: %s", err.Code, err.Msg)
				}
				return
			}
			if err == nil {
				t.Fatalf("%q 应当被拒绝", c.in)
			}
			if err.Code != c.code {
				t.Fatalf("期望错误码 %s，实际 %s（%s）", c.code, err.Code, err.Msg)
			}
		})
	}
}

func TestOriginHardDenies(t *testing.T) {
	opt := DefaultOptions()
	cases := []struct {
		host string
		code string
	}{
		{"127.0.0.1", CodeOriginLoopback},
		{"::1", CodeOriginLoopback},
		{"0.0.0.0", CodeOriginUnspecified},
		{"224.0.0.1", CodeOriginMulticast},
		{"169.254.10.10", CodeOriginLinkLocal},
		// 域名源站**不再**被一律拒绝（见 TestOriginAllowsHostname），
		// 但写法不合法的仍然要拦：通配符、带协议/端口/路径都会被 HAProxy 解析错。
		{"*.example.com", CodeOriginHostSyntax},
		{"http://a.example.com", CodeOriginHostSyntax},
		{"a.example.com:443", CodeOriginHostSyntax},
		{"源站.example.com", CodeOriginHostSyntax},
	}
	for _, c := range cases {
		r := Origin(c.host, 443, opt)
		if !hasCode(r, SevError, c.code) {
			t.Fatalf("源站 %s 期望 %s，实际 %v", c.host, c.code, codes(r))
		}
	}
}

// 源站允许三种写法：IPv4、IPv6、域名。
//
// IPv6 之前是隐性 bug：渲染层用 "%s:%d" 拼 server 行，IPv6 会拼成
// `2001:db8::1:443` —— 端口被吃掉。所以这里既校验"能存进去"，
// 也在渲染测试里校验"拼出来是 [2001:db8::1]:443"。
func TestOriginAllowsHostname(t *testing.T) {
	opt := DefaultOptions()
	for _, h := range []string{"a.example.com", "origin.internal.example.com", "2001:db8::1", "10.0.0.5"} {
		r := Origin(h, 443, opt)
		if r.Issues != nil && !r.OK() {
			t.Fatalf("源站 %s 应当被接受，实际被拒：%v", h, codes(r))
		}
	}
	// IPv6 允许但不能带方括号以外的怪写法
	if r := Origin("[2001:db8::1]", 443, opt); !r.OK() {
		t.Fatalf("带方括号的 IPv6 也应被接受，实际 %v", codes(r))
	}
}

// 域名源站必须给出"只在启动时解析一次"的 warning ——
// 否则用户改了 DNS 会以为自动生效，实际上要重新发布。
func TestOriginHostnameWarnsStartupResolve(t *testing.T) {
	r := Origin("a.example.com", 443, DefaultOptions())
	if !hasCode(r, SevWarning, CodeOriginHostnameDNS) {
		t.Fatalf("域名源站应提示启动期解析语义，实际 %v", codes(r))
	}
	if !r.OK() {
		t.Fatal("域名源站应当是 warning 而不是 error")
	}
}

// 源站等于所在节点公网 IP 会形成自环转发，必须拦。
func TestOriginSelfReference(t *testing.T) {
	r := Origin("203.0.113.10", 443, DefaultOptions(), "203.0.113.10")
	if !hasCode(r, SevError, CodeOriginSelfReference) {
		t.Fatalf("期望检测到自环，实际 %v", codes(r))
	}
}

// RFC1918 默认允许（国内源站常在内网）但必须给 warning，不能静默。
func TestOriginPrivateWarns(t *testing.T) {
	r := Origin("10.1.2.3", 8080, DefaultOptions())
	if !hasCode(r, SevWarning, CodeOriginDenied) {
		t.Fatalf("内网源站应给 warning，实际 %v", codes(r))
	}
	if hasCode(r, SevError, CodeOriginDenied) {
		t.Fatal("默认策略下内网源站不应是 error")
	}

	strict := DefaultOptions()
	strict.AllowPrivateOrigin = false
	if !hasCode(Origin("10.1.2.3", 8080, strict), SevError, CodeOriginDenied) {
		t.Fatal("严格策略下内网源站必须是 error")
	}
}

func TestOriginDeniedCIDR(t *testing.T) {
	opt := DefaultOptions()
	n := mustCIDR(t, "198.51.100.0/24")
	opt.DeniedOriginCIDRs = append(opt.DeniedOriginCIDRs, n)
	r := Origin("198.51.100.9", 443, opt)
	if !hasCode(r, SevError, CodeOriginDenied) {
		t.Fatalf("命中自定义禁止网段应报错，实际 %v", codes(r))
	}
}

func TestBusinessTimeoutRules(t *testing.T) {
	opt := DefaultOptions()
	b := model.Business{Mode: model.ModeSNITLS, PrimaryNodeID: "nd_1", ClientTimeoutMS: 1000}
	r := Business(b, []string{"a.example.com"}, opt)
	if !hasCode(r, SevError, CodeTimeoutTooSmall) {
		t.Fatalf("1s 客户端超时应被拒（会掐断长连接），实际 %v", codes(r))
	}
	if i := findIssue(r, SevError, CodeTimeoutTooSmall); i != nil && i.Hint == "" {
		t.Fatal("超时类错误必须带「怎么办」提示")
	}

	b.ClientTimeoutMS = 45000
	r = Business(b, []string{"a.example.com"}, opt)
	if hasCode(r, SevError, CodeTimeoutTooSmall) {
		t.Fatalf("45s 应可接受，实际 %v", codes(r))
	}
	if !hasCode(r, SevWarning, CodeTimeoutWarnSmall) {
		t.Fatalf("45s 应给「偏小」warning，实际 %v", codes(r))
	}

	b.ClientTimeoutMS = 300000
	if hasCode(Business(b, []string{"a.example.com"}, opt), SevError, CodeTimeoutWarnSmall) {
		t.Fatal("5 分钟不应再报偏小")
	}
}

func TestBusinessModeRequirements(t *testing.T) {
	opt := DefaultOptions()

	// SNI 模式必须有域名
	r := Business(model.Business{Mode: model.ModeSNITLS, PrimaryNodeID: "nd_1"}, nil, opt)
	if !hasCode(r, SevError, CodeDomainRequired) {
		t.Fatalf("SNI 模式缺域名应报错，实际 %v", codes(r))
	}

	// SNI 模式域名重复
	r = Business(model.Business{Mode: model.ModeSNITLS, PrimaryNodeID: "nd_1"}, []string{"a.example.com", "a.example.com"}, opt)
	if !hasCode(r, SevError, CodeDomainDuplicate) {
		t.Fatalf("重复域名应报错，实际 %v", codes(r))
	}

	// TCP 模式填了域名 → warning 而非 error
	r = Business(model.Business{Mode: model.ModeTCPPort, PrimaryNodeID: "nd_1"}, []string{"a.example.com"}, opt)
	if hasCode(r, SevError, CodeDomainNotAllowed) {
		t.Fatal("TCP 模式填域名只应警告")
	}
	if !hasCode(r, SevWarning, CodeDomainNotAllowed) {
		t.Fatalf("TCP 模式填域名应给 warning，实际 %v", codes(r))
	}

	// 主备同机
	r = Business(model.Business{Mode: model.ModeTCPPort, PrimaryNodeID: "nd_1", BackupNodeID: "nd_1"}, nil, opt)
	if !hasCode(r, SevError, CodeListenConflict) {
		t.Fatalf("主备同一节点应报错，实际 %v", codes(r))
	}
}

func stateFixture() model.DesiredState {
	return model.DesiredState{
		Node: model.Node{
			ID: "nd_A", Name: "relay-a", Enabled: true, PublicIPv4: "203.0.113.10",
			Capabilities: model.Capabilities{Version: "2.8.5", Supported: true, MasterWorker: true, SNICapture: true},
		},
		SNIEntries: []model.SNIEntry{{ID: "sni_443", NodeID: "nd_A", BindAddr: "0.0.0.0", BindPort: 443, Enabled: true}},
		Version:    3,
	}
}

// 需求 §四：检测重复规则、监听冲突。
func TestStateListenConflicts(t *testing.T) {
	st := stateFixture()
	mk := func(biz, addr string, port int) model.RouteView {
		return model.RouteView{
			Route: model.Route{
				ID: "rt_" + biz, BusinessID: biz, NodeID: "nd_A", Role: model.RolePrimary,
				Mode: model.ModeTCPPort, EntryAddr: addr, EntryPort: port,
				OriginHost: "198.51.100.7", OriginPort: 8080, Enabled: true,
			},
			Mode: model.ModeTCPPort, Enabled: true,
		}
	}
	st.Routes = []model.RouteView{mk("biz_1", "0.0.0.0", 9443), mk("biz_2", "0.0.0.0", 9443)}
	r := State(st, DefaultOptions())
	if !hasCode(r, SevError, CodeListenConflict) {
		t.Fatalf("两条 TCP 业务占同一端口应报错，实际 %v", codes(r))
	}

	// TCP 入口占用 SNI 入口端口 → 也必须冲突
	st.Routes = []model.RouteView{mk("biz_1", "0.0.0.0", 443)}
	if !hasCode(State(st, DefaultOptions()), SevError, CodeListenConflict) {
		t.Fatal("TCP 入口占用 443（已被 SNI 入口占用）应报错")
	}
}

// 同一节点上同一域名只能属于一条业务，否则会出现"串流"。
func TestStateDomainDuplication(t *testing.T) {
	st := stateFixture()
	mk := func(biz string, domains ...string) model.RouteView {
		return model.RouteView{
			Route: model.Route{
				ID: "rt_" + biz, BusinessID: biz, NodeID: "nd_A", Role: model.RolePrimary,
				Mode: model.ModeSNITLS, SNIEntryID: "sni_443",
				OriginHost: "198.51.100.7", OriginPort: 443, Enabled: true,
			},
			Mode: model.ModeSNITLS, Domains: domains, Enabled: true,
		}
	}
	st.Routes = []model.RouteView{mk("biz_1", "a.example.com"), mk("biz_2", "a.example.com")}
	if !hasCode(State(st, DefaultOptions()), SevError, CodeDomainDuplicate) {
		t.Fatal("同一节点同一域名挂两条业务应报错")
	}
}

// 需求 §五 + §十二.1：不支持 SNI 捕获的节点不能承载 SNI 业务。
func TestStateCapabilityGate(t *testing.T) {
	st := stateFixture()
	st.Node.Capabilities.SNICapture = false
	st.Routes = []model.RouteView{{
		Route: model.Route{
			ID: "rt_1", BusinessID: "biz_1", NodeID: "nd_A", Role: model.RolePrimary,
			Mode: model.ModeSNITLS, SNIEntryID: "sni_443",
			OriginHost: "198.51.100.7", OriginPort: 443, Enabled: true,
		},
		Mode: model.ModeSNITLS, Domains: []string{"a.example.com"}, Enabled: true,
	}}
	r := State(st, DefaultOptions())
	if !hasCode(r, SevError, CodeCapabilityMissing) {
		t.Fatalf("节点不支持 SNI 捕获时不应允许发布 SNI 业务，实际 %v", codes(r))
	}
	if i := findIssue(r, SevError, CodeCapabilityMissing); i != nil && i.Hint == "" {
		t.Fatal("能力不足必须给「怎么办」提示")
	}
}

func TestStateEntryModeMismatch(t *testing.T) {
	st := stateFixture()
	st.Routes = []model.RouteView{{
		Route: model.Route{
			ID: "rt_1", BusinessID: "biz_1", NodeID: "nd_A", Role: model.RolePrimary,
			Mode: model.ModeSNITLS, SNIEntryID: "sni_443",
			EntryAddr: "0.0.0.0", EntryPort: 8443, // SNI 模式不允许单独指定入口
			OriginHost: "198.51.100.7", OriginPort: 443, Enabled: true,
		},
		Mode: model.ModeSNITLS, Domains: []string{"a.example.com"}, Enabled: true,
	}}
	r := State(st, DefaultOptions())
	if !hasCode(r, SevError, CodeEntryNotAllowed) {
		t.Fatalf("SNI 模式单独指定入口应报错，实际 %v", codes(r))
	}
	if i := findIssue(r, SevError, CodeEntryNotAllowed); i != nil && i.Hint == "" {
		t.Fatal("应解释为什么不允许（多域名共享 443 是设计约束）")
	}
}

func TestStateValidPasses(t *testing.T) {
	st := stateFixture()
	st.Routes = []model.RouteView{{
		Route: model.Route{
			ID: "rt_1", BusinessID: "biz_1", NodeID: "nd_A", Role: model.RolePrimary,
			Mode: model.ModeSNITLS, SNIEntryID: "sni_443",
			OriginHost: "198.51.100.7", OriginPort: 443, Enabled: true,
		},
		Mode: model.ModeSNITLS, Domains: []string{"a.example.com"}, Enabled: true,
		ClientTimeoutMS: 300000, ServerTimeoutMS: 300000,
	}}
	r := State(st, DefaultOptions())
	if !r.OK() {
		t.Fatalf("合法配置不应有 error，实际 %v", codes(r))
	}
}

func TestStateObjectNameCollision(t *testing.T) {
	st := stateFixture()
	// 两个不同业务 ID 但 Handle 相同（只保留 [a-z0-9] 并截断后一致）
	mk := func(id, snie string) model.RouteView {
		return model.RouteView{
			Route: model.Route{
				ID: "rt_" + id, BusinessID: id, NodeID: "nd_A", Role: model.RolePrimary,
				Mode: model.ModeSNITLS, SNIEntryID: snie, OriginHost: "198.51.100.7", OriginPort: 443, Enabled: true,
			},
			Mode: model.ModeSNITLS, Domains: []string{strings.ToLower(id) + ".example.com"}, Enabled: true,
		}
	}
	st.Routes = []model.RouteView{mk("biz_AAAA-1111", "sni_443"), mk("biz_AAAA1111", "sni_443")}
	if !hasCode(State(st, DefaultOptions()), SevError, CodeHandleCollision) {
		t.Fatalf("对象名手柄碰撞应被拦下，实际 %v", codes(State(st, DefaultOptions())))
	}
}

func TestStateDisabledNode(t *testing.T) {
	st := stateFixture()
	st.Node.Enabled = false
	if !hasCode(State(st, DefaultOptions()), SevError, CodeNodeDisabled) {
		t.Fatal("停用节点不应能发布")
	}
}

// 需求 §七：不能凭 timeout/reset 就下"已被封"的结论 —— 校验器里也不允许出现这类断言。
// 这里做的是"契约测试"：确认校验结论里没有任何"封"相关结论码。
func TestNoBlockedVerdictsAnywhere(t *testing.T) {
	st := stateFixture()
	st.Routes = []model.RouteView{{
		Route: model.Route{
			ID: "rt_1", BusinessID: "biz_1", NodeID: "nd_A", Role: model.RolePrimary,
			Mode: model.ModeSNITLS, SNIEntryID: "sni_443",
			OriginHost: "127.0.0.1", OriginPort: 443, Enabled: true,
		},
		Mode: model.ModeSNITLS, Domains: []string{"*.bad.com"}, Enabled: true,
	}}
	r := State(st, DefaultOptions())
	for _, i := range r.Issues {
		if strings.Contains(i.Message, "被封") || strings.Contains(i.Message, "被墙") {
			t.Fatalf("校验结论不应包含「被封」类断言：%s", i.Message)
		}
	}
}

func TestDomainsAcrossBusinesses(t *testing.T) {
	owners := map[string]string{"a.example.com": "biz_1"}
	r := DomainsAcrossBusinesses("biz_2", []string{"a.example.com"}, owners)
	if !hasCode(r, SevError, CodeDomainDuplicate) {
		t.Fatalf("跨业务域名占用应报错，实际 %v", codes(r))
	}
	if i := findIssue(r, SevError, CodeDomainDuplicate); i == nil || !strings.Contains(i.Hint, "备用节点") {
		t.Fatal("应提示「同一业务的 A/B 双节点不该新建业务」这个常见误区")
	}
	// 自己占自己的不算冲突
	if !DomainsAcrossBusinesses("biz_1", []string{"a.example.com"}, owners).OK() {
		t.Fatal("同一业务自身不应被判为冲突")
	}
}

func TestHealthCheckValidation(t *testing.T) {
	var r Result
	r.HealthCheck(model.HealthCheck{Enabled: true, Kind: "tcp", IntervalMS: 100, TimeoutMS: 5000, Rise: 1, Fall: 1}, "hc")
	if !hasCode(r, SevError, CodeHealthCheckInvalid) {
		t.Fatalf("探测间隔过短与超时>间隔都应报错，实际 %v", codes(r))
	}

	r = Result{}
	r.HealthCheck(model.HealthCheck{Enabled: true, Kind: "tls", IntervalMS: 5000, TimeoutMS: 3000, Rise: 2, Fall: 3, SkipCertVerify: false}, "hc")
	if !hasCode(r, SevWarning, CodeHealthCheckInvalid) {
		t.Fatalf("开启源站证书校验应给 warning（透传场景下证书由客户端校验，"+
			"中转节点对自签证书做校验会把健康源站判死），实际 %v", codes(r))
	}

	r = Result{}
	r.HealthCheck(model.HealthCheck{Enabled: true, Kind: "quic", IntervalMS: 5000, TimeoutMS: 3000, Rise: 1, Fall: 1}, "hc")
	if !hasCode(r, SevError, CodeHealthCheckInvalid) {
		t.Fatal("不支持的探测类型应报错")
	}
}

func findIssue(r Result, sev Severity, code string) *Issue {
	for i := range r.Issues {
		if r.Issues[i].Severity == sev && r.Issues[i].Code == code {
			return &r.Issues[i]
		}
	}
	return nil
}

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("测试数据 CIDR 非法: %v", err)
	}
	return n
}
