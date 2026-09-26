package haproxy

import (
	"strings"
	"testing"

	"github.com/shengyu/edgelink/internal/model"
)

func capOK() model.Capabilities {
	return model.Capabilities{
		Version: "2.8.5", Supported: true, MasterWorker: true,
		ExposeFDListeners: true, SNICapture: true, JSONEscape: true, UnixDgramLog: true,
	}
}

func nodeFixture() model.Node {
	return model.Node{
		ID: "nd_7F2K9QX3ZB4M5NT8VC6W1PD0ER", Name: "relay-a", Enabled: true,
		PublicIPv4: "203.0.113.10", Capabilities: capOK(),
	}
}

func sniState() model.DesiredState {
	n := nodeFixture()
	entry := model.SNIEntry{ID: "sni_e_1", NodeID: n.ID, BindAddr: "0.0.0.0", BindPort: 443, Enabled: true}
	biz := func(id, host string, port int, domains ...string) model.RouteView {
		return model.RouteView{
			Route: model.Route{
				ID: "rt_" + id, BusinessID: id, NodeID: n.ID, Role: model.RolePrimary,
				Mode: model.ModeSNITLS, SNIEntryID: entry.ID,
				OriginHost: host, OriginPort: port, Enabled: true,
			},
			Mode: model.ModeSNITLS, Domains: domains, Enabled: true,
			EffectiveMaxConn: 0, EffectiveQueueLimit: 0,
		}
	}
	return model.DesiredState{
		Node:       n,
		SNIEntries: []model.SNIEntry{entry},
		Version:    42,
		Routes: []model.RouteView{
			biz("biz_A1", "198.51.100.7", 8443, "b.example.com", "a.example.com"),
			biz("biz_A2", "198.51.100.8", 9443, "c.example.com"),
		},
	}
}

func render(t *testing.T, st model.DesiredState) *Rendered {
	t.Helper()
	r, err := Render(st, DefaultDefaults())
	if err != nil {
		t.Fatalf("Render 失败: %v", err)
	}
	return r
}

// 需求 §四：禁止直接用域名拼接内部对象名称。
func TestObjectNamesNeverContainDomain(t *testing.T) {
	r := render(t, sniState())
	cfg := string(r.Config)
	for _, dom := range []string{"a.example.com", "b.example.com", "c.example.com"} {
		if strings.Contains(strings.Join(r.ObjectNames.Frontends, ","), dom) {
			t.Fatalf("frontend 名里出现了域名 %s", dom)
		}
		for be := range r.ObjectNames.BackendToBiz {
			if strings.Contains(be, dom) || strings.Contains(be, "example") {
				t.Fatalf("backend 名 %s 里出现了域名线索", be)
			}
		}
	}
	// 域名只允许出现在 use_backend 条件与允许清单里
	if !strings.Contains(cfg, "-m str -i a.example.com") {
		t.Fatal("use_backend 条件里应出现域名做精确匹配")
	}
}

// 需求 §三模式 A：多域名共享 443，按 SNI 精确路由，未配置的 SNI 默认拒绝。
func TestSNIFrontendShape(t *testing.T) {
	r := render(t, sniState())
	cfg := string(r.Config)

	must := []string{
		"frontend fe_sni_443",
		"bind 0.0.0.0:443",
		"tcp-request inspect-delay 5s",
		// SNI 必须在握手阶段抓进会话变量
		"tcp-request content set-var(txn.sni) req.ssl_sni",
		// 只接受 TLS ClientHello
		"tcp-request content reject unless { req.ssl_hello_type 1 }",
		// 不在允许清单里的 SNI 直接拒绝
		"tcp-request content reject unless { var(txn.sni) -m str -i -f {CONFIGDIR}/sni_allow_443.lst }",
		"use_backend bk_biza1 if { var(txn.sni) -m str -i a.example.com }",
		"default_backend bk_unmatched_sni_443",
	}
	for _, m := range must {
		if !strings.Contains(cfg, m) {
			t.Fatalf("配置缺少必需行:\n  %s\n--- 实际配置 ---\n%s", m, cfg)
		}
	}

	// 不允许出现任何 TLS 终止相关配置（透传模式不托管客户证书）
	for _, forbid := range []string{"crt ", "ssl-default", " ssl\n", "alpn "} {
		if strings.Contains(cfg, forbid) {
			t.Fatalf("SNI 透传模式不应出现 TLS 终止配置 %q", forbid)
		}
	}
	// 不允许出现通配符匹配
	if strings.Contains(cfg, "*.") {
		t.Fatal("配置里不应出现通配符域名匹配")
	}
	// 不允许出现 ssl 指令在 bind 上
	if strings.Contains(cfg, "bind 0.0.0.0:443 ssl") {
		t.Fatal("bind 不应带 ssl —— 那会变成 TLS 终止")
	}
}

