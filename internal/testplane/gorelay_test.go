package testplane

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shengyu/edgelink/internal/dataplane"
	"github.com/shengyu/edgelink/internal/haproxy"
	"github.com/shengyu/edgelink/internal/logparse"
	"github.com/shengyu/edgelink/internal/model"
)

// ============================== 测试脚手架 ==============================

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func selfSigned(t *testing.T, hosts ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: hosts[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     hosts,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书失败: %v", err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("加载证书失败: %v", err)
	}
	return c
}

// tlsEchoOrigin 启动一个真实 TLS 服务端：完成握手后把收到的内容回显。
// 用它当"源站"，才能证明中转节点没有终止 TLS（否则握手不可能成功）。
type tlsEchoOrigin struct {
	ln    net.Listener
	port  int
	mu    sync.Mutex
	peers []string
}

func startTLSEcho(t *testing.T, cert tls.Certificate) *tlsEchoOrigin {
	t.Helper()
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}}
	ln := tls.NewListener(base, cfg)
	o := &tlsEchoOrigin{ln: ln, port: base.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				o.mu.Lock()
				o.peers = append(o.peers, c.RemoteAddr().String())
				o.mu.Unlock()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						_, _ = c.Write(append([]byte("echo:"), buf[:n]...))
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return o
}

func (o *tlsEchoOrigin) connCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.peers)
}

// tcpEcho 普通 TCP 回显服务（模式 B 的源站）。
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
				_, _ = io.Copy(c, io.TeeReader(c, io.Discard))
				_ = prefix
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// collectSink 收集数据面产出的日志行。
type collectSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *collectSink) sink(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, line)
}

