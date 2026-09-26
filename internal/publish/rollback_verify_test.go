package publish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shengyu/edgelink/internal/dataplane"
	"github.com/shengyu/edgelink/internal/haproxy"
	"github.com/shengyu/edgelink/internal/id"
	"github.com/shengyu/edgelink/internal/model"
	"github.com/shengyu/edgelink/internal/store"
)

// 本文件盯的是 UI 上一句**会被当真**的话：「已自动回滚到第 N 版**并验证通过**」。
//
// 它翻过两次车，两次根因不同但表现一样：
//
//	第一次：rollbackIfNeeded 调 p.rollback 时传的是**空 nodeID**，
//	        于是 p.rollback 里那段 `if nodeID != "" && state.Node.ID != ""` 的验证被整体跳过，
//	        却仍然返回 (true, "已自动回滚到第 N 版并验证通过")。
//	第二次：评审进一步要求 —— **回滚文件恢复成功，不等于回滚验证成功**。
//	        只换回 VERSION 与 haproxy.cfg、允许清单与 meta 没换回来，
//	        同样会得到「看起来回滚了、其实线上是一份混合配置」。
//
// 为什么这句话危险：本仓库自己就写明 ——
// 「systemctl reload 返回成功只说明信号已送达，不代表新 worker 已就绪」。
// 于是完全可能出现：回滚的 reload 返回 0（命令成功），但回滚后的配置**根本没在跑**，
// 界面却告诉运维「已回滚并验证通过」。运维据此收工，而线上其实是一份坏配置。
//
// 现在的正确行为（本文件逐条钉住）：
//   - nodeID 为空 ⇒ 硬失败并给出 node_id_missing，**不退回「当作本机」**；
//   - 只有四项判据（文件集合 / 版本标记 / 统计套接字 / 监听归属）全部成立，
//     才允许 rolled_back=true + rollback_verified=true + note 里出现「验证通过」；
//   - 验证没执行 ⇒ 说「已恢复但未验证」；验证执行了没通过 ⇒ 说「回滚验证失败」；
//   - 以上两种都必须给出可逐行复制的人工处置命令。

// rollbackStub 精确模拟「reload 失败一次、随后成功」的时序。
//
// 关键点：Apply 会**先**替换 current/（版本标记变成新的）**再** reload。
// 所以即便 reload 失败，ActiveVersion 也已经指向新的那一版 ——
// 这正是 rollbackIfNeeded 会走去执行回滚、而不是走「无需回滚」短路分支的原因。
type rollbackStub struct {
	activeVersion int
	applyCalls    int
	verifyCalls   int
	verifyOK      bool
	// keepOldVersionOnFail=true 模拟「reload 失败且数据面自己已经把上一版放回去了」：
	// 此时 ActiveVersion 仍是旧版，rollbackIfNeeded 应走「无需回滚」分支；
	// false（默认）模拟「current/ 已经停在新版上，需要上层回滚兜底」。
	keepOldVersionOnFail bool
	// fileSetBad=true 模拟「文件只恢复了一半」（例如 VERSION 回去了、允许清单没回去）。
	fileSetBad bool
	// applyAlwaysOK=true 模拟「Apply 每次都成功」（显式回滚的场景：
	// 运维点按钮时并没有一次失败中的发布在前面）。
	applyAlwaysOK bool
	// failRollbackApply=true 模拟「回滚这一步本身下发失败」（真机场景：回滚目标版本目录
	// 被改坏，publishFiles 在读清单时就失败）。此时数据面**停在未获批准的新版本上**，
	// 平台必须把这件事说明白（见 TestRollbackFailureWarnsThatCurrentIsNotTheTarget）。
	failRollbackApply bool
	// compareErr 非空时模拟「文件集合根本核对不了」（数据面报错）。
	compareErr   error
	fileSetCalls int
	// versionDirExtra 模拟"目标版本目录里有清单之外的普通文件"（不阻断，但要上报）。
	versionDirExtra []string
}

func (s *rollbackStub) Name() string { return "rollback-stub" }

