package publish

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/shengyu/edgelink/internal/haproxy"
	"github.com/shengyu/edgelink/internal/id"
	"github.com/shengyu/edgelink/internal/model"
	"github.com/shengyu/edgelink/internal/store"
)

// seedSNIBusiness 造一条挂在共享 SNI 入口上的业务 —— 渲染器会为它产出一个允许清单文件，
// 这样清单才有"逐版增删"可言（否则清单里永远只有 cfg/meta/state 三个文件）。
func seedSNIBusiness(t *testing.T, st *store.Store, nodeID, sniID string) {
	t.Helper()
	cust := &model.Customer{ID: id.New("cus"), Name: "清单测试客户"}
	if err := st.CreateCustomer(cust); err != nil {
		t.Fatal(err)
	}
	biz := &model.Business{
		ID: id.New("biz"), CustomerID: cust.ID, Name: "清单测试业务", Mode: model.ModeSNITLS,
		PrimaryNodeID: nodeID, Enabled: true, Domains: []string{"manifest.example.com"},
	}
	if err := st.CreateBusiness(biz); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRoute(&model.Route{
		ID: id.New("rt"), BusinessID: biz.ID, NodeID: nodeID, Role: model.RolePrimary,
		Mode: model.ModeSNITLS, SNIEntryID: sniID,
		OriginHost: "1.1.1.1", OriginPort: 443, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
}

// 发布候选版本时必须落一份**完整文件清单**（manifest.json）。
//
// 清单是"这一版应该有哪些文件"的唯一权威来源，发布时的清理与回滚后的核对都以它为准。
// 所以它必须是"刚刚真的写下了哪些文件"的副产物 —— 一旦清单失真，
// 清理会删错、核对会放过"只恢复了一半"的回滚。本文件钉住这一点。
func TestVersionDirManifestIsComplete(t *testing.T) {
	ap := &fakeApplier{}
	pl, st, nodeID := newPipelineForTest(t, ap)

	// 造一个 SNI 入口 + 一条业务，好让渲染器产出一个允许清单文件
	// （没有允许清单也能通过，但那样清单只有 3 个文件，钉不住"逐版增删"这件事）。
	sniID := id.New("sni")
	if err := st.CreateSNIEntry(&model.SNIEntry{
		ID: sniID, NodeID: nodeID, BindAddr: "0.0.0.0", BindPort: 9443, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	seedSNIBusiness(t, st, nodeID, sniID)

	res, err := pl.Publish(context.Background(), nodeID, "tester", "生成清单")
	if err != nil {
		t.Fatalf("发布不该失败: %v", err)
	}
	dir := pl.VersionsDir(res.Version)

	// ① 清单文件必须存在
	raw, err := os.ReadFile(filepath.Join(dir, haproxy.ManifestFileName))
	if err != nil {
		t.Fatalf("版本目录必须有 %s: %v", haproxy.ManifestFileName, err)
	}
	var mf haproxy.VersionManifest
	if err := json.Unmarshal(raw, &mf); err != nil {
		t.Fatalf("清单必须是合法 JSON: %v", err)
	}
	if mf.Version != res.Version {
		t.Fatalf("清单里的版本号应为 %d，实际 %d", res.Version, mf.Version)
	}
	if mf.NodeID != nodeID {
		t.Fatalf("清单里的节点应为 %s，实际 %s", nodeID, mf.NodeID)
	}
	if mf.ContentHash == "" || mf.RendererVersion == "" {
		t.Fatalf("清单应记录内容哈希与渲染器版本：%+v", mf)
	}

	// ② 清单必须与实际落盘的文件集合**完全一致**（除 VERSION 与清单自身）
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk []string
	for _, e := range entries {
		if e.IsDir() || e.Name() == haproxy.ManifestFileName || e.Name() == dataplaneVersionFileName {
			continue
		}
		onDisk = append(onDisk, e.Name())
	}
	sort.Strings(onDisk)
	want := append([]string{}, mf.Files...)
	sort.Strings(want)
	if strings.Join(onDisk, ",") != strings.Join(want, ",") {
		t.Fatalf("清单与实际文件不一致：\n  清单   = %v\n  实际   = %v", want, onDisk)
	}

	// ③ 清单里每一项都必须真的存在（否则发布时会拒绝，说明清单已经失真好久了）
	for _, n := range mf.Files {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("清单列出了 %s 但文件不存在: %v", n, err)
		}
	}

	// ④ 这个用例必须在清单里看到 9443 的允许清单 —— 否则"逐版增删"根本没被覆盖到
	if !containsSub(mf.Files, "sni_allow_9443.lst") {
		t.Fatalf("带 9443 入口的这一版应有 sni_allow_9443.lst，实际清单 %v", mf.Files)
	}
	// 注：current/ 那一侧（清单被一起发布过去、陈旧文件被清掉）由 dataplane 包的
	// manifest_fileset_test.go 覆盖 —— 这里的数据面是替身，不落文件。
}

// dataplane.VersionFileName 的本地别名：publish 包不直接 import dataplane 的常量表。
const dataplaneVersionFileName = "VERSION"

// 第二个版本不再使用 9443 时，新版本的**清单与目录**里都不该有那个允许清单 ——
// 发布后 current/ 里的残留由 dataplane 那侧的清理负责（见 manifest_fileset_test.go）。
func TestVersionManifestShrinksWithRemovedEntry(t *testing.T) {
	ap := &fakeApplier{}
	pl, st, nodeID := newPipelineForTest(t, ap)

	sniID := id.New("sni")
	if err := st.CreateSNIEntry(&model.SNIEntry{
		ID: sniID, NodeID: nodeID, BindAddr: "0.0.0.0", BindPort: 9443, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	seedSNIBusiness(t, st, nodeID, sniID)
	read := func(v int) haproxy.VersionManifest {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(pl.VersionsDir(v), haproxy.ManifestFileName))
		if err != nil {
			t.Fatalf("第 %d 版没有清单: %v", v, err)
		}
		var mf haproxy.VersionManifest
		if err := json.Unmarshal(raw, &mf); err != nil {
			t.Fatal(err)
		}
		return mf
	}

	res1, err := pl.Publish(context.Background(), nodeID, "tester", "有 9443")
	if err != nil {
		t.Fatal(err)
	}
	if !containsSub(read(res1.Version).Files, "sni_allow_9443.lst") {
		t.Fatalf("第 %d 版清单应含 9443 允许清单，实际 %v", res1.Version, read(res1.Version).Files)
	}

	// 停用该入口后重新发布：新版本的清单里不应再出现它
	if _, err := st.DB().Exec(`UPDATE sni_entries SET enabled=0 WHERE id=?`, sniID); err != nil {
		t.Fatal(err)
	}
	res2, err := pl.Publish(context.Background(), nodeID, "tester", "去掉 9443")
	if err != nil {
		t.Fatal(err)
	}
	if res2.Version == res1.Version {
		t.Fatal("内容变了就应该产生新版本")
	}
	if containsSub(read(res2.Version).Files, "sni_allow_9443.lst") {
		t.Fatalf("第 %d 版不再使用 9443，清单里不该有它：%v", res2.Version, read(res2.Version).Files)
	}
}
