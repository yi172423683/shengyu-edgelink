package haproxy

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CitedFile 一条"配置引用了某个文件"的记录。
//
// 它存在的意义是把评审要求变成可检查的判据：
// **清单外、但被配置引用的文件必须拒绝**，因为配置是按绝对路径去读**版本目录**的。
type CitedFile struct {
	// Name 被引用的文件名（相对版本目录）。
	Name string
	// ByName 引用它的那个配置文件名（版本目录内的文件）。
	ByName string
}

// ScanCitedFiles 找出"配置文本里以绝对路径引用了本版本目录下某个文件"的引用。
//
// 为什么必须有这条判据（评审第 3 条）：
// 版本目录里"清单之外"的文件不会被复制到 current/，看上去无害；
// 但渲染时 {CONFIGDIR} 被替换成**版本目录的绝对路径**
// （见 publish.Pipeline.writeVersionDir），于是运行中的 HAProxy 读的是
// `<版本目录>/sni_allow_*.lst` —— 一个没进清单、却在运行时真实生效的文件。
// 一旦允许这种情况，"清单"就不再是"这一版由哪些文件组成"的权威说明，
// 而所有基于清单的判断（current/ 该有哪些文件、回滚是否完整）都会跟着失真。
// 所以只要引用存在，就必须拒绝发布/回滚，让人明确地把文件纳入清单、或去掉这条引用。
//
// 只识别**绝对路径**引用（dir 前缀），这是本平台渲染产物的实际形状
// （渲染器写的是 `-f {CONFIGDIR}/sni_allow_8443.lst`，替换后即成绝对路径）。
// 这样做同时把误报压到最低：注释里、别名里出现的文件名不会被当成引用。
//
// texts 是"要扫描的文本文件"（文件名 -> 内容）。含 NUL 的内容按二进制跳过。
func ScanCitedFiles(dir string, texts map[string][]byte) []CitedFile {
	prefixes := dirPrefixes(dir)
	if len(prefixes) == 0 {
		return nil
	}
	names := make([]string, 0, len(texts))
	for n := range texts {
		names = append(names, n)
	}
	sort.Strings(names)

	// 被引用的文件名 -> 首个引用它的配置文件名。
	// 用 map 去重：同一个文件被多处引用时，报告里出现一次就够。
	seen := map[string]string{}
	for _, cfg := range names {
		raw := texts[cfg]
		if bytes.IndexByte(raw, 0) >= 0 {
			continue // 二进制内容不扫，避免噪声
		}
		s := string(raw)
		for _, pre := range prefixes {
			idx := 0
			for {
				i := strings.Index(s[idx:], pre)
				if i < 0 {
					break
				}
				start := idx + i + len(pre)
				name := refToken(s, start)
				idx = start // 单调前进（len(pre) > 0），不会死循环
				if name == "" {
					continue
				}
				if _, ok := seen[name]; !ok {
					seen[name] = cfg
				}
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]CitedFile, 0, len(seen))
	for n, by := range seen {
		out = append(out, CitedFile{Name: n, ByName: by})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// refToken 从 from 处取出一个"路径片段"，遇到配置语法里的定界符就停。
//
// 定界符覆盖了 HAProxy 配置里会出现的情形：空白、引号、逗号、分号、
// 多种括号、注释符，以及反斜杠（Windows 路径分隔符不该出现在路径尾部）。
func refToken(s string, from int) string {
	if from >= len(s) {
		return ""
	}
	for i := from; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r', '\n', '"', '\'', ',', ';', ')', '(', '[', ']', '{', '}', '#', '\\':
			return s[from:i]
		}
	}
	return s[from:]
}

// dirPrefixes 给出"一个绝对路径引用本目录下的文件"时可能出现的若干前缀写法。
//
// 同时给 native 分隔符与 slash 两种：写入时用的是 slash（见 writeVersionDir），
// 但人为编辑过的配置可能用 native 分隔符，两种都要认。
func dirPrefixes(dir string) []string {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	abs := dir
	if a, err := filepath.Abs(dir); err == nil {
		abs = a
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range []string{
		filepath.ToSlash(abs) + "/",
		abs + string(filepath.Separator),
	} {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// ScanVersionDir 扫一个版本目录相对其清单的一致性。
//
// 返回：
//
//	extra —— 目录里存在、但清单没列的普通文件（已排序）。
//	         调用方**不应**因此阻断发布/回滚（评审决定），而应作为 warning 上报；
//	         唯一例外见返回的 cited。
//	cited —— 被清单内配置以绝对路径引用的、不在清单内的文件（已排序）。**必须阻断**。
//
// 供"以文件为准"的数据面（HAProxy）用；纯 Go 数据面没有 current/ 与版本目录，
// 不适用这条判据（调用方需明确记为"不适用"，而不是静默当作通过）。
func ScanVersionDir(dir string, manifestFiles []string) (extra []string, cited []CitedFile, err error) {
	entries, derr := os.ReadDir(dir)
	if derr != nil {
		return nil, nil, derr
	}
	allowed := map[string]bool{ManifestFileName: true}
	for _, n := range manifestFiles {
		if n == "" || filepath.Base(n) != n {
			return nil, nil, fmt.Errorf("版本清单里的文件名不合法: %q", n)
		}
		allowed[n] = true
	}
	texts := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		if !allowed[e.Name()] {
			extra = append(extra, e.Name())
			continue // 清单外的文件是被报告的对象，不当作"引用方"去扫描
		}
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			return nil, nil, rerr
		}
		texts[e.Name()] = b
	}
	sort.Strings(extra)
	var outside []CitedFile
	for _, c := range ScanCitedFiles(dir, texts) {
		if !allowed[c.Name] {
			outside = append(outside, c)
		}
	}
	return extra, outside, nil
}

// DescribeCited 把引用记录拼成一句人话，用于报错与界面展示。
func DescribeCited(cs []CitedFile) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, fmt.Sprintf("%s（被 %s 引用）", c.Name, c.ByName))
	}
	return strings.Join(parts, "、")
}
