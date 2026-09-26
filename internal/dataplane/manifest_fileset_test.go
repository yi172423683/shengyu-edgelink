package dataplane

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/shengyu/edgelink/internal/haproxy"
)

// current/ 的文件集合必须以**版本清单**为准，而不是"目录里有什么就抄什么"。
//
// 真机复现（docs/08 回滚 UI 验收）：测试用的 9443 入口带出了 sni_allow_9443.lst，
// 之后删掉那条入口再发布，那个文件仍然留在 current/ 里。
// 它不会被 HAProxy 读到（渲染出的配置引用的是**版本目录**的绝对路径），
// 但 `ls current/` 会显示一个看起来"当前生效"的允许清单，
// 足以让排查者得出与事实相反的结论 —— 这正是本平台到处在避免的假象。
//
// 本文件把"清单驱动"这件事钉死（边界按评审结论，见 docs/08 §10）：
//  1. 清单里的文件必须全部存在（否则版本目录不完整 ⇒ **拒绝**发布，且不得进入 verified）；
//  2. 目录里有清单没列的文件 ⇒ **不阻断**发布/回滚，作为 warning 上报
//     （这些文件不会进入 current/，而当硬错误会在事故中挡住回滚）；
//  3. 但"配置**实际引用**了清单外文件" ⇒ **拒绝**（引用的是版本目录绝对路径，运行时会生效）；
//  4. 发布成功后 current/ 的文件集合必须与目标版本清单**完全一致**（除 VERSION）——
//     多余文件绝不进入 current/；
//  5. CompareCurrentToVersion 能识别"只恢复了一半"（VERSION 回去了、正文没回去）。

// writeVersionDirWithManifest 造一个"像真实发布产物一样"的版本目录（含清单）。
func writeVersionDirWithManifest(t *testing.T, dir string, version int, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(files))
	for n, body := range files {
		names = append(names, n)
		if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(names)
	mf := haproxy.VersionManifest{
		Version: version, NodeID: "node_test", ContentHash: "hash-" + strconv.Itoa(version),
		RendererVersion: haproxy.RendererVersion, Files: names,
	}
	b, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, haproxy.ManifestFileName), b, 0o640); err != nil {
		t.Fatal(err)
	}
}

// newTestHAProxy 建一个指向临时目录、命令全部成功的 HAProxy 应用器。
func newTestHAProxy(t *testing.T, root string) *HAProxy {
	t.Helper()
	h := NewHAProxy()
	h.ConfigRoot = root
	h.CurrentDir = filepath.Join(root, "current")
	h.SetRunner((&fakeRunner{}).run)
	return h
}

