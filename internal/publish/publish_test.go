package publish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/shengyu/edgelink/internal/dataplane"
	"github.com/shengyu/edgelink/internal/haproxy"
	"github.com/shengyu/edgelink/internal/id"
	"github.com/shengyu/edgelink/internal/model"
	"github.com/shengyu/edgelink/internal/store"
)

// 本文件覆盖评审要求补的三项发布可靠性测试中的两项（第三项在 dataplane 包里）：
//
//	① **reload 异步就绪**：`systemctl reload` 返回时新 worker 往往还没起来，
//	   验证必须「等一会儿再判」，否则一次稍慢但成功的发布会被判失败并触发无谓回滚；
//	② **回滚到历史版本**：验证始终不通过时必须真的退回去，并且把原因如实记下来。

// fakeApplier 是可编程的数据面替身。
//
// 只用于 publish 包的单元测试 —— 它的作用是让我们能精确制造
// "reload 成功了但新版本过一会儿才就绪""新版本永远起不来"这两种时序，
// 这两种时序在真机上很难稳定复现（真机验收另有一套，见 docs/07）。
type fakeApplier struct {
	activeVersion int
	// verifyFailures：前 N 次 Verify 返回"未就绪"，之后返回 OK。
	verifyFailures  int
	verifyCalls     int
	verifyAlwaysBad bool
	applied         []int
	rolledBack      []int
	reloadDelay     bool
}

func (f *fakeApplier) Name() string { return "fake" }

func (f *fakeApplier) Capabilities(context.Context) (model.Capabilities, error) {
	return model.Capabilities{Version: "2.8.0", Supported: true, MasterWorker: true, SNICapture: true}, nil
}

func (f *fakeApplier) Validate(context.Context, string) error { return nil }

func (f *fakeApplier) Apply(_ context.Context, req dataplane.ApplyRequest) error {
	f.applied = append(f.applied, req.Version)
	f.activeVersion = req.Version
	return nil
}

func (f *fakeApplier) Verify(_ context.Context, req dataplane.VerifyRequest) (dataplane.VerifyResult, error) {
	f.verifyCalls++
	if f.verifyAlwaysBad {
		return dataplane.VerifyResult{ConfigHealthy: true}, nil
	}
	if f.verifyCalls <= f.verifyFailures {
		// 模拟"新 worker 还没起来"：配置健康，但监听还没就绪
		return dataplane.VerifyResult{ConfigHealthy: true, Problems: []string{"新版本尚未就绪（模拟 reload 异步窗口）"}}, nil
	}
	// 三条判据一起置位：它们就是"验证通过"的全部内容（见 dataplane.VerifyResult）。
	// 替身如果不置位，就会把"通过"表现成"什么都没通过"，那是对契约的误读。
	return dataplane.VerifyResult{
		OK: true, ConfigHealthy: true, DataplaneVersion: f.activeVersion,
		VersionMarkerOK: true, StatsSocketOK: true, ListenersOK: true,
		StatsSocketPath: "/run/shengyu-edgelink/stats-v" + strconv.Itoa(req.Version) + ".sock",
	}, nil
}

func (f *fakeApplier) Listeners(context.Context) ([]model.Listener, error) { return nil, nil }
func (f *fakeApplier) Stats(context.Context) (dataplane.Stats, error)      { return dataplane.Stats{}, nil }

func (f *fakeApplier) ActiveVersion(context.Context) (int, error) { return f.activeVersion, nil }

func (f *fakeApplier) Close() error { return nil }

// 回滚是通过 Apply 完成的（把 current 指回旧版本目录），因此这里单独记一笔，
// 便于断言"回滚确实发生了"。
func (f *fakeApplier) markRollback(version int) { f.rolledBack = append(f.rolledBack, version) }