func (s *collectSink) waitFor(t *testing.T, pred func(string) bool, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		for _, l := range s.lines {
			if pred(l) {
				s.mu.Unlock()
				return l
			}
		}
		s.mu.Unlock()
		time.Sleep(15 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t.Fatalf("等待日志超时；已有 %d 行：\n%s", len(s.lines), strings.Join(s.lines, ""))
	return ""
}

// mapResolver 实现 logparse.Resolver：把 backend 对象名反解回业务与源站。
//
// 它刻意**复用 haproxy.BackendName** 而不是自己拼字符串 —— 这样测试验证的是
// "渲染器命名的对象名，确实能被解析器反解回来"这条契约；如果哪天有人改了命名规则
// 却忘了改解析，这条测试会立刻红。
type mapResolver struct {
	biz   map[string]string // backend -> businessID
	route map[string]string // backend -> routeID
	orig  map[string]net.TCPAddr
}

func resolverFor(st model.DesiredState) *mapResolver {
	r := &mapResolver{biz: map[string]string{}, route: map[string]string{}, orig: map[string]net.TCPAddr{}}
	for _, rv := range st.Routes {
		be := haproxy.BackendName(rv)
		r.biz[be] = rv.Route.BusinessID
		r.route[be] = rv.Route.ID
		r.orig[be] = net.TCPAddr{IP: net.ParseIP(rv.Route.OriginHost), Port: rv.Route.OriginPort}
	}
	return r
}

func (m *mapResolver) BusinessForBackend(be string) (string, string, bool) {
	b, ok := m.biz[be]
	if !ok {
		return "", "", false
	}
	return b, m.route[be], true
}

func (m *mapResolver) OriginForBackend(be string) (string, int, bool) {
	a, ok := m.orig[be]
	if !ok {
		return "", 0, false
	}
	return a.IP.String(), a.Port, true
}

// ============================== 用例 ==============================

// 需求 §十一：「多域名共享 443，路由准确且互不串流」。
func TestGoRelaySNIRoutingDoesNotCrossTalk(t *testing.T) {
	originA := startTLSEcho(t, selfSigned(t, "a.example.com"))
	originB := startTLSEcho(t, selfSigned(t, "b.example.com"))

	sniPort := freePort(t)
	st := model.DesiredState{
		Node:       model.Node{ID: "node1"},
		SNIEntries: []model.SNIEntry{{ID: "sni1", NodeID: "node1", BindAddr: "127.0.0.1", BindPort: sniPort, Enabled: true}},
		Routes: []model.RouteView{
			{Route: model.Route{ID: "rt_a", BusinessID: "biz_a", NodeID: "node1", Mode: model.ModeSNITLS,
				SNIEntryID: "sni1",
				OriginHost: "127.0.0.1", OriginPort: originA.port, Enabled: true},
				Domains: []string{"a.example.com"}, Mode: model.ModeSNITLS, Enabled: true},
			{Route: model.Route{ID: "rt_b", BusinessID: "biz_b", NodeID: "node1", Mode: model.ModeSNITLS,
				SNIEntryID: "sni1",
				OriginHost: "127.0.0.1", OriginPort: originB.port, Enabled: true},
				Domains: []string{"b.example.com"}, Mode: model.ModeSNITLS, Enabled: true},
		},
		Version: 1,
	}

	sink := &collectSink{}
	relay := NewGoRelay("node1", sink.sink)
	t.Cleanup(func() { _ = relay.Close() })
	if err := relay.Apply(context.Background(), dataplane.ApplyRequest{State: st, Version: 1}); err != nil {
		t.Fatalf("应用配置失败: %v", err)
	}

	// 两个域名共享同一个入口端口
	gotA := dialThroughSNI(t, sniPort, "a.example.com")
	if gotA != "echo:hello-A" {
		t.Fatalf("a.example.com 应答不对: %q", gotA)
	}
	gotB := dialThroughSNI(t, sniPort, "b.example.com")
	if gotB != "echo:hello-B" {
		t.Fatalf("b.example.com 应答不对: %q", gotB)
	}

	// 互不串流：各自只应该收到自己那条连接
	if originA.connCount() != 1 || originB.connCount() != 1 {
		t.Fatalf("串流了：originA=%d originB=%d 次连接", originA.connCount(), originB.connCount())
	}

	// 日志必须能落到"正确的业务 + 正确的源站"
	res := resolverFor(st)
	line := sink.waitFor(t, func(l string) bool { return strings.Contains(l, `"sni":"a.example.com"`) }, 3*time.Second)
	rec, err := logparse.Parse([]byte(line), res)
	if err != nil {
		t.Fatalf("日志解析失败: %v\n原始行: %s", err, line)
	}
	if rec.SNI != "a.example.com" {
		t.Fatalf("日志里的 SNI 不对: %q", rec.SNI)
	}
	if rec.BusinessID != "biz_a" {
		t.Fatalf("日志应归属到 biz_a，实际 %q（backend=%q）", rec.BusinessID, rec.BeName)
	}
	if rec.OriginAddr != "127.0.0.1" || rec.OriginPort == nil || *rec.OriginPort != originA.port {
		t.Fatalf("日志里的源站不对: %s:%v（期望 127.0.0.1:%d）", rec.OriginAddr, rec.OriginPort, originA.port)
	}
	if rec.BytesDown == nil || *rec.BytesDown == 0 {
		t.Fatalf("下行字节（源站→客户端）应被记录: %v", rec.BytesDown)
	}
	if rec.BytesUp == nil {
		t.Fatalf("上行字节字段必须存在（即便为 0 也要能区分「0」与「不可用」）: %v", rec.BytesUp)
	}
}

func dialThroughSNI(t *testing.T, port int, serverName string) string {
	t.Helper()
	d := &net.Dialer{Timeout: 3 * time.Second}
	raw, err := d.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("连接中转入口失败: %v", err)
	}
	defer raw.Close()
	tc := tls.Client(raw, &tls.Config{ServerName: serverName, InsecureSkipVerify: true})
	_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tc.Handshake(); err != nil {
		t.Fatalf("TLS 握手失败（说明中转没有正确透传 ClientHello）: %v", err)
	}
	payload := "hello-" + strings.ToUpper(serverName[:1])
	if _, err := tc.Write([]byte(payload)); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	buf := make([]byte, 64)
	n, err := tc.Read(buf)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	return string(buf[:n])
}

