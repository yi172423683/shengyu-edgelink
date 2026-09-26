package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandlerServesRequiredInputs 保证管理台页面确实提供了需求要求的全部输入项。
//
// 这个测试存在的理由：页面是 go:embed 进二进制的，"页面内容对不对"在编译期查不出来，
// 而需求明确要求界面必须给出这些填项。少了任何一项都应当在 CI 里炸掉，
// 而不是等用户在生产上发现"没有地方填源站端口"。
func TestHandlerServesRequiredInputs(t *testing.T) {
	h := Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / 返回 %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	for _, must := range []string{
		"管理端口",   // 需求4：管理端口
		"监听地址",   // 需求4：监听地址
		"监听端口",   // 需求4：监听端口
		"域名",     // 需求4：域名
		"源站地址",   // 需求4：源站地址
		"源站端口",   // 需求4：源站端口
		"健康检查方式", // 需求4：健康检查方式
	} {
		if !strings.Contains(body, must) {
			t.Errorf("管理台页面缺少要求的输入项：%q", must)
		}
	}
	// 端口不能被写成"只能 443"：页面必须说明 443 只是默认推荐值。
	if !strings.Contains(body, "443 只是默认推荐值") {
		t.Errorf("页面未说明「443 只是默认推荐值，不是唯一入口」")
	}
}

func TestFSContainsIndex(t *testing.T) {
	f, err := FS().Open("index.html")
	if err != nil {
		t.Fatalf("嵌入的 index.html 打不开：%v", err)
	}
	_ = f.Close()
}

// TestHandlerServesWizard 钉住"首次进入走向导"这条产品流程。
//
// 需求要求：系统还没有任何已发布业务时，登录后**直接进向导**，而不是先给一个空白后台。
// 页面是 go:embed 进二进制的，向导被误删在编译期看不出来，只能靠这条测试守住。
func TestHandlerServesWizard(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	for _, must := range []string{
		"步骤 1 / 5", // 欢迎页
		"步骤 2 / 5", // 数据面入口
		"步骤 3 / 5", // 业务
		"步骤 4 / 5", // 检查
		"步骤 5 / 5", // 发布
		"DNS 应指向的 IP",
		"实际访问地址",
		"保存并发布",
	} {
		if !strings.Contains(body, must) {
			t.Errorf("向导页面缺少：%q", must)
		}
	}
}

// TestHandlerDoesNotPretendSSLIsImplemented 钉住"不把未实现的 TLS 终止说成可用"。
//
// 这是本轮最需要防的一类回归：一旦有人在界面上把"TLS 终止 / 自动申请证书"
// 渲染成可选，用户会真的去选，然后拿到一个看不懂的失败。
// 页面必须同时出现"透传可用"与"终止尚未实现"两种表述。
func TestHandlerDoesNotPretendSSLIsImplemented(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "证书由源站自己持有") {
		t.Errorf("页面未说明「TLS 透传下证书由源站持有」")
	}
	if !strings.Contains(body, "尚未实现") {
		t.Errorf("页面未标注 TLS 终止「尚未实现」")
	}
	if !strings.Contains(body, "不会申请") {
		t.Errorf("页面未明确说明平台不会申请证书")
	}
	// TLS 终止选项必须是 disabled，而不是一个可选项
	if !strings.Contains(body, `value="tls_terminate" disabled`) {
		t.Errorf("TLS 终止必须渲染成不可选（disabled），实际页面里未找到")
	}
}

// TestHandlerShowsFailureAndRollbackFields 钉住失败展示的三要素。
func TestHandlerShowsFailureAndRollbackFields(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	for _, must := range []string{
		"失败阶段",
		"当前运行版本",
		"是否已回滚",
		"人工处理",
	} {
		if !strings.Contains(body, must) {
			t.Errorf("失败展示缺少字段：%q", must)
		}
	}
}

