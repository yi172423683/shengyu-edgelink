package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/shengyu/edgelink/internal/id"
	"github.com/shengyu/edgelink/internal/logstore"
	"github.com/shengyu/edgelink/internal/model"
	"github.com/shengyu/edgelink/internal/store"
)

// 本文件覆盖**远程日志上报协议**（评审：远程上报与入库补齐 boot_id）。
//
// 三条断言对应三种真实场景：
//
//  1. 重传（网络抖动导致 agent 重发同一批）→ 必须判重，不能产生重复行；
//  2. 重启（agent 进程重启，序号从 1 重来）→ 必须全部写进去，不能被误判成重复；
//  3. 旧协议（不带 boot_id 的 agent）→ 必须明确拒绝并说明原因，
//     而不是"照收、顺便把重启那一段日志静默丢掉"。

type agentHarness struct {
	t     *testing.T
	srv   *Server
	logs  *logstore.Store
	cred  string
	node  string
	httpH http.Handler
}

func newAgentHarness(t *testing.T) *agentHarness {
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
	if err := st.CreateNode(&model.Node{ID: nodeID, Name: "远程测试节点", Enabled: true, Health: model.NodeOnline}); err != nil {
		t.Fatal(err)
	}
	tok, err := st.CreateAgentToken(nodeID, "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := st.IssueNodeCredential(nodeID, "test")
	if err != nil {
		t.Fatal(err)
	}
	_ = tok
	srv := New(st, logs, nil, Config{SessionTTL: time.Hour})
	return &agentHarness{t: t, srv: srv, logs: logs, cred: cred, node: nodeID, httpH: srv.Handler()}
}

func (h *agentHarness) postLogs(bootID string, rows []map[string]any) *httptest.ResponseRecorder {
	h.t.Helper()
	body := map[string]any{"rows": rows}
	if bootID != "" {
		body["boot_id"] = bootID
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/agent/logs", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Token", h.cred)
	rec := httptest.NewRecorder()
	h.httpH.ServeHTTP(rec, req)
	return rec
}

func line(node string, ver int) string {
	return fmt.Sprintf(`{"ts":"22/Sep/2026:15:04:05.123","node":%q,"ver":%d,"ci":"203.0.113.9","tsc":"--"}`, node, ver)
}

func (h *agentHarness) countRows() int {
	h.t.Helper()
	rows, _, err := h.logs.QueryConn(context.Background(), logstore.Query{
		From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour),
		NodeID: h.node, Limit: logstore.MaxLimit,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return len(rows)
}

func TestAgentLogsRequiresBootID(t *testing.T) {
	h := newAgentHarness(t)
	// 旧协议：没有 boot_id
	rec := h.postLogs("", []map[string]any{{"seq": 1, "line": line(h.node, 1)}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺少 boot_id 的上报必须被拒绝（否则重启后的日志会被静默丢弃），实际 %d: %s",
			rec.Code, rec.Body.String())
	}
	if got := h.countRows(); got != 0 {
		t.Fatalf("被拒绝的请求不应写入任何数据，实际 %d 行", got)
	}
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Code != "missing_boot_id" {
		t.Fatalf("应给出可编程识别的错误码，实际 %q", body.Code)
	}
	// 空格串同样视为缺失（不能靠 TrimSpace 之后的空串绕过校验）
	rec = h.postLogs("   ", []map[string]any{{"seq": 1, "line": line(h.node, 1)}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空白 boot_id 也必须被拒绝，实际 %d", rec.Code)
	}
}

func TestAgentLogsRetransmitAndRestart(t *testing.T) {
	h := newAgentHarness(t)
	batch := []map[string]any{
		{"seq": 1, "line": line(h.node, 1)},
		{"seq": 2, "line": line(h.node, 2)},
	}

	rec := h.postLogs("boot-A", batch)
	if rec.Code != http.StatusOK {
		t.Fatalf("首次上报应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if got := h.countRows(); got != 2 {
		t.Fatalf("首次上报应写入 2 行，实际 %d", got)
	}

	// ① 重传：同一 boot_id 同一批 → 全部判重，不新增
	rec = h.postLogs("boot-A", batch)
	if rec.Code != http.StatusOK {
		t.Fatalf("重传应被接受（幂等），实际 %d: %s", rec.Code, rec.Body.String())
	}
	var acc struct {
		Accepted   int `json:"accepted"`
		Duplicated int `json:"duplicated"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &acc)
	if acc.Accepted != 0 || acc.Duplicated != 2 {
		t.Fatalf("重传必须全部判重，实际 %+v", acc)
	}
	if got := h.countRows(); got != 2 {
		t.Fatalf("重传不应增加行数，实际 %d", got)
	}

	// ② 重启：新 boot_id，序号从 1 重新开始 → 必须全部写入
	rec = h.postLogs("boot-B", batch)
	if rec.Code != http.StatusOK {
		t.Fatalf("重启后上报应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &acc)
	if acc.Accepted != 2 || acc.Duplicated != 0 {
		t.Fatalf("重启后序号从 1 重来必须被当作新数据（评审 F07 的远程侧），实际 %+v", acc)
	}
	if got := h.countRows(); got != 4 {
		t.Fatalf("重启前后共应有 4 行，实际 %d（说明重启后的日志被丢弃了）", got)
	}
}

func TestAgentLogsRejectsOversizedBatch(t *testing.T) {
	h := newAgentHarness(t)
	rows := make([]map[string]any, MaxAgentLogRows+1)
	for i := range rows {
		rows[i] = map[string]any{"seq": i + 1, "line": line(h.node, 1)}
	}
	rec := h.postLogs("boot-big", rows)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超过单次上限的上报应被拒绝（否则被攻陷的 agent 能吃光内存），实际 %d", rec.Code)
	}
}

// 入库时按该日志所属版本富化：新版本发布后结束的旧连接不能被归到新源站。
func TestAgentLogsEnrichedByLogVersion(t *testing.T) {
	h := newAgentHarness(t)

	// 造两个配置版本，各自带**不同的源站**快照
	bizID := id.New("biz")
	beName := "bk_" + id.Handle(bizID)
	if err := h.srv.Store.CreateConfigVersion(&store.ConfigVersion{
		ID: id.New("cfgver"), NodeID: h.node, Version: 1, ContentHash: "h1",
		Status: store.CfgActive, RouteCount: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.srv.Store.CreateConfigVersion(&store.ConfigVersion{
		ID: id.New("cfgver"), NodeID: h.node, Version: 2, ContentHash: "h2",
		Status: store.CfgActive, RouteCount: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.srv.Store.SetConfigVersionObjectNames(h.node, 1, map[string]any{
		"backend_to_business": map[string]string{beName: bizID},
		"backend_to_origin":   map[string]string{beName: "198.51.100.1:8443"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.srv.Store.SetConfigVersionObjectNames(h.node, 2, map[string]any{
		"backend_to_business": map[string]string{beName: bizID},
		"backend_to_origin":   map[string]string{beName: "198.51.100.2:8443"},
	}); err != nil {
		t.Fatal(err)
	}

	// 一条 ver=1 的日志（旧 worker 的长连接在新版本发布后才结束）
	old := fmt.Sprintf(`{"ts":"22/Sep/2026:15:04:05.123","node":%q,"ver":1,"ci":"203.0.113.9","be":%q,"srv":"s1","tsc":"--"}`,
		h.node, beName)
	rec := h.postLogs("boot-v1", []map[string]any{{"seq": 1, "line": old}})
	if rec.Code != http.StatusOK {
		t.Fatalf("上报失败 %d: %s", rec.Code, rec.Body.String())
	}
	rows, _, err := h.logs.QueryConn(context.Background(), logstore.Query{
		From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour),
		NodeID: h.node, Limit: logstore.MaxLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("应有 1 行，实际 %d", len(rows))
	}
	r := rows[0]
	if r.BusinessID != bizID {
		t.Fatalf("业务归属应解析出来，实际 %q", r.BusinessID)
	}
	if r.OriginAddr != "198.51.100.1" {
		t.Fatalf("ver=1 的日志必须按**第 1 版快照**解析源站（198.51.100.1），实际 %q —— "+
			"用当前配置反查会把跨版本长连接指向错误的源站", r.OriginAddr)
	}
}
