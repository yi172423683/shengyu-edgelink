package logstore

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	"github.com/shengyu/edgelink/internal/db"
)

// 本文件负责分片连接的获取与回收 —— 也就是"连接复用"的落点。
//
// 设计要点（评审要求：去掉 immutable 假设、正确并发读 WAL、连接复用、批量写入）：
//
//	① 读侧不再用 immutable。只读连接正常读 WAL，因此**写连接可以被复用**而不必
//	   "写完就关"来触发 checkpoint。这是这次改动的因果核心，不是两个独立的优化。
//	② 写侧按分片复用。KeepAlive>0 时同一小时分片在整个使用期内只建立一次连接；
//	   保活时长可按部署调整，进程退出时由 Close 统一回收。
//	③ 批量写入由调用方（agent.Ingestor / agent.Batcher）攒批，
//	   一次 Ingest 一个事务；Store 这一层保证"一批一个事务"而不是"一行一个事务"。
//	④ 保留期/压缩删掉分片后必须丢弃缓存句柄，否则会在已删除文件上继续读写。

// acquireWriter 取得某分片的写连接，首次使用时建表并做列迁移。
//
// 返回的 release 必须在写完后调用：KeepAlive=0 时它会关闭连接（并且关闭动作
// 顺带触发 WAL checkpoint），KeepAlive>0 时它什么都不做，连接留在缓存里。
func (s *Store) acquireWriter(path string) (*sql.DB, func(), error) {
	if s.KeepAlive <= 0 {
		d, err := s.openWriteShard(path)
		if err != nil {
			return nil, nil, err
		}
		return d, func() { _ = d.Close() }, nil
	}

	s.mu.Lock()
	if d, ok := s.writers[path]; ok {
		s.touchLocked(path)
		s.mu.Unlock()
		return d, func() {}, nil
	}
	s.mu.Unlock()

	d, err := s.openWriteShard(path)
	if err != nil {
		return nil, nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// 双重检查：并发首次写入时只保留一个连接，另一个关掉。
	if old, ok := s.writers[path]; ok {
		_ = d.Close()
		s.touchLocked(path)
		return old, func() {}, nil
	}
	s.evictLocked()
	s.writers[path] = d
	s.lru = append(s.lru, path)
	return d, func() {}, nil
}

// openWriteShard 打开写连接并确保表结构就绪（含向旧分片的列迁移）。
//
// ⚠️ **必须串行执行**（s.openMu）。首次写入一个新分片时，会有多个 goroutine
// 同时走到这里（acquireWriter 的双重检查拦不住"都还没建表"这个窗口），
// 每人都执行一遍 CREATE TABLE / CREATE INDEX —— 而 SQLite 的 DDL 要写锁，
// 抢不到的那次直接报错，调用方把**整批**日志判失败并丢掉。
// 实测：并发写 24 行只落 16 行，且不报竞争告警（见 ingest_integrity_test.go）。
//
// 这一条不能靠"通常不会并发"来绕过：它只在"两个会话几乎同时首次写入同一分片"
// 时出现，而那一刻恰好是进程刚起来、日志最密的时候。
func (s *Store) openWriteShard(path string) (*sql.DB, error) {
	s.openMu.Lock()
	defer s.openMu.Unlock()

	d, err := db.Open(path, false)
	if err != nil {
		return nil, err
	}
	if s.schemaReady(path) {
		// 等锁期间别的 goroutine 已经把表建好了：这个连接可以直接用。
		return d, nil
	}
	if err := db.ApplySchema(d, connDDL); err != nil {
		_ = d.Close()
		return nil, err
	}
	if err := migrateConnShard(d); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("logstore: 迁移分片 %s 失败: %w", filepath.Base(path), err)
	}
	s.markSchemaReady(path)
	return d, nil
}

