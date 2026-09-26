package dataplane

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件钉住"清理旧统计套接字"的边界。
//
// 这个动作唯一的风险是**删错**：删掉一个还有进程在用的套接字，
// 不会影响转发（转发不依赖统计接口），但会让排障时看到的现场与事实不符 ——
// 而排障恰恰发生在这种细节最要紧的时候。所以判据按保守程度排序，
// 前一条否决后一条：当前版本 ⇒ 有人在应答 ⇒ 判不出来 ⇒ 才轮到"删"。

// fakeDirEntry 只实现 os.DirEntry 的最小面，用来伪造目录条目。
type fakeDirEntry struct {
	name  string
	isDir bool
}

func (e fakeDirEntry) Name() string               { return e.name }
func (e fakeDirEntry) IsDir() bool                { return e.isDir }
func (e fakeDirEntry) Type() os.FileMode          { return 0 }
func (e fakeDirEntry) Info() (os.FileInfo, error) { return nil, os.ErrInvalid }

// 当前版本的、有人在应答的、判不出来的 —— 一个都不能删；只有"明确无人使用"才删。
func TestPruneStatsSocketsKeepsAliveCurrentAndUnknown(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		"stats-v1.sock", "stats-v2.sock", "stats-v3.sock", "stats-v9.sock", "not-a-socket.txt",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h := NewHAProxy()
	h.StatsSocketDir = dir
	h.CurrentDir = filepath.Join(t.TempDir(), "current")
	if err := os.MkdirAll(h.CurrentDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// 当前生效版本 = 2：它的套接字**一律保留** —— reload 窗口里它可能暂时连不上，
	// 删它没有收益（下一轮还会重建），却会让 Stats() 的兜底候选少一个。
	if err := os.WriteFile(filepath.Join(h.CurrentDir, VersionFileName), []byte("2\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	h.SetSocketProbe(func(_ context.Context, path string) (bool, bool) {
		switch filepath.Base(path) {
		case "stats-v1.sock":
			return true, true // 有 worker 在应答（典型场景：正在优雅退出的旧 worker）
		case "stats-v9.sock":
			return false, false // 判不出来（例如权限不足）
		}
		return false, true // 明确无人监听
	})

	out := h.PruneStaleStatsSockets(context.Background())
	if out.Scanned != 4 {
		t.Fatalf("只应扫描 stats-v*.sock（不含 .txt），实际 %d", out.Scanned)
	}
	if len(out.Removed) != 1 || out.Removed[0] != "stats-v3.sock" {
		t.Fatalf("只应删掉 v3（明确无人使用），实际 %v", out.Removed)
	}
	// 保留项必须逐条给出原因 —— 否则运维看到"没删"也不知道是为什么，只能自己猜。
	joined := strings.Join(out.Kept, " | ")
	for _, must := range []string{
		"stats-v1.sock", "有 worker 在应答",
		"stats-v2.sock", "当前生效版本",
		"stats-v9.sock", "无法确认",
	} {
		if !strings.Contains(joined, must) {
			t.Fatalf("Kept 里应说明 %q，实际 %v", must, out.Kept)
		}
	}
	// 文件系统层面的真相（不只信返回体）。
	if _, err := os.Stat(filepath.Join(dir, "stats-v3.sock")); !os.IsNotExist(err) {
		t.Fatal("已确认无进程使用的套接字应当真的被删掉")
	}
	for _, n := range []string{"stats-v1.sock", "stats-v2.sock", "stats-v9.sock", "not-a-socket.txt"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("%s 必须保留：%v", n, err)
		}
	}
	if len(out.Problems) != 0 {
		t.Fatalf("本用例不该有问题项，实际 %v", out.Problems)
	}
}

// 删不掉时：如实记进 Problems、给出说明，**不 panic 也不返回 error**。
//
// 这条对应评审要求"清理失败只告警，不影响已成功发布的配置"：
// 清理是顺手打扫，把它升级成可用性问题就本末倒置了。
func TestPruneStatsSocketsReportsFailureWithoutError(t *testing.T) {
	dir := t.TempDir()
	// 造一个"看起来是普通文件、实际是非空目录"的条目：os.Remove 对它必然失败（ENOTEMPTY），
	// 而按过滤规则它不会被跳过（IsDir 由注入的条目说了算）——
	// 正好用来验证"删不掉时怎么办"。
	victim := filepath.Join(dir, "stats-v7.sock")
	if err := os.MkdirAll(victim, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := NewHAProxy()
	h.StatsSocketDir = dir
	h.CurrentDir = filepath.Join(t.TempDir(), "current") // 没有 VERSION ⇒ 当前版本视为 0
	h.readDir = func(string) ([]os.DirEntry, error) {
		return []os.DirEntry{fakeDirEntry{name: "stats-v7.sock"}}, nil
	}
	h.SetSocketProbe(func(context.Context, string) (bool, bool) { return false, true })

	out := h.PruneStaleStatsSockets(context.Background())
	if len(out.Removed) != 0 {
		t.Fatalf("删不掉的不该计入 Removed（否则界面会说「已清理」），实际 %v", out.Removed)
	}
	if len(out.Problems) != 1 || !strings.Contains(out.Problems[0], "stats-v7.sock") {
		t.Fatalf("删不掉必须如实记进 Problems，实际 %v", out.Problems)
	}
	if !strings.Contains(out.Note, "不影响") {
		t.Fatalf("Note 要说明它不影响转发与发布结果，实际 %q", out.Note)
	}
}

// 未启用按版本区分的套接字（StatsSocketDir 为空）⇒ 明确说"无需清理"，而不是报错。
func TestPruneStatsSocketsNoDirConfigured(t *testing.T) {
	h := NewHAProxy()
	h.StatsSocketDir = ""
	out := h.PruneStaleStatsSockets(context.Background())
	if out.Scanned != 0 || len(out.Removed) != 0 || len(out.Problems) != 0 {
		t.Fatalf("未配置目录时不该有任何动作，实际 %+v", out)
	}
	if !strings.Contains(out.Note, "无需清理") {
		t.Fatalf("应明确说明无需清理，实际 %q", out.Note)
	}
}
