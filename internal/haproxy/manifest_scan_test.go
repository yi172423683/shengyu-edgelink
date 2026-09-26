package haproxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件钉住"配置是否引用了清单外文件"这条判据的**识别规则**。
//
// 上层（dataplane.HAProxy.publishFiles）用它来决定"拒绝还是放行"，
// 而它判错的两种方向代价完全不同：
//   - 漏报：一个未纳入清单的文件在运行时被读到 ⇒ 清单不再是权威说明（必须避免）；
//   - 误报：把注释里的路径当成引用 ⇒ 正常配置被拒绝，且是在最不能拖的时候（回滚）。
//
// 所以这里既测"能认出来"，也测"不该认的别认"。

// 渲染产物的真实形状：`-f {CONFIGDIR}/sni_allow_8443.lst`，
// 其中 {CONFIGDIR} 在写入版本目录时被替换成该版本的绝对路径。
func TestScanCitedFilesFindsRendererStyleReference(t *testing.T) {
	dir := t.TempDir()
	cfg := "global\n" +
		"frontend fe_sni_8443\n" +
		"  tcp-request content reject unless { var(txn.sni) -m str -i -f " +
		filepath.ToSlash(filepath.Join(dir, "sni_allow_8443.lst")) + " }\n"
	got := ScanCitedFiles(dir, map[string][]byte{"haproxy.cfg": []byte(cfg)})
	if len(got) != 1 {
		t.Fatalf("应识别出 1 条引用，实际 %v", got)
	}
	if got[0].Name != "sni_allow_8443.lst" {
		t.Fatalf("被引用的文件名应去掉版本目录前缀，实际 %q", got[0].Name)
	}
	if got[0].ByName != "haproxy.cfg" {
		t.Fatalf("应记下是哪个配置引用的，实际 %q", got[0].ByName)
	}
}

// 一个文件被多处引用时只报一次（否则报错信息会被重复条目淹没）。
func TestScanCitedFilesDeduplicates(t *testing.T) {
	dir := t.TempDir()
	p := filepath.ToSlash(filepath.Join(dir, "shared.lst"))
	cfg := "frontend a\n  -f " + p + "\nfrontend b\n  -f " + p + "\n  -f " + p + "\n"
	got := ScanCitedFiles(dir, map[string][]byte{"haproxy.cfg": []byte(cfg)})
	if len(got) != 1 || got[0].Name != "shared.lst" {
		t.Fatalf("同一文件的多次引用只应报一次，实际 %v", got)
	}
}

// 不该被当成引用的三种情形。
//
// 这里刻意**不**追求"识别一切写法"：只认"以本版本目录绝对路径开头"的引用。
// 这样注释里提到的文件名、其它目录的文件、以及纯文本里出现的名字都不会误报 ——
// 误报会让正常配置在回滚时被拒，代价比漏报更直接。
func TestScanCitedFilesIgnoresNonReferences(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	cfg := strings.Join([]string{
		"# 备注：这个文件以前叫 sni_allow_9443.lst（裸文件名，不是引用）",
		"global",
		"  log /run/shengyu-edgelink/log.sock local0",                       // 别的目录
		"  ca-base " + filepath.ToSlash(filepath.Join(other, "ca.pem")),     // 其它目录的绝对路径
		"  stats socket /run/shengyu-edgelink/stats-v3.sock mode 660 level", // 运行时套接字
	}, "\n")
	got := ScanCitedFiles(dir, map[string][]byte{"haproxy.cfg": []byte(cfg)})
	if len(got) != 0 {
		t.Fatalf("不该把非本目录引用当成引用，实际 %v", got)
	}
}

// 含 NUL 的内容按二进制跳过：把二进制当文本找路径只会产生噪声。
func TestScanCitedFilesSkipsBinary(t *testing.T) {
	dir := t.TempDir()
	blob := append([]byte(filepath.ToSlash(dir)+"/hidden.lst\x00"), make([]byte, 16)...)
	got := ScanCitedFiles(dir, map[string][]byte{"blob.bin": blob})
	if len(got) != 0 {
		t.Fatalf("二进制内容不应被扫描，实际 %v", got)
	}
}

// ScanVersionDir：多余文件与"被引用的清单外文件"必须分别报出来。
func TestScanVersionDirSeparatesExtraFromCited(t *testing.T) {
	dir := t.TempDir()
	cfg := "global\nfrontend fe\n  -f " + filepath.ToSlash(filepath.Join(dir, "cited.lst")) + " }\n"
	mustWrite(t, filepath.Join(dir, "haproxy.cfg"), cfg)
	mustWrite(t, filepath.Join(dir, "extra.lst"), "x\n") // 多余但无人引用
	mustWrite(t, filepath.Join(dir, "cited.lst"), "a\n") // 多余且被引用

	extra, cited, err := ScanVersionDir(dir, []string{"haproxy.cfg"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(extra, ",") != "cited.lst,extra.lst" {
		t.Fatalf("多余文件应含这两个，实际 %v", extra)
	}
	if len(cited) != 1 || cited[0].Name != "cited.lst" {
		t.Fatalf("被引用的清单外文件应只有 cited.lst，实际 %v", cited)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}