func TestAllowlistIsSortedUnique(t *testing.T) {
	r := render(t, sniState())
	al, ok := r.Allowlists["sni_allow_443.lst"]
	if !ok {
		t.Fatal("缺少 sni_allow_443.lst")
	}
	got := strings.Fields(string(al))
	want := []string{"a.example.com", "b.example.com", "c.example.com"}
	if len(got) != len(want) {
		t.Fatalf("允许清单应为 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("允许清单应为 %v，实际 %v", want, got)
		}
	}
}

func TestExpectedListeners(t *testing.T) {
	r := render(t, sniState())
	if len(r.ExpectedListeners) != 1 {
		t.Fatalf("应只有 1 个监听端口，实际 %d: %+v", len(r.ExpectedListeners), r.ExpectedListeners)
	}
	if r.ExpectedListeners[0].Key() != "0.0.0.0:443" {
		t.Fatalf("监听端口错误: %s", r.ExpectedListeners[0].Key())
	}
}

// 关键：同样输入必须产出字节级相同的配置，否则 content_hash 失去意义、版本号会疯涨。
func TestRenderIsDeterministic(t *testing.T) {
	base := sniState()
	r1 := render(t, base)

	shuffled := sniState()
	// 只交换顺序，**不改变任何一条路由的内容**（内容变了就不是"同样输入"了）
	shuffled.Routes = []model.RouteView{shuffled.Routes[1], shuffled.Routes[0]}
	r2 := render(t, shuffled)

	if r1.ContentHash != r2.ContentHash {
		t.Fatalf("同样输入产生了不同 hash：%s vs %s\n--- r1 ---\n%s\n--- r2 ---\n%s",
			r1.ContentHash, r2.ContentHash, r1.Config, r2.Config)
	}
	if string(r1.Config) != string(r2.Config) {
		t.Fatal("同样输入应产出字节级相同的配置")
	}
}

