// Package api 提供管理后台 HTTP 接口与节点 Agent 接口。
//
// 安全模型（对应需求 §九）：
//
//	管理面                                  节点面
//	────────                                ────────
//	HTTPS（由外层 nginx/Caddy 终结）          双向认证（注册令牌 → 节点密钥）
//	会话 Cookie（HttpOnly, SameSite=Strict）  X-Agent-Token
//	CSRF 头（写操作必须带）                   注册令牌一次性、可撤销、可轮换
//	可选来源 IP 白名单                        无法执行任意命令，只有结构化动作
//
// 两条面的**鉴权中间件不同**，且节点面接口全部挂在 /api/agent/ 下，
// 便于在外层只对管理面开内网、对节点面开公网（或反之）。
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shengyu/edgelink/internal/agent"
	"github.com/shengyu/edgelink/internal/auth"
	"github.com/shengyu/edgelink/internal/id"
	"github.com/shengyu/edgelink/internal/logstore"
	"github.com/shengyu/edgelink/internal/publish"
	"github.com/shengyu/edgelink/internal/store"
	"github.com/shengyu/edgelink/internal/ui"
	"github.com/shengyu/edgelink/internal/validate"
)

// Config 服务配置。
type Config struct {
	// SessionTTL 会话有效期。
	SessionTTL time.Duration
	// LoginRateLimit 单 IP 每分钟允许的登录尝试次数。
	LoginRateLimit int
	// SecureCookies 生产必须为 true（只在 HTTPS 下发送 Cookie）。
	SecureCookies bool
	// AllowOrigins 允许的 CORS 来源；为空表示不开启 CORS（同源部署）。
	AllowOrigins []string
	// Version 本构建的版本号（由 -ldflags 注入），向导欢迎页与总览页展示。
	Version string
	// ListenAddr 管理面实际监听地址（install.sh / -listen 决定）。
	//
	// 界面**只读展示**它。管理端口属于启动参数，改它必须改 systemd 单元并重启服务，
	// 让网页去改等于给一个非 root 的服务开了一条改自身单元的路 —— 那是提权。
	ListenAddr string
	// AllowLoopbackOrigin 允许把回环地址作为源站。
	//
	// 生产必须为 false。仅本地开发/端到端验收需要打开（源站与中转同机）。
	// 见 validate.Options.AllowLoopbackOrigin 的说明。
	AllowLoopbackOrigin bool
}

// validationOptions 返回本次部署生效的校验策略。
//
// 集中在一处生成，避免"业务接口用了宽松策略、发布接口用了严格策略"
// 这种不一致 —— 那种不一致会表现为"能存进去但发布不了"，很难排查。
func (s *Server) validationOptions() validate.Options {
	opt := validate.DefaultOptions()
	opt.AllowLoopbackOrigin = s.Config.AllowLoopbackOrigin
	return opt
}

// Server 管理 API。
type Server struct {
	Store    *store.Store
	Logs     *logstore.Store
	Pipeline *publish.Pipeline
	Config   Config

	// NodeID 本实例同机部署时作为"本机节点"的 ID（需求 §二.5 允许同机部署）。
	NodeID string
	// AgentVersion 本机 agent 版本；仅用于 /api/agent/capabilities 之类。
	AgentVersion string
	// DataplaneName 本机数据面名（生产恒为 haproxy）。
	DataplaneName string
	// ReloadLocalDataplane 同机部署时，"发布"需要真正reload本机数据面；
	// 由 cmd 层注入，避免 api 包直接依赖具体数据面实现。
	OnPublished func(nodeID string, version int)

	// Ingest / Batch 是"数据面日志摄入"这条链路的两个环节（解析入库 / 攒批）。
	//
	// 为什么总览页需要它们：这条链路的失败是**静默**的 ——
	// 数据面产出日志 → 攒批 → 落盘，任何一环丢弃都只累加一个计数器，
	// 而界面上"查不到日志"会被解释成"这段时间确实没有连接"。
	// 把丢弃计数摆出来，是"让静默失败变可见"的最小一步。
	// 为 nil 时（测试、或未启用本机日志摄入）总览页不显示这一项。
	Ingest *agent.Ingestor
	Batch  *agent.Batcher

	now   func() time.Time
	mu    sync.Mutex
	login map[string][]time.Time // IP → 最近登录尝试时间

	mux *http.ServeMux
}

