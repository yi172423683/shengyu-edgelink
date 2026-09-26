// Package logstore 负责连接日志的**小时分片**存储、查询与容量控制。
//
// 为什么不用一个 SQLite 存全部日志（需求 §二.4 明确禁止）：
//  1. 单文件会无限膨胀，vacuum/备份/迁移都变得不可行；
//  2. 「查询不扫全历史」这件事在单文件下只能靠索引祈祷，做不到**物理隔离**；
//  3. 保留期/配额策略在分片下就是"删文件"，简单可靠。
//
// 分片粒度固定 1 小时：路径 <root>/<kind>/YYYY/MM/DD/HH.db
// 查询只会打开时间范围内的分片，这一点在 Query.Explain 里如实回报给界面。
package logstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shengyu/edgelink/internal/logparse"
)

// Kind 日志流类型。不同流独立分片、独立保留期、独立配额。
type Kind string

const (
	KindConn   Kind = "conn"
	KindMetric Kind = "metric"
	KindProbe  Kind = "probe"
)

// AllKinds 供清理任务遍历。
func AllKinds() []Kind { return []Kind{KindConn, KindMetric, KindProbe} }

// connDDL 是每个连接日志分片的表结构。
//
// 设计要点：
//   - `(node_id, boot_id, seq)` 提供**幂等写入**：
//     agent 重传同一批不会产生重复行；而 agent/接收器**重启**后 boot_id 变化，
//     新连接不会因为序号从 1 重新计数而被当成重复丢弃（见 Ingest 的说明）；
//   - 数值列全部可空（NULL = 不可用），与 logparse 的语义一致 —— **0 和不可用是两回事**；
//   - accept_ts 与 end_ts 分开：TCP 日志在会话结束时产生，长连接的"建立时间"可能比
//     "结束时间"早很久。**分片轴与查询轴都用 end_ts**，accept_ts 只作为数据保留；
//   - raw_line 保留原文（截断），诊断时能看到 HAProxy 到底写了什么；
//   - 索引按"最常用的筛选维度 + end_ts"成对建，保证分页排序走索引；
//   - bytes_up / bytes_down 是**两个方向各自的字节数**（评审 F11）：%U = 客户端→源站，
//     %B = 源站→客户端。旧列 bytes_in 只作为历史数据的兼容位保留，见 shard.go 的迁移。
const connDDL = `
CREATE TABLE IF NOT EXISTS conn_log (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  boot_id        TEXT    NOT NULL DEFAULT '',
  seq            INTEGER NOT NULL DEFAULT 0,
  accept_ts      TEXT    NOT NULL,
  end_ts         TEXT    NOT NULL,
  recv_ts        TEXT    NOT NULL,
  node_id        TEXT    NOT NULL,
  business_id    TEXT    NOT NULL DEFAULT '',
  route_id       TEXT    NOT NULL DEFAULT '',
  config_version INTEGER NOT NULL DEFAULT 0,
  conn_id        TEXT    NOT NULL DEFAULT '',
  client_ip      TEXT    NOT NULL DEFAULT '',
  client_port    INTEGER,
  entry_addr     TEXT    NOT NULL DEFAULT '',
  entry_port     INTEGER,
  sni            TEXT    NOT NULL DEFAULT '',
  be_name        TEXT    NOT NULL DEFAULT '',
  srv_name       TEXT    NOT NULL DEFAULT '',
  origin_addr    TEXT    NOT NULL DEFAULT '',
  origin_port    INTEGER,
  origin_reported INTEGER NOT NULL DEFAULT 0,
  queue_ms       INTEGER,
  connect_ms     INTEGER,
  session_ms     INTEGER,
  bytes_up       INTEGER,
  bytes_down     INTEGER,
  bytes_in       INTEGER,
  term_code      TEXT    NOT NULL DEFAULT '',
  term_reason    TEXT    NOT NULL DEFAULT '',
  retries        INTEGER,
  queue_max      INTEGER,
  unavailable    TEXT    NOT NULL DEFAULT '',
  parse_ok       INTEGER NOT NULL DEFAULT 1,
  parse_err      TEXT    NOT NULL DEFAULT '',
  raw_line       TEXT    NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_conn_seq  ON conn_log(node_id, boot_id, seq);
CREATE INDEX IF NOT EXISTS ix_conn_ts          ON conn_log(end_ts DESC);
CREATE INDEX IF NOT EXISTS ix_conn_biz_ts      ON conn_log(business_id, end_ts DESC);
CREATE INDEX IF NOT EXISTS ix_conn_client_ts   ON conn_log(client_ip, end_ts DESC);
CREATE INDEX IF NOT EXISTS ix_conn_sni_ts      ON conn_log(sni, end_ts DESC);
CREATE INDEX IF NOT EXISTS ix_conn_ver         ON conn_log(config_version);
`

