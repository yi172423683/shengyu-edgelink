package deployscan

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 本文件钉住"公网访问"这一整块能力的脚本级约束。
//
// 为什么 shell 脚本也需要回归测试：这块逻辑会去装 nginx、签证书、写 systemd 单元，
// 而**这三样东西出错的后果都不在自己身上** ——
// 证书续期失败要等证书过期才被发现；nginx 配置写错会让这台机器上**别的**站点一起 502；
// 定时器指向不存在的脚本则安静地永远不跑。
// Go 代码有编译器兜底，shell 只能靠断言把"不许出现的东西"钉住。

// panelScript 返回 panel-https.sh 的内容；不存在则跳过。
func panelScript(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	return readIfExists(t, filepath.Join(root, "deploy", "panel-https.sh"))
}

// TestPanelScriptIsInvokedByInstall 钉住"安装脚本必须真的调用面板模块"。
//
// 需求是「用户只执行 sudo bash install.sh」就能拿到完整的公网 HTTPS 入口。
// 一旦 install.sh 不再调用它（改名、漏改路径、重构时误删），
// 现象是"安装全程成功、但公网地址打不开"，而且没有任何报错。
func TestPanelScriptIsInvokedByInstall(t *testing.T) {
	root := repoRoot(t)
	install := readIfExists(t, filepath.Join(root, "deploy", "install.sh"))
	if install == "" {
		return
	}
	if !strings.Contains(install, "panel-https.sh") {
		t.Fatal("install.sh 没有引用 panel-https.sh —— 公网访问能力不会被安装")
	}
	if !strings.Contains(install, `bash "$PANEL_SCRIPT"`) {
		t.Error("install.sh 应当通过 $SELF_DIR 拼出的路径调用面板模块（保证发布包内相对位置正确）")
	}
	if !strings.Contains(install, "PANEL_ENABLED") {
		t.Error("install.sh 缺少「是否开通公网访问」的判定变量 PANEL_ENABLED")
	}
}

// TestPanelNeverListensOnReservedPorts 钉住"面板绝不监听 80/443/8443"。
//
// 三条都是有真实代价的：
//
//	· 80：本机 acme.sh 用 standalone(HTTP-01) 续期时要**临时抢占** 80。
//	  nginx 常驻 80 会让别人的证书续期静默失败 —— 这通常要等证书过期、
//	  浏览器报错才发现，中间没有任何告警。
//	· 443：在很多机器上属于既有服务（反代、伪装站点）。动了它等于改别人的业务。
//	· 8443：本平台自己的转发入口默认端口，撞上就是自己打自己。
//
// 只检查**行首的 listen 指令**（允许前导空白），因为注释里必然会提到这些端口号
// （例如解释"为什么不占 80"），把注释也算进来会导致测试永远失败。
func TestPanelNeverListensOnReservedPorts(t *testing.T) {
	body := panelScript(t)
	if body == "" {
		return
	}
	re := regexp.MustCompile(`(?m)^[ \t]*listen[ \t]+(80|443|8443)\b`)
	if m := re.FindString(body); m != "" {
		t.Errorf("panel-https.sh 里出现了受保护端口的 listen 指令：%q\n"+
			"80 要留给本机 acme 续期、443 常属既有服务、8443 是本平台转发入口。", m)
	}
	// 参数层面也要拒绝：光靠"我们没写 listen 80"不够，
	// 用户完全可能用 --port 80 让模板渲染出 listen 80。
	if !strings.Contains(body, "80 | 443)") {
		t.Error("panel-https.sh 必须显式拒绝 --port 80/443（不能只靠模板里不写）")
	}
}

// TestPanelDropsDistroDefaultSite 钉住"装完 nginx 必须撤掉发行版默认站点"。
//
// Debian 系的 /etc/nginx/sites-enabled/default 内容含 `listen 80 default_server`。
// 装完 nginx 不动它，就等于违反了上一条 —— 而且违反得很隐蔽：
// 面板本身跑在 9443 上一切正常，坏掉的是**别人的** HTTP-01 续期。
func TestPanelDropsDistroDefaultSite(t *testing.T) {
	body := panelScript(t)
	if body == "" {
		return
	}
	if !strings.Contains(body, "/etc/nginx/sites-enabled/default") {
		t.Fatal("panel-https.sh 没有处理发行版默认站点（它会占用 80）")
	}
	if !strings.Contains(body, "rm -f /etc/nginx/sites-enabled/default") {
		t.Error("必须真的把默认站点从 sites-enabled 移除，而不只是检测它")
	}
	// 只删软链、备份内容：删掉原件会让"想恢复发行版默认行为"变得不可能。
	if !strings.Contains(body, `default.site`) {
		t.Error("移除前应先备份默认站点内容（回滚与手工恢复都要用）")
	}
}