// New 构造 API 服务。
func New(st *store.Store, logs *logstore.Store, pl *publish.Pipeline, cfg Config) *Server {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	if cfg.LoginRateLimit <= 0 {
		cfg.LoginRateLimit = 10
	}
	s := &Server{
		Store: st, Logs: logs, Pipeline: pl, Config: cfg,
		now: time.Now, login: map[string][]time.Time{},
	}
	s.mux = s.routes()
	return s
}

// Handler 返回可直接挂到 http.Server 的路由。
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) SetClock(f func() time.Time) {
	if f != nil {
		s.now = f
	}
}

// ============================== 基础工具 ==============================

// errBody 统一错误响应。
//
// 刻意把「给用户看的话」与「技术细节」分成两个字段：
// 前者可以直接显示在界面上，后者进日志/诊断包。
// 混在一起的结果通常是界面弹出一句带有文件路径和 SQL 的英文报错。
type errBody struct {
	Error   string `json:"error"`
	Code    string `json:"code,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Hint    string `json:"hint,omitempty"`
	HTTPCde int    `json:"-"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg, code, detail string) {
	writeJSON(w, status, errBody{Error: msg, Code: code, Detail: detail})
}

// mapStoreErr 把 store 的错误翻译成合适的 HTTP 状态码。
// 没有这一层，重复域名、端口冲突这类可预期冲突会全部变成 500，
// 运维看到 500 会以为是系统 bug 而不是自己的配置问题。
func mapStoreErr(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, what+"不存在", "not_found", err.Error())
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, err.Error(), "conflict", "")
	default:
		writeErr(w, http.StatusInternalServerError, "服务内部错误", "internal", err.Error())
	}
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("请求体不是合法 JSON 或包含未知字段: %w", err)
	}
	return nil
}

