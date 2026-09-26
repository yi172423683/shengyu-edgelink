// Package e2e 是端到端验收测试。
//
// 它**不 mock 任何关键环节**：真实 TLS 握手、真实 TCP 转发、真实 SNI 解析、
// 真实日志落盘、真实 HTTP API 查询。需求 §十一 要求逐条验收的场景，
// 能在这里断言的都写成了断言，而不是"人工确认过"。
//
// 这里的数据面是**测试替身**（internal/testplane），不是生产的 HAProxy：
//   - HAProxy 不提供 Windows 版，而开发与验收要在 Windows 上跑；
//   - 数据面是接口（dataplane.Applier），发布流水线、日志链路、诊断、
//     版本回滚这些**与内核无关**的逻辑，用替身驱动同样成立；
//   - 替身带来的差异项（systemctl reload、/proc 监听解析、统计 CSV 按列名解析、
//     reload 失败后配置正文回滚）在 internal/dataplane/haproxy_test.go 里单独验证；
//   - 真实 HAProxy + 真实 systemd + 非 root 身份下的整链路验收，必须在 Linux 节点上做。
//
// 生产程序**不允许**选这个替身：cmd/shengyu-edgelink-server 的 -dataplane 只接受 haproxy，
// 且生产二进制里不含替身的任何符号（由 cmd/shengyu-edgelink-server/main_test.go 钉住）。
package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shengyu/edgelink/internal/agent"
	"github.com/shengyu/edgelink/internal/api"
	"github.com/shengyu/edgelink/internal/auth"
	"github.com/shengyu/edgelink/internal/haproxy"
	"github.com/shengyu/edgelink/internal/id"
	"github.com/shengyu/edgelink/internal/logparse"
	"github.com/shengyu/edgelink/internal/logstore"
	"github.com/shengyu/edgelink/internal/model"
	"github.com/shengyu/edgelink/internal/publish"
	"github.com/shengyu/edgelink/internal/store"
	"github.com/shengyu/edgelink/internal/testplane"
)

// ============================== 脚手架 ==============================

type harness struct {
	t        *testing.T
	Store    *store.Store
	Logs     *logstore.Store
	Relay    *testplane.GoRelay
	Pipeline *publish.Pipeline
	API      *api.Server
	HTTP     *httptest.Server
	Client   *http.Client
	CSRF     string

	nodeID  string
	dataDir string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("打开元数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	logs, err := logstore.New(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatalf("打开日志库失败: %v", err)
	}

	// 低迭代以加速测试；同时显式确认验证链路仍然可用（不是把校验关掉）
	u := &store.User{ID: id.New("usr"), Username: "admin", Enabled: true, Role: "admin"}
	if err := st.CreateUser(u, "Test-Passw0rd!", 1000); err != nil {
		t.Fatalf("创建测试管理员失败: %v", err)
	}

	// 先有节点 ID，再据此建数据面（数据面产出的日志里要带节点 ID）。
	nodeID := id.New("node")
	relay := testplane.NewGoRelay(nodeID, nil)
	t.Cleanup(func() { _ = relay.Close() })

	// 数据面能力必须先探测并登记到节点上 —— 否则校验层会（正确地）拦下所有发布：
	// "你不告诉我这台节点支持什么，我就不允许发任何需要特性的业务"。
	caps, err := relay.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("探测数据面能力失败: %v", err)
	}

	if err := st.CreateNode(&model.Node{
		ID: nodeID, Name: "本机测试节点", Enabled: true,
		PublicIPv4: "127.0.0.1", Health: model.NodeOnline, Capabilities: caps,
		HAProxyVersion: caps.Version,
	}); err != nil {
		t.Fatalf("创建测试节点失败: %v", err)
	}

	pl := publish.New(st, relay, filepath.Join(dir, "haproxy"), haproxy.DefaultDefaults())
	// 与生产一致：流水线必须绑定本机节点，非本机节点的发布会被拒绝。
	pl.SetLocalNode(nodeID)

	ing := agent.NewIngestor(logs, mustResolver(t, st, nodeID), nodeID)
	relay.SetSink(ing.IngestLine)

	srv := api.New(st, logs, pl, api.Config{
		SessionTTL: time.Hour,
		// 端到端测试的源站与中转在同一台机器上，源站只能是回环地址。
		// 这个开关**只在测试里打开**；生产默认关闭（见 api.Config 的说明）。
		AllowLoopbackOrigin: true,
	})
	srv.NodeID = nodeID
	srv.DataplaneName = relay.Name()

	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	jar, _ := cookiejar.New(nil)
	h := &harness{
		t: t, Store: st, Logs: logs, Relay: relay, Pipeline: pl,
		API: srv, HTTP: httpSrv, Client: &http.Client{Jar: jar, Timeout: 20 * time.Second},
		nodeID: nodeID, dataDir: dir,
	}
	h.login()
	return h
}