// 需求 §十/§四：日志必须带节点 ID 与配置版本（渲染期字面量），且字符串字段必须转义。
func TestLogFormatCarriesNodeAndVersion(t *testing.T) {
	r := render(t, sniState())
	cfg := string(r.Config)
	if !strings.Contains(cfg, `\"node\":\"`+nodeFixture().ID+`\"`) {
		t.Fatal("log-format 里应包含节点 ID 字面量")
	}
	if !strings.Contains(cfg, `\"ver\":42`) {
		t.Fatal("log-format 里应包含配置版本字面量（数字不加引号）")
	}
	if !strings.Contains(cfg, `%[var(txn.sni),`+JSONConverter+`]`) {
		t.Fatalf("SNI 字段必须经过 %s 转义（2.8 没有 json_escape 这个转换器，评审 F01）", JSONConverter)
	}
	// 时间字段：手册 8.2.6 的别名 %T（GMT 的 accept date）。
	// 之前写的是 %[accept_date,...]，而 accept_date 根本不是 sample fetch ——
	// 那种配置在 `haproxy -c` 阶段就会失败（评审 F01）。
	if !strings.Contains(cfg, `\"ts\":\"%T\"`) {
		t.Fatalf("log-format 的时间字段应使用别名 %%T（accept date，GMT）:\n%s", cfg)
	}
	if strings.Contains(cfg, "accept_date") || strings.Contains(cfg, "json_escape") {
		t.Fatal("不允许再出现 accept_date / json_escape（二者都不属于 HAProxy 2.8，评审 F01）")
	}
	// 字节方向必须分开且语义明确（评审 F11）
	for _, k := range []string{`\"up\":\"%U\"`, `\"down\":\"%B\"`, `\"oip\":\"%si\"`} {
		if !strings.Contains(cfg, k) {
			t.Fatalf("log-format 缺少方向/目标地址字段 %q（评审 F11）", k)
		}
	}
	if strings.Contains(cfg, `\"bin\"`) {
		t.Fatal("已经不使用含义含糊的 bin 字段：up/down 分别对应上行/下行")
	}
	// 不可用字段不应被写进日志
	for _, k := range []string{`\"cid\"`, `\"err\"`, `\"uid\"`} {
		if strings.Contains(cfg, k) {
			t.Fatalf("默认关闭/不可用的字段 %s 不应出现在 log-format 里", k)
		}
	}
	if !strings.Contains(cfg, "log /run/shengyu-edgelink/log.sock format raw local0") {
		t.Fatal("应直投 unix dgram 日志 socket")
	}
	// expose-fd listeners 不再是 global 关键字：手册 5.1 明确它"only usable with the
	// stats socket"，且 master-worker 模式下已不需要（评审 F01）。
	if strings.Contains(cfg, "    expose-fd listeners\n") {
		t.Fatal("不应有独立的全局 expose-fd listeners 指令")
	}
	if !strings.Contains(cfg, "master-worker") {
		t.Fatal("平滑 reload 需要 master-worker")
	}
	if !strings.Contains(cfg, "stats socket ") || !strings.Contains(cfg, "level operator") {
		t.Fatal("统计套接字必须存在且只给 operator 级别（用它可以验证「运行版本」）")
	}
	if !strings.Contains(cfg, "timeout tunnel") {
		t.Fatal("必须设置 timeout tunnel，否则长连接会被 client/server 超时掐断")
	}
	// SNI 捕捉必须用 2.8 的正式名（带点的写法），
	// 不带点的 req_ssl_sni / req_ssl_hello_type 是已弃用别名。
	// 比较时先剥掉注释行：注释里为了说明"为什么不用旧写法"会提到旧名字，
	// 直接对全文做子串匹配会把注释也算成问题。
	body := stripComments(cfg)
	if strings.Contains(body, "req_ssl_sni") || strings.Contains(body, "req_ssl_hello_type") {
		t.Fatal("应使用 req.ssl_sni / req.ssl_hello_type（不带点的写法在 2.8 已弃用）")
	}
}

