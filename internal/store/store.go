// Package store 是管理面元数据库（SQLite）的访问层。
//
// 与 logstore 的分工：
//   - store：配置与审计（低频写、强一致、必须可靠）；
//   - logstore：连接日志/指标（高频写、可按时间丢弃）。
//
// 二者都用 SQLite 文件，但**物理分开**，这样清理日志永远不会碰到配置。
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shengyu/edgelink/internal/db"
)

// ErrNotFound 资源不存在。上层 API 据此映射 404。
var ErrNotFound = errors.New("store: 记录不存在")

// ErrConflict 唯一约束冲突（例如重复域名、重复入口端口）。
// 上层据此映射 409 并在响应里说明"和谁冲突"，而不是抛 500。
var ErrConflict = errors.New("store: 冲突")

// Store 元数据库句柄。
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open 打开元数据库并确保表结构就绪。
func Open(path string) (*Store, error) {
	d, err := db.Open(path, false)
	if err != nil {
		return nil, err
	}
	// SQLite 的外键约束默认关闭，必须显式打开（db.Open 的 pragma 已包含，这里再兜一次）。
	if _, err := d.Exec("PRAGMA foreign_keys = ON"); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("store: 开启外键约束失败: %w", err)
	}
	if err := db.ApplySchema(d, schemaSQL); err != nil {
		_ = d.Close()
		return nil, err
	}
	s := &Store{db: d, now: time.Now}
	// 先迁移再 bootstrap：老库缺列时，bootstrap 之后的任何查询都会失败。
	// 顺序错了的话，升级第一次启动会以一句 "no such column" 收场。
	if err := s.migrate(); err != nil {
		_ = d.Close()
		return nil, err
	}
	if err := s.bootstrap(); err != nil {
		_ = d.Close()
		return nil, err
	}
	return s, nil
}

// DB 暴露底层句柄，仅供同进程内的内部包（如 publish 需要跨表事务）使用。
func (s *Store) DB() *sql.DB { return s.db }

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// SetClock 注入时钟，供测试构造"过期/超时"场景。
func (s *Store) SetClock(f func() time.Time) {
	if f != nil {
		s.now = f
	}
}

func (s *Store) Now() time.Time { return s.now() }

// bootstrap 写入必要的元信息行，并校准已存在的账号数量（首次启动要能创建管理员）。
func (s *Store) bootstrap() error {
	_, err := s.db.Exec(
		`INSERT INTO meta(k, v) VALUES('schema_version', ?) ON CONFLICT(k) DO NOTHING`,
		schemaVersion)
	return err
}

const schemaVersion = "1"

// SchemaVersion 读回当前库的 schema 版本，供 `shengyu-edgelink-server --check` 之类使用。
func (s *Store) SchemaVersion() (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k='schema_version'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// ============ 事务与通用辅助 ============

// Tx 在一个事务里执行 fn；fn 返回错误则回滚。
// 所有"先校验后写入"的多表操作都必须走这里，避免半写状态。
func (s *Store) Tx(fn func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: 开启事务失败: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ts 统一时间序列化格式：UTC + RFC3339 纳秒，定宽保证字符串序 == 时间序。
func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func intToBool(i int) bool { return i != 0 }

// toJSON 把结构序列化成 JSON 文本；失败时返回一个保守的默认值而不是报错，
// 因为这些列是附属信息，不该让主流程失败。调用方会在真需要一致性时自行校验。
func toJSON(v any, fallback string) string {
	b, err := json.Marshal(v)
	if err != nil || len(b) == 0 {
		return fallback
	}
	return string(b)
}

func fromJSON(s string, v any) {
	if strings.TrimSpace(s) == "" {
		return
	}
	_ = json.Unmarshal([]byte(s), v)
}

// isUniqueViolation 判定 SQLite 唯一约束冲突。
// modernc 驱动的错误信息里带 "UNIQUE constraint failed"，按此识别，
// 避免把「重复域名」这类可预期的业务冲突报成 500。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed") ||
		strings.Contains(msg, "constraint failed: unique") ||
		strings.Contains(msg, "sqlite_constraint_unique")
}

// wrapUnique 把唯一冲突转成 ErrConflict，其它错误原样返回并带上上下文。
func wrapUnique(err error, what string) error {
	if err == nil {
		return nil
	}
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: %s", ErrConflict, what)
	}
	return fmt.Errorf("store: %s: %w", what, err)
}

// scanErr 把 sql.ErrNoRows 归一成 ErrNotFound。
func scanErr(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	if err != nil {
		return fmt.Errorf("store: %s: %w", what, err)
	}
	return nil
}