// TestPanelRenewalLoopIsClosed 钉住"自动续期的闭环"。
//
// 续期要真正生效，需要三件事**同时**成立，缺任何一件都会变成
// "证书续了但 nginx 还在用旧的"（无报错）或"根本没人续"（等过期才发现）：
//
//	① 部署证书时写 --reloadcmd，让续期成功后 nginx 重新加载；
//	② 有一个定时任务真的会去跑 acme.sh --cron；
//	③ 该定时任务被设为开机启动并立即生效。
func TestPanelRenewalLoopIsClosed(t *testing.T) {
	body := panelScript(t)
	if body == "" {
		return
	}
	if !strings.Contains(body, "--install-cert") {
		t.Fatal("panel-https.sh 必须用 --install-cert 部署证书（它同时负责登记续期后的 reload）")
	}
	if !strings.Contains(body, "--reloadcmd") {
		t.Fatal("缺少 --reloadcmd：续期成功后 nginx 不会加载新证书（属于静默失效）")
	}
	if !strings.Contains(body, "systemctl reload nginx") {
		t.Error("--reloadcmd 的内容应当是 systemctl reload nginx")
	}
	if !strings.Contains(body, "acme.sh --cron") && !strings.Contains(body, "--cron") {
		t.Error("续期任务必须调用 acme.sh 的 --cron（否则只是装了个空定时器）")
	}
	if !strings.Contains(body, ".timer") {
		t.Error("缺少 systemd timer：需求要求续期任务可被 systemctl 管理")
	}
	if !strings.Contains(body, `systemctl enable --now "$RENEW_TIMER"`) {
		t.Error("续期定时器必须 enable --now（只写单元文件不启用，等于没装）")
	}
}

// TestPanelCertDeployedOutsideRootHome 钉住"证书必须复制出 /root"。
//
// acme.sh 把证书放在 /root/.acme.sh/ 下，而 /root 权限是 0700，
// nginx 的 worker（www-data/nginx，非 root）读不到。
// 现象是 nginx 启动报 `cannot load certificate ... Permission denied`，
// 而文件确实"存在" —— 很容易被误判成"证书没签成功"去重签。
func TestPanelCertDeployedOutsideRootHome(t *testing.T) {
	body := panelScript(t)
	if body == "" {
		return
	}
	// nginx 配置里的证书路径必须指向部署目录，而不是 acme.sh 的家目录。
	re := regexp.MustCompile(`(?m)^[ \t]*ssl_certificate[ \t]+\$TLS_DIR/fullchain\.pem`)
	if !re.MatchString(body) {
		t.Error("nginx 配置的 ssl_certificate 应指向 $TLS_DIR（nginx 可读），而不是 acme.sh 的目录")
	}
	if regexp.MustCompile(`ssl_certificate[\s\S]{0,40}\.acme\.sh`).MatchString(body) {
		t.Error("不要让 nginx 直接读 /root/.acme.sh 下的证书：/root 是 0700，worker 读不到")
	}
	if !strings.Contains(body, "--key-file") || !strings.Contains(body, "--fullchain-file") {
		t.Error("必须把证书复制到指定路径（--key-file / --fullchain-file）")
	}
}

// TestPanelBasicPasswordNotFromArgv 钉住"口令只能从环境变量或交互输入取"。
//
// 命令行参数会出现在 shell history 与 ps 输出里 —— 同机的其他用户
// （甚至只是 `ps aux` 的无心一瞥）就能看到面板的 Basic 口令。
// 这条与"DNS 凭据走环境变量"是同一个理由，但更严重：那是 API Key，这是登录口令。
func TestPanelBasicPasswordNotFromArgv(t *testing.T) {
	body := panelScript(t)
	if body == "" {
		return
	}
	if strings.Contains(body, "--basic-pass)") || strings.Contains(body, "--basic-pass=") {
		t.Error("不得提供 --basic-pass 命令行参数：会进 shell history 与 ps")
	}
	if !strings.Contains(body, "SHENGYU_PANEL_BASIC_PASS") {
		t.Error("Basic 口令应从环境变量 SHENGYU_PANEL_BASIC_PASS 读取")
	}
	if !strings.Contains(body, "read -r -s") {
		t.Error("交互式读取口令必须用 read -s（不回显）")
	}
}

// TestPanelRollsBackOnFailure 钉住"失败必须回滚 nginx 配置 / 证书配置 / systemd 修改"。
//
// 需求原文里这一条是硬要求。为什么必须自动做：
// 失败现场往往只剩半份配置（nginx 指向一个不存在的证书、timer 指向不存在的脚本），
// 这些残留**不会立刻报错**，只会在下次 reload 或重启时才炸，
// 而那时已经没人记得"这是上次装了一半留下的"。
func TestPanelRollsBackOnFailure(t *testing.T) {
	body := panelScript(t)
	if body == "" {
		return
	}
	if !strings.Contains(body, "rollback_all") {
		t.Fatal("panel-https.sh 缺少失败回滚逻辑（rollback_all）")
	}
	if !strings.Contains(body, "trap") || !strings.Contains(body, "ERR") {
		t.Error("回滚必须由 ERR trap 触发，才能覆盖「任何一步失败」")
	}
	// 三类东西都要撤：systemd 单元、nginx 配置、本次新签的证书。
	for _, must := range []string{
		"systemctl disable --now", // 撤 systemd 单元
		"--remove",                // 撤本次签发的证书
		"nginx -t",                // 回滚后先验证配置合法再 reload
	} {
		if !strings.Contains(body, must) {
			t.Errorf("回滚逻辑缺少：%q", must)
		}
	}
	// 回滚不能去动**别人**的证书：复用机器上已有证书时删掉它，
	// 会把同机的其它服务一起弄坏（泛域名证书尤其常见）。
	if !strings.Contains(body, "CERT_CREATED_HERE") {
		t.Error("必须区分「本次新签的证书」与「复用的已有证书」，回滚只撤前者")
	}
}

