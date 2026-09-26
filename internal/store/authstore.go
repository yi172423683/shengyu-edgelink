package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/shengyu/edgelink/internal/auth"
	"github.com/shengyu/edgelink/internal/model"
)

// ============================== 账号 ==============================

// User 管理后台账号。
type User struct {
	ID           string
	Username     string
	PasswordHash string
	Salt         string
	Iterations   int
	Role         string
	Enabled      bool
	IPAllowlist  string // 逗号分隔的 CIDR/IP；为空表示不限制
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastLoginAt  time.Time
}

// CountUsers 用于首次启动判断"是否需要引导创建管理员"。
func (s *Store) CountUsers() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: 统计账号: %w", err)
	}
	return n, nil
}

// CreateUser 创建账号。密码在这里就哈希掉，**明文不经过本函数之外**。
func (s *Store) CreateUser(u *User, password string, iterations int) error {
	if strings.TrimSpace(u.Username) == "" {
		return fmt.Errorf("store: 用户名不能为空")
	}
	hash, salt, it, err := auth.Hash(password, iterations)
	if err != nil {
		return err
	}
	if u.ID == "" {
		return fmt.Errorf("store: 账号 ID 必须由调用方生成")
	}
	if u.Role == "" {
		u.Role = "admin"
	}
	now := s.now()
	u.PasswordHash, u.Salt, u.Iterations = hash, salt, it
	u.CreatedAt, u.UpdatedAt = now, now
	_, err = s.db.Exec(`INSERT INTO users(id,username,password_hash,salt,iterations,role,enabled,ip_allowlist,created_at,updated_at,last_login_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		u.ID, u.Username, hash, salt, it, u.Role, boolToInt(u.Enabled), u.IPAllowlist,
		ts(now), ts(now), "")
	return wrapUnique(err, "创建账号")
}

func (s *Store) GetUserByUsername(name string) (*User, error) {
	var (
		u          User
		enabled    int
		ca, ua, la string
	)
	err := s.db.QueryRow(`SELECT id,username,password_hash,salt,iterations,role,enabled,ip_allowlist,created_at,updated_at,last_login_at
		FROM users WHERE username=?`, name).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Salt, &u.Iterations, &u.Role, &enabled,
			&u.IPAllowlist, &ca, &ua, &la)
	if err != nil {
		return nil, scanErr(err, "账号 "+name)
	}
	u.Enabled = intToBool(enabled)
	u.CreatedAt, u.UpdatedAt, u.LastLoginAt = parseTS(ca), parseTS(ua), parseTS(la)
	return &u, nil
}

func (s *Store) GetUser(id string) (*User, error) {
	var (
		u          User
		enabled    int
		ca, ua, la string
	)
	err := s.db.QueryRow(`SELECT id,username,password_hash,salt,iterations,role,enabled,ip_allowlist,created_at,updated_at,last_login_at
		FROM users WHERE id=?`, id).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Salt, &u.Iterations, &u.Role, &enabled,
			&u.IPAllowlist, &ca, &ua, &la)
	if err != nil {
		return nil, scanErr(err, "账号 "+id)
	}
	u.Enabled = intToBool(enabled)
	u.CreatedAt, u.UpdatedAt, u.LastLoginAt = parseTS(ca), parseTS(ua), parseTS(la)
	return &u, nil
}

// Authenticate 校验用户名口令。成功后更新 last_login_at。
//
// 安全要点：**用户不存在与口令错误返回同一个错误**，避免账号枚举。
func (s *Store) Authenticate(username, password string) (*User, error) {
	u, err := s.GetUserByUsername(username)
	if err != nil {
		if IsNotFound(err) {
			// 走一次哈希计算，让"用户不存在"和"口令错误"耗时接近，
			// 否则可以按响应时间差枚举出哪些用户名有效。
			_, _, _, _ = auth.Hash("dummy-password-for-timing", 1000)
			return nil, auth.ErrMismatch
		}
		return nil, err
	}
	if !u.Enabled {
		return nil, auth.ErrMismatch
	}
	if err := auth.Verify(password, u.PasswordHash, u.Salt, u.Iterations); err != nil {
		return nil, err
	}
	now := s.now()
	_, _ = s.db.Exec(`UPDATE users SET last_login_at=?, updated_at=? WHERE id=?`, ts(now), ts(now), u.ID)
	u.LastLoginAt = now
	return u, nil
}

// UpdateUserPassword 改密并**吊销该用户全部会话** —— 改密后旧会话还能用是个常见漏洞。
func (s *Store) UpdateUserPassword(userID, newPassword string, iterations int) error {
	hash, salt, it, err := auth.Hash(newPassword, iterations)
	if err != nil {
		return err
	}
	return s.Tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE users SET password_hash=?,salt=?,iterations=?,updated_at=? WHERE id=?`,
			hash, salt, it, ts(s.now()), userID)
		if err != nil {
			return fmt.Errorf("store: 更新口令: %w", err)
		}
		if err := mustAffect(res, "账号"); err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM sessions WHERE user_id=?`, userID)
		return err
	})
}

// SetUserIPAllowlist 设置该账号允许登录的来源（逗号分隔 CIDR/IP，空=不限制）。
func (s *Store) SetUserIPAllowlist(userID, allowlist string) error {
	res, err := s.db.Exec(`UPDATE users SET ip_allowlist=?, updated_at=? WHERE id=?`,
		strings.TrimSpace(allowlist), ts(s.now()), userID)
	if err != nil {
		return fmt.Errorf("store: 设置来源限制: %w", err)
	}
	return mustAffect(res, "账号")
}

func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT id,username,password_hash,salt,iterations,role,enabled,ip_allowlist,created_at,updated_at,last_login_at
		FROM users ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询账号: %w", err)
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var (
			u          User
			enabled    int
			ca, ua, la string
		)
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Salt, &u.Iterations, &u.Role,
			&enabled, &u.IPAllowlist, &ca, &ua, &la); err != nil {
			return nil, fmt.Errorf("store: 读取账号行: %w", err)
		}
		u.Enabled = intToBool(enabled)
		u.CreatedAt, u.UpdatedAt, u.LastLoginAt = parseTS(ca), parseTS(ua), parseTS(la)
		out = append(out, u)
	}
	return out, rows.Err()
}

// ============================== 会话 ==============================

// Session 一条登录会话。返回给调用方时只带明文 token（仅此一次），库里存哈希。
type Session struct {
	ID        string
	UserID    string
	Token     string // 明文，仅登录响应里出现一次
	CSRF      string // 明文，下发给前端
	CreatedAt time.Time
	ExpiresAt time.Time
	LastSeen  time.Time
	ClientIP  string
	UserAgent string
}

// CreateSession 新建会话。ttl 由调用方给（默认 12 小时，见 api 包）。
func (s *Store) CreateSession(userID, clientIP, userAgent string, ttl time.Duration) (*Session, error) {
	token, err := auth.NewToken(32)
	if err != nil {
		return nil, err
	}
	csrf, err := auth.NewToken(32)
	if err != nil {
		return nil, err
	}
	id, err := auth.NewToken(16)
	if err != nil {
		return nil, err
	}
	now := s.now()
	sess := &Session{
		ID: id, UserID: userID, Token: token, CSRF: csrf,
		CreatedAt: now, ExpiresAt: now.Add(ttl), LastSeen: now,
		ClientIP: clientIP, UserAgent: truncateStr(userAgent, 255),
	}
	_, err = s.db.Exec(`INSERT INTO sessions(id,user_id,token_hash,csrf_hash,created_at,expires_at,last_seen,client_ip,user_agent)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		sess.ID, userID, auth.TokenHash(token), auth.TokenHash(csrf),
		ts(now), ts(sess.ExpiresAt), ts(now), clientIP, sess.UserAgent)
	if err != nil {
		return nil, fmt.Errorf("store: 创建会话: %w", err)
	}
	return sess, nil
}

