package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shengyu/edgelink/internal/id"
	"github.com/shengyu/edgelink/internal/logstore"
	"github.com/shengyu/edgelink/internal/model"
	"github.com/shengyu/edgelink/internal/store"
)

// 本文件覆盖两件事：
//
//  1. `DELETE /api/nodes/{id}/sni-entries/{eid}` 的语义（拒绝 / 允许 / 提示重新发布 / 审计）；
//  2. `/api/diagnose` 能否报出**悬空引用**。
//
// 为什么单给它们开一组测试：这是"删掉一条记录会让别的记录变成悬空引用"的唯一入口，
// 而悬空引用在本平台会退化成最难查的故障（渲染器按入口 ID 索引，找不到就**静默丢掉**那条路由
// ⇒ "发布成功但业务不转发"）。真机上正因为缺这个接口，验收只能改库，
// 才留下一条悬空引用，之后每次发布都被 422 拒掉（docs/08 §8）。

type sniHarness struct {
	t      *testing.T
	srv    *Server
	st     *store.Store
	nodeID string
}

func newSNIHarness(t *testing.T) *sniHarness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// diagnose 会用日志库；这里给一个空的真实库（不是 nil），
	// 否则测的是"日志库为空时会不会 panic"，那不是本用例关心的事。
	logs, err := logstore.New(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logs.Close() })

	nodeID := id.New("node")
	if err := st.CreateNode(&model.Node{
		ID: nodeID, Name: "本机验收节点", Enabled: true, Health: model.NodeOnline,
	}); err != nil {
		t.Fatal(err)
	}
	return &sniHarness{
		t: t, srv: New(st, logs, nil, Config{SessionTTL: time.Hour}),
		st: st, nodeID: nodeID,
	}
}

// deleteEntry 走**真实的路由匹配**（这样 PathValue 才会被填充），但不经过 auth/csrf 中间件 ——
// 那两层由别的测试覆盖，这里只关心"删除的语义"。
func (h *sniHarness) deleteEntry(entryID string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/nodes/{id}/sni-entries/{eid}", h.srv.handleDeleteSNIEntry)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete,
		"/api/nodes/"+h.nodeID+"/sni-entries/"+entryID, nil))
	return rec
}