// 需求 §十一：「未知 SNI 被拒绝，并可定位原因」。
func TestGoRelayRejectsUnknownSNI(t *testing.T) {
	origin := startTLSEcho(t, selfSigned(t, "known.example.com"))
	sniPort := freePort(t)
	st := model.DesiredState{
		Node:       model.Node{ID: "node1"},
		SNIEntries: []model.SNIEntry{{ID: "sni1", NodeID: "node1", BindAddr: "127.0.0.1", BindPort: sniPort, Enabled: true}},
		Routes: []model.RouteView{
			{Route: model.Route{ID: "rt", BusinessID: "biz_known", NodeID: "node1", Mode: model.ModeSNITLS,
				SNIEntryID: "sni1",
				OriginHost: "127.0.0.1", OriginPort: origin.port, Enabled: true},
				Domains: []string{"known.example.com"}, Mode: model.ModeSNITLS, Enabled: true},
		},
		Version: 1,
	}
	sink := &collectSink{}
	relay := NewGoRelay("node1", sink.sink)
	t.Cleanup(func() { _ = relay.Close() })
	if err := relay.Apply(context.Background(), dataplane.ApplyRequest{State: st, Version: 1}); err != nil {
		t.Fatalf("应用失败: %v", err)
	}

	// 未知 SNI：连接必须被拒绝，且源站一次都不该被碰到
	raw, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(sniPort)), 3*time.Second)
	if err != nil {
		t.Fatalf("连接入口失败: %v", err)
	}
	tc := tls.Client(raw, &tls.Config{ServerName: "evil.example.com", InsecureSkipVerify: true})
	_ = tc.SetDeadline(time.Now().Add(3 * time.Second))
	if err := tc.Handshake(); err == nil {
		t.Fatal("未知 SNI 的握手竟然成功了 —— 按策略必须拒绝")
	}
	_ = raw.Close()

	if origin.connCount() != 0 {
		t.Fatalf("未知 SNI 不应触达源站，实际 %d 次", origin.connCount())
	}

	// 日志必须能说清"为什么被拒"，而不是只留一个 timeout
	line := sink.waitFor(t, func(l string) bool { return strings.Contains(l, "evil.example.com") }, 3*time.Second)
	if !strings.Contains(line, "未匹配 SNI") {
		t.Fatalf("拒绝原因未写清: %s", line)
	}
	rec, err := logparse.Parse([]byte(line), nil)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if rec.BusinessID != "" {
		t.Fatalf("被拒连接不该归属到任何业务，实际 %q", rec.BusinessID)
	}
}

// 需求 §三 模式 B + §十一「无 SNI 的连接可以走指定 TCP 专用端口」。
func TestGoRelayTCPPortForward(t *testing.T) {
	originPort := startTCPEcho(t, "")
	entryPort := freePort(t)
	st := model.DesiredState{
		Node: model.Node{ID: "node1"},
		Routes: []model.RouteView{
			{Route: model.Route{ID: "rt", BusinessID: "biz_tcp", NodeID: "node1", Mode: model.ModeTCPPort,
				EntryAddr: "127.0.0.1", EntryPort: entryPort,
				OriginHost: "127.0.0.1", OriginPort: originPort, Enabled: true},
				Mode: model.ModeTCPPort, Enabled: true},
		},
		Version: 1,
	}
	relay := NewGoRelay("node1", nil)
	t.Cleanup(func() { _ = relay.Close() })
	if err := relay.Apply(context.Background(), dataplane.ApplyRequest{State: st, Version: 1}); err != nil {
		t.Fatalf("应用失败: %v", err)
	}

	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(entryPort)), 3*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte("plain-tcp-payload")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(buf[:n]) != "plain-tcp-payload" {
		t.Fatalf("回显不对: %q", string(buf[:n]))
	}
}

