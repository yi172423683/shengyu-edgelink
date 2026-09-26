package publish

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shengyu/edgelink/internal/dataplane"
	"github.com/shengyu/edgelink/internal/haproxy"
	"github.com/shengyu/edgelink/internal/store"
)

// 本文件钉住"版本目录里有清单之外的文件"这条边界（评审确定的四条要求，见 docs/08 §10）：
//
//	① 多余普通文件               ⇒ **不阻断**发布/回滚；warning + 审计 + 页面提示；
//	② 清单列出的文件缺失         ⇒ **拒绝**（且不得进入 verified）；
//	③ 配置**实际引用**了清单外文件 ⇒ **拒绝**（那种文件运行时会按绝对路径被读到）；
//	④ current/ 里最终只能有清单允许的文件。
//
// ②④ 的实现在数据面层（internal/dataplane/manifest_fileset_test.go），
// 这里覆盖 ①③ 在**发布流水线**这一层的行为 —— 也就是界面与审计真正读到的那些字段。

// writeManifestFor 给某个版本目录补一份清单（模拟"新格式发布产物"）。
func writeManifestFor(t *testing.T, dir string, version int, files []string) {
	t.Helper()
	mf := haproxy.VersionManifest{
		Version: version, NodeID: "node_test", ContentHash: "hash",
		RendererVersion: haproxy.RendererVersion, Files: files,
	}
	b, err := jsonMarshalIndent(mf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, haproxy.ManifestFileName), b, 0o640); err != nil {
		t.Fatal(err)
	}
}