// stripComments 去掉整行注释，便于对"真正的配置正文"做断言。
func stripComments(cfg string) string {
	var b strings.Builder
	for _, l := range strings.Split(cfg, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}

// 需求 §三模式 B：无 SNI 的普通 TCP 也可以走专用端口。
func TestTCPPortMode(t *testing.T) {
	n := nodeFixture()
	st := model.DesiredState{
		Node: n, Version: 7,
		Routes: []model.RouteView{{
			Route: model.Route{
				ID: "rt_t1", BusinessID: "biz_TCP1", NodeID: n.ID, Role: model.RolePrimary,
				Mode: model.ModeTCPPort, EntryAddr: "0.0.0.0", EntryPort: 9443,
				OriginHost: "198.51.100.20", OriginPort: 3306, Enabled: true,
			},
			Mode: model.ModeTCPPort, Enabled: true,
		}},
	}
	r := render(t, st)
	cfg := string(r.Config)

	for _, m := range []string{
		"frontend fe_tcp_biztcp1",
		"bind 0.0.0.0:9443",
		"backend bk_biztcp1",
		"server s1 198.51.100.20:3306 check",
	} {
		if !strings.Contains(cfg, m) {
			t.Fatalf("TCP 模式配置缺少:\n  %s\n--- 实际 ---\n%s", m, cfg)
		}
	}
	if strings.Contains(cfg, "sni") {
		t.Fatal("TCP 端口转发模式不应出现 SNI 相关配置")
	}
	if len(r.ExpectedListeners) != 1 || r.ExpectedListeners[0].Key() != "0.0.0.0:9443" {
		t.Fatalf("期望监听端口错误: %+v", r.ExpectedListeners)
	}
}

// 需求 §二.8：不混用商业版特性。白名单必须真的能拦住越界指令。
func TestDirectiveWhitelistCatchesViolations(t *testing.T) {
	bad := CheckDirectives([]byte(`
global
    log ring@myring local0
    lua-load /etc/haproxy/x.lua
defaults
    mode tcp
backend b1
    stick-table type ip size 1m
`))
	if len(bad) == 0 {
		t.Fatal("越界指令应当被拦住")
	}
	joined := strings.Join(bad, "\n")
	for _, want := range []string{"ring", "lua-load", "stick-table"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("应当报出 %q，实际：\n%s", want, joined)
		}
	}
}

// 合法配置不能被白名单误杀（尤其是域名里含 ring 这种子串的情况）。
func TestDirectiveWhitelistNoFalsePositive(t *testing.T) {
	st := sniState()
	st.Routes = []model.RouteView{{
		Route: model.Route{
			ID: "rt_x", BusinessID: "biz_X", NodeID: nodeFixture().ID, Role: model.RolePrimary,
			Mode: model.ModeSNITLS, SNIEntryID: "sni_e_1",
			OriginHost: "198.51.100.9", OriginPort: 443, Enabled: true,
		},
		Mode: model.ModeSNITLS, Domains: []string{"ring.example.com"}, Enabled: true,
	}}
	r, err := Render(st, DefaultDefaults())
	if err != nil {
		t.Fatalf("域名里含 ring 不应导致渲染被误拦: %v", err)
	}
	if !strings.Contains(string(r.Config), "ring.example.com") {
		t.Fatal("域名应被渲染进 use_backend 条件")
	}
}

// 端口冲突在渲染层就应暴露为"两个监听"，由 validate 负责拦；这里确认渲染不合并、不静默丢规则。
func TestDisabledRoutesAreSkipped(t *testing.T) {
	st := sniState()
	st.Routes[0].Enabled = false
	r := render(t, st)
	if strings.Contains(string(r.Config), "bk_biza1") {
		t.Fatal("停用的路由不应被渲染")
	}
	if !strings.Contains(string(r.Config), "bk_biza2") {
		t.Fatal("启用的路由应被渲染")
	}
}

func TestVersionZeroRejected(t *testing.T) {
	st := sniState()
	st.Version = 0
	if _, err := Render(st, DefaultDefaults()); err == nil {
		t.Fatal("版本号 0 应当被拒绝")
	}
}

// TestBaselineRendersVersionZero 是本次真机部署发现缺陷的回归测试：
// 安装脚本的 -write-baseline 必须用 version=0 渲染出一份合法基线配置（不监听端口），
// 否则安装流程走不完、HAProxy 首次启动因无配置而失败。
// Render(version=0) 仍应报错（见 TestVersionZeroRejected）；只有 RenderBaseline 允许 0。
func TestBaselineRendersVersionZero(t *testing.T) {
	r, err := RenderBaseline(DefaultDefaults())
	if err != nil {
		t.Fatalf("RenderBaseline 不应失败: %v", err)
	}
	if r.Version != 0 {
		t.Fatalf("基线版本应为 0，实际 %d", r.Version)
	}
	cfg := string(r.Config)
	if !strings.Contains(cfg, "global") || !strings.Contains(cfg, "defaults") {
		t.Fatal("基线配置应至少含 global/defaults 段")
	}
	// 基线刻意不监听任何端口：不应出现 bind。
	if strings.Contains(cfg, "bind ") {
		t.Fatal("基线配置不应监听任何端口（不应出现 bind）")
	}
}