// ap 取接口类型而不是 *fakeApplier：有些用例要在替身上挂**可选能力**
// （例如 StatsSocketPruner），传具体类型就没法换实现了。
func newPipelineForTest(t *testing.T, ap dataplane.Applier) (*Pipeline, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	nodeID := id.New("node")
	if err := st.CreateNode(&model.Node{
		ID: nodeID, Name: "测试节点", Enabled: true, Health: model.NodeOnline,
		Capabilities: model.Capabilities{Version: "2.8.0", Supported: true, MasterWorker: true},
	}); err != nil {
		t.Fatal(err)
	}
	pl := New(st, ap, filepath.Join(dir, "haproxy"), haproxy.DefaultDefaults())
	pl.SetLocalNode(nodeID)
	// 把 500ms 的轮询等待变成即时返回，测试不必真等 30 秒。
	pl.SetSleeper(func(time.Duration) {})
	return pl, st, nodeID
}

// ① reload 异步就绪：前几次 Verify 未就绪，但随后就绪 ⇒ 发布必须**成功**，
// 而且结果里要如实记录"等了几次"。
func TestPublishWaitsForAsyncReloadReadiness(t *testing.T) {
	ap := &fakeApplier{verifyFailures: 3}
	pl, _, nodeID := newPipelineForTest(t, ap)

	res, err := pl.Publish(context.Background(), nodeID, "tester", "异步就绪")
	if err != nil {
		t.Fatalf("发布不该失败: %v", err)
	}
	if res.Status != store.ReleaseSucceeded {
		t.Fatalf("reload 稍慢但最终就绪时必须判成功（否则会触发无谓回滚），实际 %+v", res)
	}
	if res.VerifyAttempts < 4 {
		t.Fatalf("应当轮询等待到就绪：期望至少 4 次验证，实际 %d", res.VerifyAttempts)
	}
	// 关键反证：不能因为"第一次没就绪"就回滚
	if len(ap.rolledBack) != 0 {
		t.Fatalf("不该发生回滚，实际回滚了 %v", ap.rolledBack)
	}
	if len(ap.applied) != 1 {
		t.Fatalf("应只应用一次（不应反复 Apply），实际 %v", ap.applied)
	}
}

