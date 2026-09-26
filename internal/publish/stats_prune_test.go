package publish

import (
	"context"
	"testing"

	"github.com/shengyu/edgelink/internal/dataplane"
	"github.com/shengyu/edgelink/internal/store"
)

// 本文件钉住评审要求的那条：**统计套接字清理失败只告警，不影响发布结果**。
//
// 为什么这条值得单独测：它很容易在实现里被"顺手"破坏 ——
// 例如把清理失败直接 return 成发布错误，或者把 Problems 当成 Status 的依据。
// 那等于把卫生问题升级成可用性问题：一次删不掉一个垃圾文件，
// 就让一次已经验证通过的发布显示为失败，运维会去查一个根本不存在的故障。

// pruneStub 在成功发布的替身上挂"统计套接字清理"这条可选能力。
type pruneStub struct {
	*fakeApplier
	out   dataplane.StatsSocketPrune
	calls int
}

func (s *pruneStub) PruneStaleStatsSockets(context.Context) dataplane.StatsSocketPrune {
	s.calls++
	return s.out
}

var _ dataplane.StatsSocketPruner = (*pruneStub)(nil)

// 清理成功：结果里带上清理明细，warning 与审计都要有。
func TestPublishReportsStatsSocketPrune(t *testing.T) {
	ap := &pruneStub{
		fakeApplier: &fakeApplier{},
		out: dataplane.StatsSocketPrune{
			Dir:     "/run/shengyu-edgelink",
			Scanned: 5,
			Removed: []string{"stats-v12.sock", "stats-v15.sock"},
			Kept:    []string{"stats-v18.sock（当前生效版本，保留）"},
			Note:    "扫描 5 个统计套接字：清理 2 个（已确认无进程使用），保留 3 个",
		},
	}
	pl, st, nodeID := newPipelineForTest(t, ap)

	res, err := pl.Publish(context.Background(), nodeID, "tester", "清理旧统计套接字")
	if err != nil {
		t.Fatalf("发布应成功: %v", err)
	}
	if res.Status != store.ReleaseSucceeded {
		t.Fatalf("状态应为 succeeded，实际 %s", res.Status)
	}
	if ap.calls != 1 {
		t.Fatalf("清理应当被调用恰好一次，实际 %d", ap.calls)
	}
	if res.StatsSocketPrune == nil || len(res.StatsSocketPrune.Removed) != 2 {
		t.Fatalf("结果里应带上清理明细，实际 %+v", res.StatsSocketPrune)
	}
	if !containsSub(res.Warnings, "已清理 2 个无进程使用的旧统计套接字") {
		t.Fatalf("清理了东西就该说一声，实际 %v", res.Warnings)
	}
	entries, _, aerr := st.ListAudit(store.AuditFilter{Action: "config.prune_stats_sockets"})
	if aerr != nil {
		t.Fatal(aerr)
	}
	if len(entries) != 1 {
		t.Fatalf("删 /run 下的文件属于动过系统资源，必须留痕，实际 %d 条", len(entries))
	}
	if entries[0].Result != "ok" {
		t.Fatalf("一切正常时审计结果应是 ok，实际 %q", entries[0].Result)
	}
}

// 清理失败：**发布仍然成功**，只多一条 warning，审计记为 warn。
func TestPublishSucceedsEvenWhenStatsSocketPruneFails(t *testing.T) {
	ap := &pruneStub{
		fakeApplier: &fakeApplier{},
		out: dataplane.StatsSocketPrune{
			Dir:      "/run/shengyu-edgelink",
			Scanned:  3,
			Problems: []string{"删除 stats-v7.sock 失败: permission denied"},
			Note: "扫描 3 个统计套接字：清理 0 个（已确认无进程使用），保留 3 个；" +
				"另有 1 项未能完成（不影响转发，也不影响本次发布结果）",
		},
	}
	pl, st, nodeID := newPipelineForTest(t, ap)

	res, err := pl.Publish(context.Background(), nodeID, "tester", "清理失败也要发布成功")
	if err != nil {
		t.Fatalf("清理失败**不允许**让发布失败（它是顺手打扫，不是发布的一部分），实际: %v", err)
	}
	if res.Status != store.ReleaseSucceeded {
		t.Fatalf("状态仍应是 succeeded，实际 %s", res.Status)
	}
	if !containsSub(res.Warnings, "未能完成") || !containsSub(res.Warnings, "不影响本次发布") {
		t.Fatalf("必须把「清理没做成」如实说出来并注明不影响结论，实际 %v", res.Warnings)
	}
	// 失败也不该让 manual_fix 冒出来：那不是"要运维动手"的事。
	if len(res.ManualFix) != 0 {
		t.Fatalf("清理问题不该产生人工处置命令，实际 %v", res.ManualFix)
	}
	entries, _, aerr := st.ListAudit(store.AuditFilter{Action: "config.prune_stats_sockets"})
	if aerr != nil {
		t.Fatal(aerr)
	}
	if len(entries) != 1 || entries[0].Result != "warn" {
		t.Fatalf("有未完成项时审计结果应是 warn，实际 %+v", entries)
	}
}

// 数据面不提供这条能力（纯 Go 替身）⇒ 静默跳过：不报错、不产生噪声。
func TestPublishSkipsStatsSocketPruneWhenUnsupported(t *testing.T) {
	ap := &fakeApplier{} // 不实现 StatsSocketPruner
	pl, _, nodeID := newPipelineForTest(t, ap)

	res, err := pl.Publish(context.Background(), nodeID, "tester", "数据面不支持清理")
	if err != nil {
		t.Fatalf("发布应成功: %v", err)
	}
	if res.StatsSocketPrune != nil {
		t.Fatalf("不提供该能力时不该有清理结果，实际 %+v", res.StatsSocketPrune)
	}
	for _, w := range res.Warnings {
		if containsSub([]string{w}, "统计套接字") {
			t.Fatalf("不适用的事不该出现在 warning 里（会让人以为发生过清理），实际 %q", w)
		}
	}
}
