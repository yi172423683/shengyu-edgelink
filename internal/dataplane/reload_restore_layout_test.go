package dataplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 本文件针对一个**真机才发现**的缺陷，它是"测试夹具与生产目录布局不一致"的典型：
//
//	生产布局：  <ConfigRoot>/versions/<nodeID>/vN        （见 publish.Pipeline.VersionsDir）
//	旧测试夹具：<ConfigRoot>/versions/vN                （见 TestHAProxyReloadFailureRestoresPreviousVersion）
//
// 于是 HAProxy.Apply 在 reload 失败后"把上一版放回去"的那段逻辑，
// 沿着 `versions/vN` 去找目录 —— 在生产布局下**永远找不到**，于是**静默跳过恢复**。
// 结果就是代码注释里明确说不允许出现的那种状态：
//
//	「新正文已经在 current/ 里，但跑着的还是旧 worker」
//
// 下一次 reload 或机器重启，就会直接加载这份未获批准的配置。
//
// 下面这个测试用**生产布局**复现：它必须在修复前失败、修复后通过。

func TestHAProxyReloadFailureRestoresPreviousVersionInProductionLayout(t *testing.T) {
	const nodeID = "node_06GCGE2NPCC624J5AMKYQWC4C0"

	root := t.TempDir()
	versions := filepath.Join(root, "versions", nodeID) // 生产布局：带节点目录
	bodies := map[string]string{
		"v1": "global\n  # v1\n",
		"v2": "global\n  # v2\n",
	}
	for v, body := range bodies {
		dir := filepath.Join(versions, v)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "haproxy.cfg"), []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	current := filepath.Join(root, "current")
	h := NewHAProxy()
	h.ConfigRoot = root
	h.CurrentDir = current
	f := &fakeRunner{responses: map[string]fakeResp{
		"systemctl reload": {errb: "Job for shengyu-edgelink-haproxy.service failed", err: errors.New("exit status 1")},
	}}
	h.SetRunner(f.run)

	// 模拟线上正在跑 v1
	if _, err := h.publishFiles(filepath.Join(versions, "v1"), 1); err != nil {
		t.Fatalf("初始化 current 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(versions, "v1", "sni_allow_8443.lst"), []byte("a.example.com\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	err := h.Apply(context.Background(), ApplyRequest{
		ConfigDir:       filepath.Join(versions, "v2"),
		ConfigPath:      filepath.Join(versions, "v2", "haproxy.cfg"),
		Version:         2,
		PreviousVersion: 1,
	})
	if err == nil {
		t.Fatal("reload 失败时 Apply 必须返回错误")
	}

	// ① 版本标记必须回到 1
	ver, verr := h.ActiveVersion(context.Background())
	if verr != nil {
		t.Fatalf("读取生效版本失败: %v", verr)
	}
	if ver != 1 {
		t.Fatalf("reload 失败后 current/VERSION 必须回到 v1（否则下一次 reload/重启会加载未获批准的配置），实际 v%d", ver)
	}
	// ② 配置正文也必须是 v1 的
	body, rerr := os.ReadFile(filepath.Join(current, "haproxy.cfg"))
	if rerr != nil {
		t.Fatalf("读取 current 配置失败: %v", rerr)
	}
	if !containsSub(string(body), "# v1") {
		t.Fatalf("回滚后配置正文必须是 v1 的，实际:\n%s", string(body))
	}
}

// 反向对照：当"上一版目录不可读"时，数据面**无法**自行恢复 current/。
// 这时它必须把错误报出来（而不是假装恢复成功），让上层 publish.rollbackIfNeeded
// 再走一次"带验证的回滚"；同时 current/VERSION 会停在新区，
// 这一点是"上层必须兜底"的直接理由 —— 本测试把这个事实钉住。
func TestHAProxyApplyCannotRestoreWhenPreviousDirMissing(t *testing.T) {
	const nodeID = "node_test"
	root := t.TempDir()
	versions := filepath.Join(root, "versions", nodeID)
	v2dir := filepath.Join(versions, "v2")
	if err := os.MkdirAll(v2dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v2dir, "haproxy.cfg"), []byte("global\n  # v2\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	// 故意**不建** v1 目录：模拟"上一版目录被误删 / 不在预期位置"
	h := NewHAProxy()
	h.ConfigRoot = root
	h.CurrentDir = filepath.Join(root, "current")
	f := &fakeRunner{responses: map[string]fakeResp{
		"systemctl reload": {err: errors.New("exit status 1")},
	}}
	h.SetRunner(f.run)

	err := h.Apply(context.Background(), ApplyRequest{
		ConfigDir: v2dir, ConfigPath: filepath.Join(v2dir, "haproxy.cfg"),
		Version: 2, PreviousVersion: 1,
	})
	if err == nil {
		t.Fatal("reload 失败时 Apply 必须返回错误")
	}
	if !containsSub(err.Error(), "reload") {
		t.Fatalf("错误信息里应包含 reload 失败原因，实际: %v", err)
	}
	ver, verr := h.ActiveVersion(context.Background())
	if verr != nil {
		t.Fatalf("读取生效版本失败: %v", verr)
	}
	if ver != 2 {
		t.Fatalf("上一版目录缺失时数据面无从恢复，current/VERSION 会停在 v2；"+
			"上层必须据此兜底。实际 v%d", ver)
	}
}