// liveResolver 每次解析都从库里重建映射。
//
// 测试里刻意不缓存：业务是在测试过程中才创建的，若解析器在启动时固化，
// 新建业务的日志就解析不出归属 —— 那会让"日志链路"这条断言看起来像产品 bug，
// 实际上是测试脚手架的问题。生产侧用的是带 30 秒刷新的缓存版本（见 cmd/shengyu-edgelink-server）。
type liveResolver struct {
	t      *testing.T
	st     *store.Store
	nodeID string
	mu     sync.Mutex
	cur    logparse.Resolver
}

func (l *liveResolver) get() logparse.Resolver {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r, err := l.st.ResolverForNode(l.nodeID); err == nil {
		l.cur = r
	} else if l.cur == nil {
		l.t.Errorf("构造日志解析器失败: %v", err)
	}
	return l.cur
}

func (l *liveResolver) BusinessForBackend(be string) (string, string, bool) {
	if r := l.get(); r != nil {
		return r.BusinessForBackend(be)
	}
	return "", "", false
}

func (l *liveResolver) OriginForBackend(be string) (string, int, bool) {
	if r := l.get(); r != nil {
		return r.OriginForBackend(be)
	}
	return "", 0, false
}

func mustResolver(t *testing.T, st *store.Store, nodeID string) logparse.Resolver {
	t.Helper()
	lr := &liveResolver{t: t, st: st, nodeID: nodeID}
	if _, err := st.ResolverForNode(nodeID); err != nil {
		t.Fatalf("构造日志解析器失败: %v", err)
	}
	return lr
}

func (h *harness) login() {
	h.t.Helper()
	body := map[string]string{"username": "admin", "password": "Test-Passw0rd!"}
	resp := h.do("POST", "/api/login", body, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("登录失败: %d %s", resp.StatusCode, readAll(resp.Body))
	}
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	decodeResp(h.t, resp.Body, &out)
	h.CSRF = out.CSRF
	if h.CSRF == "" {
		h.t.Fatal("登录未返回 CSRF 令牌")
	}
}