func clientIP(r *http.Request) string {
	// 只信任直连地址。X-Forwarded-For 是可伪造的，
	// 若用它做 IP 白名单判定，攻击者加一个头就绕过了。
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ============================== 路由 ==============================

func (s *Server) routes() *http.ServeMux {
	m := http.NewServeMux()

	// ---- 公共 ----
	m.HandleFunc("POST /api/login", s.handleLogin)
	m.HandleFunc("POST /api/logout", s.handleLogout)
	m.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "at": s.now()})
	})

	// ---- 内置管理台（go:embed 打进二进制，与 API 同源） ----
	//
	// 用 "/{$}" 精确匹配根路径：单独的 "/" 是前缀通配，会把所有未匹配的 GET
	// 都回退到页面（那是 SPA 的行为），这里刻意不要 —— 未注册的路径应当 404，
	// 这样"接口路径写错"才会立刻暴露，而不是拿到一页 HTML。
	m.Handle("GET /{$}", ui.Handler())
	m.Handle("GET /ui", ui.Handler())
	m.Handle("GET /ui/", ui.Handler())

	// ---- 需要登录 ----
	m.Handle("GET /api/me", s.auth(http.HandlerFunc(s.handleMe)))
	m.Handle("POST /api/me/password", s.auth(s.csrf(http.HandlerFunc(s.handleChangeMyPassword))))
	m.Handle("GET /api/users", s.auth(http.HandlerFunc(s.handleListUsers)))
	m.Handle("POST /api/users", s.auth(s.csrf(http.HandlerFunc(s.handleCreateUser))))
	m.Handle("POST /api/users/{id}/password", s.auth(s.csrf(http.HandlerFunc(s.handleResetUserPassword))))
	m.Handle("GET /api/meta", s.auth(http.HandlerFunc(s.handleMeta)))

	m.Handle("GET /api/overview", s.auth(http.HandlerFunc(s.handleOverview)))

	m.Handle("GET /api/customers", s.auth(http.HandlerFunc(s.handleListCustomers)))
	m.Handle("POST /api/customers", s.auth(s.csrf(http.HandlerFunc(s.handleCreateCustomer))))
	m.Handle("GET /api/customers/{id}", s.auth(http.HandlerFunc(s.handleGetCustomer)))
	m.Handle("PUT /api/customers/{id}", s.auth(s.csrf(http.HandlerFunc(s.handleUpdateCustomer))))
	m.Handle("DELETE /api/customers/{id}", s.auth(s.csrf(http.HandlerFunc(s.handleDeleteCustomer))))

	m.Handle("GET /api/nodes", s.auth(http.HandlerFunc(s.handleListNodes)))
	m.Handle("POST /api/nodes", s.auth(s.csrf(http.HandlerFunc(s.handleCreateNode))))
	m.Handle("GET /api/nodes/{id}", s.auth(http.HandlerFunc(s.handleGetNode)))
	m.Handle("PUT /api/nodes/{id}", s.auth(s.csrf(http.HandlerFunc(s.handleUpdateNode))))
	m.Handle("DELETE /api/nodes/{id}", s.auth(s.csrf(http.HandlerFunc(s.handleDeleteNode))))
	m.Handle("GET /api/nodes/{id}/sni-entries", s.auth(http.HandlerFunc(s.handleListSNIEntries)))
	m.Handle("POST /api/nodes/{id}/sni-entries", s.auth(s.csrf(http.HandlerFunc(s.handleCreateSNIEntry))))
	// 删除入口：有业务路由引用时返回 409 sni_entry_in_use（守卫在 store 层的事务里，
	// 见 store.DeleteSNIEntry 的说明 —— 缺这个接口时验收只能改库，而改库绕过了
	// 所有一致性检查并留下了悬空引用，见 docs/08 §8）。
	m.Handle("DELETE /api/nodes/{id}/sni-entries/{eid}", s.auth(s.csrf(http.HandlerFunc(s.handleDeleteSNIEntry))))
	// 创建入口前的端口预检（只查不写），供向导"先问一句这个端口能不能用"。
	m.Handle("POST /api/nodes/{id}/port-check", s.auth(s.csrf(http.HandlerFunc(s.handlePortCheck))))
	m.Handle("POST /api/nodes/{id}/tokens", s.auth(s.csrf(http.HandlerFunc(s.handleCreateNodeToken))))
	m.Handle("POST /api/nodes/{id}/publish", s.auth(s.csrf(http.HandlerFunc(s.handlePublish))))
	m.Handle("POST /api/nodes/{id}/rollback", s.auth(s.csrf(http.HandlerFunc(s.handleRollback))))
	m.Handle("GET /api/nodes/{id}/versions", s.auth(http.HandlerFunc(s.handleListVersions)))
	m.Handle("GET /api/nodes/{id}/releases", s.auth(http.HandlerFunc(s.handleListReleases)))
	m.Handle("GET /api/nodes/{id}/preview", s.auth(http.HandlerFunc(s.handlePreviewConfig)))
	m.Handle("GET /api/nodes/{id}/stats", s.auth(http.HandlerFunc(s.handleNodeStats)))

	m.Handle("GET /api/businesses", s.auth(http.HandlerFunc(s.handleListBusinesses)))
	m.Handle("POST /api/businesses", s.auth(s.csrf(http.HandlerFunc(s.handleCreateBusiness))))
	m.Handle("GET /api/businesses/{id}", s.auth(http.HandlerFunc(s.handleGetBusiness)))
	m.Handle("PUT /api/businesses/{id}", s.auth(s.csrf(http.HandlerFunc(s.handleUpdateBusiness))))
	m.Handle("DELETE /api/businesses/{id}", s.auth(s.csrf(http.HandlerFunc(s.handleDeleteBusiness))))
	m.Handle("GET /api/businesses/{id}/routes", s.auth(http.HandlerFunc(s.handleListRoutes)))
	m.Handle("GET /api/businesses/{id}/dns-instructions", s.auth(http.HandlerFunc(s.handleDNSInstructions)))

	m.Handle("GET /api/logs/conn", s.auth(http.HandlerFunc(s.handleQueryLogs)))
	m.Handle("GET /api/logs/fields", s.auth(http.HandlerFunc(s.handleLogFields)))
	m.Handle("GET /api/diagnose", s.auth(http.HandlerFunc(s.handleDiagnose)))
	m.Handle("GET /api/audit", s.auth(http.HandlerFunc(s.handleListAudit)))
	m.Handle("GET /api/retention", s.auth(http.HandlerFunc(s.handleRetention)))

	// ---- 节点 Agent 面（token 鉴权，独立中间件） ----
	m.Handle("POST /api/agent/register", http.HandlerFunc(s.handleAgentRegister))
	m.Handle("POST /api/agent/heartbeat", s.agentAuth(http.HandlerFunc(s.handleAgentHeartbeat)))
	m.Handle("GET /api/agent/desired", s.agentAuth(http.HandlerFunc(s.handleAgentDesired)))
	m.Handle("POST /api/agent/logs", s.agentAuth(http.HandlerFunc(s.handleAgentLogs)))
	return m
}