// TestHealthCheckRendering 是评审 F09 的回归测试：
// 健康探测只允许影响**探测**，绝不允许改动业务传输。
func TestHealthCheckRendering(t *testing.T) {
	// ---- 1) TLS 探测：必须用 check-ssl / check-sni ----
	st := sniState()
	st.Routes[0].HealthCheck = model.HealthCheck{
		Enabled: true, Kind: "tls", IntervalMS: 5000, TimeoutMS: 3000, Rise: 2, Fall: 3,
		SNI: "origin.example.com", SkipCertVerify: true,
	}
	cfg := string(render(t, st).Config)
	serverLine := lineWithPrefix(cfg, "    server s1 ")
	for _, want := range []string{"check", "inter 5000ms", "rise 2", "fall 3", "check-ssl", "check-sni origin.example.com"} {
		if !strings.Contains(serverLine, want) {
			t.Fatalf("TLS 健康探测的 server 行缺少 %q:\n%s", want, serverLine)
		}
	}
	// 铁律：server 行上不能出现会改动**业务传输**的参数。
	// `ssl` 会让 HAProxy 对源站发起 TLS（透传被套了一层）；
	// `sni` 会改写发给源站的 SNI（客户端发的就不是原样了）；
	// `verify` 会让业务流量也做证书校验。
	for _, forbid := range []string{" ssl", " sni ", " verify ", " timeout ", " uri "} {
		if strings.Contains(serverLine, forbid) {
			t.Fatalf("server 行不允许出现 %q（那是业务传输/非法参数，评审 F09）:\n%s", forbid, serverLine)
		}
	}
	// 探测超时必须落在 backend 的 timeout check 上
	if !strings.Contains(cfg, "    timeout check 3000ms") {
		t.Fatal("探测超时必须渲染成 backend 的 timeout check（server 行没有 timeout 参数）")
	}

	// ---- 2) 未启用：仍做最宽松的 TCP connect 检查，并显式给出探测间隔 ----
	st2 := sniState()
	st2.Routes[0].HealthCheck = model.HealthCheck{}
	line2 := lineWithPrefix(string(render(t, st2).Config), "    server s1 ")
	if !strings.Contains(line2, " check") || strings.Contains(line2, "check-ssl") {
		t.Fatalf("未启用健康探测时只应做 TCP connect 检查:\n%s", line2)
	}

	// ---- 3) HTTP 探测：option httpchk + http-check send/expect，且不改传输 ----
	st3 := sniState()
	st3.Routes[0].HealthCheck = model.HealthCheck{
		Enabled: true, Kind: "http", IntervalMS: 5000, TimeoutMS: 3000, Rise: 2, Fall: 3,
		HTTPPath: "/healthz", ExpectStatus: 200, SkipCertVerify: true,
	}
	cfg3 := string(render(t, st3).Config)
	for _, want := range []string{"    option httpchk", "http-check send uri /healthz", "http-check expect status 200"} {
		if !strings.Contains(cfg3, want) {
			t.Fatalf("HTTP 探测缺少 %q:\n%s", want, cfg3)
		}
	}
	line3 := lineWithPrefix(cfg3, "    server s1 ")
	if strings.Contains(line3, " uri ") || strings.Contains(line3, "proto http") {
		t.Fatalf("server 行不允许出现 uri/proto（那会把探测写法当成业务参数，评审 F09）:\n%s", line3)
	}
}