// Options 仓库级选项。
type Options struct {
	// KeepAlive 空闲分片连接保活时长。
	//
	// 为什么需要它：日志接收是**持续高频**的（每条连接一行）。若每次 flush 都
	// 新建/关闭一次 SQLite 连接，连接建立与首次页读取的开销会随日志量线性增长。
	// 保活之后，同一个小时分片在整个使用期内只建立一次连接。
	//
	// ⚠️ 为什么默认值是 0（不保活）：任何仍然打开的 SQLite 句柄在 Windows 上
	// 都会让 os.RemoveAll 失败（modernc 的 VFS 以 FILE_SHARE_READ|WRITE 打开，
	// 不含 FILE_SHARE_DELETE）。测试用 t.TempDir() 做数据目录，句柄遗留会变成
	// "功能全对、清理报错"的假失败。所以保活必须由调用方**显式**开启 ——
	// 生产进程（常驻服务）开启它，测试与短命进程不开启。
	//
	// 注意：保活与"读方能否看到已提交数据"**无关**。这一点是这次改动的核心：
	// 只读打开不再用 immutable、正常读 WAL，所以写连接开着也能读到最新数据。
	KeepAlive time.Duration
	// MaxOpenShards 同时保持打开的写分片数上限（仅 KeepAlive>0 时有意义）。
	MaxOpenShards int
	// RawLineMax raw_line 截断长度。
	RawLineMax int
	// DisableDiskCheck 关闭磁盘水位检查（仅测试用；生产不应关闭）。
	DisableDiskCheck bool

	// now 是时钟，测试可注入。
	now func() time.Time
}

// Option 函数式选项。
type Option func(*Options)

// WithKeepAlive 开启连接保活（生产进程用）。
func WithKeepAlive(d time.Duration) Option {
	return func(o *Options) { o.KeepAlive = d }
}

// WithMaxOpenShards 设置写分片缓存上限。
func WithMaxOpenShards(n int) Option {
	return func(o *Options) {
		if n > 0 {
			o.MaxOpenShards = n
		}
	}
}

// WithClock 注入时钟（测试用）。
func WithClock(f func() time.Time) Option {
	return func(o *Options) {
		if f != nil {
			o.now = f
		}
	}
}

// Store 分片日志仓库。
type Store struct {
	Root       string // 例如 /var/lib/shengyu-edgelink/logs
	RawLineMax int    // raw_line 截断长度，0 表示用默认 1024
	now        func() time.Time

	// KeepAlive / MaxOpenShards 见 Options。
	KeepAlive     time.Duration
	MaxOpenShards int
	diskCheck     bool

	mu sync.Mutex
	// openMu 串行化"打开写分片 + 首次建表"（见 openWriteShard 的说明）。
	// 它与 mu 分开，是为了让"建表"这段慢操作不要占着 mu —— mu 还要保护缓存与 LRU。
	openMu sync.Mutex
	// writers 写连接缓存（保活开启时才会留驻）。
	writers map[string]*sql.DB
	// readers 读连接缓存（保活开启时才会留驻）。
	readers map[string]*sql.DB
	// init 已建表（含列迁移）完成的分片路径。
	init map[string]bool
	// lru writers/readers 的淘汰顺序（按路径，最旧在前）。
	lru []string
}

// New 创建仓库并确保根目录存在。
//
// 保持 `New(root)` 可用的形式（变参），这样测试与既有调用无需改动；
// 生产进程传 WithKeepAlive 开启连接保活。
func New(root string, opts ...Option) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("logstore: root 不能为空")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("logstore: 创建根目录失败: %w", err)
	}
	o := Options{RawLineMax: 1024, MaxOpenShards: 8}
	for _, f := range opts {
		f(&o)
	}
	s := &Store{
		Root: root, RawLineMax: o.RawLineMax, now: time.Now,
		KeepAlive: o.KeepAlive, MaxOpenShards: o.MaxOpenShards,
		diskCheck: !o.DisableDiskCheck,
		writers:   map[string]*sql.DB{},
		readers:   map[string]*sql.DB{},
		init:      map[string]bool{},
	}
	if o.now != nil {
		s.now = o.now
	}
	return s, nil
}