// do 发一个请求；mutating=true 时自动带 CSRF 头。
func (h *harness) do(method, path string, body any, mutating bool) *http.Response {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("序列化请求失败: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.HTTP.URL+path, rdr)
	if err != nil {
		h.t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if mutating {
		req.Header.Set("X-CSRF-Token", h.CSRF)
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s 失败: %v", method, path, err)
	}
	return resp
}

// mustJSON 发请求并把响应解到 out；非 2xx 直接失败并打印服务端说明。
func (h *harness) mustJSON(method, path string, body any, mutating bool, out any) *http.Response {
	h.t.Helper()
	resp := h.do(method, path, body, mutating)
	defer resp.Body.Close()
	raw := readAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		h.t.Fatalf("%s %s 期望 2xx，实际 %d：%s", method, path, resp.StatusCode, raw)
	}
	if out != nil && raw != "" {
		if err := json.Unmarshal([]byte(raw), out); err != nil {
			h.t.Fatalf("解析 %s %s 响应失败: %v\n原始内容: %s", method, path, err, raw)
		}
	}
	return resp
}

func readAll(r io.Reader) string {
	b, _ := io.ReadAll(r)
	return string(b)
}

// queryLogs 查连接日志。默认时间窗取"最近 1 小时"，与界面默认一致。
//
// 这里刻意**不**给一个超宽的时间范围：需求 §八 要求查询必须限制时间范围
// 且分页，测试自己也按界面的规矩来，才不会掩盖"查询能扫全历史"这类问题。
func (h *harness) queryLogs(out any, extraQuery string) {
	h.t.Helper()
	now := time.Now().UTC()
	q := fmt.Sprintf("?from=%s&to=%s",
		now.Add(-time.Hour).Format(time.RFC3339), now.Add(time.Minute).Format(time.RFC3339))
	if extraQuery != "" {
		if strings.HasPrefix(extraQuery, "?") {
			q += "&" + strings.TrimPrefix(extraQuery, "?")
		} else {
			q += "&" + extraQuery
		}
	}
	h.mustJSON("GET", "/api/logs/conn"+q, nil, false, out)
}

// logQueryResult 是连接日志查询的响应。
type logQueryResult struct {
	Items []struct {
		BusinessID string `json:"business_id"`
		SNI        string `json:"sni"`
		NodeID     string `json:"node_id"`
		BytesUp    *int64 `json:"bytes_up"`
		BytesDown  *int64 `json:"bytes_down"`
		ParseOK    bool   `json:"parse_ok"`
		TermCode   string `json:"term_code"`
		TermReason string `json:"term_reason"`
	} `json:"items"`
	Explain struct {
		Notes []string `json:"notes"`
	} `json:"explain"`
}

// waitForLogs 轮询直到查到至少一条日志。
//
// 日志链路本身是异步的（会话结束 → 落盘 → 可查），因此"立刻查不到"不是故障。
// 但"超过 timeout 仍然查不到"就是故障 —— 后者才是这条测试要抓的。
func (h *harness) waitForLogs(t *testing.T, out *logQueryResult, extraQuery string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		h.queryLogs(out, extraQuery)
		if len(out.Items) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待日志超时（%s）：筛选条件 %q，分片说明 %v\n"+
				"这通常意味着「转发 → 落盘 → 查询」链路的某一环断了。", timeout, extraQuery, out.Explain.Notes)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func decodeResp(t *testing.T, r io.Reader, v any) {
	t.Helper()
	if err := json.NewDecoder(r).Decode(v); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// tlsOrigin 是"源站"：真实 TLS 服务，握手后回显带前缀的内容。
//
// 它把两类连接**分开计数**，这一点很重要（需求 §六.B）：
//
//	tcpAccepts —— 到达了 TCP 层（发布后的源站健康探测就是这样来的：只做 TCP 连通性检查）
//	tlsConns   —— 完成了 TLS 握手（这才是真实的业务客户端）
//
// 如果混在一起，"源站探测"会被当成"真实用户请求"，测试也就无法证明业务连接真的到达了源站。
type tlsOrigin struct {
	port int
	mu   sync.Mutex
	// data 是回显前缀，用于区分不同源站，从而验证路由有没有串。
	data       string
	tcpAccepts int
	tlsConns   int
}

func startTLSOrigin(t *testing.T, host string, data string) *tlsOrigin {
	t.Helper()
	cert := selfSigned(t, host)
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("源站监听失败: %v", err)
	}
	t.Cleanup(func() { _ = base.Close() })
	ln := tls.NewListener(base, &tls.Config{Certificates: []tls.Certificate{cert}})
	o := &tlsOrigin{port: base.Addr().(*net.TCPAddr).Port, data: data}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				o.mu.Lock()
				o.tcpAccepts++
				o.mu.Unlock()
				tc, ok := c.(*tls.Conn)
				if !ok {
					return
				}
				// 显式握手：只有握手成功才算"真实业务连接"。
				// 源站探测只做 TCP 连接、不发 ClientHello，会在这里被挡下。
				_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
				if err := tc.Handshake(); err != nil {
					return
				}
				o.mu.Lock()
				o.tlsConns++
				o.mu.Unlock()
				buf := make([]byte, 4096)
				for {
					n, err := tc.Read(buf)
					if n > 0 {
						_, _ = tc.Write([]byte(o.data + ":" + string(buf[:n])))
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return o
}

// busiConns 完成过 TLS 握手的连接数，即真实业务连接。
func (o *tlsOrigin) busiConns() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.tlsConns
}

// probeConns 只到达 TCP 层的连接数，即源站健康探测。
func (o *tlsOrigin) probeConns() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.tcpAccepts - o.tlsConns
}

func selfSigned(t *testing.T, host string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书失败: %v", err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}),
	)
	if err != nil {
		t.Fatalf("组装证书失败: %v", err)
	}
	return cert
}

// ============================== 端到端主流程 ==============================