// TestHandlerDistinguishesRestoredFromVerified 钉住「回滚」的五种结论必须分开措辞。
//
// 这是本轮修复的界面侧要求：曾经界面把「改动没生效、无需回滚」和
// 「文件恢复了但没验证」都渲染成「已自动回滚并验证通过」，
// 运维据此收工，而线上到底在跑什么其实没人确认过。
// 页面字面量在编译期查不出来，只能用这条测试守住。
func TestHandlerDistinguishesRestoredFromVerified(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	for _, must := range []string{
		"已自动回滚并验证通过",            // verified
		"改动未生效，原版本仍在正常运行（无需回滚）", // not_needed
		"已恢复但未验证",               // restored_not_verified
		"回滚验证失败",                // verify_failed
		"未成功回滚",                 // failed
		"回滚验证",                  // 验证到底跑没跑
		"配置正文是否已恢复",
		"结论代码",
		"回滚判据",
	} {
		if !strings.Contains(body, must) {
			t.Errorf("失败面板缺少回滚状态表述：%q", must)
		}
	}
	// 这五个结论必须由服务端字段驱动，界面不许自己推断。
	for _, must := range []string{"rollback_outcome", "rollback_verified", "rollback_verify_ran", "rollback_restored", "rollback_code"} {
		if !strings.Contains(body, must) {
			t.Errorf("界面必须读服务端给出的 %q，而不是自行推断", must)
		}
	}
}

// TestHandlerReadsNestedListenerAddress 钉住发布成功气泡读的是**嵌套**地址。
//
// 真机缺陷（docs/08 §9.3）：publish 返回的 `verify.listeners` 是 ListenerStatus，
// 地址在嵌套的 expected 里；而 /preview 返回的 expected_listeners 是扁平的。
// 界面当初把两者当同一种形状，于是成功气泡显示成"监听：undefined:undefined"。
func TestHandlerReadsNestedListenerAddress(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "l.expected.addr") {
		t.Errorf("发布结果里的监听必须按 l.expected.addr 读（嵌套结构）")
	}
	// /preview 那一侧是扁平结构，必须保持 l.addr
	if !strings.Contains(body, "expected_listeners") || !strings.Contains(body, "l.addr + \":\" + l.port") {
		t.Errorf("预检结果的监听是扁平结构，应保持 l.addr + \":\" + l.port")
	}
	// 不允许再出现"拿 ListenerStatus 当扁平结构读"的写法
	if strings.Contains(body, ".listeners || []).map(l => l.addr") {
		t.Errorf("发布结果的监听不该按扁平结构读（会显示 undefined:undefined）")
	}
}

// TestHandlerShowsWarningsOutsideManifest 钉住"清单外多余文件"在界面上的呈现。
//
// 这条 warning 的语义是"这次成了，但有一件事你必须知道"：它**不影响**发布/回滚结论
// （评审确定的边界，见 docs/08 §10）。但如果只放在返回体里、界面不显示，
// 运维就永远不知道某个版本目录被人手工改过 —— 而那正是下次回滚出问题时的伏笔。
func TestHandlerShowsWarningsOutsideManifest(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	for _, must := range []string{
		"warningsBlock", // 成功页与失败页共用同一个渲染器（措辞不许分叉）
		"存在未纳入清单的额外文件",  // 用户能看懂的那句话
		"r.warnings",    // 失败面板读服务端字段
		"d.warnings",    // 发布成功路径读服务端字段
	} {
		if !strings.Contains(body, must) {
			t.Errorf("界面缺少清单外多余文件的提示要素：%q", must)
		}
	}
}

// TestHandlerOffersEntryDeletion 钉住"删除共享入口"在界面上的呈现。
//
// 为什么值得钉：接口有了而界面上没有入口，运维就会去用 curl ——
// 而 curl 会绕过界面给出的两句关键提示（"有业务在用会被拒绝"、"删完要发布"），
// 那恰好是最容易出事的两个点：前者会留下悬空引用，后者会让界面与节点上的东西不一致。
func TestHandlerOffersEntryDeletion(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	for _, must := range []string{
		"delSNI",        // 删除动作存在
		"data-sni",      // 参数走 data-*（不把 id 拼进内联 onclick）
		"/sni-entries/", // 调的是那个删除接口
		"删除后需要执行一次「发布」", // 必须提示"删完要发布"
	} {
		if !strings.Contains(body, must) {
			t.Errorf("界面缺少入口删除的要素：%q", must)
		}
	}
}