// TestPanelDoesNotTouchForeignServices 钉住"绝不动别人的服务"。
//
// 这台机器上跑着别的东西是很常见的（反代、代理节点、其它面板）。
// 一个"装个面板"的动作把别人的进程杀了，是最不可接受的一类越界。
func TestPanelDoesNotTouchForeignServices(t *testing.T) {
	for _, name := range []string{"panel-https.sh", "install.sh"} {
		root := repoRoot(t)
		body := readIfExists(t, filepath.Join(root, "deploy", name))
		if body == "" {
			continue
		}
		// 逐行检查并**跳过注释行**：脚本里本来就有"本脚本不会 pkill 任何进程"这类
		// 说明性注释，把它算成违规会让测试变成噪声，而噪声最终会被忽略 ——
		// 那比没有这条测试更糟。
		for _, ln := range strings.Split(body, "\n") {
			code := strings.TrimSpace(ln)
			if code == "" || strings.HasPrefix(code, "#") {
				continue
			}
			for _, banned := range []string{"pkill", "killall", "stop x-ui", "stop xray", "stop nginx"} {
				if strings.Contains(code, banned) {
					t.Errorf("deploy/%s 里出现 %q —— 不允许按名字杀进程、也不允许停掉别人的服务：\n\t%s",
						name, banned, code)
				}
			}
		}
	}
}

// TestUninstallRemovesPanelConfig 钉住"卸载必须能撤掉公网入口"。
//
// 开通/回退必须成对。留下一个指向已停服务的 nginx 站点，
// 会让 9443 上一直有个"连得上但永远 502"的入口 ——
// 而运维会以为服务还在。
func TestUninstallRemovesPanelConfig(t *testing.T) {
	root := repoRoot(t)
	body := readIfExists(t, filepath.Join(root, "deploy", "uninstall.sh"))
	if body == "" {
		return
	}
	for _, must := range []string{
		"/etc/nginx/sites-enabled/shengyu-panel",
		"shengyu-panel-ratelimit.conf",
		"nginx -t",
	} {
		if !strings.Contains(body, must) {
			t.Errorf("uninstall.sh 未清理面板配置：%q", must)
		}
	}
	if !strings.Contains(body, "shengyu-panel-acme-renew") {
		t.Error("uninstall.sh 未清理续期定时器（会留下一个指向不存在脚本的 timer）")
	}
}

// TestInstallWizardAsksRequiredInputs 钉住"安装向导必须问全这些项"。
//
// 需求把向导要问的项一条条列了出来。这里面每一项漏掉都会有明确后果：
//
//	· 内网地址/端口漏问 → 用户只能接受默认值，而默认值不一定合他的网络规划；
//	· 公网域名/端口漏问 → 无法生成正确的入口；
//	· ACME 邮箱漏问 → 证书出问题时唯一的提前通知渠道没了；
//	· 证书验证方式漏问 → 用户无法在"不占端口(DNS)"与"不用配凭据(HTTP)"之间取舍；
//	· 管理员初始密码漏问 → 只能被动接受自动生成的口令。
func TestInstallWizardAsksRequiredInputs(t *testing.T) {
	root := repoRoot(t)
	body := readIfExists(t, filepath.Join(root, "deploy", "install.sh"))
	if body == "" {
		return
	}
	for _, prompt := range []string{
		"内网监听地址",
		"内网端口",
		"公网访问域名",
		"公网 HTTPS 端口",
		"ACME 账号邮箱",
		"证书验证方式",
		"管理员初始密码",
	} {
		if !strings.Contains(body, prompt) {
			t.Errorf("安装向导缺少对「%s」的询问", prompt)
		}
	}
}

// TestInstallOutputsAccessInfo 钉住"结尾必须给出可用的访问信息"。
//
// 需求要求最后输出：URL、管理员用户名、SSH 备用访问方式。
// 少任何一项，用户装完之后都要自己摸索"我该打开哪个地址、用哪个账号"。
func TestInstallOutputsAccessInfo(t *testing.T) {
	root := repoRoot(t)
	body := readIfExists(t, filepath.Join(root, "deploy", "install.sh"))
	if body == "" {
		return
	}
	for _, must := range []string{
		"管理员用户名",   // 明确写出用户名（而不是只说"默认账号"）
		"ssh -L",   // SSH 隧道备用访问方式
		"https://", // 公网地址
	} {
		if !strings.Contains(body, must) {
			t.Errorf("安装结尾的输出缺少：%q", must)
		}
	}
}