// TestLimitsGoToServerLine 断言并发/排队限制落在 server 行（手册 5.2）。
func TestLimitsGoToServerLine(t *testing.T) {
	st := sniState()
	st.Routes[0].EffectiveMaxConn = 50
	st.Routes[0].EffectiveQueueLimit = 100
	cfg := string(render(t, st).Config)
	line := lineWithPrefix(cfg, "    server s1 ")
	if !strings.Contains(line, "maxconn 50") || !strings.Contains(line, "maxqueue 100") {
		t.Fatalf("maxconn/maxqueue 必须在 server 行（backend 段没有 maxqueue 关键字）:\n%s", line)
	}
	// backend 段内（server 行之前）不允许出现这两个关键字
	body := cfg[:strings.Index(cfg, "    server s1 ")]
	lastBackend := body[strings.LastIndex(body, "\nbackend "):]
	if strings.Contains(lastBackend, "maxqueue") {
		t.Fatalf("backend 段不允许出现 maxqueue（它不是 backend 关键字）:\n%s", lastBackend)
	}
}

// TestServerLineParamsAreWhitelisted 钉住"server 行参数必须有手册依据"这条机械保障。
func TestServerLineParamsAreWhitelisted(t *testing.T) {
	bad := CheckDirectives([]byte("backend x\n    server s1 1.2.3.4:443 check timeout 3000ms\n"))
	if len(bad) == 0 {
		t.Fatal("server 行上的 timeout 不是合法参数，必须被拦下（这正是评审 F09 的原样错误）")
	}
	bad2 := CheckDirectives([]byte("backend x\n    server s1 1.2.3.4:443 check uri /health\n"))
	if len(bad2) == 0 {
		t.Fatal("server 行上的 uri 不是合法参数，必须被拦下")
	}
	good := CheckDirectives([]byte("backend x\n    server s1 1.2.3.4:443 check check-ssl check-sni a.example.com maxconn 10 maxqueue 5\n"))
	if len(good) != 0 {
		t.Fatalf("合法 server 行不应报警: %v", good)
	}
}

// lineWithPrefix 取第一行以 prefix 开头的行。
func lineWithPrefix(cfg, prefix string) string {
	for _, l := range strings.Split(cfg, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

// TestOriginIPv6AndHostnameRender 钉住"源站三种写法都能正确渲染"。
//
// IPv6 是这里最关键的一条：早先用 "%s:%d" 直接拼，IPv6 会输出
// `server s1 2001:db8::1:443`，冒号被当成端口分隔符，端口直接丢失。
// 这类错误 haproxy -c 未必报（会被解析成另一个合法地址），
// 只有在现网连错源站时才会暴露，所以必须用测试钉死。
func TestOriginIPv6AndHostnameRender(t *testing.T) {
	base := sniState()
	// 第一条路由的源站端口固定为 8443（见 sniState）。
	const port = 8443
	cases := []struct {
		host string
		want string
	}{
		{"2001:db8::1", "[2001:db8::1]:8443"},
		{"origin.example.com", "origin.example.com:8443"},
		{"198.51.100.7", "198.51.100.7:8443"},
	}
	for _, c := range cases {
		st := base
		rt := base.Routes[0]
		rt.Route.OriginHost = c.host
		rt.Route.OriginPort = port
		st.Routes = []model.RouteView{rt}
		line := lineWithPrefix(string(render(t, st).Config), "    server s1 ")
		wantPrefix := "    server s1 " + c.want
		if !strings.HasPrefix(line, wantPrefix) {
			t.Fatalf("源站 %s 渲染成 %q，期望以 %q 开头", c.host, line, wantPrefix)
		}
	}
}

func TestRenderToFileSmoke(t *testing.T) {
	r := render(t, sniState())
	if len(r.Config) < 800 {
		t.Fatalf("配置过短，可能渲染出错：\n%s", r.Config)
	}
	t.Logf("content_hash=%s listeners=%v", r.ContentHash, r.ExpectedListeners)
}