// acquireReader 取得某分片的读连接。
//
// 与写侧的关键差别：**读连接永远不加 immutable**（除非调用方显式要求归档模式）。
// 活跃分片随时可能被写入，immutable 会让 SQLite 跳过 WAL、读到过期快照 ——
// 那正是"刚结束的长连接查不到"的来源之一。见 db.Open 的说明。
//
// 返回的 release 必须调用；note 非空时表示这次打开做了降级（归档只读模式），
// 调用方应当把它如实报给界面，而不是悄悄降级。
func (s *Store) acquireReader(path string) (d *sql.DB, release func(), note string, err error) {
	if s.KeepAlive > 0 {
		s.mu.Lock()
		if cached, ok := s.readers[path]; ok {
			s.touchLocked(path)
			s.mu.Unlock()
			return cached, func() {}, "", nil
		}
		s.mu.Unlock()
	}
	opened, n, err := openReadShard(path)
	if err != nil {
		return nil, nil, "", err
	}
	if s.KeepAlive <= 0 {
		return opened, func() { _ = opened.Close() }, n, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.readers[path]; ok {
		_ = opened.Close()
		s.touchLocked(path)
		return old, func() {}, n, nil
	}
	s.evictLocked()
	s.readers[path] = opened
	s.lru = append(s.lru, path)
	return opened, func() {}, n, nil
}

// openReadShard 以只读方式打开分片。
//
// 正常情况下用 mode=ro（无 immutable）—— 能读 WAL、能看到其它连接的已提交数据。
// 若这一步失败（典型场景：分片目录被挂成只读，SQLite 无法创建/映射 -shm），
// 退到 immutable 归档模式并**返回说明**：这种降级是真实存在的数据可见性差异，
// 必须让运维看见（"这期间写入的数据我可能没看到"），不能静默。
func openReadShard(path string) (*sql.DB, string, error) {
	d, err := db.Open(path, true)
	if err == nil {
		return d, "", nil
	}
	arch, aerr := db.OpenImmutable(path)
	if aerr != nil {
		// 两种方式都打不开：返回原始错误（它更有诊断价值）。
		return nil, "", err
	}
	return arch, "分片以只读归档模式打开（无法读 WAL）：" +
		filepath.Base(path) + " —— 该分片在只读期间写入的数据本次查询不可见", nil
}

// schemaReady 判断该分片是否已经在本进程内建过表。
//
// 只缓存"建过表"这个事实，不重复执行 DDL —— 这是原来"每写一行重跑一遍
// CREATE TABLE/CREATE INDEX"那类热路径开销的根治办法。
func (s *Store) schemaReady(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.init[path]
}

func (s *Store) markSchemaReady(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init[path] = true
}

func (s *Store) forgetSchema(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.init, path)
}

// dropCached 丢弃某个分片的缓存句柄。分片被删除/压缩时必须调用，
// 否则缓存的连接会指向一个已经不存在的文件，后续读写全部失败到进程重启。
func (s *Store) dropCached(path string) {
	s.dropCachedLocked(path)
}

func (s *Store) dropCachedLocked(path string) {
	if d, ok := s.writers[path]; ok {
		delete(s.writers, path)
		_ = d.Close()
	}
	if d, ok := s.readers[path]; ok {
		delete(s.readers, path)
		_ = d.Close()
	}
	delete(s.init, path)
	for i, p := range s.lru {
		if p == path {
			s.lru = append(s.lru[:i], s.lru[i+1:]...)
			break
		}
	}
}

func (s *Store) touchLocked(path string) {
	for i, p := range s.lru {
		if p == path {
			s.lru = append(s.lru[:i], s.lru[i+1:]...)
			break
		}
	}
	s.lru = append(s.lru, path)
}

// evictLocked 淘汰最久未用的分片连接。调用时须持有 s.mu。
//
// 一台节点一小时只写一个分片，跨小时时最多同时有 2 个；
// MaxOpenShards 默认 8 是为了容忍"批量补传历史日志"这种一次跨多小时的场景。
func (s *Store) evictLocked() {
	total := len(s.writers) + len(s.readers)
	for total >= s.MaxOpenShards && len(s.lru) > 0 {
		oldest := s.lru[0]
		s.lru = s.lru[1:]
		if d, ok := s.writers[oldest]; ok {
			delete(s.writers, oldest)
			_ = d.Close()
			total--
		}
		if d, ok := s.readers[oldest]; ok {
			delete(s.readers, oldest)
			_ = d.Close()
			total--
		}
		delete(s.init, oldest)
	}
}

// CheckpointAll 把所有缓存写连接的 WAL 合并回主文件。
//
// 什么时候需要它：进程即将退出时。WAL 不合并并不丢数据（下次打开会自动恢复），
// 但合并之后分片是一个干净的单文件 —— 备份、拷走、用别的工具打开都更省事。
func (s *Store) CheckpointAll() error {
	s.mu.Lock()
	dbs := make([]*sql.DB, 0, len(s.writers))
	for _, d := range s.writers {
		dbs = append(dbs, d)
	}
	s.mu.Unlock()
	var firstErr error
	for _, d := range dbs {
		if _, err := d.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// OpenShardCount 返回当前缓存的写/读连接数（诊断与测试用）。
func (s *Store) OpenShardCount() (writers, readers int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writers), len(s.readers)
}

// keepAliveTick 仅用于测试注入时间的可读性。
var _ = time.Second
