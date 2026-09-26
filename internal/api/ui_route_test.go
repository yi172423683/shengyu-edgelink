package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRootServesUI 根路径提供内置管理台。
func TestRootServesUI(t *testing.T) {
	mux := (&Server{}).routes()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / 返回 %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "盛愈边缘网关") {
		t.Errorf("根路径未返回管理台页面")
	}
}

// TestAlternateUIPath /ui 同样可达（有些反代会把管理台挂在子路径下）。
func TestAlternateUIPath(t *testing.T) {
	mux := (&Server{}).routes()
	for _, p := range []string{"/ui", "/ui/"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s 返回 %d, want 200", p, rec.Code)
		}
	}
}

// TestUnknownPathDoesNotFallBackToUI 未注册路径必须 404，不能回退成页面。
//
// 为什么专门测这一条：路由用的是 "/{$}"（精确匹配根路径）而不是 "/"（前缀通配）。
// 用 "/" 的话，任何未匹配的 GET 都会拿到一页 HTML —— 于是"接口路径写错"会表现为
// 200 + 一页网页，而不是 404，这类问题极难定位。这条测试把该行为钉死。
func TestUnknownPathDoesNotFallBackToUI(t *testing.T) {
	mux := (&Server{}).routes()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nope-not-real", nil))
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "盛愈边缘网关") {
		t.Errorf("未注册路径回退到了管理台页面（应为 404 或 JSON 错误）")
	}
}

// TestHealthEndpointUnaffected 健康检查仍走 /api/health，且不因新增 UI 路由而受影响。
func TestHealthEndpointUnaffected(t *testing.T) {
	// 必须注入时钟：/api/health 的 handler 会调 s.now()，零值 Server 里它是 nil，
	// 直接 new 一个 &Server{} 去打这个接口会 panic。
	s := &Server{}
	s.SetClock(time.Now)
	mux := s.routes()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/health 返回 %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("/api/health 响应异常：%s", rec.Body.String())
	}
}

// TestMetaEndpointRequiresAuth /api/meta 必须挂在鉴权之后。
//
// 它返回版本号、监听地址、节点公网 IP 与"是否已初始化"——
// 未登录就能拿到等于把部署信息泄露出去，所以这条路由必须被 auth 包住。
func TestMetaEndpointRequiresAuth(t *testing.T) {
	mux := (&Server{}).routes()
	for _, p := range []string{"/api/meta", "/api/nodes/nd_x/port-check"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, p, nil)
		if strings.HasPrefix(p, "/api/nodes") {
			req = httptest.NewRequest(http.MethodPost, p, strings.NewReader("{}"))
		}
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s 未登录时返回 %d, want 401", p, rec.Code)
		}
	}
}

// TestProductNameIsChinese 产品中文名必须一致（界面抬头与接口同源）。
func TestProductNameIsChinese(t *testing.T) {
	if ProductName != "盛愈边缘网关" {
		t.Errorf("ProductName = %q, want 盛愈边缘网关", ProductName)
	}
}