// ============================== 鉴权中间件 ==============================

type ctxKey string

const (
	ctxUser    ctxKey = "user"
	ctxSession ctxKey = "session"
	ctxToken   ctxKey = "agent_token"
)

// auth 管理面鉴权：会话 Cookie + 账号状态 + 来源白名单。
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil || c.Value == "" {
			writeErr(w, http.StatusUnauthorized, "请先登录", "unauthorized", "")
			return
		}
		sess, err := s.Store.LookupSession(c.Value)
		if err != nil {
			// 会话失效时清掉 Cookie，避免前端反复拿一个死 Cookie 重试
			http.SetCookie(w, &http.Cookie{
				Name: cookieName, Value: "", Path: "/", MaxAge: -1,
				HttpOnly: true, Secure: s.Config.SecureCookies, SameSite: http.SameSiteStrictMode,
			})
			writeErr(w, http.StatusUnauthorized, "会话已失效，请重新登录", "session_expired", "")
			return
		}
		u, err := s.Store.GetUser(sess.UserID)
		if err != nil || !u.Enabled {
			writeErr(w, http.StatusUnauthorized, "账号不可用", "account_disabled", "")
			return
		}
		ip := clientIP(r)
		if !ipAllowed(u.IPAllowlist, ip) {
			writeErr(w, http.StatusForbidden, "当前来源地址不在允许范围内", "ip_not_allowed", "来源 "+ip)
			return
		}
		ctx := context.WithValue(r.Context(), ctxUser, u)
		ctx = context.WithValue(ctx, ctxSession, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// csrf 写操作必须带 X-CSRF-Token，且与登录时下发的值一致。
//
// 为什么 Cookie 已经 SameSite=Strict 还要 CSRF：
// SameSite 是浏览器行为，一旦将来（为了嵌入别的系统）放宽成 Lax/None，
// 或者被老浏览器忽略，防护就没了。CSRF 头是服务端强制的，不依赖浏览器配合。
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, _ := r.Context().Value(ctxSession).(*store.SessionView)
		if sess == nil {
			writeErr(w, http.StatusUnauthorized, "请先登录", "unauthorized", "")
			return
		}
		tok := r.Header.Get("X-CSRF-Token")
		if tok == "" {
			writeErr(w, http.StatusForbidden, "缺少 CSRF 令牌（写操作必须带 X-CSRF-Token）", "csrf_missing", "")
			return
		}
		if subtle.ConstantTimeCompare([]byte(auth.TokenHash(tok)), []byte(sess.CSRFHash)) != 1 {
			writeErr(w, http.StatusForbidden, "CSRF 令牌无效", "csrf_invalid", "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// agentAuth 节点面鉴权。
//
// 用独立的表（agent_tokens）与独立的头，避免"管理员会话被拿去冒充节点"。
// 校验的是**已消费后派生出的节点密钥**，注册令牌本身一次性、用完即废。
func (s *Server) agentAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Agent-Token")
		if tok == "" {
			// 允许用 Authorization: Bearer 传，便于 curl 调试
			if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
				tok = strings.TrimPrefix(a, "Bearer ")
			}
		}
		if tok == "" {
			writeErr(w, http.StatusUnauthorized, "缺少节点令牌", "agent_token_missing", "")
			return
		}
		nodeID, err := s.Store.VerifyNodeCredential(tok)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "节点令牌无效或已撤销", "agent_token_invalid", err.Error())
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxToken, nodeID)))
	})
}

// ============================== 登录 ==============================