// listDirFiles 列出目录里的普通文件（跳过 .tmp 与子目录）。
func listDirFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// 核心回归（缺陷二）：旧版本有 sni_allow_9443.lst，新版本没有 ⇒ 发布成功后它必须消失，
// 且 current/ 的文件集合必须与目标版本完全一致。
func TestPublishFilesKeepsCurrentEqualToTargetManifest(t *testing.T) {
	root := t.TempDir()
	versions := filepath.Join(root, "versions", "node_test")

	v1 := filepath.Join(versions, "v1")
	v2 := filepath.Join(versions, "v2")
	// v1：只有 8443 入口
	writeVersionDirWithManifest(t, v1, 1, map[string]string{
		"haproxy.cfg":        "global\n  # v1\n",
		"sni_allow_8443.lst": "a.example.com\n",
		"meta.json":          `{"version":1}`,
		"state.json":         `{"node":{"id":"node_test"}}`,
	})
	// v2：多了 9443 入口（于是多一个允许清单）
	writeVersionDirWithManifest(t, v2, 2, map[string]string{
		"haproxy.cfg":        "global\n  # v2\n",
		"sni_allow_8443.lst": "a.example.com\n",
		"sni_allow_9443.lst": "t.example.com\n",
		"meta.json":          `{"version":2}`,
		"state.json":         `{"node":{"id":"node_test"}}`,
	})

	h := newTestHAProxy(t, root)
	ctx := context.Background()

	// 先到 v2：current/ 应含 9443 清单，且与 v2 目录完全一致
	if err := h.Apply(ctx, ApplyRequest{
		ConfigDir: v2, ConfigPath: filepath.Join(v2, "haproxy.cfg"), Version: 2,
	}); err != nil {
		t.Fatalf("Apply v2 失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.CurrentDir, "sni_allow_9443.lst")); err != nil {
		t.Fatalf("v2 生效时 current/ 应含 sni_allow_9443.lst: %v", err)
	}
	assertCurrentMatches(t, h, v2, 2)

	// 再发布 v1（新版本不再使用 9443）⇒ 那个清单必须被清掉
	if err := h.Apply(ctx, ApplyRequest{
		ConfigDir: v1, ConfigPath: filepath.Join(v1, "haproxy.cfg"),
		Version: 1, PreviousVersion: 2,
	}); err != nil {
		t.Fatalf("Apply v1 失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.CurrentDir, "sni_allow_9443.lst")); !os.IsNotExist(err) {
		t.Fatalf("切到 v1 后 current/ 仍残留 sni_allow_9443.lst（err=%v）—— "+
			"它会让人误以为 9443 那条入口还生效", err)
	}
	assertCurrentMatches(t, h, v1, 1)

	// 清单本身也要落到 current/，这样 `cat current/manifest.json` 就能回答
	// "现在这一版应该有哪几个文件"，而不用去猜哪些残留是生效的。
	if _, err := os.Stat(filepath.Join(h.CurrentDir, haproxy.ManifestFileName)); err != nil {
		t.Fatalf("current/ 应含版本清单 %s: %v", haproxy.ManifestFileName, err)
	}
	// 临时文件不得残留
	for _, f := range listDirFiles(t, h.CurrentDir) {
		if strings.HasSuffix(f, ".tmp") {
			t.Fatalf("不应残留临时文件 %s", f)
		}
	}
}

// assertCurrentMatches 断言 current/ 的文件集合与目标版本目录**完全一致**（除 VERSION）。
func assertCurrentMatches(t *testing.T, h *HAProxy, versionDir string, version int) {
	t.Helper()
	// 期望集合来自**清单**，不是目录列举：
	// current/ 只应含清单允许的文件，目录里"清单之外"的多余文件本来就不该进来
	// （这正是评审确认的边界，见本文件头部说明）。
	set, fromManifest, extra, err := h.versionFileSet(versionDir)
	if err != nil {
		t.Fatalf("读版本清单失败: %v", err)
	}
	if !fromManifest {
		t.Fatal("本用例的版本目录都写了清单，不应退回目录列举")
	}
	got := listDirFiles(t, h.CurrentDir)
	// 目录列举里剔除"清单外多余文件"，再加 current/ 独有的 VERSION。
	extraSet := map[string]bool{}
	for _, n := range extra {
		extraSet[n] = true
	}
	var want []string
	for _, n := range listDirFiles(t, versionDir) {
		if n == VersionFileName || extraSet[n] {
			continue
		}
		if !set[n] {
			t.Fatalf("目录列举与清单不一致：%s 既不在清单里、也没被判为多余文件", n)
		}
		want = append(want, n)
	}
	want = append(want, VersionFileName)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("current/ 文件集合与第 %d 版清单不一致：\n  current = %v\n  期望    = %v", version, got, want)
	}
	// 多余文件必须一个都没进 current/。
	for _, x := range extra {
		for _, g := range got {
			if g == x {
				t.Fatalf("清单外多余文件 %s 不允许出现在 current/ 里", x)
			}
		}
	}
	diff, err := h.CompareCurrentToVersion(context.Background(), versionDir)
	if err != nil {
		t.Fatalf("CompareCurrentToVersion 出错: %v", err)
	}
	if !diff.OK() {
		t.Fatalf("CompareCurrentToVersion 应报一致，实际 %s", diff.Describe())
	}
	if diff.ManifestMissing {
		t.Fatal("本用例的版本目录都写了清单，不应报 ManifestMissing")
	}
	// 多余文件不参与 current/ 比对，但必须如实上报（它是"目录被改过"的唯一信号）。
	if len(diff.VersionDirExtra) != len(extra) {
		t.Fatalf("CompareCurrentToVersion 应上报 %d 个清单外多余文件，实际 %v", len(extra), diff.VersionDirExtra)
	}
	ver, err := h.ActiveVersion(context.Background())
	if err != nil || ver != version {
		t.Fatalf("生效版本应为 %d，实际 %d (%v)", version, ver, err)
	}
}

// CompareCurrentToVersion 必须能识别"只恢复了一半"：VERSION 回来了、正文没回来。
// 这是"看起来回滚成功"里最危险的一种（磁盘标记说是旧版，重启后加载的是新正文）。
func TestCompareCurrentToVersionDetectsHalfRestore(t *testing.T) {
	root := t.TempDir()
	v1 := filepath.Join(root, "versions", "node_test", "v1")
	writeVersionDirWithManifest(t, v1, 1, map[string]string{
		"haproxy.cfg": "global\n  # v1\n",
		"meta.json":   `{"version":1}`,
	})
	h := newTestHAProxy(t, root)
	if err := h.Apply(context.Background(), ApplyRequest{
		ConfigDir: v1, ConfigPath: filepath.Join(v1, "haproxy.cfg"), Version: 1,
	}); err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}

	// 制造"半恢复"：VERSION 与 meta.json 回到 v1，但正文仍是别的；再塞一个陈旧残留。
	if err := os.WriteFile(filepath.Join(h.CurrentDir, "haproxy.cfg"), []byte("global\n  # 新版本正文\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.CurrentDir, "sni_allow_9443.lst"), []byte("t.example.com\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	diff, err := h.CompareCurrentToVersion(context.Background(), v1)
	if err != nil {
		t.Fatal(err)
	}
	if diff.OK() {
		t.Fatal("正文与清单不一致时必须报告差异，否则「回滚成功」就成了假话")
	}
	if !containsStr(diff.Mismatched, "haproxy.cfg") {
		t.Fatalf("应报 haproxy.cfg 内容不一致，实际 %+v", diff)
	}
	if !containsStr(diff.Extra, "sni_allow_9443.lst") {
		t.Fatalf("应报 sni_allow_9443.lst 是多余残留，实际 %+v", diff)
	}
	if !strings.Contains(diff.Describe(), "haproxy.cfg") {
		t.Fatalf("描述里要能看出是哪个文件出的问题：%q", diff.Describe())
	}
}

// 清单列出目录里不存在的文件 ⇒ 版本目录不完整 ⇒ 拒绝发布，且 current/ 一个字节不许动。
func TestPublishRefusesWhenManifestListsMissingFile(t *testing.T) {
	root := t.TempDir()
	v1 := filepath.Join(root, "versions", "node_test", "v1")
	writeVersionDirWithManifest(t, v1, 1, map[string]string{
		"haproxy.cfg": "global\n  # v1\n",
	})
	// 清单里追加一个并不存在的文件
	mfPath := filepath.Join(v1, haproxy.ManifestFileName)
	var mf haproxy.VersionManifest
	b, _ := os.ReadFile(mfPath)
	if err := json.Unmarshal(b, &mf); err != nil {
		t.Fatal(err)
	}
	mf.Files = append(mf.Files, "sni_allow_9443.lst")
	nb, _ := json.MarshalIndent(mf, "", "  ")
	if err := os.WriteFile(mfPath, nb, 0o640); err != nil {
		t.Fatal(err)
	}

	h := newTestHAProxy(t, root)
	err := h.Apply(context.Background(), ApplyRequest{
		ConfigDir: v1, ConfigPath: filepath.Join(v1, "haproxy.cfg"), Version: 1,
	})
	if err == nil {
		t.Fatal("清单列出了不存在的文件时必须拒绝发布（否则 f 发出去的是一份不完整的配置）")
	}
	if _, serr := os.Stat(filepath.Join(h.CurrentDir, VersionFileName)); serr == nil {
		t.Fatal("拒绝发布时不允许写入 VERSION 标记")
	}
	if got := listDirFiles(t, h.CurrentDir); len(got) != 0 {
		t.Fatalf("拒绝发布时 current/ 不应被改动，实际 %v", got)
	}
}

// 目录里有清单没列的文件（清单生成后目录被人改过）⇒ **不阻断**发布，但必须如实报出来。
//
// 边界是评审定的（docs/08 §10）：这类文件常见于运维手工备份
// （cp haproxy.cfg haproxy.cfg.orig），把它当硬错误会**在事故中挡住回滚** ——
// 而回滚恰好是最不能拖延的时候。它也不会进入 current/：写入以清单为准。
// 所以它降级为 extra 返回值，由发布层记 warning + 审计 + 界面提示。
//
// **唯一例外**是"配置真的引用了它" ⇒ 必须拒绝，见下一个用例。
func TestPublishReportsExtraFilesWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	v1 := filepath.Join(root, "versions", "node_test", "v1")
	writeVersionDirWithManifest(t, v1, 1, map[string]string{
		"haproxy.cfg": "global\n  # v1\n",
	})
	if err := os.WriteFile(filepath.Join(v1, "haproxy.cfg.orig"),
		[]byte("global\n  # 运维手工留的旧正文备份\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	h := newTestHAProxy(t, root)
	extra, err := h.publishFiles(v1, 1)
	if err != nil {
		t.Fatalf("清单外的多余文件不允许阻断发布（会在事故中挡住回滚），实际报错: %v", err)
	}
	if len(extra) != 1 || extra[0] != "haproxy.cfg.orig" {
		t.Fatalf("必须把多余文件如实报出来（否则它是隐形的），实际 %v", extra)
	}
	// current/ 仍必须与清单完全一致 —— 多余文件绝不进入 current/。
	assertCurrentMatches(t, h, v1, 1)
	for _, f := range listDirFiles(t, h.CurrentDir) {
		if f == "haproxy.cfg.orig" {
			t.Fatal("清单外的文件不允许被写进 current/")
		}
	}
}

// 配置**实际引用**了清单外的文件 ⇒ 必须拒绝（评审第 3 条）。
//
// 为什么这条不能像"多余文件"那样放过：渲染产物里的引用是**版本目录的绝对路径**
// （writeVersionDir 把 {CONFIGDIR} 替换成版本目录），
// 所以那个文件在运行时真的会被 HAProxy 读到 ——
// "未纳入清单的配置会影响运行"，清单也就不再是"这一版由哪些文件组成"的权威说明。
func TestPublishRefusesWhenConfigCitesFileOutsideManifest(t *testing.T) {
	root := t.TempDir()
	v1 := filepath.Join(root, "versions", "node_test", "v1")
	// cfg 里引用了同目录下的允许清单 —— 但那个文件没被写进清单。
	cfg := "global\n  # v1\nfrontend fe\n" +
		"  tcp-request content reject unless { var(txn.sni) -m str -i -f " +
		filepath.ToSlash(filepath.Join(v1, "sni_allow_9443.lst")) + " }\n"
	writeVersionDirWithManifest(t, v1, 1, map[string]string{"haproxy.cfg": cfg})
	if err := os.WriteFile(filepath.Join(v1, "sni_allow_9443.lst"),
		[]byte("a.example.com\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	h := newTestHAProxy(t, root)
	_, err := h.publishFiles(v1, 1)
	if err == nil {
		t.Fatal("配置引用了清单外的文件时必须拒绝发布（否则未纳入清单的配置会影响运行）")
	}
	if !strings.Contains(err.Error(), "sni_allow_9443.lst") {
		t.Fatalf("报错要点名是哪个文件被引用：%v", err)
	}
	// 拒绝必须发生在改动任何文件之前：current/ 一个字节都不许动。
	if got := listDirFiles(t, h.CurrentDir); len(got) != 0 {
		t.Fatalf("拒绝时 current/ 不应被改动，实际 %v", got)
	}
}

// 清单损坏 ⇒ 拒绝发布（读不到权威清单就无法判断 current/ 该有哪些文件）。
func TestPublishRefusesWhenManifestCorrupt(t *testing.T) {
	root := t.TempDir()
	v1 := filepath.Join(root, "versions", "node_test", "v1")
	writeVersionDirWithManifest(t, v1, 1, map[string]string{
		"haproxy.cfg": "global\n  # v1\n",
	})
	if err := os.WriteFile(filepath.Join(v1, haproxy.ManifestFileName), []byte("{ 这不是 json"), 0o640); err != nil {
		t.Fatal(err)
	}

	h := newTestHAProxy(t, root)
	if err := h.Apply(context.Background(), ApplyRequest{
		ConfigDir: v1, ConfigPath: filepath.Join(v1, "haproxy.cfg"), Version: 1,
	}); err == nil {
		t.Fatal("清单损坏时必须拒绝发布")
	}
}

// 清理陈旧文件这一步失败 ⇒ 发布整体失败，并恢复快照（current/ 与 VERSION 一个字节不许动）。
//
// 这里用"读不到权威清单"来触发清理失败：publishFiles 的顺序是
// 快照 → 清理 → 写文件 → 写 VERSION，所以清理失败时 current/ 应完好如初。
func TestPublishFilesRestoresSnapshotWhenPruneFails(t *testing.T) {
	root := t.TempDir()
	versions := filepath.Join(root, "versions", "node_test")
	v1 := filepath.Join(versions, "v1")
	v2 := filepath.Join(versions, "v2")
	writeVersionDirWithManifest(t, v1, 1, map[string]string{"haproxy.cfg": "global\n  # v1\n"})
	writeVersionDirWithManifest(t, v2, 2, map[string]string{"haproxy.cfg": "global\n  # v2\n"})

	h := newTestHAProxy(t, root)
	ctx := context.Background()
	if err := h.Apply(ctx, ApplyRequest{
		ConfigDir: v2, ConfigPath: filepath.Join(v2, "haproxy.cfg"), Version: 2,
	}); err != nil {
		t.Fatalf("Apply v2 失败: %v", err)
	}
	before := listDirFiles(t, h.CurrentDir)
	beforeCfg, err := os.ReadFile(filepath.Join(h.CurrentDir, "haproxy.cfg"))
	if err != nil {
		t.Fatal(err)
	}

	// 把待发布版本的清单改坏 ⇒ "清理"这一步看不到权威文件集合 ⇒ 必须失败
	if err := os.WriteFile(filepath.Join(v1, haproxy.ManifestFileName), []byte("{ 坏掉的清单"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := h.Apply(ctx, ApplyRequest{
		ConfigDir: v1, ConfigPath: filepath.Join(v1, "haproxy.cfg"),
		Version: 1, PreviousVersion: 2,
	}); err == nil {
		t.Fatal("读不到权威清单时必须报错（否则清理会退化成靠猜）")
	}

	after := listDirFiles(t, h.CurrentDir)
	if strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("清理失败时必须恢复快照：\n  之前 = %v\n  之后 = %v", before, after)
	}
	afterCfg, err := os.ReadFile(filepath.Join(h.CurrentDir, "haproxy.cfg"))
	if err != nil || string(afterCfg) != string(beforeCfg) {
		t.Fatalf("失败后 current/haproxy.cfg 应保持原样：err=%v", err)
	}
	if ver, _ := h.ActiveVersion(ctx); ver != 2 {
		t.Fatalf("失败后生效版本必须仍是 2，实际 %d", ver)
	}
}