func (s *rollbackStub) Capabilities(context.Context) (model.Capabilities, error) {
	return model.Capabilities{Version: "2.8.0", Supported: true, MasterWorker: true}, nil
}
func (s *rollbackStub) Validate(context.Context, string) error { return nil }

func (s *rollbackStub) Apply(_ context.Context, req dataplane.ApplyRequest) error {
	s.applyCalls++
	if s.applyCalls == 1 && s.keepOldVersionOnFail {
		// 文件换过又放回去了：生效版本仍是旧的
		return errors.New("平滑 reload 失败: 执行 systemctl reload shengyu-edgelink-haproxy 失败: exit status 1")
	}
	if s.applyCalls > 1 && s.failRollbackApply {
		// 回滚下发失败：**不动** activeVersion —— 也就是说数据面还停在上一版发布推上去的
		// 那个未获批准的新版本上。这正是真机上 current/VERSION 被留在 15 的那种状态。
		return errors.New("haproxy: 清理 current/ 里不属于新版本的文件失败，已完整恢复旧配置")
	}
	s.activeVersion = req.Version // 先换文件、后 reload
	if s.applyCalls == 1 && !s.applyAlwaysOK {
		return errors.New("平滑 reload 失败: 执行 systemctl reload shengyu-edgelink-haproxy 失败: exit status 1")
	}
	return nil
}

// Verify 如实回报三条判据。verifyOK=false 表示「命令成功但什么都没生效」。
func (s *rollbackStub) Verify(_ context.Context, req dataplane.VerifyRequest) (dataplane.VerifyResult, error) {
	s.verifyCalls++
	sock := "/run/shengyu-edgelink/stats-v" + strconv.Itoa(req.Version) + ".sock"
	if !s.verifyOK {
		return dataplane.VerifyResult{
			ConfigHealthy:    true,
			DataplaneVersion: req.Version,
			StatsSocketPath:  sock,
			Problems: []string{"无法与版本 v" + strconv.Itoa(req.Version) + " 的统计接口通信，" +
				"因此**无法确认该版本正在运行**（模拟 reload 命令返回 0 但配置没生效）"},
		}, nil
	}
	return dataplane.VerifyResult{
		OK: true, ConfigHealthy: true, DataplaneVersion: req.Version,
		StatsSocketPath: sock,
		VersionMarkerOK: true, StatsSocketOK: true, ListenersOK: true,
	}, nil
}

// CompareCurrentToVersion 实现 dataplane.FileSetInspector：
// 生产实现（HAProxy）会真的逐字节比对，这里用可编程替身。
func (s *rollbackStub) CompareCurrentToVersion(context.Context, string) (dataplane.FileSetDiff, error) {
	s.fileSetCalls++
	if s.compareErr != nil {
		return dataplane.FileSetDiff{}, s.compareErr
	}
	if s.fileSetBad {
		return dataplane.FileSetDiff{Mismatched: []string{"haproxy.cfg"}}, nil
	}
	// versionDirExtra 模拟"目标版本目录里有清单之外的普通文件"：
	// 它不影响 diff.OK()（多余文件不参与 current/ 比对），但必须被上报 ——
	// 见 extra_files_test.go 的自动回滚用例。
	return dataplane.FileSetDiff{Same: 5, VersionDirExtra: s.versionDirExtra}, nil
}

func (s *rollbackStub) Listeners(context.Context) ([]model.Listener, error) { return nil, nil }
func (s *rollbackStub) Stats(context.Context) (dataplane.Stats, error)      { return dataplane.Stats{}, nil }
func (s *rollbackStub) ActiveVersion(context.Context) (int, error)          { return s.activeVersion, nil }
func (s *rollbackStub) Close() error                                        { return nil }

// 编译期钉住两条契约：既满足数据面接口，也提供文件集合核对能力。
var (
	_ dataplane.Applier          = (*rollbackStub)(nil)
	_ dataplane.FileSetInspector = (*rollbackStub)(nil)
)