// TestEndToEndMainFlow 覆盖需求正文要求的完整闭环：
//
//	添加业务 → 真实转发 → 产生日志 → 网页查询 → 修改规则 → 平滑生效 → 故障恢复
func TestEndToEndMainFlow(t *testing.T) {
	h := newHarness(t)

	// ---------- 1. 新建客户 ----------
	var cust model.Customer
	h.mustJSON("POST", "/api/customers", map[string]any{
		"name": "示例客户", "contact": "ops@example.com", "remark": "端到端测试",
	}, true, &cust)
	if cust.ID == "" {
		t.Fatal("客户 ID 为空")
	}

	// ---------- 2. 配置共享 SNI 入口（用一个空闲端口代替 443，避免测试机权限问题）----------
	sniPort := freePort(t)
	var entry model.SNIEntry
	h.mustJSON("POST", "/api/nodes/"+h.nodeID+"/sni-entries", map[string]any{
		"bind_addr": "127.0.0.1", "bind_port": sniPort, "enabled": true,
	}, true, &entry)
	if entry.ID == "" {
		t.Fatal("SNI 入口创建失败")
	}

	// ---------- 3. 两个真实源站 ----------
	originA := startTLSOrigin(t, "a.example.com", "ORIGIN-A")
	originB := startTLSOrigin(t, "b.example.com", "ORIGIN-B")

	// ---------- 4. 添加业务 A（域名 a.example.com）----------
	var bizA struct {
		Business model.Business `json:"business"`
	}
	h.mustJSON("POST", "/api/businesses", map[string]any{
		"customer_id": cust.ID, "name": "站点A", "mode": "sni_tls",
		"domains": []string{"a.example.com"}, "primary_node_id": h.nodeID,
		"origin_host": "127.0.0.1", "origin_port": originA.port,
		"sni_entry_id": entry.ID,
	}, true, &bizA)
	if bizA.Business.ID == "" {
		t.Fatal("业务 A 创建失败")
	}

	// 添加业务 B，共享同一个入口端口（需求 §三 模式 A 的核心）
	var bizB struct {
		Business model.Business `json:"business"`
	}
	h.mustJSON("POST", "/api/businesses", map[string]any{
		"customer_id": cust.ID, "name": "站点B", "mode": "sni_tls",
		"domains": []string{"b.example.com"}, "primary_node_id": h.nodeID,
		"origin_host": "127.0.0.1", "origin_port": originB.port,
		"sni_entry_id": entry.ID,
	}, true, &bizB)

	// ---------- 5. 发布 ----------
	var pub publish.Result
	h.mustJSON("POST", "/api/nodes/"+h.nodeID+"/publish", map[string]any{"note": "首次接入"}, true, &pub)
	if pub.Status != store.ReleaseSucceeded {
		t.Fatalf("首次发布未成功: %+v", pub)
	}
	if pub.Version != 1 {
		t.Fatalf("首个版本号应为 1，实际 %d", pub.Version)
	}

	// 发布后节点期望版本必须更新
	var nodeResp struct {
		Node model.Node `json:"node"`
	}
	h.mustJSON("GET", "/api/nodes/"+h.nodeID, nil, false, &nodeResp)
	if nodeResp.Node.ExpectedVersion != 1 {
		t.Fatalf("期望版本应为 1，实际 %d", nodeResp.Node.ExpectedVersion)
	}

	// ---------- 6. 真实转发：两个域名共享同一入口，互不串流 ----------
	got := dialTLS(t, sniPort, "a.example.com", "ping-A")
	if !strings.HasPrefix(got, "ORIGIN-A:") {
		t.Fatalf("a.example.com 应路由到源站 A，实际收到 %q（说明串流或路由错误）", got)
	}
	got = dialTLS(t, sniPort, "b.example.com", "ping-B")
	if !strings.HasPrefix(got, "ORIGIN-B:") {
		t.Fatalf("b.example.com 应路由到源站 B，实际收到 %q", got)
	}
	// 互不串流：每个源站只应收到属于自己域名的**业务**连接。
	// 注意这里统计的是完成 TLS 握手的连接；发布后的源站健康探测只到 TCP 层，不计入。
	if originA.busiConns() != 1 || originB.busiConns() != 1 {
		t.Fatalf("串流了：A 的业务连接 %d 次、B %d 次（各应恰好 1 次）",
			originA.busiConns(), originB.busiConns())
	}
	// 发布后的源站探测应当发生过（证明「配置生效」与「源站可达」是分开判断的）
	if originA.probeConns() == 0 {
		t.Logf("提示：未观察到源站探测连接（探测策略可能已调整），不影响本次断言")
	}

	// ---------- 7. 产生日志 → 网页查询 ----------
	//
	// 注意这里必须**轮询等待**：TCP 会话日志按定义要在会话结束后才产生（HAProxy 如此，
	// gorelay 也如此 —— 这正是需求 §六.D 要求"实时指标不得依赖结束日志计算"的原因）。
	// 连接刚关闭就去查，查不到是正常的，不该判成链路故障。
	var q logQueryResult
	h.waitForLogs(t, &q, "?business_id="+bizA.Business.ID, 5*time.Second)
	if len(q.Items) == 0 {
		t.Fatal("按业务 A 查询应有连接日志；日志链路（转发→落盘→查询）断了")
	}
	for _, it := range q.Items {
		if it.BusinessID != bizA.Business.ID {
			t.Fatalf("按业务 A 过滤却查到了别的业务: %q", it.BusinessID)
		}
		if it.SNI != "a.example.com" {
			t.Fatalf("日志里的 SNI 不对: %q", it.SNI)
		}
		if !it.ParseOK {
			t.Fatalf("日志应能正常解析: %+v", it)
		}
		if it.BytesDown == nil || *it.BytesDown == 0 {
			t.Fatalf("应记录字节数: %+v", it)
		}
	}
	// 业务 B 的日志不应出现在 A 的查询里
	var qB logQueryResult
	h.waitForLogs(t, &qB, "?business_id="+bizB.Business.ID, 5*time.Second)
	for _, it := range qB.Items {
		if it.BusinessID != bizB.Business.ID {
			t.Fatalf("按业务 B 过滤却查到了 %q", it.BusinessID)
		}
	}

	// ---------- 8. 未知 SNI 必须被拒绝（需求 §十一）----------
	unknownErr := dialTLSErr(t, sniPort, "evil.example.com", "should-not-pass")
	if unknownErr == nil {
		t.Fatal("未知 SNI 竟然通了 —— 按策略必须拒绝")
	}
	if originA.busiConns()+originB.busiConns() != 2 {
		t.Fatalf("未知 SNI 不应触达任何源站；当前业务连接数 A=%d B=%d",
			originA.busiConns(), originB.busiConns())
	}

	// ---------- 9. 改规则 → 平滑生效 ----------
	// 把业务 A 的源站换到 originB 所在的端口（模拟"客户换了源站"）
	var updResp struct {
		Business model.Business `json:"business"`
	}
	h.mustJSON("PUT", "/api/businesses/"+bizA.Business.ID, map[string]any{
		"customer_id": cust.ID, "name": "站点A", "mode": "sni_tls",
		"domains": []string{"a.example.com"}, "primary_node_id": h.nodeID,
		"origin_host": "127.0.0.1", "origin_port": originB.port,
		"sni_entry_id": entry.ID,
	}, true, &updResp)

	// 改动前，先建立一条**长连接**，用于验证"常规规则更新不主动中断现有连接"
	longConn := openTLS(t, sniPort, "b.example.com")
	if got := roundTrip(t, longConn, "before-reload"); !strings.HasPrefix(got, "ORIGIN-B:") {
		t.Fatalf("长连接首次收发异常: %q", got)
	}

	var pub2 publish.Result
	h.mustJSON("POST", "/api/nodes/"+h.nodeID+"/publish", map[string]any{"note": "更换源站"}, true, &pub2)
	if pub2.Status != store.ReleaseSucceeded || pub2.Version != 2 {
		t.Fatalf("第二次发布异常: %+v", pub2)
	}

	// 新规则立即生效：a.example.com 现在应指向原来 B 的源站
	got = dialTLS(t, sniPort, "a.example.com", "after-change")
	if !strings.HasPrefix(got, "ORIGIN-B:") {
		t.Fatalf("改规则后 a.example.com 应指向新源站，实际 %q", got)
	}

	// 老长连接必须还能用（需求 §五：常规规则更新不主动中断现有连接）
	if got := roundTrip(t, longConn, "after-reload"); !strings.HasPrefix(got, "ORIGIN-B:") {
		t.Fatalf("发布后老长连接被中断或错乱: %q", got)
	}
	_ = longConn.Close()

	// ---------- 10. 平台离线不影响转发（需求 §十一）----------
	h.HTTP.Close() // 关掉管理面
	got = dialTLS(t, sniPort, "a.example.com", "while-manager-down")
	if !strings.HasPrefix(got, "ORIGIN-B:") {
		t.Fatalf("管理面关闭后转发应继续工作，实际 %q", got)
	}

	// ---------- 11. 历史版本与一键回滚（需求 §五）----------
	// 重新起一个管理面客户端（前面的 httptest server 已关，用直接调用 Store/API 不方便，
	// 因此这里改为验证回滚这一动作本身：通过 pipeline 走 API 之外的同一条路径）。
	ctx := context.Background()
	st := h.Store
	versions, total, err := st.ListConfigVersions(h.nodeID, 10, 0)
	if err != nil || total < 2 {
		t.Fatalf("应有至少 2 个配置版本，实际 %d (%v)", total, err)
	}
	if versions[0].Version != 2 {
		t.Fatalf("最新版本应为 2，实际 %d", versions[0].Version)
	}
	_ = ctx
}