// Close 关闭所有缓存的连接（写与读）。
//
// 调用方（进程退出、以及测试的清理阶段）应当调用它：在 Windows 上，
// 仍被打开的 SQLite 文件会让临时目录删除失败 —— 那会让一个"功能正确"的测试
// 以清理错误收场，排查起来很浪费时间。
func (s *Store) Close() error {
	s.mu.Lock()
	var dbs []*sql.DB
	for _, d := range s.writers {
		dbs = append(dbs, d)
	}
	for _, d := range s.readers {
		dbs = append(dbs, d)
	}
	s.writers = map[string]*sql.DB{}
	s.readers = map[string]*sql.DB{}
	s.lru = nil
	s.init = map[string]bool{}
	s.mu.Unlock()
	var firstErr error
	for _, d := range dbs {
		if err := d.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// SetClock 供测试注入时钟（保留期/配额逻辑必须可测，不能靠"等时间过去"）。
func (s *Store) SetClock(f func() time.Time) {
	if f != nil {
		s.now = f
	}
}

// Partition 一个分片的元信息。
type Partition struct {
	Kind       Kind      `json:"kind"`
	Start      time.Time `json:"start"`
	Path       string    `json:"path"`
	Compressed bool      `json:"compressed"`
	Bytes      int64     `json:"bytes"`
}

// partitionStart 把时间截断到小时。
func partitionStart(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc)
}

// PartitionPath 返回某个时刻所属分片的路径（.db）。
func (s *Store) PartitionPath(kind Kind, t time.Time) string {
	u := t.UTC()
	return filepath.Join(s.Root, string(kind),
		fmt.Sprintf("%04d", u.Year()),
		fmt.Sprintf("%02d", u.Month()),
		fmt.Sprintf("%02d", u.Day()),
		fmt.Sprintf("%02d", u.Hour())+".db")
}

// PartitionsBetween 返回 [from, to] 覆盖的所有分片（按时间升序）。
// **这是"不扫全历史"的实现基础**：调用方只可能打开这些文件。
func (s *Store) PartitionsBetween(kind Kind, from, to time.Time) []Partition {
	if to.Before(from) {
		from, to = to, from
	}
	var out []Partition
	for t := partitionStart(from, time.UTC); !t.After(to.UTC()); t = t.Add(time.Hour) {
		p := Partition{Kind: kind, Start: t, Path: s.PartitionPath(kind, t)}
		if _, err := os.Stat(p.Path); err == nil {
			p.Bytes = fileSize(p.Path)
		} else if _, err := os.Stat(p.Path + ".gz"); err == nil {
			p.Path = p.Path + ".gz"
			p.Compressed = true
			p.Bytes = fileSize(p.Path)
		} else {
			// 该小时没有数据 —— 不返回，避免查询打开一堆空文件
			continue
		}
		out = append(out, p)
	}
	return out
}

// ConnRow 一行待写入的连接日志（带上 agent 的批次序号，用于幂等）。
type ConnRow struct {
	// BootID 是采集端本次启动的标识。它是幂等键的一部分，见 Ingest 的说明。
	BootID string
	Seq    int64
	RecvTS time.Time
	Rec    logparse.Record
}

// IngestResult 一次写入的结果。
type IngestResult struct {
	Accepted    int      `json:"accepted"`
	Duplicated  int      `json:"duplicated"`
	Partitions  []string `json:"partitions"`
	ParseErrors int      `json:"parse_errors"`
	OldestSeq   int64    `json:"oldest_seq"`
	NewestSeq   int64    `json:"newest_seq"`
}

// Ingest 把日志行落到对应的小时分片。
//
// **分片轴 = end_ts（本平台收到/会话结束的时刻），不是 accept_ts。**
//
// 这一条是硬约束，理由是一个真实的排障失明场景：
// TCP 连接日志在**会话结束**时才产生。一条 3 小时前建立、刚刚断开的长连接，
// 它的 accept_ts 在 3 小时前。若按 accept_ts 分片，这行会落进 3 小时前的文件；
// 而运维排查"刚刚掉线的长连接"时查的是最近几分钟 —— 于是永远查不到关键证据。
// 让「分片轴」与「查询轴」与「写入端能确定的时刻」三者一致，才不会出现这种缺口。
//
// 代价：按"连接建立时间"检索就不在一个分片里了，需要放宽查询窗口让
// PartitionsBetween 覆盖到对应时段。accept_ts 仍然完整保留在行内，界面上可以按它排序/显示。
//
// 幂等由 UNIQUE(node_id, boot_id, seq) + INSERT OR IGNORE 保证：
//   - 同一批**重传**（同一 boot_id + seq）不会产生重复行；
//   - 采集端**重启**后 boot_id 变化，新连接不会因为从 1 重新计数而被误判成重复丢弃。
func (s *Store) Ingest(ctx context.Context, rows []ConnRow) (IngestResult, error) {
	res := IngestResult{}
	if len(rows) == 0 {
		return res, nil
	}
	groups := map[string][]ConnRow{}
	for _, r := range rows {
		ts := r.RecvTS
		if ts.IsZero() {
			// RecvTS 是我们自己打的时间戳，正常情况下不会为空；
			// 兜底用 accept_ts，并为 0 的情况用当前时钟，绝不 panic。
			ts = r.Rec.AcceptTS
			if ts.IsZero() {
				ts = s.now()
			}
		}
		key := s.PartitionPath(KindConn, ts)
		groups[key] = append(groups[key], r)
	}

	paths := make([]string, 0, len(groups))
	for k := range groups {
		paths = append(paths, k)
	}
	sort.Strings(paths)

	for _, path := range paths {
		n, dup, err := s.insertPartition(ctx, path, groups[path])
		if err != nil {
			return res, err
		}
		res.Accepted += n
		res.Duplicated += dup
		res.Partitions = append(res.Partitions, path)
	}
	for _, r := range rows {
		if !r.Rec.ParseOK {
			res.ParseErrors++
		}
		if res.OldestSeq == 0 || r.Seq < res.OldestSeq {
			res.OldestSeq = r.Seq
		}
		if r.Seq > res.NewestSeq {
			res.NewestSeq = r.Seq
		}
	}
	return res, nil
}

// insertPartition 把一个分片的多行写入落盘。
//
// 批量写入的位置就在这一层：**一批行 = 一个事务**（而不是一行一个事务）。
// 连接由 acquireWriter 提供：KeepAlive>0 时同一个小时分片复用一条连接，
// 因此"每条日志一次连接建立"那类开销被彻底消掉。
//
// 这里还处理一种容易漏掉的情况：分片被保留期清理掉了，但进程内的"已建表"标记还在，
// 于是下一次写入会碰到 `no such table`。这种情况必须**自动重建**，
// 而不是让日志写入从此永久失败到进程重启。
func (s *Store) insertPartition(ctx context.Context, path string, rows []ConnRow) (int, int, error) {
	for attempt := 1; attempt <= 2; attempt++ {
		d, release, err := s.acquireWriter(path)
		if err != nil {
			return 0, 0, err
		}
		ins, dup, werr := s.insertRows(ctx, d, rows)
		release()
		if werr == nil {
			return ins, dup, nil
		}
		if attempt == 1 && isMissingTable(werr) {
			s.forgetSchema(path)
			continue
		}
		return ins, dup, werr
	}
	return 0, 0, fmt.Errorf("logstore: 写入分片 %s 重试后仍失败", filepath.Base(path))
}

// isMissingTable 识别"表不存在"。用它把"分片需要重建"与"真的写坏了"区分开。
func isMissingTable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such table")
}

