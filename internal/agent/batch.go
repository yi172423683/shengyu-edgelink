package agent

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shengyu/edgelink/internal/logstore"
)

// Batcher 把日志攒批后交给 Ingestor 一次写入。
//
// 为什么必须有它（而不只是"逐行调用 IngestLine"）：
//
//	TCP 连接日志是按连接产生的，转发平台的日志量直接跟连接量挂钩。
//	逐行写一次 = 一次事务 + 一次 WAL 提交（fsync 级别的代价），
//	量一上来，磁盘会把转发路径拖慢。攒批之后，N 行只付一次提交代价。
//
// 三条设计约束：
//
//	① **不阻塞转发**：Add 永不阻塞。日志接收是从 Unix 数据报套接字读出来的，
//	   一旦阻塞，内核接收缓冲区会溢出，丢的就是不可恢复的数据报。
//	② **不静默丢弃**：缓冲区超过上限时丢最旧的并计数，管理面能看到丢弃量。
//	   "溢出就丢"是可以接受的工程取舍，"溢出却装作没事"不是。
//	③ **可强制冲刷**：进程退出、以及需要"写完立刻可查"的场景都调 Flush。
type Batcher struct {
	Ingest *Ingestor
	// Size 攒够多少行就立即冲刷。
	Size int
	// Every 最长等待多久就冲刷（保证低流量时日志不会一直压在内存里）。
	Every time.Duration
	// MaxBuffer 内存中最多缓存多少行，超出则丢最旧并计数。
	MaxBuffer int

	mu       sync.Mutex
	flushMu  sync.Mutex
	flushReq chan struct{}
	buf      []string
	drops    atomic.Int64
	flushes  atomic.Int64
	lastErr  atomic.Value // string
}

// NewBatcher 创建攒批器，默认值面向"每秒数百到数千行"的节点规模。
func NewBatcher(ing *Ingestor) *Batcher {
	return &Batcher{Ingest: ing, Size: 512, Every: time.Second, MaxBuffer: 20000, flushReq: make(chan struct{}, 1)}
}

// Add 追加一行。永不阻塞。
func (b *Batcher) Add(line string) {
	b.mu.Lock()
	if len(b.buf) >= b.MaxBuffer {
		// 丢最旧的：最新日志的排障价值高于最旧的。
		b.buf = b.buf[1:]
		b.drops.Add(1)
	}
	b.buf = append(b.buf, line)
	n := len(b.buf)
	b.mu.Unlock()
	if n >= b.Size {
		select {
		case b.flushReq <- struct{}{}:
		default:
		}
	}
}

// Flush 立刻写入当前缓冲区。
func (b *Batcher) Flush(ctx context.Context) (logstore.IngestResult, error) {
	b.flushMu.Lock()
	defer b.flushMu.Unlock()
	b.mu.Lock()
	lines := b.buf
	b.buf = nil
	b.mu.Unlock()
	if len(lines) == 0 {
		return logstore.IngestResult{}, nil
	}
	b.flushes.Add(1)
	res, err := b.Ingest.IngestLines(lines)
	if err != nil {
		b.lastErr.Store(err.Error())
		return res, err
	}
	return res, nil
}

// Run 周期性冲刷，直到 ctx 结束；退出前一定做最后一次冲刷。
//
// "退出前冲刷"这一条是硬要求：日志已经收进内存了，最后不写盘等于在停机时丢一段数据。
func (b *Batcher) Run(ctx context.Context) {
	every := b.Every
	if every <= 0 {
		every = time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if _, err := b.Flush(context.Background()); err != nil {
				log.Printf("停止前冲刷日志失败: %v", err)
			}
			return
		case <-t.C:
			if _, err := b.Flush(ctx); err != nil {
				log.Printf("冲刷日志失败: %v", err)
			}
		case <-b.flushReq:
			if _, err := b.Flush(ctx); err != nil {
				log.Printf("冲刷日志失败: %v", err)
			}
		}
	}
}

// BatchStats 攒批器统计，供管理面展示"采集是否正常"。
type BatchStats struct {
	Pending      int    `json:"pending"`
	Flushes      int64  `json:"flushes"`
	BufferDrops  int64  `json:"buffer_drops"`
	LastError    string `json:"last_error,omitempty"`
	LastIngested string `json:"last_ingested,omitempty"`
}

// Stats 返回当前统计。
func (b *Batcher) Stats() BatchStats {
	b.mu.Lock()
	pending := len(b.buf)
	b.mu.Unlock()
	s := BatchStats{
		Pending:     pending,
		Flushes:     b.flushes.Load(),
		BufferDrops: b.drops.Load(),
	}
	if v, ok := b.lastErr.Load().(string); ok {
		s.LastError = v
	}
	if !b.Ingest.LastIngestAt().IsZero() {
		s.LastIngested = b.Ingest.LastIngestAt().Format(time.RFC3339Nano)
	}
	return s
}
