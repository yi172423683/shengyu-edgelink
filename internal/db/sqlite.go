// Package db 是 SQLite 的唯一入口。
//
// 全平台只有这一个包 import 第三方 SQLite 驱动，好处：
//   - 驱动被换掉（例如换成 CGO 版或换 PostgreSQL）时只改这一处；
//   - 其他包只用 database/sql（标准库），保持可审计、可离线编译。
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动，无需 CGO，注册名为 "sqlite"
)

// writePragmas 是写入方必须设的 SQLite 参数（以 `?` 开头，因为是第一个参数）。
//
// 逐条说明（这些参数直接决定"日志写入会不会阻塞转发路径上的 SQLite"）：
//   - busy_timeout：遇到锁时最多等 5s 再报错，避免瞬时 SQLITE_BUSY 直接失败；
//   - synchronous=NORMAL：WAL 下的推荐值，掉电最多丢最近若干事务，但不会损坏库；
//   - temp_store=MEMORY / cache_size：分片查询常见「小表大扫」，放内存更快；
//   - foreign_keys=ON：让约束真的生效（SQLite 默认是关的，容易踩坑）。
//
// ⚠️ 这里**刻意不含 journal_mode(WAL)**，改由 ensureWAL 在打开后设一次。
// 原因见 ensureWAL 的说明：DSN 里的 pragma 会被**每个新物理连接**执行一次，
// 而切换 journal_mode 需要独占锁 —— 连接池一并发就互相抢锁并失败。
const writePragmas = "?_pragma=busy_timeout(5000)" +
	"&_pragma=synchronous(NORMAL)" +
	"&_pragma=temp_store(MEMORY)" +
	"&_pragma=cache_size(-16000)" +
	"&_pragma=foreign_keys(ON)"

// 只读方必须设的 pragma。
//
// ⚠️ **不能复用写入方那份**：里面有 `journal_mode(WAL)`，而 journal_mode 是**写操作** ——
// 在一个 `mode=ro` 的连接上设置它会被 SQLite 拒绝，表现为"打开成功、第一次查询就报错"。
// 更糟的是这个错误会被查询层当成"分片打不开"静默跳过，最终表现成
// 「日志明明写进去了却查不到，而且没有任何错误」。
// 这是把只读从 immutable 改成正常读 WAL 时最容易踩的一脚，写在这里免得再踩。
// readOnlyPragmas 是只读连接的附加参数（注意以 `&` 开头：调用方已经写了 `?mode=ro`）。
//
// 曾经的写法是把它也以 `?` 开头、直接拼在 `?mode=ro` 后面 —— 结果是
// `?mode=ro?_pragma=busy_timeout(5000)`：第二个问号之后的内容被当成了 **mode 的值**，
// 于是驱动拿到的 mode 是一串乱码，报出一个和"参数拼错"毫无关系的
// `SQL logic error: out of memory (1)`，而查询层又把它当成"分片打不开"静默跳过，
// 最终表现成「日志明明写进去了却查不到，且没有任何错误」。这类错误一定要在源头拦住。
const readOnlyPragmas = "&_pragma=busy_timeout(5000)" +
	"&_pragma=temp_store(MEMORY)" +
	"&_pragma=cache_size(-16000)"

// Open 打开（必要时创建）一个 SQLite 库。readOnly=true 时以只读打开。
//
// ⚠️ 只读打开的语义（评审"活跃分片必须去掉 immutable 假设"）：
//
//	旧实现在只读 DSN 上加了 `immutable=1`。immutable 的含义是
//	**"这个文件永远不会变"** —— SQLite 因此跳过加锁，并且**不读 WAL**。
//	对活跃分片（正在被写入的小时片）这是错的：已提交但还没 checkpoint 的数据
//	在 WAL 里，immutable 读者看不到，于是表现为"刚写进去的日志查不到"。
//	旧代码靠"写完就关连接、关闭时 checkpoint"来绕开，等于把连接生命周期
//	绑死成一个隐式的一致性协议 —— 一旦哪天为了性能把连接缓存起来，日志就会静默消失。
//
//	现在改为：只读打开**不加 immutable**，SQLite 正常读 WAL。
//	这样"写连接可以常驻"和"读方能立刻看到已提交数据"两件事同时成立。
//	代价是只读打开会在分片目录里创建/映射 -shm 文件（需要目录可写），
//	这在 /var/lib/shengyu-edgelink/logs（shengyu 用户所有）上是正常的；若目录不可写，
//	调用方会收到明确错误，可显式退到只读归档模式（见 logstore 的 fallback）。
//
// 仍然提供 Immutable 开关：它只用于**归档目录**（明确不会再写的分片，
// 例如挂载成只读的备份卷）。这不是默认路径，也不会被自动选中。
func Open(path string, readOnly bool) (*sql.DB, error) {
	return open(path, readOnly, false)
}

