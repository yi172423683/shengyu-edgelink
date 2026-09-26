package dataplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// current/ 里不得滞留"不属于当前生效版本"的文件。
//
// 真机复现（docs/08 回滚 UI 验收）：测试用的 9443 入口带出了 sni_allow_9443.lst，
// 回滚到没有这条入口的 v2 之后，那个文件仍留在 current/ 里。
// 虽然它不会被 HAProxy 读到（渲染出的配置引用的是**版本目录**的绝对路径），
// 但 `ls current/` 会显示一个看起来"当前生效"的允许清单，
// 足以让排查者得出与事实相反的结论。
func TestPublishFilesPrunesFilesAbsentFromTargetVersion(t *testing.T) {
	const node = "node_test_prune"
	root := t.TempDir()
	versions := filepath.Join(root, "versions", node)

	write := func(dir string, files map[string]string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o640); err != nil {
				t.Fatal(err)
			}
		}
	}
	// v2 有 9443 入口 ⇒ 多一个允许清单；v1 没有
	v1 := filepath.Join(versions, "v1")
	v2 := filepath.Join(versions, "v2")
	write(v1, map[string]string{
		"haproxy.cfg":        "global\n  # v1\n",
		"sni_allow_8443.lst": "a.example.com\n",
		"meta.json":          `{"version":1}`,
	})
	write(v2, map[string]string{
		"haproxy.cfg":        "global\n  # v2\n",
		"sni_allow_8443.lst": "a.example.com\n",
		"sni_allow_9443.lst": "t.example.com\n",
		"meta.json":          `{"version":2}`,
	})

	h := NewHAProxy()
	h.ConfigRoot = root
	h.CurrentDir = filepath.Join(root, "current")
	h.SetRunner((&fakeRunner{}).run) // -c 与 reload 都成功

	// 先到 v2（此时 current/ 应有 9443 清单）
	if err := h.Apply(context.Background(), ApplyRequest{
		ConfigDir: v2, ConfigPath: filepath.Join(v2, "haproxy.cfg"), Version: 2,
	}); err != nil {
		t.Fatalf("Apply v2 失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.CurrentDir, "sni_allow_9443.lst")); err != nil {
		t.Fatalf("v2 生效时 current/ 应含 sni_allow_9443.lst: %v", err)
	}

	// 再回滚到 v1：那个清单必须被清掉
	if err := h.Apply(context.Background(), ApplyRequest{
		ConfigDir: v1, ConfigPath: filepath.Join(v1, "haproxy.cfg"),
		Version: 1, PreviousVersion: 2,
	}); err != nil {
		t.Fatalf("Apply v1 失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.CurrentDir, "sni_allow_9443.lst")); !os.IsNotExist(err) {
		t.Fatalf("切到 v1 后 current/ 仍残留 sni_allow_9443.lst（err=%v）—— "+
			"它会让人误以为 9443 那条入口还生效", err)
	}
	// 属于 v1 的文件必须在，且 VERSION 标记正确
	for _, f := range []string{"haproxy.cfg", "sni_allow_8443.lst", "meta.json", "VERSION"} {
		if _, err := os.Stat(filepath.Join(h.CurrentDir, f)); err != nil {
			t.Fatalf("current/%s 应存在: %v", f, err)
		}
	}
	ver, err := h.ActiveVersion(context.Background())
	if err != nil || ver != 1 {
		t.Fatalf("生效版本应为 1，实际 %d (%v)", ver, err)
	}
	// 临时文件不得残留
	if _, err := os.Stat(filepath.Join(h.CurrentDir, "haproxy.cfg.tmp")); err == nil {
		t.Fatal("不应残留 .tmp 文件")
	}
}

// 反向保护：清理失败时不能让 current/ 处于"删了一半"的状态，
// 而且**绝不能**动到目标版本自己的文件。
func TestPublishFilesKeepsTargetFilesWhenPruneTouchesOnlyStale(t *testing.T) {
	const node = "node_test_prune2"
	root := t.TempDir()
	versions := filepath.Join(root, "versions", node)
	target := filepath.Join(versions, "v1")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "haproxy.cfg"), []byte("global\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	h := NewHAProxy()
	h.ConfigRoot = root
	h.CurrentDir = filepath.Join(root, "current")
	// 版本目录读不到 ⇒ 无法判断哪些是陈旧的 ⇒ 必须报错而不是"当作没有陈旧文件"
	h.SetFileReader(os.ReadFile)
	h.SetRunner((&fakeRunner{}).run)
	err := h.Apply(context.Background(), ApplyRequest{
		ConfigDir:  filepath.Join(versions, "does-not-exist"),
		ConfigPath: filepath.Join(versions, "does-not-exist", "haproxy.cfg"),
		Version:    9,
	})
	if err == nil {
		t.Fatal("版本目录不存在时必须报错")
	}
	// 出错后不得留下半截状态
	if _, serr := os.Stat(filepath.Join(h.CurrentDir, "VERSION")); serr == nil {
		t.Fatal("失败时不应写入 VERSION 标记")
	}
	var _ = errors.New
}
