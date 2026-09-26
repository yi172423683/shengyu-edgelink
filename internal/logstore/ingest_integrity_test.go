package logstore

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/shengyu/edgelink/internal/logparse"
)

// 本文件钉住"写入不丢行"与"按设计去重"这两件事。
//
// 为什么值得单独测：写入是**静默失败面最大**的一环 ——
// Ingest 用 `UNIQUE(node_id, boot_id, seq)` + `INSERT OR IGNORE` 做幂等，
// 任何 seq 分配失误或分片选路失误都会表现成"少了一行"，而且**不报错**。
// 真机 e2e 在 `-race` 下就出现过"两个并发会话只落一行"（见 docs/08 §10.7 的记录），
// 所以这里分三种情形分别钉住：
//
//	① 顺序写 N 行      ⇒ 必须查到 N 行；
//	② 并发写 N 行      ⇒ 必须查到 N 行（这条最容易漏）；
//	③ 同一 (node,boot,seq) 重复写 ⇒ 去重且**计入 Duplicated**（这是设计行为，不是丢数据）。

func ingestOne(t *testing.T, st *Store, node string, seq int64, sni string, at time.Time) {
	t.Helper()
	_, err := st.Ingest(context.Background(), []ConnRow{{
		BootID: "boot-1", Seq: seq, RecvTS: at,
		Rec: logparse.Record{NodeID: node, SNI: sni},
	}})
	if err != nil {
		t.Fatalf("写入第 %d 行失败: %v", seq, err)
	}
}

func countNodeRows(t *testing.T, st *Store, node string) int {
	t.Helper()
	now := time.Now()
	rows, _, err := st.QueryConn(context.Background(), Query{
		From: now.Add(-2 * time.Hour), To: now.Add(time.Hour),
		NodeID: node, Limit: MaxLimit,
	})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	return len(rows)
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// ① 顺序写：一行都不能少。
func TestIngestKeepsSequentialRows(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	const n = 5
	for i := 0; i < n; i++ {
		ingestOne(t, st, "node_seq", int64(i+1), fmt.Sprintf("h%d.example.com", i), now)
	}
	if got := countNodeRows(t, st, "node_seq"); got != n {
		t.Fatalf("顺序写入 %d 行，只查到 %d 行", n, got)
	}
}

// ② 并发写：这是最容易丢的一条（分片连接复用 + 建表竞争都在这一路径上）。
func TestIngestKeepsConcurrentRows(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	const n = 24

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 尽量让它们同时打进来
			ingestOne(t, st, "node_par", int64(i+1), fmt.Sprintf("p%d.example.com", i), now)
		}(i)
	}
	close(start)
	wg.Wait()

	if got := countNodeRows(t, st, "node_par"); got != n {
		t.Fatalf("并发写入 %d 行，只查到 %d 行 —— 写入路径在并发下丢了数据", n, got)
	}
}

// ③ 同一 (node, boot, seq) 重复写：必须**去重**，并计入 Duplicated。
//
// 这条与 ①② 的方向相反，但同样重要：如果它变成"不去重"，agent 重传就会产生重复行，
// 界面上出现同一连接的多个副本；反过来，如果它把**不同** seq 也判成重复（例如 seq 归零），
// 那就是真的丢数据 —— ①② 正是在防这一种。
func TestIngestDeduplicatesSameSeqOnly(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	ingestOne(t, st, "node_dup", 7, "dup.example.com", now)

	res, err := st.Ingest(context.Background(), []ConnRow{{
		BootID: "boot-1", Seq: 7, RecvTS: now,
		Rec: logparse.Record{NodeID: "node_dup", SNI: "dup.example.com"},
	}})
	if err != nil {
		t.Fatalf("重复写入不应报错（幂等是设计行为）: %v", err)
	}
	if res.Duplicated != 1 {
		t.Fatalf("同一 (node,boot,seq) 重复写必须计入 Duplicated，实际 %+v", res)
	}
	if got := countNodeRows(t, st, "node_dup"); got != 1 {
		t.Fatalf("重复 seq 应只留一行，实际 %d 行", got)
	}

	// 换一个 seq 就是新的一行（防止"去重"退化成"吞掉后续日志"）。
	ingestOne(t, st, "node_dup", 8, "dup2.example.com", now)
	if got := countNodeRows(t, st, "node_dup"); got != 2 {
		t.Fatalf("不同 seq 必须各自落盘，实际 %d 行", got)
	}
}