// ① 发布路径：目录里多出一个"运维手工留的备份"⇒ 发布照常成功，但必须记 warning + 审计。
//
// 为什么不是失败（评审决定）：这类文件不会被写进 current/（写入以清单为准），
// 对正确性没有影响；而当硬错误会**在事故中挡住回滚** —— 回滚是最不能拖延的时候。
func TestPublishWarnsAboutFilesOutsideManifest(t *testing.T) {
	ap := &fakeApplier{}
	pl, st, nodeID := newPipelineForTest(t, ap)
	ctx := context.Background()

	// 预置"第 1 版目录里的多余文件"：writeVersionDir 只做 MkdirAll + 写文件，不清空目录，
	// 所以预先放在那里的文件会留存下来，但不会被写进清单 —— 正是要测的场景。
	v1dir := pl.VersionsDir(1)
	if err := os.MkdirAll(v1dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v1dir, "haproxy.cfg.orig"), []byte("global\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	res, err := pl.Publish(ctx, nodeID, "tester", "目录里留了个备份")
	if err != nil {
		t.Fatalf("清单外的多余文件不允许阻断发布（会在事故中挡住回滚），实际: %v", err)
	}
	if res.Status != store.ReleaseSucceeded {
		t.Fatalf("发布应当成功，实际 status=%s message=%s", res.Status, res.Message)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("必须把「存在未纳入清单的额外文件」说进结果里（页面与审计都靠它）")
	}
	if !containsSub(res.Warnings, "haproxy.cfg.orig") {
		t.Fatalf("warning 要点名是哪个文件（否则运维无从查起）：%v", res.Warnings)
	}
	if !containsSub(res.Warnings, "未纳入清单") {
		t.Fatalf("warning 要说清性质：%v", res.Warnings)
	}
	// 审计必须留痕：页面刷新后 warning 就没了，而"某个目录被手工改过"这件事
	// 恰恰是几天后排查"为什么某次回滚不对劲"时最需要的前置事实。
	entries, _, aerr := st.ListAudit(store.AuditFilter{Action: "config.version_dir_extra_files"})
	if aerr != nil {
		t.Fatal(aerr)
	}
	if len(entries) == 0 {
		t.Fatal("warning 必须写进审计（页面会刷新，审计不会）")
	}
	if !strings.Contains(entries[0].Summary, "haproxy.cfg.orig") {
		t.Fatalf("审计里要能看出是哪个文件：%q", entries[0].Summary)
	}
}

// ① 回滚路径：目标版本目录里有多余文件 ⇒ 回滚照常完成，但必须记 warning + 审计。
func TestExplicitRollbackWarnsAboutFilesOutsideManifest(t *testing.T) {
	ap := &rollbackStub{verifyOK: true, applyAlwaysOK: true}
	pl, st, nodeID := newRollbackPipeline(t, ap)

	v1dir := pl.VersionsDir(1)
	writeManifestFor(t, v1dir, 1, []string{"haproxy.cfg", StateFileName})
	if err := os.WriteFile(filepath.Join(v1dir, "haproxy.cfg.bak"), []byte("global\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	res, err := pl.Rollback(context.Background(), nodeID, 1, "tester", "验证 warning")
	if err != nil {
		t.Fatalf("清单外的多余文件不允许阻断回滚（回滚是最不能拖的时候），实际: %v", err)
	}
	if !containsSub(res.Warnings, "haproxy.cfg.bak") {
		t.Fatalf("回滚也要把多余文件报出来：warnings=%v", res.Warnings)
	}
	entries, _, aerr := st.ListAudit(store.AuditFilter{Action: "config.version_dir_extra_files"})
	if aerr != nil {
		t.Fatal(aerr)
	}
	if len(entries) == 0 {
		t.Fatal("回滚路径的 warning 同样必须写进审计")
	}
}

// ① 自动回滚路径（发布失败后数据面自己退回旧版）也要把多余文件报出来。
//
// 这条路径用的是 dataplane 的文件集合核对结果（CompareCurrentToVersion），
// 而不是重新扫目录 —— 两者必须说同一句话，否则界面在不同路径下会自相矛盾。
func TestAutoRollbackReportsVersionDirExtraFiles(t *testing.T) {
	ap := &rollbackStub{
		keepOldVersionOnFail: true, // 第一次 Apply 失败，且数据面仍停在旧版
		verifyOK:             true, // 旧版确实在跑（否则结论会落在"无法确认"，测不到 not_needed）
		versionDirExtra:      []string{"haproxy.cfg.orig"},
	}
	pl, _, nodeID := newRollbackPipeline(t, ap)

	res, err := pl.Publish(context.Background(), nodeID, "tester", "触发一次自动回滚")
	if err == nil {
		t.Fatal("本用例构造的就是一次失败发布")
	}
	if !containsSub(res.Warnings, "haproxy.cfg.orig") {
		t.Fatalf("自动回滚路径也要报出多余文件：warnings=%v", res.Warnings)
	}
	if !containsSub(res.RollbackChecks, "额外文件") {
		t.Fatalf("回滚判据里要留下「额外文件」这一条（运维据此知道目录被改过）：%v", res.RollbackChecks)
	}
	// 关键：多余文件**不能**影响回滚结论本身。
	if !res.RollbackVerified || res.RollbackOutcome != RollbackOutcomeNotNeeded {
		t.Fatalf("多余文件不允许改变回滚结论：verified=%v outcome=%s",
			res.RollbackVerified, res.RollbackOutcome)
	}
}

// ③ 回滚目标目录被手工改过：cfg 引用了清单外的文件 ⇒ 回滚必须被拒绝。
//
// 为什么这条不能像"多余文件"那样放过（评审第 3 条）：
// 渲染产物里的引用是**版本目录的绝对路径**，所以那个文件在运行时真的会被 HAProxy 读到 ——
// "未纳入清单的配置会影响运行"，清单也就不再是"这一版由哪些文件组成"的权威说明。
//
// 这里用**真实的数据面实现**（dataplane.HAProxy）而不是替身：
// 这条判据的关键是"引用路径与版本目录的对应关系"，替身会把这件事测成假的。
func TestRollbackRefusesWhenTargetConfigCitesFileOutsideManifest(t *testing.T) {
	ap := dataplane.NewHAProxy()
	// 只注入命令执行：本用例要测的判据发生在写文件之前，跑不到 reload/verify。
	ap.SetRunner(func(context.Context, string, ...string) (string, string, error) {
		return "Configuration file is valid\n", "", nil
	})
	pl, _, nodeID := newPipelineWithV1(t, ap)
	ap.ConfigRoot = pl.VersionsRoot
	ap.CurrentDir = pl.currentDir()

	v1dir := pl.VersionsDir(1)
	outside := filepath.ToSlash(filepath.Join(v1dir, "extra_allow.lst"))
	cfg := "global\nfrontend fe\n" +
		"  tcp-request content reject unless { var(txn.sni) -m str -i -f " + outside + " }\n"
	if err := os.WriteFile(filepath.Join(v1dir, "haproxy.cfg"), []byte(cfg), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v1dir, "extra_allow.lst"), []byte("a.example.com\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	// 清单只列配置正文与状态快照 —— extra_allow.lst 不在其中。
	writeManifestFor(t, v1dir, 1, []string{"haproxy.cfg", StateFileName})

	_, err := pl.Rollback(context.Background(), nodeID, 1, "tester", "回滚到被改坏的版本")
	if err == nil {
		t.Fatal("配置引用了清单外文件时必须拒绝回滚（该文件运行时会按绝对路径被读到）")
	}
	if !strings.Contains(err.Error(), "extra_allow.lst") {
		t.Fatalf("报错要点名被引用的文件：%v", err)
	}
	if !strings.Contains(err.Error(), "不在清单") {
		t.Fatalf("报错要说清「不在清单内」这个原因：%v", err)
	}
}