// 需求 §十一：「错误配置和端口冲突不破坏现有服务」。
// 这是整个发布可靠性里最容易被忽略、后果最严重的一条：
// 一次手滑的端口冲突，不能让正在服务的业务跟着一起断。
func TestGoRelayPortConflictKeepsExistingService(t *testing.T) {
	originPort := startTCPEcho(t, "")
	entryPort := freePort(t)

	// 先正常发布一版
	stOK := model.DesiredState{
		Node: model.Node{ID: "node1"},
		Routes: []model.RouteView{
			{Route: model.Route{ID: "rt", BusinessID: "biz_tcp", NodeID: "node1", Mode: model.ModeTCPPort,
				EntryAddr: "127.0.0.1", EntryPort: entryPort,
				OriginHost: "127.0.0.1", OriginPort: originPort, Enabled: true},
				Mode: model.ModeTCPPort, Enabled: true},
		},
		Version: 1,
	}
	relay := NewGoRelay("node1", nil)
	t.Cleanup(func() { _ = relay.Close() })
	if err := relay.Apply(context.Background(), dataplane.ApplyRequest{State: stOK, Version: 1}); err != nil {
		t.Fatalf("首次发布失败: %v", err)
	}
	assertEcho(t, entryPort, "before-conflict")

	// 制造冲突：外部进程占住一个端口，然后新配置要绑它
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占用端口失败: %v", err)
	}
	defer blocker.Close()
	busyPort := blocker.Addr().(*net.TCPAddr).Port

	stBad := stOK
	stBad.Version = 2
	stBad.Routes = append(stBad.Routes, model.RouteView{
		Route: model.Route{ID: "rt2", BusinessID: "biz_conflict", NodeID: "node1", Mode: model.ModeTCPPort,
			EntryAddr: "127.0.0.1", EntryPort: busyPort,
			OriginHost: "127.0.0.1", OriginPort: originPort, Enabled: true},
		Mode: model.ModeTCPPort, Enabled: true,
	})
	err = relay.Apply(context.Background(), dataplane.ApplyRequest{State: stBad, Version: 2})
	if err == nil {
		t.Fatal("端口被占用时应用必须失败")
	}
	if !strings.Contains(err.Error(), "现有转发未受影响") {
		t.Fatalf("错误信息应明确告知旧服务未受影响: %v", err)
	}

	// 关键断言：原有业务必须照常工作
	assertEcho(t, entryPort, "after-conflict")

	// 版本不能被推进（否则界面会谎报"已到 v2"）
	ver, _ := relay.ActiveVersion(context.Background())
	if ver != 1 {
		t.Fatalf("发布失败后版本不应推进，实际 %d", ver)
	}
}

func assertEcho(t *testing.T, entryPort int, payload string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(entryPort)), 3*time.Second)
	if err != nil {
		t.Fatalf("连接中转失败（现有服务被打断了！）: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(payload)); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	buf := make([]byte, 128)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !bytes.Equal(buf[:n], []byte(payload)) {
		t.Fatalf("回显不符: got %q want %q", string(buf[:n]), payload)
	}
}

