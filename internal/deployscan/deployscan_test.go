package deployscan

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot 返回仓库根；不在源码树里运行（例如只装了二进制）时跳过而不是失败。
func repoRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skipf("未找到 go.mod，跳过（当前不在源码树中运行）：%v", err)
	}
	return root
}

func readIfExists(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("读取 %s 失败，跳过：%v", path, err)
		return ""
	}
	return string(b)
}

// deployScripts 返回需要做"脚本级回归检查"的部署脚本。
//
// 为什么把 panel-https.sh 也纳进来：它是**唯一会去动 nginx 与 TLS 证书**的地方，
// 而这两样东西错了的后果都不在自己身上 —— 证书续期失败要等证书过期才发现，
// nginx 配置写错会让这台机器上**别的**站点一起 502。这类脚本比安装脚本更需要被钉住。
func deployScripts(root string) []string {
	return []string{
		filepath.Join(root, "deploy", "install.sh"),
		filepath.Join(root, "deploy", "uninstall.sh"),
		filepath.Join(root, "deploy", "panel-https.sh"),
	}
}

// TestInstallScriptFileReferencesExist 钉住"脚本引用的 deploy/ 文件必须真的存在"。
//
// 真机故障原文：`install: cannot stat 'deploy/tmpfiles-shengyu.conf'` ——
// 文件早被改名，脚本里的字符串却没跟着改。这类引用错误在编译期完全不可见。
func TestInstallScriptFileReferencesExist(t *testing.T) {
	root := repoRoot(t)
	scripts := deployScripts(root)
	re := regexp.MustCompile(`deploy/([A-Za-z0-9._-]+)`)
	for _, s := range scripts {
		body := readIfExists(t, s)
		if body == "" {
			continue
		}
		seen := map[string]bool{}
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			name := m[1]
			if seen[name] {
				continue
			}
			seen[name] = true
			if _, err := os.Stat(filepath.Join(root, "deploy", name)); err != nil {
				t.Errorf("%s 引用了 deploy/%s，但该文件不存在（改名后引用没同步？）",
					filepath.Base(s), name)
			}
		}
	}
}

// TestSystemctlUsesFullUnitNames 钉住"systemctl 必须引用完整的单元名"。
//
// 真机故障原文：`systemctl enable --now shengyu-edgelink-haproxy shengyu-edgelink`
// —— 第二个单元名被改名规则截断成了二进制名，于是 systemd 去启一个不存在的单元。
// RE2 不支持前瞻，所以用"看下一个字符"的方式判断是不是被截断的名字。
func TestSystemctlUsesFullUnitNames(t *testing.T) {
	root := repoRoot(t)
	scripts := deployScripts(root)
	const prefix = "shengyu-edgelink"
	for _, s := range scripts {
		body := readIfExists(t, s)
		if body == "" {
			continue
		}
		for _, line := range strings.Split(body, "\n") {
			if !strings.Contains(line, "systemctl") {
				continue
			}
			idx := 0
			for {
				at := strings.Index(line[idx:], prefix)
				if at < 0 {
					break
				}
				pos := idx + at + len(prefix)
				idx = pos
				if pos >= len(line) {
					t.Errorf("%s: systemctl 行以被截断的单元名结尾：%q",
						filepath.Base(s), strings.TrimSpace(line))
					break
				}
				next := line[pos]
				// 合法的两种完整名：shengyu-edgelink-server[.service] / shengyu-edgelink-haproxy[.service]
				if next != '-' && next != '.' {
					t.Errorf("%s: systemctl 引用了不完整的单元名（%s）、应为 shengyu-edgelink-server 或 shengyu-edgelink-haproxy：%q",
						filepath.Base(s), prefix, strings.TrimSpace(line))
				}
				break
			}
		}
	}
}

// TestUnitFilesMatchScriptNames 钉住"deploy/ 里的单元文件名与脚本启用的单元名一致"。
//
// 单元文件叫什么、脚本 enable 什么，这两处一旦不一致，表现出来的现象是
// "装完了但服务起不来"，而 systemctl 只会说 unit not found，很难联想到改名。
func TestUnitFilesMatchScriptNames(t *testing.T) {
	root := repoRoot(t)
	for _, u := range []string{"shengyu-edgelink-server.service", "shengyu-edgelink-haproxy.service"} {
		if _, err := os.Stat(filepath.Join(root, "deploy", u)); err != nil {
			t.Errorf("deploy/%s 不存在：脚本要安装它却找不到", u)
		}
	}
	body := readIfExists(t, filepath.Join(root, "deploy", "install.sh"))
	if body == "" {
		return
	}
	for _, want := range []string{
		"install -m 0644 deploy/shengyu-edgelink-server.service /etc/systemd/system/shengyu-edgelink-server.service",
		"install -m 0644 deploy/shengyu-edgelink-haproxy.service /etc/systemd/system/shengyu-edgelink-haproxy.service",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("install.sh 缺少对单元文件的安装语句：%q", want)
		}
	}
}

// TestNoSigpipePronePipelines 钉住"不要在 set -euo pipefail 下写 `... | head -c N`"。
//
// 真机故障原文：安装脚本停在第 8 步，没有任何报错，服务没起来。
// 原因是口令生成写的
//
//	ADMIN_PASS="$(LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 32)"
//
// head 读够 32 字节就退出并关掉管道，tr 收到 SIGPIPE(141)；
// 在 `set -euo pipefail` 下整条命令的退出码变成 141，set -e 直接终止脚本。
// 这类故障在 shell 里完全静默，只能用"代码里不许出现这个组合"来防。
func TestNoSigpipePronePipelines(t *testing.T) {
	root := repoRoot(t)
	for _, s := range deployScripts(root) {
		name := filepath.Base(s)
		body := readIfExists(t, s)
		if body == "" {
			continue
		}
		usesPipefail := strings.Contains(body, "pipefail")
		for _, line := range strings.Split(body, "\n") {
			if !strings.Contains(line, "| head -c") {
				continue
			}
			if usesPipefail && !strings.Contains(line, "set +o pipefail") {
				t.Errorf("deploy/%s 里有 SIGPIPE 隐患：\n\t%s\n"+
					"在 set -o pipefail 下此命令会以 141 终止脚本；请写成 (set +o pipefail; ... | head -c N)",
					name, strings.TrimSpace(line))
			}
		}
	}
}

// TestBaselineWriteIsGuarded 钉住"写基线必须先看有没有已发布配置"。
//
// 真机故障现象：升级时重跑 install.sh，脚本每一行都成功，
// 但升级完**转发全没了、8443 不再监听** —— 因为第 7 步无条件用基线
// 覆盖了 current/ 里正在服务的已发布配置（VERSION 从 1 被打回 0）。
//
// 这类"脚本显示全绿、业务却断了"的问题最难排查，只能从源头禁止：
// 写基线前必须先判断 current/haproxy.cfg 是否已存在。
func TestBaselineWriteIsGuarded(t *testing.T) {
	root := repoRoot(t)
	body := readIfExists(t, filepath.Join(root, "deploy", "install.sh"))
	if body == "" {
		return
	}
	if !strings.Contains(body, "-write-baseline") {
		t.Skip("安装脚本不再写基线，无需检查")
	}
	at := strings.Index(body, "-write-baseline")
	before := body[:at]
	if !strings.Contains(before, `if [ -f "$CONF_ROOT/current/haproxy.cfg" ]`) {
		t.Errorf("install.sh 里的 -write-baseline 没有被「已有配置」判断保护；" +
			"重复安装会覆盖正在服务的已发布配置（真机已复现过一次转发中断）")
	}
}