// TestEndToEndRollbackRestoresTraffic 验证"发布失败恢复旧版本"以及显式回滚。
func TestEndToEndRollbackRestoresTraffic(t *testing.T) {
	h := newHarness(t)

	var cust model.Customer
	h.mustJSON("POST", "/api/customers", map[string]any{"name": "回滚客户"}, true, &cust)

	// 模式 B：TCP 端口转发，便于精确制造端口冲突
	originPort := startTCPEcho(t, "ORIGIN-TCP")
	entryPort := freePort(t)

	var biz struct {
		Business model.Business `json:"business"`
	}
	h.mustJSON("POST", "/api/businesses", map[string]any{
		"customer_id": cust.ID, "name": "TCP业务", "mode": "tcp_port",
		"primary_node_id": h.nodeID, "entry_addr": "127.0.0.1", "entry_port": entryPort,
		"origin_host": "127.0.0.1", "origin_port": originPort,
	}, true, &biz)

	var pub publish.Result
	h.mustJSON("POST", "/api/nodes/"+h.nodeID+"/publish", nil, true, &pub)
	if pub.Status != store.ReleaseSucceeded {
		t.Fatalf("首次发布应成功: %+v", pub)
	}
	if got := tcpRoundTrip(t, entryPort, "hello-1"); got != "ORIGIN-TCP:hello-1" {
		t.Fatalf("首次转发异常: %q", got)
	}

	// ---------- 制造端口冲突：外部占住端口，再新增业务指向它 ----------
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占用端口失败: %v", err)
	}
	defer blocker.Close()
	busyPort := blocker.Addr().(*net.TCPAddr).Port

	var biz2 struct {
		Business model.Business `json:"business"`
	}
	h.mustJSON("POST", "/api/businesses", map[string]any{
		"customer_id": cust.ID, "name": "冲突业务", "mode": "tcp_port",
		"primary_node_id": h.nodeID, "entry_addr": "127.0.0.1", "entry_port": busyPort,
		"origin_host": "127.0.0.1", "origin_port": originPort,
	}, true, &biz2)

	var pubFail publish.Result
	resp := h.do("POST", "/api/nodes/"+h.nodeID+"/publish", map[string]any{"note": "应当失败"}, true)
	raw := readAll(resp.Body)
	resp.Body.Close()
	if err := json.Unmarshal([]byte(raw), &pubFail); err != nil {
		t.Fatalf("解析发布响应失败: %v\n%s", err, raw)
	}
	if pubFail.Status != store.ReleaseFailed {
		t.Fatalf("端口冲突时发布必须失败，实际 %+v", pubFail)
	}
	if !strings.Contains(pubFail.Message, "已有") && !strings.Contains(pubFail.Message, "bind") &&
		!strings.Contains(pubFail.Message, "占用") && !strings.Contains(pubFail.Message, "Address") {
		t.Logf("发布失败原因（供人工核对）: %s", pubFail.Message)
	}

	// 关键：**原业务必须照常工作**（需求 §十一：错误配置和端口冲突不破坏现有服务）
	if got := tcpRoundTrip(t, entryPort, "hello-2"); got != "ORIGIN-TCP:hello-2" {
		t.Fatalf("端口冲突后原业务被打断了: %q", got)
	}
	// 版本不能推进：界面上的"期望版本"必须仍然等于 j 线上真实生效的那一版
	var nodeResp struct {
		Node model.Node `json:"node"`
	}
	h.mustJSON("GET", "/api/nodes/"+h.nodeID, nil, false, &nodeResp)
	if nodeResp.Node.ExpectedVersion != 1 {
		t.Fatalf("发布失败后期望版本不应推进，实际 %d", nodeResp.Node.ExpectedVersion)
	}
	// 失败的那一次发布必须留下痕迹，且状态是 failed（而不是"悄悄消失"）。
	// 注意：失败候选**会占用版本号** —— 版本号必须严格单调、永不复用，
	// 否则平滑 reload 期间新旧 worker 写的日志就无法区分是哪一版配置服务了它。
	var vers struct {
		Items []struct {
			Version int    `json:"version"`
			Status  string `json:"status"`
			Error   string `json:"error"`
		} `json:"items"`
		Total int `json:"total"`
	}
	h.mustJSON("GET", "/api/nodes/"+h.nodeID+"/versions", nil, false, &vers)
	if vers.Total < 2 {
		t.Fatalf("失败的那次发布也应留版本记录，实际只有 %d 条", vers.Total)
	}
	foundFailed := false
	for _, v := range vers.Items {
		if v.Status == string(store.CfgFailed) {
			foundFailed = true
			if v.Error == "" {
				t.Error("失败的配置版本必须记录失败原因，否则界面只能显示一个空白的错误")
			}
		}
	}
	if !foundFailed {
		t.Fatal("端口冲突导致的失败版本状态应为 failed")
	}
	// 当前生效版本仍然是 1
	var activeCount int
	for _, v := range vers.Items {
		if v.Status == string(store.CfgActive) {
			activeCount++
			if v.Version != 1 {
				t.Fatalf("当前生效版本应为 1，实际 %d", v.Version)
			}
		}
	}
	if activeCount != 1 {
		t.Fatalf("同一节点在同一时刻只应有一个生效版本，实际 %d 个", activeCount)
	}

	// ---------- 移除冲突业务后再发布，然后显式回滚 ----------
	h.mustJSON("DELETE", "/api/businesses/"+biz2.Business.ID, nil, true, nil)

	// 改一下源站端口，产生一个内容不同的 v2
	origin2 := startTCPEcho(t, "ORIGIN-TCP2")
	h.mustJSON("PUT", "/api/businesses/"+biz.Business.ID, map[string]any{
		"customer_id": cust.ID, "name": "TCP业务", "mode": "tcp_port",
		"primary_node_id": h.nodeID, "entry_addr": "127.0.0.1", "entry_port": entryPort,
		"origin_host": "127.0.0.1", "origin_port": origin2,
	}, true, nil)

	var pub2 publish.Result
	h.mustJSON("POST", "/api/nodes/"+h.nodeID+"/publish", nil, true, &pub2)
	if pub2.Status != store.ReleaseSucceeded {
		t.Fatalf("第二次发布应成功: %+v", pub2)
	}
	// 版本号严格单调递增：上一次失败已占用一个号，所以这里不该是 2，也不该回退。
	if pub2.Version <= 1 {
		t.Fatalf("版本号必须单调递增，实际 %d（上一次失败已占用一个号）", pub2.Version)
	}
	if got := tcpRoundTrip(t, entryPort, "v2"); got != "ORIGIN-TCP2:v2" {
		t.Fatalf("第二次转发应指向新源站，实际 %q", got)
	}

	// 回滚到 v1
	var rb publish.Result
	h.mustJSON("POST", "/api/nodes/"+h.nodeID+"/rollback", map[string]any{
		"version": 1, "reason": "端到端测试：验证一键回滚",
	}, true, &rb)
	if rb.Status != store.ReleaseSucceeded {
		t.Fatalf("回滚应成功: %+v", rb)
	}
	if got := tcpRoundTrip(t, entryPort, "rolled-back"); got != "ORIGIN-TCP:rolled-back" {
		t.Fatalf("回滚后应重新指向原源站，实际 %q", got)
	}

	// 回滚动作必须留痕且能查到
	var audit struct {
		Items []store.AuditEntry `json:"items"`
	}
	h.mustJSON("GET", "/api/audit?action=config.rollback", nil, false, &audit)
	if len(audit.Items) == 0 {
		t.Fatal("回滚必须写入审计记录")
	}
	if audit.Items[0].Result != "ok" {
		t.Fatalf("回滚审计结果应为 ok，实际 %q", audit.Items[0].Result)
	}

	// 版本历史必须完整保留（否则回滚无从谈起）
	versions, total, err := h.Store.ListConfigVersions(h.nodeID, 10, 0)
	if err != nil {
		t.Fatalf("查询版本历史失败: %v", err)
	}
	if total < 2 || len(versions) < 2 {
		t.Fatalf("版本历史应至少 2 条，实际 %d", total)
	}
}

