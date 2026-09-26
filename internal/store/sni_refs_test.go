package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shengyu/edgelink/internal/id"
	"github.com/shengyu/edgelink/internal/model"
)

// 本文件钉住"删除共享 SNI 入口"的引用守卫，以及"悬空引用"自检。
//
// 为什么这两件事必须放在存储层测（而不是只测 HTTP 接口）：
// "不允许留下悬空引用"是**数据完整性约束**。接口是可以被绕过的（批量脚本、未来的其它调用方），
// 约束只有落在 store 里才守得住。真机上的那次事故就是绕过了它 ——
// 当时没有删除接口，验收只能改库，于是留下一条悬空引用，
// 之后**每次**发布都被前置校验拒成 422（docs/08 §8）。

func newRefStore(t *testing.T) (*Store, string) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	nodeID := id.New("node")
	if err := st.CreateNode(&model.Node{
		ID: nodeID, Name: "引用测试节点", Enabled: true, Health: model.NodeOnline,
	}); err != nil {
		t.Fatal(err)
	}
	return st, nodeID
}

// seedEntryWithRoute 造一个入口 + 一条挂在该入口上的业务路由，返回 (入口ID, 业务名)。
func seedEntryWithRoute(t *testing.T, st *Store, nodeID string, port int) (string, string) {
	t.Helper()
	entryID := id.New("sni")
	if err := st.CreateSNIEntry(&model.SNIEntry{
		ID: entryID, NodeID: nodeID, BindAddr: "0.0.0.0", BindPort: port, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	custID := id.New("cus")
	if err := st.CreateCustomer(&model.Customer{ID: custID, Name: "引用测试客户"}); err != nil {
		t.Fatal(err)
	}
	const bizName = "引用测试业务"
	bizID := id.New("biz")
	if err := st.CreateBusiness(&model.Business{
		ID: bizID, CustomerID: custID, Name: bizName, Mode: model.ModeSNITLS,
		PrimaryNodeID: nodeID, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRoute(&model.Route{
		ID: id.New("rt"), BusinessID: bizID, NodeID: nodeID, Role: model.RolePrimary,
		Mode: model.ModeSNITLS, SNIEntryID: entryID,
		OriginHost: "1.1.1.1", OriginPort: 443, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	return entryID, bizName
}

// 有路由引用 ⇒ **拒绝**，而且必须原样保留（不能"删掉一半"）。
func TestDeleteSNIEntryRefusesWhenReferenced(t *testing.T) {
	st, nodeID := newRefStore(t)
	entryID, bizName := seedEntryWithRoute(t, st, nodeID, 8443)

	err := st.DeleteSNIEntry(entryID)
	if err == nil {
		t.Fatal("被业务路由引用的入口不允许删除（删了会留下悬空引用，渲染时那条路由会被静默丢掉）")
	}
	if !errors.Is(err, ErrSNIEntryInUse) {
		t.Fatalf("必须是 ErrSNIEntryInUse（上层据此返回 409 并给出不同建议），实际 %v", err)
	}
	// 报错要点名是哪些业务 —— 只说"被 1 条引用"会让运维去翻整个业务列表。
	if !strings.Contains(err.Error(), bizName) {
		t.Fatalf("报错要点名引用的业务名，实际 %v", err)
	}
	rows, lerr := st.ListSNIEntries(nodeID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(rows) != 1 || rows[0].ID != entryID {
		t.Fatalf("拒绝删除时入口必须原样保留，实际 %v", rows)
	}
}

// 无引用 ⇒ 允许删除，且删除后确实不在了。
func TestDeleteSNIEntryAllowsWhenUnreferenced(t *testing.T) {
	st, nodeID := newRefStore(t)
	entryID := id.New("sni")
	if err := st.CreateSNIEntry(&model.SNIEntry{
		ID: entryID, NodeID: nodeID, BindAddr: "0.0.0.0", BindPort: 9443, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSNIEntry(entryID); err != nil {
		t.Fatalf("无引用的入口应当允许删除，实际 %v", err)
	}
	rows, _ := st.ListSNIEntries(nodeID)
	if len(rows) != 0 {
		t.Fatalf("删除后不应再列出该入口，实际 %v", rows)
	}
}

// 删除不存在的入口 ⇒ ErrNotFound（上层映射 404），而不是静默成功。
func TestDeleteSNIEntryNotFound(t *testing.T) {
	st, _ := newRefStore(t)
	err := st.DeleteSNIEntry("sni_not_exist")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除不存在的入口应返回 ErrNotFound，实际 %v", err)
	}
}

// 悬空引用自检：能发现"路由引用了不存在的入口"（真机上那条就是它）。
//
// 构造方式刻意采用**直接改库**（而不是走接口）—— 因为这类脏数据的来源正是"绕过守卫的写操作"，
// 自检要能在那种情况下把它揪出来。
func TestFindDanglingReferencesDetectsMissingSNIEntry(t *testing.T) {
	st, nodeID := newRefStore(t)
	entryID, _ := seedEntryWithRoute(t, st, nodeID, 8443)

	// 改库删掉入口（routes.sni_entry_id 没有外键，所以数据库不会拦 —— 这正是危险之处）。
	if _, err := st.DB().Exec(`DELETE FROM sni_entries WHERE id=?`, entryID); err != nil {
		t.Fatal(err)
	}
	refs, err := st.FindDanglingReferences()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("应查出 1 条悬空引用，实际 %#v", refs)
	}
	if refs[0].Kind != "sni_entry" || refs[0].MissingID != entryID {
		t.Fatalf("应指出是 sni_entry 悬空且给出缺失的 ID，实际 %#v", refs[0])
	}
	if !strings.HasPrefix(refs[0].From, "rt_") {
		t.Fatalf("From 应指向来源路由记录，实际 %q", refs[0].From)
	}
	if !strings.Contains(refs[0].Detail, "静默丢掉") {
		t.Fatalf("Detail 要说明后果（渲染时被静默丢掉），实际 %q", refs[0].Detail)
	}
}

// 健康数据上自检必须**为空**：误报会让诊断页常年挂一条假问题，运维很快就学会忽略它。
func TestFindDanglingReferencesCleanOnHealthyData(t *testing.T) {
	st, nodeID := newRefStore(t)
	seedEntryWithRoute(t, st, nodeID, 8443)
	refs, err := st.FindDanglingReferences()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("健康数据不应报悬空引用，实际 %#v", refs)
	}
}