const cookieName = "shengyu_edgelink_session"

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.allowLogin(ip) {
		writeErr(w, http.StatusTooManyRequests,
			"登录尝试过于频繁，请稍后再试", "rate_limited",
			fmt.Sprintf("来源 %s 每分钟最多 %d 次", ip, s.Config.LoginRateLimit))
		return
	}
	var req loginReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "bad_request", "")
		return
	}
	u, err := s.Store.Authenticate(strings.TrimSpace(req.Username), req.Password)
	if err != nil {
		// 审计登录失败（含来源 IP）。注意**不记录口令**。
		_ = s.Store.WriteAudit(&store.AuditEntry{
			ID: id.New("aud"), Actor: strings.TrimSpace(req.Username), ActorIP: ip,
			Action: "auth.login", Result: "denied",
			Summary: "登录失败（用户名或口令错误）",
		})
		writeErr(w, http.StatusUnauthorized, "用户名或口令错误", "bad_credentials", "")
		return
	}
	if !ipAllowed(u.IPAllowlist, ip) {
		_ = s.Store.WriteAudit(&store.AuditEntry{
			ID: id.New("aud"), Actor: u.Username, ActorIP: ip,
			Action: "auth.login", Result: "denied", Summary: "来源地址不在允许范围内",
		})
		writeErr(w, http.StatusForbidden, "当前来源地址不在允许范围内", "ip_not_allowed", "")
		return
	}

	sess, err := s.Store.CreateSession(u.ID, ip, r.UserAgent(), s.Config.SessionTTL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "创建会话失败", "session_create_failed", err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: sess.Token, Path: "/",
		Expires: sess.ExpiresAt, HttpOnly: true,
		Secure: s.Config.SecureCookies, SameSite: http.SameSiteStrictMode,
	})
	_ = s.Store.WriteAudit(&store.AuditEntry{
		ID: id.New("aud"), Actor: u.Username, ActorIP: ip,
		Action: "auth.login", Result: "ok", Summary: "登录成功",
	})
	// CSRF 明文只在登录响应里返回一次，前端存内存（不放 localStorage）。
	writeJSON(w, http.StatusOK, map[string]any{
		"user":       map[string]any{"id": u.ID, "username": u.Username, "role": u.Role},
		"csrf_token": sess.CSRF,
		"expires_at": sess.ExpiresAt,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		if sess, lerr := s.Store.LookupSession(c.Value); lerr == nil {
			if u, uerr := s.Store.GetUser(sess.UserID); uerr == nil {
				_ = s.Store.WriteAudit(&store.AuditEntry{
					ID: id.New("aud"), Actor: u.Username, ActorIP: clientIP(r),
					Action: "auth.logout", Result: "ok", Summary: "退出登录",
				})
			}
		}
		_ = s.Store.DeleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.Config.SecureCookies, SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, _ := r.Context().Value(ctxUser).(*store.User)
	if u == nil {
		writeErr(w, http.StatusUnauthorized, "未登录", "unauthorized", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": u.ID, "username": u.Username, "role": u.Role,
		"last_login_at": u.LastLoginAt, "ip_allowlist": u.IPAllowlist,
	})
}

type changePasswordReq struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func adminUser(r *http.Request) (*store.User, bool) {
	u, ok := r.Context().Value(ctxUser).(*store.User)
	return u, ok && u != nil && strings.EqualFold(u.Role, "admin")
}

type createUserReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminUser(r); !ok {
		writeErr(w, http.StatusForbidden, "只有管理员可以管理账号", "admin_required", "")
		return
	}
	users, err := s.Store.ListUsers()
	if err != nil { mapStoreErr(w, err, "账号"); return }
	type userView struct {
		ID string `json:"id"`; Username string `json:"username"`; Role string `json:"role"`
		Enabled bool `json:"enabled"`; IPAllowlist string `json:"ip_allowlist"`
		CreatedAt time.Time `json:"created_at"`; LastLoginAt time.Time `json:"last_login_at"`
	}
	out := make([]userView, 0, len(users))
	for _, u := range users { out = append(out, userView{u.ID, u.Username, u.Role, u.Enabled, u.IPAllowlist, u.CreatedAt, u.LastLoginAt}) }
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	_, ok := adminUser(r)
	if !ok { writeErr(w, http.StatusForbidden, "只有管理员可以创建账号", "admin_required", ""); return }
	var req createUserReq
	if err := decodeJSON(r, &req); err != nil { writeErr(w, http.StatusBadRequest, err.Error(), "bad_request", ""); return }
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || len([]rune(req.Username)) > 64 { writeErr(w, http.StatusBadRequest, "用户名不能为空且不能超过 64 个字符", "username_invalid", ""); return }
	if len([]rune(req.Password)) < 12 { writeErr(w, http.StatusBadRequest, "口令至少需要 12 个字符", "password_too_short", ""); return }
	u := &store.User{ID: id.New("usr"), Username: req.Username, Role: "admin", Enabled: true}
	if err := s.Store.CreateUser(u, req.Password, auth.DefaultIterations); err != nil { mapStoreErr(w, err, "账号"); return }
	s.audit(r, "auth.user_create", "user", u.ID, "创建账号 "+u.Username, "ok", "")
	writeJSON(w, http.StatusCreated, map[string]any{"id": u.ID, "username": u.Username, "role": u.Role})
}