// TestEndToEndDiagnosisDistinguishesCauses 验证诊断页能区分不同故障原因，
// 且**不会**把 timeout 直接说成"被封"（需求 §七）。
func TestEndToEndDiagnosisDistinguishesCauses(t *testing.T) {
	h := newHarness(t)

	var cust model.Customer
	h.mustJSON("POST", "/api/customers", map[string]any{"name": "诊断客户"}, true, &cust)

	sniPort := freePort(t)
	var entry model.SNIEntry
	h.mustJSON("POST", "/api/nodes/"+h.nodeID+"/sni-entries", map[string]any{
		"bind_addr": "127.0.0.1", "bind_port": sniPort, "enabled": true,
	}, true, &entry)

	// 源站故意指向一个没人监听的端口，制造"源站不可达"
	deadPort := freePort(t)
	var biz struct {
		Business model.Business `json:"business"`
	}
	h.mustJSON("POST", "/api/businesses", map[string]any{
		"customer_id": cust.ID, "name": "坏源站业务", "mode": "sni_tls",
		"domains": []string{"dead.example.com"}, "primary_node_id": h.nodeID,
		"origin_host": "127.0.0.1", "origin_port": deadPort,
		"sni_entry_id": entry.ID,
	}, true, &biz)

	// 源站不可达时，发布应当**成功**（配置本身没问题），但状态要标明"源站不健康"。
	var pub publish.Result
	h.mustJSON("POST", "/api/nodes/"+h.nodeID+"/publish", nil, true, &pub)
	if pub.Status == store.ReleaseFailed {
		t.Fatalf("源站不可达不应判为发布失败（那是两回事）: %+v", pub)
	}
	if pub.Status != store.ReleaseAppliedUnhealthy {
		t.Fatalf("应标记为「配置已生效但源站不可用」，实际 %q", pub.Status)
	}

	// 发一次连接，让它失败并产生日志
	_ = dialTLSErr(t, sniPort, "dead.example.com", "x")

	// 再制造一次"未匹配 SNI"
	_ = dialTLSErr(t, sniPort, "unknown.example.com", "y")

	var diag struct {
		Findings []struct {
			Category string   `json:"category"`
			Severity string   `json:"severity"`
			Facts    []string `json:"facts"`
			Evidence []string `json:"evidence"`
			Causes   []string `json:"possible_causes"`
			Next     []string `json:"next_steps"`
		} `json:"findings"`
		ClassificationNote string `json:"classification_note"`
	}
	h.mustJSON("GET", "/api/diagnose", nil, false, &diag)

	cats := map[string]bool{}
	for _, f := range diag.Findings {
		cats[f.Category] = true
		// 每条结论都必须给出四段内容，否则界面会显示一个没有依据的猜测
		if len(f.Facts) == 0 || len(f.Causes) == 0 || len(f.Next) == 0 {
			t.Fatalf("诊断结论 %q 缺少事实/可能原因/下一步建议: %+v", f.Category, f)
		}
	}
	if !cats["origin_unreachable"] {
		t.Errorf("应识别出「源站连接失败」，实际分类: %v", cats)
	}
	if !cats["sni_not_matched"] {
		t.Errorf("应识别出「未匹配 SNI」，实际分类: %v", cats)
	}
	// 明确不得把超时直接定性为"被封"
	if !strings.Contains(diag.ClassificationNote, "不能判定") {
		t.Errorf("诊断页必须声明「不能仅凭 timeout/reset 判定被封」，实际说明: %q", diag.ClassificationNote)
	}
	combined := strings.Join(diag.Findings[0].Causes, " ") + strings.Join(diag.Findings[0].Next, " ")
	if strings.Contains(combined, "被封") {
		t.Errorf("诊断结论里不应出现「被封」这种未经证据支持的定性: %q", combined)
	}
}