// 需求 §十一：「平滑更新时现有长连接持续工作」。
func TestGoRelayExistingLongConnectionSurvivesReload(t *testing.T) {
	originPort := startTCPEcho(t, "")
	entryPort := freePort(t)
	st := model.DesiredState{
		Node: model.Node{ID: "node1"},
		Routes: []model.RouteView{
			{Route: model.Route{ID: "rt", BusinessID: "biz_tcp", NodeID: "node1", Mode: model.ModeTCPPort,
				EntryAddr: "127.0.0.1", EntryPort: entryPort,
				OriginHost: "127.0.0.1", OriginPort: originPort, Enabled: true},
				Mode: model.ModeTCPPort, Enabled: true},
		},
		Version: 1,
	}
	relay := NewGoRelay("node1", nil)
	t.Cleanup(func() { _ = relay.Close() })
	if err := relay.Apply(context.Background(), dataplane.ApplyRequest{State: st, Version: 1}); err != nil {
		t.Fatalf("首次发布失败: %v", err)
	}

	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(entryPort)), 3*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("ping-1")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	buf := make([]byte, 128)
	if n, err := c.Read(buf); err != nil || string(buf[:n]) != "ping-1" {
		t.Fatalf("首次收发失败: %q %v", string(buf[:n]), err)
	}

	// 改规则（新增一条业务）并 reload —— 现有连接不能被掐断
	st2 := st
	st2.Version = 2
	otherPort := freePort(t)
	st2.Routes = append(st2.Routes, model.RouteView{
		Route: model.Route{ID: "rt_new", BusinessID: "biz_new", NodeID: "node1", Mode: model.ModeTCPPort,
			EntryAddr: "127.0.0.1", EntryPort: otherPort,
			OriginHost: "127.0.0.1", OriginPort: originPort, Enabled: true},
		Mode: model.ModeTCPPort, Enabled: true,
	})
	if err := relay.Apply(context.Background(), dataplane.ApplyRequest{State: st2, Version: 2}); err != nil {
		t.Fatalf("第二次发布失败: %v", err)
	}

	// 老连接必须还能用
	if _, err := c.Write([]byte("ping-2")); err != nil {
		t.Fatalf("reload 后老连接写失败（长连接被打断了）: %v", err)
	}
	if n, err := c.Read(buf); err != nil || string(buf[:n]) != "ping-2" {
		t.Fatalf("reload 后老连接读失败: %q %v", string(buf[:n]), err)
	}
	// 新业务也应立即可用
	assertEcho(t, otherPort, "new-route-works")

	// 移除一条路由时，也应等老连接自然结束而不是立刻掐断
	st3 := st2
	st3.Version = 3
	st3.Routes = st3.Routes[:1] // 保留原路由，去掉新增路由
	if err := relay.Apply(context.Background(), dataplane.ApplyRequest{State: st3, Version: 3}); err != nil {
		t.Fatalf("第三次发布失败: %v", err)
	}
	if _, err := c.Write([]byte("ping-3")); err != nil {
		t.Fatalf("移除无关路由后老连接写失败: %v", err)
	}
	if n, err := c.Read(buf); err != nil || string(buf[:n]) != "ping-3" {
		t.Fatalf("移除无关路由后老连接读失败: %q %v", string(buf[:n]), err)
	}
}

// 实时指标必须能反映"尚未结束的长连接"（需求 §六.D）。
func TestGoRelayStatsSeeInFlightConnection(t *testing.T) {
	originPort := startTCPEcho(t, "")
	entryPort := freePort(t)
	st := model.DesiredState{
		Node: model.Node{ID: "node1"},
		Routes: []model.RouteView{
			{Route: model.Route{ID: "rt", BusinessID: "biz_tcp", NodeID: "node1", Mode: model.ModeTCPPort,
				EntryAddr: "127.0.0.1", EntryPort: entryPort,
				OriginHost: "127.0.0.1", OriginPort: originPort, Enabled: true},
				Mode: model.ModeTCPPort, Enabled: true},
		},
		Version: 1,
	}
	relay := NewGoRelay("node1", nil)
	t.Cleanup(func() { _ = relay.Close() })
	if err := relay.Apply(context.Background(), dataplane.ApplyRequest{State: st, Version: 1}); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(entryPort)), 3*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = c.Write([]byte("some-bytes"))
	buf := make([]byte, 64)
	if _, err := c.Read(buf); err != nil {
		t.Fatalf("读取失败: %v", err)
	}

	// 连接**还没结束**（客户端没关），此时在线连接数必须已经反映出来。
	// 如果只能靠结束日志统计，这里会是 0 —— 那正是需求禁止的做法。
	deadline := time.Now().Add(2 * time.Second)
	for {
		s, err := relay.Stats(context.Background())
		if err != nil {
			t.Fatalf("取指标失败: %v", err)
		}
		if s.ActiveConns >= 1 && s.BytesIn > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("未结束的长连接没有体现在实时指标里: active=%d bytesIn=%d",
				s.ActiveConns, s.BytesIn)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 描述一个"源站不可达"的场景：连接必须失败，并且日志能区分"这是源站问题"而不是"配置没生效"。
func TestGoRelayOriginUnreachableIsDistinguishable(t *testing.T) {
	deadPort := freePort(t) // 拿一个端口但没人监听
	entryPort := freePort(t)
	st := model.DesiredState{
		Node: model.Node{ID: "node1"},
		Routes: []model.RouteView{
			{Route: model.Route{ID: "rt", BusinessID: "biz_dead", NodeID: "node1", Mode: model.ModeTCPPort,
				EntryAddr: "127.0.0.1", EntryPort: entryPort,
				OriginHost: "127.0.0.1", OriginPort: deadPort, Enabled: true},
				Mode: model.ModeTCPPort, Enabled: true},
		},
		Version: 1,
	}
	sink := &collectSink{}
	relay := NewGoRelay("node1", sink.sink)
	t.Cleanup(func() { _ = relay.Close() })
	if err := relay.Apply(context.Background(), dataplane.ApplyRequest{State: st, Version: 1}); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(entryPort)), 3*time.Second)
	if err != nil {
		t.Fatalf("连接中转本身应成功: %v", err)
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = c.Write([]byte("x"))
	buf := make([]byte, 8)
	if _, err := c.Read(buf); err == nil {
		t.Fatal("源站不可达时不应读到数据")
	}
	_ = c.Close()

	line := sink.waitFor(t, func(l string) bool { return strings.Contains(l, "源站连接失败") }, 3*time.Second)
	if !strings.Contains(line, "源站连接失败") {
		t.Fatalf("日志应明确指出是源站连不上: %s", line)
	}
	// 该行必须仍然能归属到业务（否则诊断界面没法从日志找回业务）
	rec, perr := logparse.Parse([]byte(line), resolverFor(st))
	if perr != nil {
		t.Fatalf("解析失败: %v", perr)
	}
	if rec.BusinessID != "biz_dead" {
		t.Fatalf("源站失败的那条也应归属到 biz_dead，实际 %q", rec.BusinessID)
	}
	// 配置本身是健康的：这与"发布失败"是两回事
	v, err := relay.Verify(context.Background(), dataplane.VerifyRequest{State: st})
	if err != nil {
		t.Fatalf("Verify 失败: %v", err)
	}
	if !v.OK {
		t.Fatalf("配置验证应通过（监听都在），问题出在源站: %+v", v)
	}
}