func (s *Server) handleResetUserPassword(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminUser(r); !ok { writeErr(w, http.StatusForbidden, "只有管理员可以重置账号口令", "admin_required", ""); return }
	var req struct { Password string `json:"password"` }
	if err := decodeJSON(r, &req); err != nil { writeErr(w, http.StatusBadRequest, err.Error(), "bad_request", ""); return }
	if len([]rune(req.Password)) < 12 { writeErr(w, http.StatusBadRequest, "口令至少需要 12 个字符", "password_too_short", ""); return }
	u, err := s.Store.GetUser(r.PathValue("id")); if err != nil { mapStoreErr(w, err, "账号"); return }
	if err := s.Store.UpdateUserPassword(u.ID, req.Password, auth.DefaultIterations); err != nil { writeErr(w, http.StatusInternalServerError, "重置口令失败", "password_update_failed", err.Error()); return }
	s.audit(r, "auth.user_password_reset", "user", u.ID, "重置账号口令 "+u.Username, "ok", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": u.Username})
}

// handleChangeMyPassword 修改当前账号口令，并由 store 一次性吊销该账号旧会话。
func (s *Server) handleChangeMyPassword(w http.ResponseWriter, r *http.Request) {
	u, _ := r.Context().Value(ctxUser).(*store.User)
	if u == nil {
		writeErr(w, http.StatusUnauthorized, "未登录", "unauthorized", "")
		return
	}
	var req changePasswordReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "bad_request", "")
		return
	}
	if len([]rune(req.NewPassword)) < 12 {
		writeErr(w, http.StatusBadRequest, "新口令至少需要 12 个字符", "password_too_short", "")
		return
	}
	if _, err := s.Store.Authenticate(u.Username, req.CurrentPassword); err != nil {
		writeErr(w, http.StatusUnauthorized, "当前口令不正确", "current_password_invalid", "")
		return
	}
	if err := s.Store.UpdateUserPassword(u.ID, req.NewPassword, auth.DefaultIterations); err != nil {
		writeErr(w, http.StatusInternalServerError, "修改口令失败", "password_update_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "next_step": "口令已修改，当前会话已失效，请使用新口令重新登录"})
}

// allowLogin 简易滑动窗口限流，防暴力破解。
func (s *Server) allowLogin(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-time.Minute)
	var kept []time.Time
	for _, t := range s.login[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= s.Config.LoginRateLimit {
		s.login[ip] = kept
		return false
	}
	s.login[ip] = append(kept, s.now())
	return true
}

// ipAllowed 判断来源 IP 是否在允许列表内。
//
// 允许列表为空 = 不限制（保留给内网部署）。
// 支持单个 IP 与 CIDR；任何解析失败的条目都视为"不匹配"而不是"匹配"——
// 配置写错时应该更严格，而不是更宽松。
func ipAllowed(allowlist, ip string) bool {
	allowlist = strings.TrimSpace(allowlist)
	if allowlist == "" {
		return true
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, item := range strings.Split(allowlist, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, cidr, err := net.ParseCIDR(item); err == nil {
			if cidr.Contains(parsed) {
				return true
			}
			continue
		}
		if other := net.ParseIP(item); other != nil && other.Equal(parsed) {
			return true
		}
	}
	return false
}

// audit 便捷写审计。
func (s *Server) audit(r *http.Request, action, targetType, targetID, summary, result, detail string) {
	actor := ""
	if u, ok := r.Context().Value(ctxUser).(*store.User); ok {
		actor = u.Username
	}
	_ = s.Store.WriteAudit(&store.AuditEntry{
		ID: id.New("aud"), Actor: actor, ActorIP: clientIP(r),
		Action: action, TargetType: targetType, TargetID: targetID,
		Summary: summary, Result: result, Detail: detail,
	})
}

func pathInt(r *http.Request, name string) (int, error) {
	return strconv.Atoi(r.PathValue(name))
}

func pathID(r *http.Request) string { return r.PathValue("id") }

func logf(format string, args ...any) { log.Printf(format, args...) }