// OpenImmutable 以 immutable 模式只读打开 —— 仅用于确认不会再变的归档分片。
func OpenImmutable(path string) (*sql.DB, error) {
	return open(path, true, true)
}

func open(path string, readOnly, immutable bool) (*sql.DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("db: 路径不能为空")
	}
	if !readOnly {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("db: 创建目录失败: %w", err)
		}
	}
	dsn := "file:" + filepath.ToSlash(path)
	switch {
	case immutable:
		dsn += "?mode=ro&immutable=1&_pragma=busy_timeout(5000)&_pragma=temp_store(MEMORY)"
	case readOnly:
		// 只读但**不是** immutable：读 WAL、加共享锁、能看到其它连接的已提交数据。
		dsn += "?mode=ro" + readOnlyPragmas
	default:
		dsn += writePragmas
	}
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("db: 打开 %s 失败: %w", path, err)
	}
	// 单写少读：SQLite 的写事务本来就只有一个，1C1G 节点如果给每个分片
	// 开 8 条物理连接，会把 cache_size 和 modernc 的连接状态成倍放大。
	// 两条连接足够覆盖一个写事务和一个查询；高流量查询宁可排队，也不能
	// 把管理面和转发节点拖进内存回收风暴。
	d.SetMaxOpenConns(2)
	d.SetMaxIdleConns(1)
	d.SetConnMaxLifetime(time.Hour)

	if !readOnly {
		// 立刻做一次 ping + 建父目录检查，让"数据库不可用"在启动时就暴露，
		// 而不是等到第一条日志进来才发现。
		if err := d.Ping(); err != nil {
			_ = d.Close()
			return nil, fmt.Errorf("db: ping %s 失败: %w", path, err)
		}
		// WAL 只设一次（它是文件属性，而且切换动作要独占锁）—— 见 ensureWAL。
		if err := ensureWAL(path, d); err != nil {
			_ = d.Close()
			return nil, err
		}
	}
	return d, nil
}

// walMu / walEnsured 保证"每个库只确保一次 WAL 模式"。
//
// 为什么不能把 journal_mode(WAL) 留在 DSN 里：
//
//	journal_mode 是**持久化在数据库文件头**的属性 —— 设成 WAL 之后一直有效，
//	后面的连接不需要（也不应该）再设一次。而 `PRAGMA journal_mode=WAL` 本身是
//	**写操作、要求独占锁**，DSN 里的 pragma 又是**每个新物理连接**建立时都会执行一遍；
//	连接池一并发（写入侧 SetMaxOpenConns(8)）就会互相抢锁，
//	抢不到的那次报 `attempt to write a readonly database`。
//
//	这条报错的字面意思（"只读库"）会把排查带偏 —— 实际与文件权限无关。
//	实测后果：日志并发写入 24 行只落 16 行，而错误只出现在写入返回值里，
//	调用方把它记成 dropped 计数，界面上什么都看不出来（见 logstore 的并发写入测试）。
//
// 所以改成：进程内按路径只设一次，且用一把包级锁串行化"首次确保"这一步。
var (
	walMu      sync.Mutex
	walEnsured sync.Map
)

func ensureWAL(path string, d *sql.DB) error {
	if _, ok := walEnsured.Load(path); ok {
		return nil
	}
	walMu.Lock()
	defer walMu.Unlock()
	if _, ok := walEnsured.Load(path); ok {
		return nil
	}
	var mode string
	if err := d.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&mode); err != nil {
		return fmt.Errorf("db: 为 %s 设置 WAL 模式失败: %w", path, err)
	}
	walEnsured.Store(path, true)
	return nil
}

// ApplySchema 幂等执行一段 DDL。
func ApplySchema(d *sql.DB, ddl string) error {
	for _, stmt := range splitStatements(ddl) {
		if _, err := d.Exec(stmt); err != nil {
			return fmt.Errorf("db: 执行 DDL 失败: %w\n语句: %s", err, truncate(stmt, 200))
		}
	}
	return nil
}

// splitStatements 按分号切分 DDL。
// SQLite 的 DDL 里没有存储过程，所以按分号切分足够；
// 但仍然跳过只含空白/注释的片段，避免把注释当成语句执行。
func splitStatements(ddl string) []string {
	var out []string
	for _, raw := range strings.Split(ddl, ";") {
		s := strings.TrimSpace(raw)
		if s == "" || strings.HasPrefix(s, "--") && !strings.Contains(s, "\n") {
			continue
		}
		out = append(out, s)
	}
	return out
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
