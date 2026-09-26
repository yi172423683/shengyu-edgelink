package dataplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shengyu/edgelink/internal/model"
)

// fakeRunner 模拟外部命令，让"发布失败要回滚"这类逻辑可以在非 Linux 环境验证。
//
// 匹配规则：把 responses 的 key 当成**前缀**，第一个命中的返回其响应。
// 这样 `"/usr/sbin/haproxy -c"` 能同时匹配 `-c -f /path` 的调用。
type fakeRunner struct {
	calls     []string
	responses map[string]fakeResp
}

type fakeResp struct {
	out, errb string
	err       error
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (string, string, error) {
	key := strings.TrimSpace(name + " " + joinArgs(args))
	f.calls = append(f.calls, key)
	// 先找最长匹配前缀，避免 "/a" 抢了 "/a/b" 的响应
	best, bestLen := "", -1
	for k := range f.responses {
		if strings.HasPrefix(key, k) && len(k) > bestLen {
			best, bestLen = k, len(k)
		}
	}
	if bestLen >= 0 {
		r := f.responses[best]
		return r.out, r.errb, r.err
	}
	return "", "", nil
}

func joinArgs(a []string) string { return strings.Join(a, " ") }

func TestParseHAProxyVersionFromVV(t *testing.T) {
	cases := []struct{ in, want string }{
		{"HAProxy version 2.8.5-1 2024/01/01\nCopyright 2000-2024", "2.8.5"},
		{"HAProxy version 2.4.22 2023/05/01", "2.4.22"},
		{"HAProxy version 3.0.4-1 2024/09/03", "3.0.4"},
		{"no version here", ""},
	}
	for _, c := range cases {
		if got := parseHAProxyVersion(c.in); got != c.want {
			t.Errorf("parseHAProxyVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestVersionSupportedRange(t *testing.T) {
	// 受支持：2.4 LTS 起，到 3.2 之前（见 docs/04）
	supported := []string{"2.4.22", "2.6.15", "2.8.5", "2.9.7", "3.0.4", "3.1.0"}
	unsupported := []string{"", "2.3.9", "1.8.30", "3.2.0", "3.5.1", "abc", "2"}
	for _, v := range supported {
		if !versionSupported(v) {
			t.Errorf("%s 应受支持", v)
		}
	}
	for _, v := range unsupported {
		if versionSupported(v) {
			t.Errorf("%s 不应受支持", v)
		}
	}
}

func TestHAProxyValidateReportsSyntaxError(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{
		"/usr/sbin/haproxy -c": {errb: "[ALERT] parsing [/etc/x.cfg:12]: unknown keyword 'bogus'", err: errors.New("exit status 1")},
	}}
	h := NewHAProxy()
	h.SetRunner(f.run)
	err := h.Validate(context.Background(), "/etc/x.cfg")
	if err == nil {
		t.Fatal("语法错误必须被报出")
	}
	if !containsSub(err.Error(), "unknown keyword 'bogus'") {
		t.Fatalf("原始错误信息必须透传，便于定位行号；got %v", err)
	}
}

// 核心可靠性断言：reload 失败时，current 目录必须回到旧版本的内容。
// 否则下一次 reload 会读到那份坏配置，把一次失败放大成"重启即故障"。
func TestHAProxyReloadFailureRestoresPreviousVersion(t *testing.T) {
	root := t.TempDir()
	versions := filepath.Join(root, "versions")
	for v, body := range map[string]string{"v1": "global\n  # v1\n", "v2": "global\n  # v2\n"} {
		dir := filepath.Join(versions, v)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "haproxy.cfg"), []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	current := filepath.Join(root, "current")

	h := NewHAProxy()
	h.ConfigRoot = root
	h.CurrentDir = current
	f := &fakeRunner{responses: map[string]fakeResp{
		"systemctl reload": {errb: "Job for shengyu-edgelink-haproxy.service failed", err: errors.New("exit status 1")},
	}}
	h.SetRunner(f.run)

	// 先把 v1 放到位（模拟线上正在跑 v1）
	if _, err := h.publishFiles(filepath.Join(versions, "v1"), 1); err != nil {
		t.Fatalf("初始化 current 失败: %v", err)
	}

	err := h.Apply(context.Background(), ApplyRequest{
		ConfigDir:       filepath.Join(versions, "v2"),
		ConfigPath:      filepath.Join(versions, "v2", "haproxy.cfg"),
		Version:         2,
		PreviousVersion: 1,
	})
	if err == nil {
		t.Fatal("reload 失败时 Apply 必须返回错误")
	}

	// 关键：生效版本必须回到 1
	ver, verr := h.ActiveVersion(context.Background())
	if verr != nil {
		t.Fatalf("读取生效版本失败: %v", verr)
	}
	if ver != 1 {
		t.Fatalf("reload 失败后必须回滚到 v1，实际 v%d", ver)
	}
	// 而且**配置正文**也要是 v1 的，不能只改版本号不改内容
	body, rerr := os.ReadFile(filepath.Join(current, "haproxy.cfg"))
	if rerr != nil {
		t.Fatalf("读取 current 配置失败: %v", rerr)
	}
	if !containsSub(string(body), "# v1") {
		t.Fatalf("回滚后配置正文必须是 v1 的，实际:\n%s", string(body))
	}
	// 临时文件不能残留（否则下次 rename 会覆盖不掉，或误导排查）
	if _, serr := os.Stat(filepath.Join(current, "haproxy.cfg.tmp")); serr == nil {
		t.Fatal("原子写入的临时文件应被清理")
	}
}

func TestHAProxyApplyUsesSystemctlReloadNotKill(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "versions", "v1")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "haproxy.cfg"), []byte("global\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	h := NewHAProxy()
	h.ConfigRoot = root
	h.CurrentDir = filepath.Join(root, "current")
	f := &fakeRunner{}
	h.SetRunner(f.run)

	if err := h.Apply(context.Background(), ApplyRequest{
		ConfigDir: dir, ConfigPath: filepath.Join(dir, "haproxy.cfg"), Version: 1,
	}); err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	// 需求 §五：不通过 pkill 等方式影响无关进程
	for _, c := range f.calls {
		for _, bad := range []string{"pkill", "killall", "kill -9", "killall5"} {
			if containsSub(c, bad) {
				t.Fatalf("发布流程里出现了被禁止的强制杀进程命令: %s", c)
			}
		}
	}
	joined := ""
	for _, c := range f.calls {
		joined += c + "\n"
	}
	if !containsSub(joined, "systemctl reload shengyu-edgelink-haproxy") {
		t.Fatalf("应使用 systemctl reload 做平滑生效，实际调用:\n%s", joined)
	}
	// VERSION 必须最后写入，且内容正确
	ver, err := h.ActiveVersion(context.Background())
	if err != nil || ver != 1 {
		t.Fatalf("生效版本应为 1，实际 %d %v", ver, err)
	}
}

func TestParseProcNetTCP(t *testing.T) {
	// 0100007F:1F90 = 127.0.0.1:8080（小端）
	content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:01BB 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12345 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12346 1 0000000000000000 100 0 0 10 0
   2: 0100007F:1F91 0100007F:0050 01 00000000:00000000 00:00000000 00000000     0        0 12347 1 0000000000000000 100 0 0 10 0
`
	out := map[int][]string{}
	parseProcNetTCP(content, out)
	if len(out[443]) != 1 || out[443][0] != "0.0.0.0" {
		t.Fatalf("443 应解析为 0.0.0.0，实际 %v", out[443])
	}
	if len(out[8080]) != 1 || out[8080][0] != "127.0.0.1" {
		t.Fatalf("8080 应解析为 127.0.0.1，实际 %v", out[8080])
	}
	// 非 LISTEN（st=01）不应出现
	if _, ok := out[80]; ok {
		t.Fatalf("非监听状态的连接不应被算作监听端口: %v", out[80])
	}
}

func TestDecodeProcIPv6(t *testing.T) {
	// ::1 的小端表示
	if got := decodeProcIPv6ForTest("00000000000000000000000001000000"); got != "::1" {
		t.Fatalf("IPv6 解析错误: %q", got)
	}
	// 无效长度
	if got := decodeProcIP("ZZZZZZZZ"); got != "" {
		t.Fatalf("非法十六进制应返回空，got %q", got)
	}
}

func decodeProcIPv6ForTest(h string) string { return decodeProcIP(h) }

// Verify 必须同时给出两类结论，缺一不可：
//
//	① 缺监听 —— 期望端口没在监听；② 无法确认运行版本 —— 这正是评审指出的
//	   "用磁盘标记冒充运行版本" 与 "用任意监听冒充自己的进程" 两个缺陷的回归点。
//
// 本用例比原先**更强**：原先只断言"缺 443 之外的那个端口被报出"，
// 现在同时要求"运行版本无法确认"也必须是问题，且 443 虽然有人在听也不能算作我们自己的。
func TestVerifyDetectsMissingListenerAndUnknownRuntime(t *testing.T) {
	h := NewHAProxy()
	// /proc 里只有 443 在听，缺 25565
	h.SetFileReader(func(name string) ([]byte, error) {
		if name == "/proc/net/tcp" {
			return []byte("sl local_address rem_address st\n0: 00000000:01BB 00000000:0000 0A 0\n"), nil
		}
		return nil, os.ErrNotExist
	})
	st := modelStateWith([]int{443}, []int{25565})
	res, err := h.Verify(context.Background(), VerifyRequest{State: st, Version: 2})
	if err != nil {
		t.Fatalf("Verify 报错: %v", err)
	}
	if res.OK {
		t.Fatal("既缺监听、又无法确认运行版本，绝不允许判定通过")
	}

	var sawMissing, sawRuntimeUnconfirmed bool
	for _, p := range res.Problems {
		if containsSub(p, "25565") {
			sawMissing = true
		}
		if containsSub(p, "无法确认") {
			sawRuntimeUnconfirmed = true
		}
	}
	if !sawMissing {
		t.Fatalf("必须报出缺失端口 25565，实际 %v", res.Problems)
	}
	if !sawRuntimeUnconfirmed {
		t.Fatalf("必须报出「运行版本无法确认」——否则就是在用磁盘标记冒充运行状态，实际 %v", res.Problems)
	}

	// 443 端口确实有人在监听，但监听者不可能是我们的进程（统计接口都连不上），
	// 因此 Bound 为 true 而 OwnedByUs 必须为 false。
	found443 := false
	for _, l := range res.Listeners {
		if l.Expected.Port != 443 {
			continue
		}
		found443 = true
		if !l.Bound {
			t.Error("443 有进程在监听，Bound 应为 true")
		}
		if l.OwnedByUs {
			t.Error("无法确认监听者是自己时，OwnedByUs 必须为 false —— 这是评审点名的缺陷")
		}
		if l.Frontend == "" {
			t.Error("应给出该监听期望的 frontend 名，才能做归属核对")
		}
	}
	if !found443 {
		t.Fatal("结果里应包含 443 这一项")
	}
}

func TestStatsCSVParsedByColumnName(t *testing.T) {
	// 故意在开头插入一列（新版 HAProxy 会这么干）。
	// 按下标解析的实现会在这里悄悄取错字段 —— 这正是本测试要挡住的事。
	csv := "# pxname,extra_column,svname,scur,stot,bin,bout,qcur,econ,eresp,status\n" +
		"fe_sni_443,X,FRONTEND,7,1234,5000,9000,2,3,1,OPEN\n" +
		"bk_biza,Y,BACKEND,3,900,4000,8000,0,0,0,UP\n" +
		"bk_bizb,Y,BACKEND,0,10,10,10,0,0,0,DOWN\n"
	s := Stats{}
	parseHAProxyStatsCSV(csv, &s)
	if s.ActiveConns != 7 || s.TotalConns != 1234 {
		t.Fatalf("连接数解析错误: %+v", s)
	}
	if s.BytesIn != 5000 || s.BytesOut != 9000 {
		t.Fatalf("字节数解析错误: %+v", s)
	}
	if s.QueueDepth != 2 || s.ConnErrors != 4 {
		t.Fatalf("队列/错误数解析错误: %+v", s)
	}
	if s.BackendUp != 1 || s.BackendDown != 1 {
		t.Fatalf("后端健康统计错误: %+v", s)
	}
}

func TestActiveVersionZeroWhenNeverPublished(t *testing.T) {
	h := NewHAProxy()
	h.CurrentDir = filepath.Join(t.TempDir(), "current")
	v, err := h.ActiveVersion(context.Background())
	if err != nil {
		t.Fatalf("未发布过不该报错: %v", err)
	}
	if v != 0 {
		t.Fatalf("未发布过版本号应为 0，got %d", v)
	}
}

func TestActiveVersionRejectsGarbageMarker(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "current")
	if err := os.MkdirAll(current, 0o750); err != nil {
		t.Fatal(err)
	}
	h := NewHAProxy()
	h.CurrentDir = current

	// 内容非法必须报错，而不是"猜"一个版本号
	if err := os.WriteFile(filepath.Join(current, VersionFileName), []byte("不是数字\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ActiveVersion(context.Background()); err == nil {
		t.Fatal("版本标记内容非法时必须报错（否则界面会显示一个不存在的版本号）")
	}
	// 空文件同样必须报错
	if err := os.WriteFile(filepath.Join(current, VersionFileName), []byte("  \n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ActiveVersion(context.Background()); err == nil {
		t.Fatal("版本标记为空时必须报错")
	}
	// 负数必须报错
	if err := os.WriteFile(filepath.Join(current, VersionFileName), []byte("-3\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ActiveVersion(context.Background()); err == nil {
		t.Fatal("负数版本必须报错")
	}
}

// ---- 小工具 ----

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// modelStateWith 构造一个只关心监听端口的期望状态，用于验证 Verify 的比对逻辑。
func modelStateWith(sniPorts, tcpPorts []int) model.DesiredState {
	st := model.DesiredState{Node: model.Node{ID: "n1"}}
	for i, p := range sniPorts {
		id := "s" + strconv.Itoa(i)
		st.SNIEntries = append(st.SNIEntries, model.SNIEntry{
			ID: id, NodeID: "n1", BindAddr: "0.0.0.0", BindPort: p, Enabled: true})
		// 每个被传入的 SNI 入口都挂一条启用路由：**空入口按设计是不下发的**
		// （见 sniEntriesInUse 的说明），不挂路由的话它根本不该出现在期望监听里，
		// 而这些用例想验的是"端口有人听但不是我们的 frontend"。
		st.Routes = append(st.Routes, model.RouteView{
			Route: model.Route{ID: "rs" + strconv.Itoa(i), BusinessID: "bs" + strconv.Itoa(i),
				NodeID: "n1", Mode: model.ModeSNITLS, SNIEntryID: id,
				OriginHost: "203.0.113.1", OriginPort: 443, Enabled: true},
			BusinessName: "sni-biz-" + strconv.Itoa(i), Mode: model.ModeSNITLS,
			Domains: []string{"s" + strconv.Itoa(i) + ".example.com"}, Enabled: true})
	}
	for i, p := range tcpPorts {
		st.Routes = append(st.Routes, model.RouteView{
			Route: model.Route{ID: "r" + strconv.Itoa(i), BusinessID: "b" + strconv.Itoa(i),
				EntryAddr: "0.0.0.0", EntryPort: p, OriginHost: "203.0.113.1", OriginPort: 80, Enabled: true},
			Mode: model.ModeTCPPort, Enabled: true})
	}
	return st
}

var _ = time.Second