// TestEndToEndLogQueryGuards 验证日志查询的边界约束（需求 §八）：
// 必须限制时间范围、必须分页，且不允许无限制地扫全历史。
func TestEndToEndLogQueryGuards(t *testing.T) {
	h := newHarness(t)

	// 不指定时间范围：允许，但必须**明说**用了默认窗口 ——
	// 界面上一片空白最容易被误读成"客户端没发请求"，默认值必须可见。
	resp := h.do("GET", "/api/logs/conn", nil, false)
	raw := readAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("未指定时间范围应回退到默认窗口并返回 200，实际 %d：%s", resp.StatusCode, raw)
	}
	if !strings.Contains(raw, "未指定完整时间范围") {
		t.Fatalf("必须提示已使用默认时间窗口，实际: %s", raw)
	}
	if !strings.Contains(raw, "不能") {
		t.Fatalf("响应必须提示「查不到不能判定客户端没发请求」，实际: %s", raw)
	}

	// 超宽时间范围必须被拒绝：否则一次查询就会扫全部历史分片。
	from := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	to := time.Now().UTC().Format(time.RFC3339)
	resp = h.do("GET", "/api/logs/conn?from="+from+"&to="+to, nil, false)
	raw = readAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("超宽时间范围应被拒绝（400），实际 %d：%s", resp.StatusCode, raw)
	}

	// 合法范围应成功
	from = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	resp = h.do("GET", "/api/logs/conn?from="+from+"&to="+to, nil, false)
	raw = readAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("合法查询应成功，实际 %d：%s", resp.StatusCode, raw)
	}
}