func (s *Store) insertRows(ctx context.Context, d *sql.DB, rows []ConnRow) (inserted, duplicated int, err error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("logstore: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO conn_log (
        boot_id, seq, accept_ts, end_ts, recv_ts, node_id, business_id, route_id, config_version, conn_id,
        client_ip, client_port, entry_addr, entry_port, sni, be_name, srv_name,
        origin_addr, origin_port, origin_reported, queue_ms, connect_ms, session_ms, bytes_up, bytes_down,
        term_code, term_reason, retries, queue_max, unavailable,
        parse_ok, parse_err, raw_line
    ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, 0, fmt.Errorf("logstore: 预编译失败: %w", err)
	}
	defer stmt.Close()

	maxRaw := s.RawLineMax
	if maxRaw <= 0 {
		maxRaw = 1024
	}
	for _, r := range rows {
		rec := r.Rec
		ok := 1
		if !rec.ParseOK {
			ok = 0
		}
		reported := 0
		if rec.OriginReported {
			reported = 1
		}
		rr, err := stmt.ExecContext(ctx,
			r.BootID,
			r.Seq,
			fmtTime(rec.AcceptTS),
			fmtTime(r.RecvTS), // end_ts：日志产生的时刻（会话结束）
			fmtTime(r.RecvTS),
			rec.NodeID,
			rec.BusinessID, rec.RouteID, rec.ConfigVersion, rec.ConnID,
			rec.ClientIP, intPtr(rec.ClientPort), rec.EntryAddr, intPtr(rec.EntryPort),
			rec.SNI, rec.BeName, rec.SrvName, rec.OriginAddr, intPtr(rec.OriginPort), reported,
			i64Ptr(rec.QueueMS), i64Ptr(rec.ConnectMS), i64Ptr(rec.SessionMS),
			i64Ptr(rec.BytesUp), i64Ptr(rec.BytesDown),
			rec.TermCode, rec.TermReason, intPtr(rec.Retries), intPtr(rec.QueueMax),
			strings.Join(rec.Unavailable, ","),
			ok, rec.ParseErr, truncateBytes(rec.RawLine, maxRaw),
		)
		if err != nil {
			return inserted, duplicated, fmt.Errorf("logstore: 写入失败: %w", err)
		}
		if n, _ := rr.RowsAffected(); n == 0 {
			duplicated++
		} else {
			inserted++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("logstore: 提交失败: %w", err)
	}
	return inserted, duplicated, nil
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

// hasConnTable 判断一个分片里是否已经有 conn_log 表。
//
// 用来把"分片刚建好还没建表"与"分片真的坏了"区分开：
// 前者应当被静默跳过（下次查询自然就有数据），后者应当被报告出来。
func hasConnTable(d *sql.DB) (bool, error) {
	var n int
	err := d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='conn_log'`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// Query 连接日志查询条件。
//
// 需求 §八：**必须分页 + 限制时间范围 + 不能每次查询都扫全历史**。
// 这里把这三条做成硬约束（Validate），而不是靠调用方自觉。
type Query struct {
	From time.Time
	To   time.Time

	BusinessID  string
	NodeID      string
	ClientIP    string
	SNI         string
	OnlyAnomaly bool // 只看异常（parse_ok=0 或有错误串/异常终止）

	Limit  int
	Offset int
	// Keyset 分页（深分页用这个，避免 OFFSET 越来越慢）
	BeforeEndTS string
	BeforeID    int64
}

// 硬上限：这些数字直接对应需求 §八，改它们要同步改文档与界面文案。
const (
	MaxLimit       = 200
	DefaultLimit   = 50
	MaxOffset      = 10000
	MaxQueryWindow = 24 * time.Hour
	NarrowWindow   = time.Hour // 无筛选条件时允许的最大跨度
)

// Validate 校验查询是否在允许范围内。返回的 error 是给界面直接展示的中文说明。
func (q *Query) Validate() error {
	if q.From.IsZero() || q.To.IsZero() {
		return fmt.Errorf("必须指定时间范围（from/to）")
	}
	if q.To.Before(q.From) {
		return fmt.Errorf("时间范围反了：from 晚于 to")
	}
	if q.To.Sub(q.From) > MaxQueryWindow {
		return fmt.Errorf("单次查询时间跨度上限 %s，当前 %s。请分段查询或用更精确的筛选条件",
			MaxQueryWindow, q.To.Sub(q.From).Round(time.Minute))
	}
	if q.Limit <= 0 {
		q.Limit = DefaultLimit
	}
	if q.Limit > MaxLimit {
		return fmt.Errorf("单页上限 %d 条，当前请求 %d 条", MaxLimit, q.Limit)
	}
	if q.Offset > MaxOffset {
		return fmt.Errorf("OFFSET 上限 %d，更深的翻页请改用 keyset（before_end_ts + before_id）", MaxOffset)
	}
	hasFilter := q.BusinessID != "" || q.NodeID != "" || q.ClientIP != "" || q.SNI != ""
	// 无任何筛选时必须缩小时间窗，否则等于全量扫描。
	if !hasFilter && q.To.Sub(q.From) > NarrowWindow {
		return fmt.Errorf("未指定任何筛选条件时，时间跨度上限 %s。"+
			"请至少指定 业务 ID / 节点 / 客户端 IP / SNI 之一，或把时间范围缩小到 1 小时内", NarrowWindow)
	}
	return nil
}

// Explain 如实回报"这次查询到底打开了哪些分片"，供界面展示，也是"不扫全历史"的证据。
type Explain struct {
	Partitions []Partition `json:"partitions"`
	Truncated  bool        `json:"truncated"` // 是否因压缩归档需要先解压
	Notes      []string    `json:"notes"`
}

// ConnRowOut 查询结果行（列名与前端表格一一对应；nil 表示不可用）。
type ConnRowOut struct {
	ID            int64  `json:"id"`
	EndTS         string `json:"end_ts"`
	AcceptTS      string `json:"accept_ts"`
	NodeID        string `json:"node_id"`
	BusinessID    string `json:"business_id"`
	RouteID       string `json:"route_id"`
	ConfigVersion int    `json:"config_version"`
	ConnID        string `json:"conn_id"`
	ClientIP      string `json:"client_ip"`
	ClientPort    *int   `json:"client_port"`
	EntryAddr     string `json:"entry_addr"`
	EntryPort     *int   `json:"entry_port"`
	SNI           string `json:"sni"`
	BeName        string `json:"be_name"`
	SrvName       string `json:"srv_name"`
	OriginAddr    string `json:"origin_addr"`
	OriginPort    *int   `json:"origin_port"`
	// OriginReported 为 true 表示源站地址来自 HAProxy 实报（%si/%sp），
	// 而不是按配置反查。界面据此区分"实际连到哪里"与"配置写的是哪里"。
	OriginReported bool     `json:"origin_reported"`
	QueueMS        *int64   `json:"queue_ms"`
	ConnectMS      *int64   `json:"connect_ms"`
	SessionMS      *int64   `json:"session_ms"`
	BytesUp        *int64   `json:"bytes_up"`
	BytesDown      *int64   `json:"bytes_down"`
	TermCode       string   `json:"term_code"`
	TermReason     string   `json:"term_reason"`
	Retries        *int     `json:"retries"`
	QueueMax       *int     `json:"queue_max"`
	Unavailable    []string `json:"unavailable"`
	ParseOK        bool     `json:"parse_ok"`
	ParseErr       string   `json:"parse_err"`
	RawLine        string   `json:"raw_line"`
}

// connCols 是查询用的列清单，顺序必须与 queryAcross 的 Scan 一一对应。
const connCols = `id, end_ts, accept_ts, node_id, business_id, route_id, config_version, conn_id,
        client_ip, client_port, entry_addr, entry_port, sni, be_name, srv_name,
        origin_addr, origin_port, origin_reported, queue_ms, connect_ms, session_ms, bytes_up, bytes_down,
        term_code, term_reason, retries, queue_max, unavailable, parse_ok, parse_err, raw_line`

// QueryConn 执行连接日志查询。
func (s *Store) QueryConn(ctx context.Context, q Query) ([]ConnRowOut, Explain, error) {
	var ex Explain
	if err := q.Validate(); err != nil {
		return nil, ex, err
	}
	parts := s.PartitionsBetween(KindConn, q.From, q.To)
	ex.Partitions = parts
	if len(parts) == 0 {
		ex.Notes = append(ex.Notes, "查询区间内没有日志分片（可能是这段确实没有连接，也可能是日志未上报）")
		return nil, ex, nil
	}

	var (
		open    []*sql.DB
		release []func()
	)
	defer func() {
		for _, f := range release {
			f()
		}
	}()

	where, wargs := buildWhere(q)
	for _, p := range parts {
		path := p.Path
		var cleanup func()
		if p.Compressed {
			// 压缩分片需要先解压到一个临时文件才能用 SQL 查。
			// 不静默跳过：解压失败要如实报出来（否则运维会以为"这段时间没有连接"）。
			tmp, derr := materializeCompressed(p.Path)
			if derr != nil {
				ex.Notes = append(ex.Notes, "压缩分片解压失败（已跳过）："+filepath.Base(p.Path)+" —— "+derr.Error())
				continue
			}
			path, cleanup = tmp, func() { _ = os.Remove(tmp) }
			ex.Notes = append(ex.Notes, "分片 "+filepath.Base(p.Path)+" 为压缩归档，本次查询先解压后读取（结果一致，代价是本次查询更慢）")
		}
		d, rel, note, err := s.acquireReader(path)
		if err != nil {
			// 单个分片读不了不该让整个查询失败 —— 但必须如实报告。
			ex.Notes = append(ex.Notes, "分片打开失败（已跳过）："+filepath.Base(p.Path)+" —— "+err.Error())
			if cleanup != nil {
				cleanup()
			}
			continue
		}
		if note != "" {
			ex.Notes = append(ex.Notes, note)
		}
		// 分片文件可能刚刚被创建、还来不及建表（写入方先建文件再执行 DDL），
		// 也可能因为进程被中断而留下一个空壳。这两种情况都**不是查询失败**，
		// 而是"这一片暂时没有可用数据"。直接查会得到 "no such table: conn_log"，
		// 用户看到的就是一句莫名其妙的 SQL 报错 —— 那会把人引向完全错误的方向。
		if ok, cerr := hasConnTable(d); cerr != nil || !ok {
			rel()
			if cleanup != nil {
				cleanup()
			}
			ex.Notes = append(ex.Notes,
				"分片尚未初始化完成或为空，已跳过："+filepath.Base(p.Path))
			continue
		}
		open = append(open, d)
		release = append(release, rel)
		if cleanup != nil {
			release = append(release, cleanup)
		}
	}
	if len(open) == 0 {
		return nil, ex, nil
	}
	rows, err := s.queryAcross(ctx, open, where, wargs, q, connCols)
	if err != nil {
		return nil, ex, err
	}
	return rows, ex, nil
}

// queryAcross 逐分片查询后归并排序。
//
// 为什么不用 ATTACH + UNION ALL：跨库 UNION 的查询计划不可控，
// 而且只读连接 ATTACH 另一只读库在部分版本上会报错。逐片查询 + k 路归并：
//   - 每片最多取 limit+offset 条（各片内部走 ix_*_ts 索引，不扫全表）；
//   - 归并后截取需要的窗口；
//   - 分片数 = 时间窗小时数，天然有界（≤24）。
func (s *Store) queryAcross(ctx context.Context, shards []*sql.DB, where string, wargs []any, q Query, cols string) ([]ConnRowOut, error) {
	perShard := q.Limit + q.Offset
	var all []ConnRowOut
	for _, d := range shards {
		sqlText := "SELECT " + cols + " FROM conn_log " + where +
			" ORDER BY end_ts DESC, id DESC LIMIT " + strconv.Itoa(perShard)
		rs, err := d.QueryContext(ctx, sqlText, wargs...)
		if err != nil {
			return nil, fmt.Errorf("logstore: 查询分片失败: %w", err)
		}
		for rs.Next() {
			var r ConnRowOut
			var (
				clientPort, entryPort, originPort sql.NullInt64
				queueMS, connectMS, sessionMS     sql.NullInt64
				bytesUp, bytesDown                sql.NullInt64
				retries, queueMax                 sql.NullInt64
				parseOK, originReported           int
				unavail                           string
			)
			if err := rs.Scan(&r.ID, &r.EndTS, &r.AcceptTS, &r.NodeID, &r.BusinessID, &r.RouteID,
				&r.ConfigVersion, &r.ConnID, &r.ClientIP, &clientPort, &r.EntryAddr, &entryPort,
				&r.SNI, &r.BeName, &r.SrvName, &r.OriginAddr, &originPort, &originReported,
				&queueMS, &connectMS, &sessionMS, &bytesUp, &bytesDown,
				&r.TermCode, &r.TermReason, &retries, &queueMax, &unavail,
				&parseOK, &r.ParseErr, &r.RawLine); err != nil {
				_ = rs.Close()
				return nil, fmt.Errorf("logstore: 扫描结果失败: %w", err)
			}
			r.ClientPort = nullInt(clientPort)
			r.EntryPort = nullInt(entryPort)
			r.OriginPort = nullInt(originPort)
			r.OriginReported = originReported == 1
			r.QueueMS = nullI64(queueMS)
			r.ConnectMS = nullI64(connectMS)
			r.SessionMS = nullI64(sessionMS)
			r.BytesUp = nullI64(bytesUp)
			r.BytesDown = nullI64(bytesDown)
			r.Retries = nullInt(retries)
			r.QueueMax = nullInt(queueMax)
			r.ParseOK = parseOK == 1
			if unavail != "" {
				r.Unavailable = strings.Split(unavail, ",")
			}
			all = append(all, r)
		}
		if err := rs.Err(); err != nil {
			_ = rs.Close()
			return nil, err
		}
		_ = rs.Close()
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].EndTS != all[j].EndTS {
			return all[i].EndTS > all[j].EndTS
		}
		return all[i].ID > all[j].ID
	})
	if q.Offset >= len(all) {
		return nil, nil
	}
	end := q.Offset + q.Limit
	if end > len(all) {
		end = len(all)
	}
	return all[q.Offset:end], nil
}

func buildWhere(q Query) (string, []any) {
	var conds []string
	var args []any
	conds = append(conds, "end_ts >= ?", "end_ts <= ?")
	args = append(args, fmtTime(q.From), fmtTime(q.To))
	if q.BusinessID != "" {
		conds = append(conds, "business_id = ?")
		args = append(args, q.BusinessID)
	}
	if q.NodeID != "" {
		conds = append(conds, "node_id = ?")
		args = append(args, q.NodeID)
	}
	if q.ClientIP != "" {
		conds = append(conds, "client_ip = ?")
		args = append(args, q.ClientIP)
	}
	if q.SNI != "" {
		conds = append(conds, "sni = ?")
		args = append(args, q.SNI)
	}
	if q.OnlyAnomaly {
		// 异常 = 解析失败，或有原始错误串，或有非正常终止码
		// 注意：这里**不判断"是否被封"** —— 那只是一种可能，不是结论。
		conds = append(conds, "(parse_ok = 0 OR term_reason <> '' OR (term_code <> '' AND term_code NOT IN ('--','-','SC')))")
	}
	if q.BeforeEndTS != "" {
		conds = append(conds, "(end_ts < ? OR (end_ts = ? AND id < ?))")
		args = append(args, q.BeforeEndTS, q.BeforeEndTS, q.BeforeID)
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

// ---------------------------------------------------------------------------
// 容量控制：保留期 + 磁盘配额
// ---------------------------------------------------------------------------

// usageOf 供配额统计与界面展示。
type KindUsage struct {
	Kind       Kind      `json:"kind"`
	Partitions int       `json:"partitions"`
	Bytes      int64     `json:"bytes"`
	Oldest     time.Time `json:"oldest"`
	Newest     time.Time `json:"newest"`
}

// Usage 统计各日志流的占用（界面"日志容量"卡片用）。
func (s *Store) Usage(kind Kind) (KindUsage, error) {
	parts, err := s.listPartitions(kind)
	if err != nil {
		return KindUsage{}, err
	}
	u := KindUsage{Kind: kind, Partitions: len(parts)}
	for _, p := range parts {
		u.Bytes += p.Bytes
	}
	if len(parts) > 0 {
		u.Oldest = parts[0].Start
		u.Newest = parts[len(parts)-1].Start
	}
	return u, nil
}

func (s *Store) listPartitions(kind Kind) ([]Partition, error) {
	base := filepath.Join(s.Root, string(kind))
	var out []Partition
	err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			return nil
		}
		name := info.Name()
		if !strings.HasSuffix(name, ".db") && !strings.HasSuffix(name, ".db.gz") {
			return nil
		}
		st, perr := parsePartitionStart(kind, path)
		if perr != nil {
			return nil // 无法识别的文件不参与清理（宁可留着，也不能误删）
		}
		out = append(out, Partition{
			Kind: kind, Start: st, Path: path,
			Compressed: strings.HasSuffix(name, ".gz"),
			Bytes:      info.Size(),
		})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

// parsePartitionStart 从路径反推分片起始时间（<root>/<kind>/YYYY/MM/DD/HH.db[.gz]）。
func parsePartitionStart(kind Kind, path string) (time.Time, error) {
	p := filepath.ToSlash(path)
	name := filepath.Base(p)
	name = strings.TrimSuffix(name, ".gz")
	name = strings.TrimSuffix(name, ".db")
	hh, err := strconv.Atoi(name)
	if err != nil {
		return time.Time{}, err
	}
	dir := filepath.Dir(p)
	dd := filepath.Base(dir)
	dir = filepath.Dir(dir)
	mm := filepath.Base(dir)
	dir = filepath.Dir(dir)
	yyyy := filepath.Base(dir)
	if filepath.Base(dir) == string(kind) || yyyy == string(kind) {
		// 路径层级不对（例如 <kind>/2026/09/21.db 这种旧布局）→ 视为不可识别
	}
	y, err1 := strconv.Atoi(yyyy)
	m, err2 := strconv.Atoi(mm)
	day, err3 := strconv.Atoi(dd)
	if err1 != nil || err2 != nil || err3 != nil {
		return time.Time{}, fmt.Errorf("无法从路径解析分片时间: %s", path)
	}
	return time.Date(y, time.Month(m), day, hh, 0, 0, 0, time.UTC), nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func intPtr(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func i64Ptr(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	i := int(v.Int64)
	return &i
}

func nullI64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	i := v.Int64
	return &i
}

func truncateBytes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	// 按字节截断可能切断一个 UTF-8 字符，往前退到合法边界。
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…[truncated]"
}

func fileSize(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}