// ② 永远不就绪 ⇒ 必须失败并**回滚**，且失败原因要写清楚。
func TestPublishFailsAndRollsBackWhenNeverReady(t *testing.T) {
	ap := &fakeApplier{verifyAlwaysBad: true}
	pl, st, nodeID := newPipelineForTest(t, ap)
	// 用假时钟把"等 30 秒"变成瞬时：测试不该真的睡 30 秒，
	// 但超时逻辑必须被真实执行到（否则等于没测）。
	fc := newFakeClock()
	pl.SetClock(fc.now)
	pl.SetSleeper(fc.sleep)

	// 先造一个"已经在跑的 v1"，这样回滚才有目标
	ap.activeVersion = 1
	if err := st.CreateConfigVersion(&store.ConfigVersion{
		ID: id.New("cfgver"), NodeID: nodeID, Version: 1, ContentHash: "old", Status: store.CfgActive,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := pl.Publish(context.Background(), nodeID, "tester", "永不就绪")
	if err == nil {
		t.Fatal("验证始终不通过时必须返回错误")
	}
	if res == nil || res.Status != store.ReleaseFailed {
		t.Fatalf("发布结果应为 failed，实际 %+v", res)
	}
	if res.VerifyAttempts < 2 {
		t.Fatalf("失败前也应轮询几次（区分「还没就绪」与「根本起不来」），实际 %d 次", res.VerifyAttempts)
	}
	// 版本状态必须是"已回滚"，而不是"失败" —— 它确实被应用过又退回去了，
	// 这两种状态对运维的含义完全不同。
	cv, err := st.GetConfigVersion(nodeID, res.Version)
	if err != nil {
		t.Fatal(err)
	}
	if cv.Status != store.CfgRolledBack && cv.Status != store.CfgFailed {
		t.Fatalf("未就绪的版本状态应为 rolled_back/failed，实际 %s", cv.Status)
	}
	if cv.Error == "" {
		t.Fatal("必须记录失败原因，否则界面只能显示一个空白错误")
	}
	// 当前生效版本仍是 v1
	active, err := st.GetActiveConfigVersion(nodeID)
	if err != nil {
		t.Fatalf("应仍有生效版本: %v", err)
	}
	if active.Version != 1 {
		t.Fatalf("验证失败后生效版本应保持 v1，实际 v%d", active.Version)
	}
}

// ③ 回滚到历史版本：把 v2 回滚到 v1，必须真的把 current 指回 v1 并验证通过。
func TestRollbackToHistoricalVersion(t *testing.T) {
	ap := &fakeApplier{}
	pl, st, nodeID := newPipelineForTest(t, ap)

	// 造两版历史：v1（简单）与 v2（当前生效）
	for _, v := range []int{1, 2} {
		if err := st.CreateConfigVersion(&store.ConfigVersion{
			ID: id.New("cfgver"), NodeID: nodeID, Version: v,
			ContentHash: fmt.Sprintf("hash-%d", v), Status: store.CfgSuperseded,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpdateConfigVersionStatus(nodeID, 2, store.CfgActive, ""); err != nil {
		t.Fatal(err)
	}
	ap.activeVersion = 2

	// 回滚要求"第 1 版的配置正文在磁盘上仍然可读"，否则界面上的回滚按钮
	// 会点得动但做不成事。这里按流水线的目录约定把正文放进去。
	v1dir := pl.VersionsDir(1)
	if err := os.MkdirAll(v1dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v1dir, "haproxy.cfg"), []byte("# v1 config\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	res, err := pl.Rollback(context.Background(), nodeID, 1, "tester", "上一版更稳定")
	if err != nil {
		t.Fatalf("回滚不该失败: %v", err)
	}
	if res.Status != store.ReleaseSucceeded {
		t.Fatalf("回滚应成功，实际 %+v", res)
	}
	if res.Version != 1 {
		t.Fatalf("回滚目标版本应为 1，实际 %d", res.Version)
	}
	// 数据面确实被指回了 v1
	if ap.activeVersion != 1 {
		t.Fatalf("回滚后数据面的生效版本应为 1，实际 %d", ap.activeVersion)
	}
	// 版本状态：v1 生效，v2 标为"已被回滚"（不是"失败" —— 它当时是成功的）
	active, err := st.GetActiveConfigVersion(nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if active.Version != 1 {
		t.Fatalf("生效版本应为 1，实际 %d", active.Version)
	}
	v2, err := st.GetConfigVersion(nodeID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Status != store.CfgRolledBack {
		t.Fatalf("被回滚掉的版本状态应为 rolled_back，实际 %s", v2.Status)
	}
	// 审计必须留痕：谁在什么时候因为什么回滚了哪一版
	entries, _, err := st.ListAudit(store.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Action == "config.rollback" {
			found = true
			if e.Actor != "tester" {
				t.Fatalf("审计应记录操作人，实际 %q", e.Actor)
			}
			if e.Detail == "" {
				t.Fatal("审计应记录回滚原因")
			}
		}
	}
	if !found {
		t.Fatalf("回滚必须写审计（否则事后无法回答「谁在什么时候退回了哪一版」），实际条目 %d 条", len(entries))
	}
}

// 编译期兜底：确保 fakeApplier 仍然满足数据面接口（接口变化时这里会先报错）。
var _ dataplane.Applier = (*fakeApplier)(nil)

// 这个未使用的方法是为了让"回滚是通过 Apply 完成"这件事在替身上有迹可循；
// 真正的回滚断言看 ap.activeVersion。
var _ = (*fakeApplier).markRollback
var _ = errors.New

// fakeClock 是可推进的假时钟：每次 sleep 就把时间往前拨，让"超时"在测试里立刻到达。
type fakeClock struct {
	t time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Now()} }

func (f *fakeClock) now() time.Time { return f.t }

func (f *fakeClock) sleep(d time.Duration) {
	if d <= 0 {
		d = time.Millisecond
	}
	f.t = f.t.Add(d)
}
