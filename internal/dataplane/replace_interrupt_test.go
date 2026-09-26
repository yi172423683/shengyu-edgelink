package dataplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 本文件覆盖评审要求补的三项发布可靠性测试中的第三项：
// **配置替换期间进程中断**不会留下"新正文 + 旧版本号"的混合状态。
//
// 为什么这条最要紧：混合状态在进程内是"看不出问题"的 —— 老 worker 还在用内存里的
// 旧配置服务，界面读 VERSION 也说是旧版本。但**下一次 reload 或进程重启**就会去读
// 文件正文，于是加载一份从未被批准、也从未验证过的配置。
// 故障会推迟到重启那一刻才爆发，而那时已经找不到"是谁改的"。

// setupCurrent 造一个"v1 正在生效"的 current 目录。
func setupCurrent(t *testing.T) (*HAProxy, string, string) {
	t.Helper()
	root := t.TempDir()
	current := filepath.Join(root, "current")
	candidate := filepath.Join(root, "versions", "v2")
	for _, d := range []string{current, candidate} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(current, "haproxy.cfg"), []byte("old-config"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(current, VersionFileName), []byte("1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	// 候选版本目录（模拟发布流水线写入的版本目录）
	if err := os.WriteFile(filepath.Join(candidate, "haproxy.cfg"), []byte("new-config"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "sni_allow_443.lst"), []byte("a.example.com\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	h := NewHAProxy()
	h.ConfigRoot = root
	h.CurrentDir = current
	h.SetRunner(func(context.Context, string, ...string) (string, string, error) { return "", "", nil })
	return h, current, candidate
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// 中断发生在"内容文件已换、VERSION 还没写"的那一刻：
// 这是最危险的一种中断点，必须整体恢复，而不是留下新正文配旧版本号。
func TestInterruptedReplaceRestoresWholeCurrentDir(t *testing.T) {
	h, current, candidate := setupCurrent(t)
	// 注入：写 meta.json（排在 haproxy.cfg 之后的文件）时失败
	h.writeFile = func(path string, b []byte, m os.FileMode) error {
		if filepath.Base(path) == "meta.json" {
			return errors.New("injected disk failure")
		}
		return atomicWriteFile(path, b, m)
	}
	// 让候选目录里确实有一个 meta.json，从而一定经过那条注入失败的路径
	if err := os.WriteFile(filepath.Join(candidate, "meta.json"), []byte("{}"), 0o640); err != nil {
		t.Fatal(err)
	}

	err := h.Apply(context.Background(), ApplyRequest{
		Version: 2, PreviousVersion: 1, ConfigDir: candidate,
		ConfigPath: filepath.Join(candidate, "haproxy.cfg"),
	})
	if err == nil {
		t.Fatal("注入的写入失败必须让 Apply 报错")
	}

	// ① 正文必须是旧的（不能是 new-config）
	if got := readFileString(t, filepath.Join(current, "haproxy.cfg")); got != "old-config" {
		t.Fatalf("中断后 current 里的配置正文必须是旧版，实际 %q", got)
	}
	// ② 版本号也必须是旧的：两者必须**一起**是旧的
	if got := readFileString(t, filepath.Join(current, VersionFileName)); got != "1\n" {
		t.Fatalf("中断后版本标记必须仍为 1，实际 %q", got)
	}
	// ③ 候选版本引入的新文件不能残留（残留会让下次 reload 读到"没有登记的清单"）
	if _, err := os.Stat(filepath.Join(current, "sni_allow_443.lst")); err == nil {
		t.Fatal("中断后不应残留新版本引入的文件（那是混合状态的另一种形式）")
	}
	// ④ 生效版本必须是 1
	ver, err := h.ActiveVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ver != 1 {
		t.Fatalf("中断后生效版本应为 1，实际 %d", ver)
	}
}

// 在中断之后重新发布：必须能恢复成完整的新版本（不能因为"上次失败"就永久卡住）。
func TestRepublishAfterInterruptedReplaceSucceeds(t *testing.T) {
	h, current, candidate := setupCurrent(t)
	failNext := true
	h.writeFile = func(path string, b []byte, m os.FileMode) error {
		if failNext && filepath.Base(path) == VersionFileName {
			return errors.New("injected failure right before the commit marker")
		}
		return atomicWriteFile(path, b, m)
	}
	if err := h.Apply(context.Background(), ApplyRequest{
		Version: 2, PreviousVersion: 1, ConfigDir: candidate,
		ConfigPath: filepath.Join(candidate, "haproxy.cfg"),
	}); err == nil {
		t.Fatal("提交标记写失败时必须报错（不能返回成功却留下旧版本）")
	}
	if got := readFileString(t, filepath.Join(current, "haproxy.cfg")); got != "old-config" {
		t.Fatalf("失败后正文应已恢复，实际 %q", got)
	}

	// 恢复正常写入后重发：必须完整成功
	failNext = false
	if err := h.Apply(context.Background(), ApplyRequest{
		Version: 2, PreviousVersion: 1, ConfigDir: candidate,
		ConfigPath: filepath.Join(candidate, "haproxy.cfg"),
	}); err != nil {
		t.Fatalf("恢复后重新发布应成功，实际: %v", err)
	}
	if got := readFileString(t, filepath.Join(current, "haproxy.cfg")); got != "new-config" {
		t.Fatalf("重新发布后正文应为新版，实际 %q", got)
	}
	if got := readFileString(t, filepath.Join(current, VersionFileName)); got != "2\n" {
		t.Fatalf("重新发布后版本标记应为 2，实际 %q", got)
	}
	ver, err := h.ActiveVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ver != 2 {
		t.Fatalf("生效版本应为 2，实际 %d", ver)
	}
}

// 进程被直接杀掉（任何 defer 都不会执行），留下混合状态：
// 下一次发布必须能把它修好；在那之前，ActiveVersion 必须如实报告"版本号"，
// 而不是把磁盘上的新正文认成新版本 —— 这就是"以 VERSION 为提交标记"的意义。
func TestRecoversFromCrashLeftMixedState(t *testing.T) {
	h, current, candidate := setupCurrent(t)
	// 手工制造"崩溃现场"：新正文已经落盘，VERSION 还是旧的，还多了一个新文件
	if err := os.WriteFile(filepath.Join(current, "haproxy.cfg"), []byte("half-written-new-config"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(current, "sni_allow_443.lst"), []byte("stale\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, filepath.Join(current, VersionFileName)); got != "1\n" {
		t.Fatalf("前置条件：版本标记应为 1，实际 %q", got)
	}
	// 崩溃后、修复前：版本号如实报告为 1（提交标记没有前进）
	ver, err := h.ActiveVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ver != 1 {
		t.Fatalf("提交标记未前进时不应报告新版本，实际 %d", ver)
	}

	// 重新发布：必须把混合状态整体覆盖成一致的新版本
	if err := h.Apply(context.Background(), ApplyRequest{
		Version: 2, PreviousVersion: 1, ConfigDir: candidate,
		ConfigPath: filepath.Join(candidate, "haproxy.cfg"),
	}); err != nil {
		t.Fatalf("从崩溃残留状态重新发布应成功，实际: %v", err)
	}
	if got := readFileString(t, filepath.Join(current, "haproxy.cfg")); got != "new-config" {
		t.Fatalf("修复后正文应为新版，实际 %q", got)
	}
	if got := readFileString(t, filepath.Join(current, "sni_allow_443.lst")); got != "a.example.com\n" {
		t.Fatalf("修复后允许清单应为新版内容，实际 %q", got)
	}
	if got := readFileString(t, filepath.Join(current, VersionFileName)); got != "2\n" {
		t.Fatalf("修复后版本标记应为 2，实际 %q", got)
	}
}

// 快照拿不到时必须**拒绝发布**：没有退路就不改线上配置。
func TestRefusePublishWhenSnapshotUnavailable(t *testing.T) {
	h, _, candidate := setupCurrent(t)
	// 让读取 current 失败（模拟权限问题/目录异常）
	h.readFile = func(string) ([]byte, error) { return nil, errors.New("permission denied") }
	err := h.Apply(context.Background(), ApplyRequest{
		Version: 2, PreviousVersion: 1, ConfigDir: candidate,
		ConfigPath: filepath.Join(candidate, "haproxy.cfg"),
	})
	if err == nil {
		t.Fatal("无法快照当前配置时必须拒绝发布（否则失败时没有回退基准）")
	}
}