// SessionView 校验会话时返回的信息（不含明文 token）。
type SessionView struct {
	SessionID string
	UserID    string
	CSRFHash  string
	ExpiresAt time.Time
}

// LookupSession 按明文 token 找会话。返回 ErrNotFound 表示无效或已过期。
//
// 过期判断在这里做（而不是靠定时清理任务），因为清理任务的间隔不可控：
// 一条过期的会话在清理前仍然可以登录就太糟了。
func (s *Store) LookupSession(token string) (*SessionView, error) {
	var (
		v       SessionView
		expires string
	)
	err := s.db.QueryRow(`SELECT id,user_id,csrf_hash,expires_at FROM sessions WHERE token_hash=?`,
		auth.TokenHash(token)).Scan(&v.SessionID, &v.UserID, &v.CSRFHash, &expires)
	if err != nil {
		return nil, scanErr(err, "会话")
	}
	v.ExpiresAt = parseTS(expires)
	if s.now().After(v.ExpiresAt) {
		return nil, fmt.Errorf("%w: 会话已过期", ErrNotFound)
	}
	_, _ = s.db.Exec(`UPDATE sessions SET last_seen=? WHERE id=?`, ts(s.now()), v.SessionID)
	return &v, nil
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash=?`, auth.TokenHash(token))
	return err
}

func (s *Store) PurgeExpiredSessions() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, ts(s.now()))
	if err != nil {
		return 0, fmt.Errorf("store: 清理会话: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ============================== 节点注册令牌（需求 §九） ==============================

// AgentToken 一条节点注册令牌。明文只在创建响应里出现一次。
type AgentToken struct {
	ID        string
	NodeID    string
	Token     string // 明文，仅创建时返回
	Note      string
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    time.Time
	RevokedAt time.Time
}

// CreateAgentToken 生成一次性注册令牌。
func (s *Store) CreateAgentToken(nodeID, note string, ttl time.Duration) (*AgentToken, error) {
	token, err := auth.NewToken(32)
	if err != nil {
		return nil, err
	}
	id, err := auth.NewToken(16)
	if err != nil {
		return nil, err
	}
	now := s.now()
	t := &AgentToken{ID: id, NodeID: nodeID, Token: token, Note: note, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	_, err = s.db.Exec(`INSERT INTO agent_tokens(id,node_id,token_hash,note,created_at,expires_at,used_at,revoked_at)
		VALUES(?,?,?,?,?,?,'','')`,
		t.ID, nodeID, auth.TokenHash(token), note, ts(now), ts(t.ExpiresAt))
	if err != nil {
		return nil, fmt.Errorf("store: 创建注册令牌: %w", err)
	}
	return t, nil
}

// ConsumeAgentToken 消费一次性注册令牌，返回绑定的 node_id。
//
// 用事务 + 条件更新实现"一次性"：并发两次消费只有一次能成功
// （UPDATE ... WHERE used_at=” 的第二条影响 0 行）。
func (s *Store) ConsumeAgentToken(token string) (string, error) {
	hash := auth.TokenHash(token)
	var (
		nodeID, expires, revoked, used string
		id                             string
	)
	err := s.db.QueryRow(`SELECT id,node_id,expires_at,used_at,revoked_at FROM agent_tokens WHERE token_hash=?`, hash).
		Scan(&id, &nodeID, &expires, &used, &revoked)
	if err != nil {
		return "", scanErr(err, "注册令牌")
	}
	if revoked != "" {
		return "", fmt.Errorf("%w: 注册令牌已被撤销", ErrNotFound)
	}
	if used != "" {
		return "", fmt.Errorf("%w: 注册令牌已被使用过（一次性）", ErrNotFound)
	}
	if s.now().After(parseTS(expires)) {
		return "", fmt.Errorf("%w: 注册令牌已过期", ErrNotFound)
	}
	res, err := s.db.Exec(`UPDATE agent_tokens SET used_at=? WHERE id=? AND used_at=''`, ts(s.now()), id)
	if err != nil {
		return "", fmt.Errorf("store: 消费注册令牌: %w", err)
	}
	if err := mustAffect(res, "注册令牌"); err != nil {
		return "", fmt.Errorf("%w: 注册令牌已被并发使用", ErrNotFound)
	}
	return nodeID, nil
}

func (s *Store) RevokeAgentToken(id string) error {
	res, err := s.db.Exec(`UPDATE agent_tokens SET revoked_at=? WHERE id=? AND revoked_at=''`, ts(s.now()), id)
	if err != nil {
		return fmt.Errorf("store: 撤销注册令牌: %w", err)
	}
	return mustAffect(res, "注册令牌")
}

func (s *Store) ListAgentTokens(nodeID string) ([]AgentToken, error) {
	rows, err := s.db.Query(`SELECT id,node_id,note,created_at,expires_at,used_at,revoked_at
		FROM agent_tokens WHERE node_id=? ORDER BY created_at DESC`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询注册令牌: %w", err)
	}
	defer rows.Close()
	out := []AgentToken{}
	for rows.Next() {
		var (
			t              AgentToken
			ca, ea, ua, ra string
		)
		if err := rows.Scan(&t.ID, &t.NodeID, &t.Note, &ca, &ea, &ua, &ra); err != nil {
			return nil, fmt.Errorf("store: 读取注册令牌行: %w", err)
		}
		t.CreatedAt, t.ExpiresAt = parseTS(ca), parseTS(ea)
		t.UsedAt, t.RevokedAt = parseTS(ua), parseTS(ra)
		out = append(out, t)
	}
	return out, rows.Err()
}

// RotateAgentToken 轮换：先撤销该节点所有未用令牌，再发一个新的。
func (s *Store) RotateAgentToken(nodeID, note string, ttl time.Duration) (*AgentToken, error) {
	if _, err := s.db.Exec(`UPDATE agent_tokens SET revoked_at=? WHERE node_id=? AND revoked_at='' AND used_at=''`,
		ts(s.now()), nodeID); err != nil {
		return nil, fmt.Errorf("store: 轮换前撤销旧令牌: %w", err)
	}
	return s.CreateAgentToken(nodeID, note, ttl)
}

// ============================== 节点长期凭据 ==============================

// IssueNodeCredential 为节点签发长期凭据（注册令牌消费成功后调用）。
// 明文只返回一次，库里只存哈希。
func (s *Store) IssueNodeCredential(nodeID, note string) (string, error) {
	tok, err := auth.NewToken(32)
	if err != nil {
		return "", err
	}
	cid, err := auth.NewToken(12)
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(`INSERT INTO node_credentials(id,node_id,cred_hash,note,created_at,revoked_at,last_used)
		VALUES(?,?,?,?,?,'','')`,
		cid, nodeID, auth.TokenHash(tok), note, ts(s.now()))
	if err != nil {
		return "", fmt.Errorf("store: 签发节点凭据: %w", err)
	}
	return tok, nil
}

// VerifyNodeCredential 校验节点凭据，返回绑定的 node_id。
//
// 与注册令牌不同，长期凭据**不是一次性的**：节点每次心跳都要用。
// 但会在库里更新 last_used，便于排查"这台节点最后一次联系我们是什么时候"。
func (s *Store) VerifyNodeCredential(token string) (string, error) {
	var nodeID, revoked string
	err := s.db.QueryRow(`SELECT node_id, revoked_at FROM node_credentials WHERE cred_hash=?`,
		auth.TokenHash(token)).Scan(&nodeID, &revoked)
	if err != nil {
		return "", scanErr(err, "节点凭据")
	}
	if revoked != "" {
		return "", fmt.Errorf("%w: 节点凭据已被撤销", ErrNotFound)
	}
	_, _ = s.db.Exec(`UPDATE node_credentials SET last_used=? WHERE cred_hash=?`, ts(s.now()), auth.TokenHash(token))
	return nodeID, nil
}

// RevokeNodeCredentials 撤销某节点的全部长期凭据（节点回收/更换时用）。
func (s *Store) RevokeNodeCredentials(nodeID string) (int64, error) {
	res, err := s.db.Exec(`UPDATE node_credentials SET revoked_at=? WHERE node_id=? AND revoked_at=''`,
		ts(s.now()), nodeID)
	if err != nil {
		return 0, fmt.Errorf("store: 撤销节点凭据: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListActiveCredentials 列出节点当前有效的凭据（不含明文）。
func (s *Store) ListActiveCredentials(nodeID string) ([]map[string]any, error) {
	rows, err := s.db.Query(`SELECT id,note,created_at,last_used FROM node_credentials
		WHERE node_id=? AND revoked_at='' ORDER BY created_at DESC`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询节点凭据: %w", err)
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var cid, note, ca, lu string
		if err := rows.Scan(&cid, &note, &ca, &lu); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id": cid, "note": note, "created_at": parseTS(ca), "last_used": parseTS(lu),
		})
	}
	return out, rows.Err()
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

var _ = model.NodeUnknown
