package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shengyu/edgelink/internal/agent"
	"github.com/shengyu/edgelink/internal/id"
	"github.com/shengyu/edgelink/internal/logstore"
	"github.com/shengyu/edgelink/internal/model"
	"github.com/shengyu/edgelink/internal/store"
)

// 本文件钉住"日志摄入健康在总览页可见"。
//
// 为什么值得单开一组测试：日志写入失败是**静默**的 ——
// 既不报错也不影响转发，只在 Ingestor / Batcher 里累加一个计数器。
// 于是界面上"查不到日志"会被解释成"这段时间确实没有连接"，
// 而真实原因可能是写盘失败或缓冲区溢出。这类误判最费排查时间，
// 所以"丢弃计数必须能被看到"要有一条测试守着（真机证据见 docs/08 §10.7）。

// nilResolver 不解析任何 backend：本用例只关心"统计能否暴露"，
// 富化结果由 logparse / agent 各自的测试覆盖。
type nilResolver struct{}

func (nilResolver) BusinessForBackend(string) (string, string, bool) { return "", "", false }
func (nilResolver) OriginForBackend(string) (string, int, bool)      { return "", 0, false }

func newOverviewHarness(t *testing.T) (*Server, *logstore.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	logs, err := logstore.New(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logs.Close() })
	nodeID := id.New("node")
	if err := st.CreateNode(&model.Node{
		ID: nodeID, Name: "总览测试节点", Enabled: true, Health: model.NodeOnline,
	}); err != nil {
		t.Fatal(err)
	}
	return New(st, logs, nil, Config{SessionTTL: time.Hour}), logs, nodeID
}

func overviewBody(t *testing.T, srv *Server) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	// 直接调 handler：/api/overview 在路由上带 auth，auth 另有测试覆盖。
	srv.handleOverview(rec, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("总览接口应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	return body
}

// 挂上 Ingestor/Batcher 后，总览里必须能看到摄入计数。
func TestOverviewExposesLogIngestHealth(t *testing.T) {
	srv, logs, nodeID := newOverviewHarness(t)
	ing := agent.NewIngestor(logs, nilResolver{}, nodeID)
	srv.Ingest = ing
	srv.Batch = agent.NewBatcher(ing)

	// 真正写进去一条，让计数不是全 0 —— 全 0 的字段无法证明"接线是通的"。
	line := `{"ts":"2026-09-25T03:00:00Z","node":"` + nodeID + `","ver":1,"ci":"127.0.0.1","cp":"1",` +
		`"fi":"0.0.0.0","fp":"8443","sni":"x.example.com","be":"bk_x","srv":"s1",` +
		`"tq":"-","tc":"-","tt":"10","up":"1","down":"2","oip":"1.1.1.1","oport":"443",` +
		`"tsc":"CD","err":"-","rc":"0","bq":"0","uid":"c1"}`
	if _, err := ing.IngestLines([]string{line}); err != nil {
		t.Fatalf("写入一条日志不该失败: %v", err)
	}

	body := overviewBody(t, srv)
	raw, ok := body["log_ingest"]
	if !ok {
		t.Fatal("总览必须暴露日志摄入健康（否则写入失败永远不可见）")
	}
	info, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("log_ingest 应是对象，实际 %T", raw)
	}
	for _, key := range []string{"ingested", "duplicated", "parse_errors", "dropped"} {
		if _, ok := info[key]; !ok {
			t.Fatalf("log_ingest 缺少关键计数 %q：%v", key, info)
		}
	}
	if v, _ := info["ingested"].(float64); v < 1 {
		t.Fatalf("刚写入一条，ingested 应 ≥1，实际 %v", info["ingested"])
	}
	if _, ok := info["warning"]; ok {
		t.Fatalf("没有丢弃时不该出现 warning（否则界面会常年挂着一条假告警）：%v", info)
	}
}

// 有丢弃时：必须给出 warning，并且说清"这可能就是查不到日志的原因"。
//
// 这里用**缓冲溢出**来造丢弃（把 MaxBuffer 压到 2 行再塞 5 行）而不是"关掉日志库"：
// 后者并不会让写入失败 —— 分片连接是按需重新打开的，
// 于是测试会得到一个假的"没有丢弃"，反而把真正的分支漏掉。
func TestOverviewWarnsWhenLogsWereDropped(t *testing.T) {
	srv, logs, nodeID := newOverviewHarness(t)
	ing := agent.NewIngestor(logs, nilResolver{}, nodeID)
	srv.Ingest = ing
	b := agent.NewBatcher(ing)
	b.MaxBuffer = 2  // 只留 2 行，超出就丢最旧并计数
	b.Size = 1 << 30 // 关掉自动冲刷，保证丢弃发生在缓冲区里
	srv.Batch = b
	for i := 0; i < 5; i++ {
		b.Add(`{"ts":"2026-09-25T03:00:00Z","node":"x","ver":1}`)
	}
	if drops := b.Stats().BufferDrops; drops < 1 {
		t.Fatalf("本用例应造出缓冲丢弃，实际 %d", drops)
	}

	body := overviewBody(t, srv)
	raw, ok := body["log_ingest"]
	if !ok {
		t.Fatal("总览必须暴露日志摄入健康")
	}
	info := raw.(map[string]any)
	if v, _ := info["buffer_drops"].(float64); v < 1 {
		t.Fatalf("缓冲丢弃必须被报出来，实际 %v", info["buffer_drops"])
	}
	warn, ok := info["warning"].(string)
	if !ok || warn == "" {
		t.Fatalf("有丢弃时必须给出 warning，实际 %v", info)
	}
	// 措辞必须把"查不到日志"与"没有连接"区分开 —— 这正是这条 warning 存在的理由。
	if !strings.Contains(warn, "查不到日志") {
		t.Fatalf("warning 要指出它会导致「查不到日志」，实际 %q", warn)
	}
	// pending 也要在：运维据此判断"是不是堆积了"。
	if _, ok := info["pending"]; !ok {
		t.Fatalf("应带上缓冲区待写入行数，实际 %v", info)
	}
}

// 未挂载摄入链路（例如纯远程节点管理）时：字段为 null 且不 panic。
func TestOverviewLogIngestNullWhenNotWired(t *testing.T) {
	srv, _, _ := newOverviewHarness(t)
	body := overviewBody(t, srv)
	if v, ok := body["log_ingest"]; !ok || v != nil {
		t.Fatalf("未挂载时 log_ingest 应为 null，实际 %v", v)
	}
}