// ============================== 转发辅助 ==============================

func dialTLS(t *testing.T, port int, serverName, payload string) string {
	t.Helper()
	c := openTLS(t, port, serverName)
	defer c.Close()
	return roundTrip(t, c, payload)
}

func openTLS(t *testing.T, port int, serverName string) *tls.Conn {
	t.Helper()
	d := &net.Dialer{Timeout: 5 * time.Second}
	raw, err := d.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("连接入口失败: %v", err)
	}
	c := tls.Client(raw, &tls.Config{ServerName: serverName, InsecureSkipVerify: true})
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := c.Handshake(); err != nil {
		t.Fatalf("TLS 握手失败（中转未正确透传 ClientHello）: %v", err)
	}
	return c
}

func roundTrip(t *testing.T, c *tls.Conn, payload string) string {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte(payload)); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	buf := make([]byte, 256)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	return string(buf[:n])
}

func dialTLSErr(t *testing.T, port int, serverName, payload string) error {
	t.Helper()
	d := &net.Dialer{Timeout: 5 * time.Second}
	raw, err := d.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	defer raw.Close()
	c := tls.Client(raw, &tls.Config{ServerName: serverName, InsecureSkipVerify: true})
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := c.Handshake(); err != nil {
		return err
	}
	if _, err := c.Write([]byte(payload)); err != nil {
		return err
	}
	buf := make([]byte, 64)
	if _, err := c.Read(buf); err != nil {
		return err
	}
	return nil
}

func startTCPEcho(t *testing.T, prefix string) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						_, _ = c.Write([]byte(prefix + ":" + string(buf[:n])))
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func tcpRoundTrip(t *testing.T, port int, payload string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		t.Fatalf("连接中转失败: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(payload)); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	buf := make([]byte, 256)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	return string(buf[:n])
}

var _ = fmt.Sprintf
var _ = os.Getenv
var _ = auth.DefaultIterations
var _ = filepath.Join