// newRollbackPipeline 建一个「已经有一个成功发布的 v1」的流水线，并备好 v1 的版本目录。
func newRollbackPipeline(t *testing.T, ap *rollbackStub) (*Pipeline, *store.Store, string) {
	t.Helper()
	pl, st, nodeID := newPipelineWithV1(t, ap)
	ap.activeVersion = 1 // 数据面上确实在跑 v1
	return pl, st, nodeID
}

// newPipelineWithV1 是通用的脚手架：数据面这里只要求满足 Applier，
// 便于同时覆盖「提供文件集合核对」与「不提供」两类实现。
func newPipelineWithV1(t *testing.T, ap dataplane.Applier) (*Pipeline, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	nodeID := id.New("node")
	if err := st.CreateNode(&model.Node{
		ID: nodeID, Name: "回滚验收节点", Enabled: true, Health: model.NodeOnline,
		Capabilities: model.Capabilities{Version: "2.8.0", Supported: true, MasterWorker: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateConfigVersion(&store.ConfigVersion{
		ID: id.New("cfgver"), NodeID: nodeID, Version: 1, ContentHash: "v1hash", Status: store.CfgActive,
	}); err != nil {
		t.Fatal(err)
	}

	pl := New(st, ap, filepath.Join(dir, "haproxy"), haproxy.DefaultDefaults())
	pl.SetLocalNode(nodeID)
	pl.SetSleeper(func(time.Duration) {})

	// 备好 v1 的版本目录：回滚要能读到它，否则会先在 os.Stat 上失败。
	v1dir := pl.VersionsDir(1)
	if err := os.MkdirAll(v1dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v1dir, "haproxy.cfg"), []byte("global\n  # v1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	// 状态快照必须有：没有它就无法按「第 1 版当时期望的监听集合」核对 frontend 归属，
	// 验证会被判「没有执行」（见 TestRollbackRestoredButNotVerifiedWhenSnapshotMissing）。
	writeStateSnapshot(t, pl, 1, nodeID)
	return pl, st, nodeID
}

func writeStateSnapshot(t *testing.T, pl *Pipeline, version int, nodeID string) {
	t.Helper()
	b, err := jsonMarshalIndent(model.DesiredState{Node: model.Node{ID: nodeID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pl.VersionsDir(version), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pl.VersionsDir(version), StateFileName), b, 0o640); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// 缺陷一：nodeID 空值不得绕过验证
// ---------------------------------------------------------------------------

// 核心回归（评审明确要求的那一条）：rollbackIfNeeded(nodeID="") 时
//
//	① Verify 不许「被静默跳过之后还当成成功」；
//	② 结果里不许出现「并验证通过」；
//	③ 必须返回 node_id_missing（或 rollback_verification_not_run）。
func TestRollbackIfNeededRejectsEmptyNodeID(t *testing.T) {
	ap := &rollbackStub{verifyOK: true}
	pl, _, _ := newRollbackPipeline(t, ap)

	rb, skipped := pl.rollbackIfNeeded(context.Background(), "", 1)

	// ① 关于「Verify 有没有被跳过」：两种结果都允许，但不许说谎。
	if !rb.VerifyRan && ap.verifyCalls > 0 {
		t.Fatalf("Verify 被调用过（%d 次）却没有记录 VerifyRan=true —— 界面无法知道验证到底跑没跑", ap.verifyCalls)
	}
	if rb.VerifyRan && ap.verifyCalls == 0 {
		t.Fatal("声称验证执行过，但 Verify 一次都没被调用 —— 这正是「报告与事实不符」的原型")
	}
	if ap.verifyCalls == 0 {
		if rb.Code != RollbackCodeNodeIDMissing && rb.Code != RollbackCodeVerificationNotRun {
			t.Fatalf("跳过验证时必须给出明确原因码（node_id_missing 或 rollback_verification_not_run），实际 %q", rb.Code)
		}
	}

	// ② 空 nodeID 的硬性要求：必须是 node_id_missing，且不得宣称任何「完成」。
	if rb.Code != RollbackCodeNodeIDMissing {
		t.Fatalf("空 nodeID 必须返回 node_id_missing，实际 %q（note=%q）", rb.Code, rb.Note)
	}
	if rb.Verified || rb.Restored {
		t.Fatalf("空 nodeID 时什么都没做，不允许报「已恢复/已验证」：restored=%v verified=%v note=%q",
			rb.Restored, rb.Verified, rb.Note)
	}
	if skipped {
		t.Fatal("空 nodeID 不允许报「无需回滚」——那等于用一句没有依据的话让运维收工")
	}
	// ③ 措辞红线。
	if strings.Contains(rb.Note, "验证通过") {
		t.Fatalf("空 nodeID 时不可能验证过，note 不允许出现「验证通过」：%q", rb.Note)
	}
	if rb.Note == "" {
		t.Fatal("拒绝回滚时必须说明原因，否则界面只能显示一个空白错误")
	}
	// ④ 必须能给出下一步动作。
	if len(pl.manualFixCommands(1)) == 0 {
		t.Fatal("拒绝/失败的回滚必须附带可逐行复制的人工处置命令")
	}
	// ⑤ 硬性反证：整个过程中数据面一次都没被改动过。
	if ap.applyCalls != 0 {
		t.Fatalf("空 nodeID 时不允许碰数据面，实际 Apply 被调用 %d 次", ap.applyCalls)
	}
}

// 节点不匹配（另一个节点的 ID）同样必须拒绝：猜错机器会把别处正在服务的监听拆掉。
func TestRollbackRefusesForeignNodeID(t *testing.T) {
	ap := &rollbackStub{verifyOK: true}
	pl, _, _ := newRollbackPipeline(t, ap)

	rb, _ := pl.rollbackIfNeeded(context.Background(), "node_someone_else", 1)
	if rb.Code != RollbackCodeNodeIDMissing {
		t.Fatalf("非本机节点必须被拒绝（node_id_missing），实际 %q", rb.Code)
	}
	if rb.Verified {
		t.Fatal("对别的节点不允许宣称验证通过")
	}
	if ap.applyCalls != 0 || ap.verifyCalls != 0 {
		t.Fatalf("对别的节点不允许做任何动作：apply=%d verify=%d", ap.applyCalls, ap.verifyCalls)
	}
}

// ---------------------------------------------------------------------------
// 四项判据：全部成立才允许说「已回滚并验证通过」
// ---------------------------------------------------------------------------

// 反向：验证确实通过时，才允许显示「已回滚并验证通过」，且判据要逐条留在结果里。
func TestAutoRollbackClaimsVerifiedOnlyAfterRealVerification(t *testing.T) {
	ap := &rollbackStub{verifyOK: true}
	pl, _, nodeID := newRollbackPipeline(t, ap)

	res, err := pl.Publish(context.Background(), nodeID, "tester", "reload 失败一次")
	if err == nil {
		t.Fatal("Apply 失败时发布必须返回错误")
	}
	if ap.verifyCalls == 0 {
		t.Fatal("回滚路径必须真的调用 Verify")
	}
	if !res.RollbackVerifyRan {
		t.Fatal("验证确实执行了，结果里必须记为 rollback_verify_ran=true")
	}
	if !res.RollbackRestored {
		t.Fatal("文件集合核对通过，结果里应记为 rollback_restored=true")
	}
	if !res.RolledBack || !res.RollbackVerified {
		t.Fatalf("回滚且验证通过时应报 rolled_back=true / rollback_verified=true，实际 %+v", res)
	}
	if res.RollbackOutcome != RollbackOutcomeVerified {
		t.Fatalf("结论应为 %s，实际 %q", RollbackOutcomeVerified, res.RollbackOutcome)
	}
	if res.RollbackCode != "" {
		t.Fatalf("成功时不该有失败原因码，实际 %q", res.RollbackCode)
	}
	if !strings.Contains(res.RollbackNote, "验证通过") {
		t.Fatalf("回滚说明应写明已验证通过，实际 %q", res.RollbackNote)
	}
	// 判据必须逐条可核（「验证通过」这四个字背后到底是什么）。
	for _, must := range []string{"文件集合", "版本标记", "统计套接字", "监听归属", "状态来源"} {
		if !containsSub(res.RollbackChecks, must) {
			t.Fatalf("验证通过必须给出逐条判据，缺少 %q：%v", must, res.RollbackChecks)
		}
	}
	if len(res.ManualFix) != 0 {
		t.Fatalf("回滚成功时不该再要求人工处置，实际 %v", res.ManualFix)
	}
}

// 核心回归：回滚「命令成功但验证未通过」时，绝不能报「已回滚并验证通过」。
func TestAutoRollbackMustNotClaimVerifiedWhenVerificationFails(t *testing.T) {
	ap := &rollbackStub{verifyOK: false}
	pl, _, nodeID := newRollbackPipeline(t, ap)

	res, err := pl.Publish(context.Background(), nodeID, "tester", "reload 失败一次")
	if err == nil {
		t.Fatal("Apply 失败时发布必须返回错误")
	}
	if res == nil {
		t.Fatal("必须带回结果对象（界面要靠它渲染失败面板）")
	}
	if res.Phase != "reload" {
		t.Fatalf("失败阶段应为 reload，实际 %q", res.Phase)
	}
	if ap.verifyCalls == 0 {
		t.Fatal("自动回滚必须对回滚结果做验证（历史上这里传了空 nodeID，把验证整段跳过了）")
	}
	if ap.applyCalls != 2 {
		t.Fatalf("应发生两次 Apply（发布一次、回滚一次），实际 %d 次", ap.applyCalls)
	}
	if !res.RollbackVerifyRan {
		t.Fatal("Verify 被调用过，结果里必须记为 rollback_verify_ran=true")
	}
	if !res.RollbackRestored {
		t.Fatal("文件确实放回了 current/，应记为 rollback_restored=true")
	}
	if res.RolledBack || res.RollbackVerified {
		t.Fatalf("验证未通过时 rolled_back/rollback_verified 必须为 false，否则界面会显示「已自动回滚并验证通过」。"+
			"实际 rolled_back=%v rollback_verified=%v outcome=%q", res.RolledBack, res.RollbackVerified, res.RollbackOutcome)
	}
	if res.RollbackOutcome != RollbackOutcomeVerifyFailed {
		t.Fatalf("结论应为 %s（已恢复但验证没通过），实际 %q", RollbackOutcomeVerifyFailed, res.RollbackOutcome)
	}
	if res.RollbackCode != RollbackCodeVerifyFailed {
		t.Fatalf("原因码应为 %s，实际 %q", RollbackCodeVerifyFailed, res.RollbackCode)
	}
	if !strings.Contains(res.RollbackNote, "回滚验证未通过") {
		t.Fatalf("回滚说明应明确指出验证未通过，实际 %q", res.RollbackNote)
	}
	if strings.Contains(res.RollbackNote, "并验证通过") {
		t.Fatalf("回滚说明不得在验证未通过时宣称「并验证通过」，实际 %q", res.RollbackNote)
	}
	if len(res.ManualFix) == 0 {
		t.Fatal("回滚未成功时必须给出可逐行复制的人工处置命令")
	}
}

// 文件集合没恢复完整（只换回了 VERSION 之类）⇒ 连「已恢复」都不许说。
func TestAutoRollbackRefusedWhenFileSetIncomplete(t *testing.T) {
	ap := &rollbackStub{verifyOK: true, fileSetBad: true}
	pl, _, nodeID := newRollbackPipeline(t, ap)

	res, err := pl.Publish(context.Background(), nodeID, "tester", "reload 失败且文件没换全")
	if err == nil {
		t.Fatal("Apply 失败时发布必须返回错误")
	}
	if ap.fileSetCalls == 0 {
		t.Fatal("必须做文件集合核对：只恢复了 VERSION 而正文没换，是最危险的「看起来回滚成功」")
	}
	if res.RollbackRestored {
		t.Fatalf("文件集合不一致时不允许报「已恢复」，实际 %+v", res)
	}
	if res.RolledBack || res.RollbackVerified {
		t.Fatalf("文件都没恢复完整，不允许报回滚成功：%+v", res)
	}
	if res.RollbackOutcome != RollbackOutcomeFailed {
		t.Fatalf("结论应为 %s，实际 %q", RollbackOutcomeFailed, res.RollbackOutcome)
	}
	if res.RollbackCode != RollbackCodeRestoreFailed {
		t.Fatalf("原因码应为 %s，实际 %q", RollbackCodeRestoreFailed, res.RollbackCode)
	}
	if strings.Contains(res.RollbackNote, "并验证通过") {
		t.Fatalf("不得宣称验证通过，实际 %q", res.RollbackNote)
	}
	if len(res.ManualFix) == 0 {
		t.Fatal("必须给出人工处置命令")
	}
}

// 文件集合核对不了（数据面报错）时是「验证没有执行」，不是「通过」。
func TestAutoRollbackRefusedWhenFileSetUnreadable(t *testing.T) {
	ap := &rollbackStub{verifyOK: true, compareErr: errors.New("current 目录不可读")}
	pl, _, nodeID := newRollbackPipeline(t, ap)

	res, err := pl.Publish(context.Background(), nodeID, "tester", "文件集合核对不了")
	if err == nil {
		t.Fatal("Apply 失败时发布必须返回错误")
	}
	if res.RollbackVerified || res.RolledBack {
		t.Fatalf("文件集合核对不了时不允许报回滚成功：%+v", res)
	}
	if res.RollbackCode != RollbackCodeVerificationNotRun {
		t.Fatalf("原因码应为 %s，实际 %q", RollbackCodeVerificationNotRun, res.RollbackCode)
	}
	if ap.verifyCalls != 0 {
		t.Fatalf("文件集合都没核对上，不该再去跑 frontend 归属验证（顺序是 fail-closed），实际跑了 %d 次", ap.verifyCalls)
	}
	if len(res.ManualFix) == 0 {
		t.Fatal("必须给出人工处置命令")
	}
}

// 「已恢复但未验证」：文件回来了、但验证没执行（这里制造「没有状态快照」）。
// 这是评审明确要求界面必须能表达的中间状态。
func TestRollbackRestoredButNotVerifiedWhenSnapshotMissing(t *testing.T) {
	ap := &rollbackStub{verifyOK: true}
	pl, _, nodeID := newRollbackPipeline(t, ap)
	// 抽掉状态快照：此时「验证」所需的「该版期望的监听集合」无从获得。
	if err := os.Remove(filepath.Join(pl.VersionsDir(1), StateFileName)); err != nil {
		t.Fatal(err)
	}

	res, err := pl.Publish(context.Background(), nodeID, "tester", "回滚后无法验证")
	if err == nil {
		t.Fatal("Apply 失败时发布必须返回错误")
	}
	if ap.verifyCalls != 0 {
		t.Fatalf("没有状态快照时不该硬凑一次验证，实际调用了 %d 次", ap.verifyCalls)
	}
	if res.RollbackVerifyRan {
		t.Fatal("验证没执行就必须记 rollback_verify_ran=false，界面靠它区分「未验证」与「验证失败」")
	}
	if !res.RollbackRestored {
		t.Fatal("文件已经放回 current/ 了，rollback_restored 应为 true（这正是「已恢复但未验证」）")
	}
	if res.RolledBack || res.RollbackVerified {
		t.Fatalf("验证没执行时不允许报回滚成功：%+v", res)
	}
	if res.RollbackOutcome != RollbackOutcomeNotVerified {
		t.Fatalf("结论应为 %s，实际 %q", RollbackOutcomeNotVerified, res.RollbackOutcome)
	}
	if res.RollbackCode != RollbackCodeVerificationNotRun {
		t.Fatalf("原因码应为 %s，实际 %q", RollbackCodeVerificationNotRun, res.RollbackCode)
	}
	if !strings.Contains(res.RollbackNote, "回滚验证没有执行") {
		t.Fatalf("说明里必须写清「验证没有执行」，实际 %q", res.RollbackNote)
	}
	if strings.Contains(res.RollbackNote, "并验证通过") {
		t.Fatalf("不得宣称验证通过，实际 %q", res.RollbackNote)
	}
	if len(res.ManualFix) == 0 {
		t.Fatal("必须给出人工处置命令")
	}
	// 人工命令里必须有「先看清现场」的三条（服务状态 / current 文件与 VERSION / 统计套接字）。
	joined := strings.Join(res.ManualFix, "\n")
	for _, must := range []string{"systemctl status", "VERSION", "stats-v"} {
		if !strings.Contains(joined, must) {
			t.Fatalf("人工命令应包含 %q 以便先看清现场：\n%s", must, joined)
		}
	}
}

// 第三种状态：数据面自己已经把上一版放回去了（ActiveVersion 仍是旧版）。
// 这时的正确说法是「改动没有生效，无需回滚」——**而且这句话同样要验证**，
// 不能只凭磁盘上的版本标记。验证通过才算安全，且不能报成「已回滚」
// （否则运维会去找一个根本没发生过的切换）。
func TestNoRollbackNeededMustBeVerifiedNotAssumed(t *testing.T) {
	ap := &rollbackStub{verifyOK: true, keepOldVersionOnFail: true}
	pl, _, nodeID := newRollbackPipeline(t, ap)

	res, err := pl.Publish(context.Background(), nodeID, "tester", "reload 失败且已自动放回")
	if err == nil {
		t.Fatal("Apply 失败时发布必须返回错误")
	}
	if ap.verifyCalls == 0 {
		t.Fatal("「改动未生效、旧版仍在跑」这句结论也必须来自真实验证，不能只看磁盘版本标记")
	}
	if res.RolledBack {
		t.Fatalf("没有发生回滚时 rolled_back 不能为 true，否则界面会说「已自动回滚并验证通过」。"+
			"实际 note=%q", res.RollbackNote)
	}
	if !res.NoRollbackNeeded {
		t.Fatalf("已验证原版本仍在运行时应报 no_rollback_needed=true，实际 %+v", res)
	}
	if !res.RollbackVerified || !res.RollbackVerifyRan {
		t.Fatalf("「无需回滚」同样是验证结论：rollback_verified=%v rollback_verify_ran=%v",
			res.RollbackVerified, res.RollbackVerifyRan)
	}
	if res.RollbackOutcome != RollbackOutcomeNotNeeded {
		t.Fatalf("结论应为 %s，实际 %q", RollbackOutcomeNotNeeded, res.RollbackOutcome)
	}
	if len(res.ManualFix) != 0 {
		t.Fatalf("确认安全时不该要求人工处置，实际 %v", res.ManualFix)
	}
	if ap.applyCalls != 1 {
		t.Fatalf("这种情况不该再补一次回滚，实际 Apply %d 次", ap.applyCalls)
	}
}

// 反向：磁盘上写着旧版，但确认不了它真的在跑 ⇒ 不能报「无需回滚」，必须给人工命令。
func TestNoRollbackNeededRefusedWhenCannotConfirmRunning(t *testing.T) {
	ap := &rollbackStub{verifyOK: false, keepOldVersionOnFail: true}
	pl, _, nodeID := newRollbackPipeline(t, ap)

	res, err := pl.Publish(context.Background(), nodeID, "tester", "reload 失败且无法确认")
	if err == nil {
		t.Fatal("Apply 失败时发布必须返回错误")
	}
	if res.RolledBack || res.NoRollbackNeeded {
		t.Fatalf("无法确认运行状态时不允许报「安全」：rolled_back=%v no_rollback_needed=%v",
			res.RolledBack, res.NoRollbackNeeded)
	}
	if res.RollbackVerified {
		t.Fatal("无法确认时不允许报 rollback_verified=true")
	}
	if len(res.ManualFix) == 0 {
		t.Fatal("无法确认运行状态时必须给出人工处置命令")
	}
	if !strings.Contains(res.RollbackNote, "无法确认") {
		t.Fatalf("说明里应写清「无法确认」，实际 %q", res.RollbackNote)
	}
}

// 数据面不支持文件集合核对（不是以文件为准）时，这条判据算「不适用」而不是「没通过」——
// 但必须留痕，让人知道这次的「验证通过」背后少了哪一条。
func TestFileSetCheckRecordedAsNotApplicableWhenUnsupported(t *testing.T) {
	ap := &fakeApplier{}
	pl, _, nodeID := newPipelineWithV1(t, ap)

	rb := pl.inspectRollbackTarget(context.Background(), nodeID, 1, true)
	if !rb.Verified {
		t.Fatalf("不以文件为准的数据面应能通过其余三条判据：%+v", rb)
	}
	if !containsSub(rb.Checks, "不适用") {
		t.Fatalf("必须留痕说明「文件集合判据不适用」，实际 %v", rb.Checks)
	}
}

// 显式回滚（运维点按钮）同样要记录"验证执行过"，界面上的「已回滚」才有依据。
func TestExplicitRollbackRecordsVerificationEvidence(t *testing.T) {
	ap := &rollbackStub{verifyOK: true, applyAlwaysOK: true}
	pl, _, nodeID := newRollbackPipeline(t, ap)
	ap.activeVersion = 2
	if err := pl.Store.CreateConfigVersion(&store.ConfigVersion{
		ID: id.New("cfgver"), NodeID: nodeID, Version: 2, ContentHash: "v2hash", Status: store.CfgActive,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := pl.Rollback(context.Background(), nodeID, 1, "tester", "上一版更稳定")
	if err != nil {
		t.Fatalf("回滚不该失败: %v", err)
	}
	if !res.RollbackVerified || !res.RollbackVerifyRan {
		t.Fatalf("显式回滚也必须有验证证据：%+v", res)
	}
	if res.RollbackOutcome != RollbackOutcomeVerified {
		t.Fatalf("结论应为 %s，实际 %q", RollbackOutcomeVerified, res.RollbackOutcome)
	}
}

// 真机暴露的第二种"看起来只是回滚失败、其实更危险"的状态：
// 回滚这一步自己下发失败，于**current/ 停在未获批准的新版本上**。
//
// 真机实测（docs/08 第 6 节 4B）：回滚目标 v12 的目录被改坏 ⇒ 平台 fail-closed 拒绝回滚
// （这是对的），但 `current/VERSION` 留在了 15 —— 而正在服务的是 v12 的 worker。
// 此时任何一次 `systemctl reload` 都会把 v15 那份没验证过的配置加载起来。
//
// 所以说明里必须同时出现三件事：current/ 现在是第几版、它不是哪一版、先别 reload。
func TestRollbackFailureWarnsThatCurrentIsNotTheTarget(t *testing.T) {
	ap := &rollbackStub{verifyOK: true, failRollbackApply: true}
	pl, _, nodeID := newRollbackPipeline(t, ap)

	res, err := pl.Publish(context.Background(), nodeID, "tester", "回滚下发失败")
	if err == nil {
		t.Fatal("Apply 失败时发布必须返回错误")
	}
	if ap.activeVersion != 2 {
		t.Fatalf("本用例要构造「数据面停在新版本上」，实际 activeVersion=%d", ap.activeVersion)
	}
	if res.RollbackOutcome != RollbackOutcomeFailed {
		t.Fatalf("结论应为 %s，实际 %q", RollbackOutcomeFailed, res.RollbackOutcome)
	}
	if !strings.Contains(res.RollbackNote, "警告：current/ 目前停留在第 2 版") {
		t.Fatalf("回滚失败时必须写明 current/ 实际停在哪一版，实际 %q", res.RollbackNote)
	}
	if !strings.Contains(res.RollbackNote, "不要执行 systemctl reload") {
		t.Fatalf("必须明确警告先别 reload（否则会加载未通过验证的配置），实际 %q", res.RollbackNote)
	}
	if res.RolledBack || res.RollbackVerified {
		t.Fatalf("回滚没成功不允许报成功：%+v", res)
	}
	joined := strings.Join(res.ManualFix, "\n")
	if !strings.Contains(joined, "不要执行 systemctl reload") {
		t.Fatalf("人工命令的第一条就必须是「先别 reload」：\n%s", joined)
	}
}

func containsSub(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