// 配置校验：期望监听集合必须稳定且可预测（否则每次渲染 hash 都变）。
func TestExpectedListeners(t *testing.T) {
	st := model.DesiredState{
		SNIEntries: []model.SNIEntry{
			{ID: "s1", BindAddr: "*", BindPort: 443, Enabled: true},
			{ID: "s2", BindAddr: "0.0.0.0", BindPort: 8443, Enabled: true},
			{ID: "s3", BindAddr: "0.0.0.0", BindPort: 9443, Enabled: false}, // 未启用，不应出现
		},
		Routes: []model.RouteView{
			{Route: model.Route{ID: "r1", EntryPort: 25565, EntryAddr: "1.2.3.4"}, Mode: model.ModeTCPPort, Enabled: true},
			{Route: model.Route{ID: "r2", EntryPort: 25566}, Mode: model.ModeTCPPort, Enabled: false}, // 未启用
			{Route: model.Route{ID: "r3", SNIEntryID: "s1"}, Mode: model.ModeSNITLS, Enabled: true},   // 挂在 s1 上
			{Route: model.Route{ID: "r4", SNIEntryID: "s2"}, Mode: model.ModeSNITLS, Enabled: true},   // 挂在 s2 上
		},
	}
	got := dataplane.ExpectedListeners(st)
	want := []string{"0.0.0.0:443", "0.0.0.0:8443", "1.2.3.4:25565"}
	if len(got) != len(want) {
		t.Fatalf("期望监听数量不符: got %v want %v", keysOf(got), want)
	}
	for i := range want {
		if dataplane.ListenerKey(got[i]) != want[i] {
			t.Fatalf("期望监听不符: got %v want %v", keysOf(got), want)
		}
	}
	// 稳定性：同样的输入两次必须给出同样顺序
	again := dataplane.ExpectedListeners(st)
	for i := range again {
		if dataplane.ListenerKey(again[i]) != dataplane.ListenerKey(got[i]) {
			t.Fatal("期望监听顺序不稳定，会导致 content_hash 每次都变")
		}
	}
}

func keysOf(ls []model.Listener) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.Key()
	}
	return out
}

var _ = fmt.Sprintf