// seedEntry 造一个入口；withRoute=true 时再挂一条业务路由上去。返回 (入口ID, 业务名)。
func (h *sniHarness) seedEntry(t *testing.T, withRoute bool) (string, string) {
	t.Helper()
	entryID := id.New("sni")
	if err := h.st.CreateSNIEntry(&model.SNIEntry{
		ID: entryID, NodeID: h.nodeID, BindAddr: "0.0.0.0", BindPort: 9443, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if !withRoute {
		return entryID, ""
	}
	custID := id.New("cus")
	if err := h.st.CreateCustomer(&model.Customer{ID: custID, Name: "删除接口测试客户"}); err != nil {
		t.Fatal(err)
	}
	const bizName = "删除接口测试业务"
	bizID := id.New("biz")
	if err := h.st.CreateBusiness(&model.Business{
		ID: bizID, CustomerID: custID, Name: bizName, Mode: model.ModeSNITLS,
		PrimaryNodeID: h.nodeID, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.CreateRoute(&model.Route{
		ID: id.New("rt"), BusinessID: bizID, NodeID: h.nodeID, Role: model.RolePrimary,
		Mode: model.ModeSNITLS, SNIEntryID: entryID,
		OriginHost: "1.1.1.1", OriginPort: 443, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	return entryID, bizName
}

// 有引用 ⇒ 409 `sni_entry_in_use`，并且**入口必须还在**。
func TestDeleteSNIEntryEndpointRefusesWhenReferenced(t *testing.T) {
	h := newSNIHarness(t)
	entryID, bizName := h.seedEntry(t, true)

	rec := h.deleteEntry(entryID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("有引用时必须 409，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if body.Code != "sni_entry_in_use" {
		t.Fatalf("错误码应是 sni_entry_in_use（界面据此给出不同建议），实际 %q", body.Code)
	}
	if !strings.Contains(body.Error, bizName) {
		t.Fatalf("错误信息要点名引用的业务，实际 %q", body.Error)
	}
	rows, _ := h.st.ListSNIEntries(h.nodeID)
	if len(rows) != 1 {
		t.Fatalf("拒绝删除时入口必须原样保留，实际 %v", rows)
	}
}

// 无引用 ⇒ 200，且必须提示"要重新发布"，并写审计。
func TestDeleteSNIEntryEndpointAllowsWhenUnreferenced(t *testing.T) {
	h := newSNIHarness(t)
	entryID, _ := h.seedEntry(t, false)

	rec := h.deleteEntry(entryID)
	if rec.Code != http.StatusOK {
		t.Fatalf("无引用时应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		OK       bool   `json:"ok"`
		NextStep string `json:"next_step"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if !body.OK {
		t.Fatal("应返回 ok=true")
	}
	// 删掉入口只改了"期望状态"；不发布的话，界面上的入口列表与节点上跑的东西就不一致了。
	if !strings.Contains(body.NextStep, "发布") {
		t.Fatalf("必须提示需要重新发布，实际 %q", body.NextStep)
	}
	if rows, _ := h.st.ListSNIEntries(h.nodeID); len(rows) != 0 {
		t.Fatalf("删除后不应再列出该入口，实际 %v", rows)
	}
	entries, _, aerr := h.st.ListAudit(store.AuditFilter{Action: "sni_entry.delete"})
	if aerr != nil {
		t.Fatal(aerr)
	}
	if len(entries) != 1 {
		t.Fatalf("删除入口必须写审计（它改变所有挂在该入口上的业务的可达性），实际 %d 条", len(entries))
	}
	if !strings.Contains(entries[0].Summary, "0.0.0.0:9443") {
		t.Fatalf("审计摘要要点出是哪个入口，实际 %q", entries[0].Summary)
	}
}

// 删一个不存在的入口 ⇒ 404（而不是 200 假成功）。
func TestDeleteSNIEntryEndpointNotFound(t *testing.T) {
	h := newSNIHarness(t)
	if rec := h.deleteEntry("sni_not_exist"); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的入口应 404，实际 %d", rec.Code)
	}
}

// 诊断页必须报出悬空引用 —— 这是它"提前暴露"价值的来源：
// 真机上这个问题第一次暴露是"每次发布都被 422 拒掉"，而那时运维正在改另一件事。
func TestDiagnoseReportsDanglingReference(t *testing.T) {
	h := newSNIHarness(t)
	entryID, _ := h.seedEntry(t, true)

	// 模拟"绕过守卫的改库删除"：routes.sni_entry_id 没有外键，数据库不会拦。
	if _, err := h.st.DB().Exec(`DELETE FROM sni_entries WHERE id=?`, entryID); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	// 直接调 handler：/api/diagnose 在路由上带 auth，而 auth 另有测试覆盖。
	h.srv.handleDiagnose(rec, httptest.NewRequest(http.MethodGet, "/api/diagnose", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("诊断接口应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Findings []Finding `json:"findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	var got *Finding
	for i := range body.Findings {
		if body.Findings[i].Category == "dangling_reference" {
			got = &body.Findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("诊断结果里必须有一条 dangling_reference，实际 %s", rec.Body.String())
	}
	if got.Severity != "error" {
		t.Fatalf("悬空引用必须是 error 级（它会静默吞掉一条业务规则），实际 %q", got.Severity)
	}
	if !strings.Contains(strings.Join(got.Facts, " "), entryID) {
		t.Fatalf("facts 要点出缺失的入口 ID，实际 %v", got.Facts)
	}
	if !strings.Contains(strings.Join(got.Facts, " "), "SNI 共享入口") {
		t.Fatalf("facts 要说人话（SNI 共享入口），实际 %v", got.Facts)
	}
	if !strings.Contains(strings.Join(got.Next, " "), "发布") {
		t.Fatalf("next_steps 必须提醒「修完数据后要发布」，实际 %v", got.Next)
	}
}
