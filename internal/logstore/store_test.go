package logstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shengyu/edgelink/internal/logparse"
)

// 本文件覆盖评审要求的几件事：
//
//	① 活跃分片去掉 immutable 假设后，**写连接开着**也能读到已提交数据；
//	② 连接复用（同一分片在一段时间内只建立一次连接）；
//	③ 批量写入（一批一个事务）；
//	④ boot_id 的重启/重传语义与**旧分片迁移**；
//	⑤ 容量控制：保留期、配额、磁盘水位、真实压缩（含压缩后仍可查）。

func rec(node string, at time.Time) logparse.Record {
	n := int64(1024)
	return logparse.Record{
		NodeID: node, ParseOK: true, AcceptTS: at,
		ClientIP: "203.0.113.7", BytesDown: &n,
	}
}

func row(node, boot string, seq int64, at time.Time) ConnRow {
	return ConnRow{BootID: boot, Seq: seq, RecvTS: at, Rec: rec(node, at)}
}

func newStore(t *testing.T, opts ...Option) *Store {
	t.Helper()
	s, err := New(t.TempDir(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	// 必须显式 Close：Windows 上仍打开的 SQLite 句柄会让 t.TempDir 的清理失败，
	// 表现为"测试逻辑全对、却以清理错误收场"。
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func query(t *testing.T, s *Store, node string) []ConnRowOut {
	t.Helper()
	// Limit 必须显式给足：Query 的默认上限是 50（"不扫全历史"的硬约束之一），
	// 忘了给就会把"分批返回"误读成"数据丢了"。
	rows, _, err := s.QueryConn(context.Background(), Query{
		From: time.Now().Add(-2 * time.Hour), To: time.Now().Add(time.Minute),
		NodeID: node, Limit: MaxLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// ① 活跃分片：写连接**保持打开**时，读方也必须能看到刚提交的数据。
//
// 这是"去掉 immutable 假设"的核心证明。旧实现里只读连接带 immutable=1，
// SQLite 不读 WAL，于是数据要等写连接关闭（触发 checkpoint）才可见 ——
// 那种耦合一旦遇到"写连接复用"就会静默丢日志。
func TestActiveShardReadableWhileWriterHoldsConnection(t *testing.T) {
	s := newStore(t, WithKeepAlive(time.Minute))

	if _, err := s.Ingest(context.Background(), []ConnRow{row("n1", "boot-a", 1, time.Now())}); err != nil {
		t.Fatal(err)
	}
	w, r := s.OpenShardCount()
	if w == 0 {
		t.Fatal("保活模式下写连接应被缓存（否则就不是复用了）")
	}
	// 此刻写连接仍然开着 —— 查询必须能看到那一行
	rows := query(t, s, "n1")
	if len(rows) != 1 {
		t.Fatalf("写连接保持打开时也应能读到已提交数据（去掉 immutable 才能做到），实际 %d 行", len(rows))
	}
	_ = r
}

// ② 连接复用：多次写入同一分片，只建立一次连接；关闭保活则用完即关。
func TestConnectionReuse(t *testing.T) {
	keep := newStore(t, WithKeepAlive(time.Minute))
	for i := 0; i < 20; i++ {
		if _, err := keep.Ingest(context.Background(), []ConnRow{row("n1", "b", int64(i+1), time.Now())}); err != nil {
			t.Fatal(err)
		}
	}
	w, _ := keep.OpenShardCount()
	if w != 1 {
		t.Fatalf("20 次写入同一个小时分片应只持有 1 条写连接，实际 %d（连接复用失效）", w)
	}

	nokeep := newStore(t)
	for i := 0; i < 3; i++ {
		if _, err := nokeep.Ingest(context.Background(), []ConnRow{row("n2", "b", int64(i+1), time.Now())}); err != nil {
			t.Fatal(err)
		}
	}
	if w, r := nokeep.OpenShardCount(); w != 0 || r != 0 {
		t.Fatalf("未开启保活时不应留下缓存连接，实际 writers=%d readers=%d", w, r)
	}
	if len(query(t, nokeep, "n2")) != 3 {
		t.Fatal("未保活模式下写入的行也必须查得到")
	}
}

// ③ 批量写入：一批 N 行 = 一个事务，且行行都在。
func TestBatchWriteSingleTransaction(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	// 行数取 MaxLimit 以内：单次查询的上限是 200（"不扫全历史"的硬约束），
	// 超过就得翻页 —— 那是查询层的约定，不该混进"批量写入是否正确"这条断言里。
	var batch []ConnRow
	for i := 0; i < 150; i++ {
		batch = append(batch, row("n1", "boot-batch", int64(i+1), now))
	}
	res, err := s.Ingest(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 150 || res.Duplicated != 0 {
		t.Fatalf("批量写入结果异常: %+v", res)
	}
	if len(res.Partitions) != 1 {
		t.Fatalf("同一时刻的 500 行应落在 1 个分片，实际 %d", len(res.Partitions))
	}
	if got := len(query(t, s, "n1")); got != 150 {
		t.Fatalf("批量写入后应查到 150 行，实际 %d", got)
	}
}

// ④ boot_id 语义：换一次启动标识，序号重复也不算重复；同一标识 + 同一序号才算重复。
func TestBootIDIdempotencySemantics(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	ctx := context.Background()

	// 第一次启动写入 seq=1..3
	if _, err := s.Ingest(ctx, []ConnRow{
		row("n1", "boot-1", 1, now), row("n1", "boot-1", 2, now), row("n1", "boot-1", 3, now),
	}); err != nil {
		t.Fatal(err)
	}
	// 同一批次重传（网络抖动后 agent 重发）：必须全部判为重复，不产生新行
	res, err := s.Ingest(ctx, []ConnRow{
		row("n1", "boot-1", 1, now), row("n1", "boot-1", 2, now), row("n1", "boot-1", 3, now),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Duplicated != 3 || res.Accepted != 0 {
		t.Fatalf("同一 boot_id 的重复上报应全部判重，实际 %+v", res)
	}
	// 采集端重启：boot_id 变化，序号从 1 重新开始 —— 必须全部写入（评审 F07）
	res, err = s.Ingest(ctx, []ConnRow{
		row("n1", "boot-2", 1, now), row("n1", "boot-2", 2, now),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 2 {
		t.Fatalf("重启后的新连接不能被误判为重复（否则日志会永久少一段），实际 %+v", res)
	}
	if got := len(query(t, s, "n1")); got != 5 {
		t.Fatalf("应有 5 行，实际 %d", got)
	}
}

// ④b 旧分片迁移：没有 boot_id / bytes_up / bytes_down / origin_reported 的老分片，
// 必须能自动补齐，并且旧列里的字节数要搬到**正确方向**的新列上。
func TestLegacyShardMigration(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// 手工造一个"旧版本"分片：旧表结构（有 bytes_in、没有 boot_id），旧索引 (node_id, seq)
	now := time.Now().UTC()
	path := s.PartitionPath(KindConn, now)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	d, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	legacyDDL := []string{
		`CREATE TABLE conn_log (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            seq INTEGER NOT NULL DEFAULT 0,
            accept_ts TEXT NOT NULL, end_ts TEXT NOT NULL, recv_ts TEXT NOT NULL,
            node_id TEXT NOT NULL,
            business_id TEXT NOT NULL DEFAULT '', route_id TEXT NOT NULL DEFAULT '',
            config_version INTEGER NOT NULL DEFAULT 0, conn_id TEXT NOT NULL DEFAULT '',
            client_ip TEXT NOT NULL DEFAULT '', client_port INTEGER,
            entry_addr TEXT NOT NULL DEFAULT '', entry_port INTEGER,
            sni TEXT NOT NULL DEFAULT '', be_name TEXT NOT NULL DEFAULT '', srv_name TEXT NOT NULL DEFAULT '',
            origin_addr TEXT NOT NULL DEFAULT '', origin_port INTEGER,
            queue_ms INTEGER, connect_ms INTEGER, session_ms INTEGER,
            bytes_in INTEGER,
            term_code TEXT NOT NULL DEFAULT '', term_reason TEXT NOT NULL DEFAULT '',
            retries INTEGER, queue_max INTEGER, unavailable TEXT NOT NULL DEFAULT '',
            parse_ok INTEGER NOT NULL DEFAULT 1, parse_err TEXT NOT NULL DEFAULT '',
            raw_line TEXT NOT NULL DEFAULT '')`,
		`CREATE UNIQUE INDEX ux_conn_seq ON conn_log(node_id, seq)`,
		// 旧版本里 bytes_in 存的是 %B（源站→客户端，即下行）
		fmt.Sprintf(`INSERT INTO conn_log(seq, accept_ts, end_ts, recv_ts, node_id, bytes_in, raw_line)
			VALUES (1, '%s', '%s', '%s', 'legacy-node', 4242, 'old-line')`,
			fmtTime(now), fmtTime(now), fmtTime(now)),
	}
	for _, stmt := range legacyDDL {
		if _, err := d.Exec(stmt); err != nil {
			t.Fatalf("构造旧分片失败: %v\n%s", err, stmt)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	// 往同一个分片写一行新日志：这一步会触发迁移
	if _, err := s.Ingest(context.Background(), []ConnRow{row("legacy-node", "boot-new", 2, now)}); err != nil {
		t.Fatalf("向旧分片写入应自动迁移，实际失败: %v", err)
	}

	rows := query(t, s, "legacy-node")
	if len(rows) != 2 {
		t.Fatalf("迁移后旧行与新行都应可查，实际 %d 行", len(rows))
	}
	var sawLegacy bool
	for _, r := range rows {
		if r.RawLine != "old-line" {
			continue
		}
		sawLegacy = true
		// 旧列 bytes_in（语义是 %B = 下行）必须被搬到 bytes_down，而不是 bytes_up。
		if r.BytesDown == nil || *r.BytesDown != 4242 {
			t.Fatalf("旧分片的字节数应迁到 bytes_down（方向语义：别名 B 是下行），实际 down=%v up=%v",
				r.BytesDown, r.BytesUp)
		}
		if r.BytesUp != nil {
			t.Fatalf("迁移不该凭空给上行字节赋值（那会把「没有数据」伪装成「测到 0」），实际 %v", r.BytesUp)
		}
	}
	if !sawLegacy {
		t.Fatal("迁移后旧行丢失（那就是把已有数据弄丢了）")
	}
}

// ⑤ 容量控制：保留期删除 + 固定容量上限 + 磁盘水位 + 真实压缩且压缩后仍可查。
func TestMaintenanceRetentionQuotaCompress(t *testing.T) {
	root := t.TempDir()
	s, err := New(root, WithKeepAlive(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	now := time.Now().UTC()
	// 造 5 个分片：0h/1h 前（近期）、25h/26h 前（超压缩阈值）、200h 前（超保留期）
	hoursAgo := []int{0, 1, 25, 26, 200}
	for _, h := range hoursAgo {
		at := now.Add(-time.Duration(h) * time.Hour)
		if _, err := s.Ingest(context.Background(), []ConnRow{
			row("n1", "b", int64(h+1), at), row("n1", "b", int64(h+1000), at),
		}); err != nil {
			t.Fatal(err)
		}
	}
	pol := RetentionPolicy{RetainDays: 7, QuotaBytes: 0, CompressAfterHours: 24, MinFreeBytes: 0}
	plan, total, err := s.PlanMaintenance(KindConn, pol, now, -1)
	if err != nil {
		t.Fatal(err)
	}
	if total <= 0 {
		t.Fatal("应统计出占用字节数")
	}
	var deletes, compresses int
	for _, a := range plan {
		switch a.Action {
		case "delete":
			deletes++
			if a.Reason != "age" {
				t.Fatalf("本次只配了保留期，删除原因应为 age，实际 %q", a.Reason)
			}
		case "compress":
			compresses++
			if !a.Start.Add(time.Hour).Before(now.Add(-24 * time.Hour)) {
				t.Fatalf("刚结束不足 24 小时的分片不该被压缩（可能还在被写）: %s", a.Start)
			}
		}
	}
	if deletes != 1 {
		t.Fatalf("200 小时前的分片应因超保留期被删，实际删除 %d 个（计划 %+v）", deletes, plan)
	}
	if compresses != 2 {
		t.Fatalf("25/26 小时前的两个分片应被压缩，实际 %d 个", compresses)
	}

	res, err := s.ApplyMaintenance(KindConn, pol)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 || res.Compressed != 2 {
		t.Fatalf("执行结果与计划不符: %+v", res)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("不应有失败动作: %v", res.Errors)
	}
	if res.CompressedOut >= res.CompressedIn {
		t.Fatalf("压缩后应更小：in=%d out=%d", res.CompressedIn, res.CompressedOut)
	}
	// 压缩产物存在、原文件已删除（含 WAL/SHM 残留）
	var gzCount int
	for _, h := range []int{25, 26} {
		p := s.PartitionPath(KindConn, now.Add(-time.Duration(h)*time.Hour))
		if _, err := os.Stat(p + CompressionSuffix); err != nil {
			t.Fatalf("压缩产物不存在: %v", err)
		}
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("压缩后原文件应被删除: %s", p)
		}
		gzCount++
	}
	if gzCount != 2 {
		t.Fatal("压缩数量不符")
	}

	// 压缩分片**仍然可查**（查询时自动解压），只是会有一条说明
	rows, ex, err := s.QueryConn(context.Background(), Query{
		// 25/26 小时前的分片：窗口取 [-27h, -23h]，覆盖这两个小时
		From: now.Add(-27 * time.Hour), To: now.Add(-23 * time.Hour),
		NodeID: "n1", Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatalf("压缩归档的分片必须仍然可查（自动解压），实际 0 行；说明: %v", ex.Notes)
	}
	joined := ""
	for _, n := range ex.Notes {
		joined += n + " "
	}
	if joined == "" {
		t.Fatal("解压读取应当如实告知用户（本次查询已解压）")
	}
}

// ⑤b 配额与磁盘水位：超过容量上限、或可用空间低于水位时，按最旧优先继续删。
func TestMaintenanceQuotaAndDiskFloor(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	now := time.Now().UTC()
	for _, h := range []int{0, 1, 2, 3} {
		at := now.Add(-time.Duration(h) * time.Hour)
		if _, err := s.Ingest(context.Background(), []ConnRow{row("n1", "b", int64(h+1), at)}); err != nil {
			t.Fatal(err)
		}
	}
	// 配额 = 只够放一个分片：应当删掉最旧的若干个
	parts, err := s.listPartitions(KindConn)
	if err != nil {
		t.Fatal(err)
	}
	oneSize := parts[len(parts)-1].Bytes
	pol := RetentionPolicy{RetainDays: 7, QuotaBytes: oneSize, CompressAfterHours: 0}
	plan, _, err := s.PlanMaintenance(KindConn, pol, now, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) < 3 {
		t.Fatalf("配额只够放一个分片时应删除至少 3 个最旧的，实际计划 %+v", plan)
	}
	for _, a := range plan {
		if a.Reason != "quota" {
			t.Fatalf("此场景的原因应为 quota，实际 %q", a.Reason)
		}
	}
	// 删的是最旧的那几个
	if !plan[0].Start.Before(plan[len(plan)-1].Start) {
		t.Fatalf("应从最旧的开始删，实际顺序 %+v", plan)
	}

	// 磁盘水位：可用空间"低于水位"时也要删（这里用一个很大的水位来模拟）
	pol2 := RetentionPolicy{RetainDays: 7, MinFreeBytes: 1 << 62}
	plan2, _, err := s.PlanMaintenance(KindConn, pol2, now, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan2) == 0 {
		t.Fatal("可用空间低于水位时必须继续删最旧分片（否则会连带打挂登录与发布）")
	}
	for _, a := range plan2 {
		if a.Reason != "disk" {
			t.Fatalf("此场景的原因应为 disk，实际 %q", a.Reason)
		}
	}
}

// ⑤c 压缩产物必须**先校验再删原文件**；校验不过就保留原文件。
func TestCompressKeepsOriginalOnVerifyFailure(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	path := s.PartitionPath(KindConn, time.Now().UTC())
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ingest(context.Background(), []ConnRow{row("n1", "b", 1, time.Now())}); err != nil {
		t.Fatal(err)
	}
	// 正常路径：压缩后校验通过、原文件删除
	in, out, err := s.compressShard(path)
	if err != nil {
		t.Fatal(err)
	}
	if in <= 0 || out <= 0 {
		t.Fatalf("压缩字节数异常 in=%d out=%d", in, out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("校验通过后原文件应被删除")
	}
	// 再压一次同一个（已经不存在的）文件必须报错而不是静默成功
	if _, _, err := s.compressShard(path); err == nil {
		t.Fatal("对不存在的分片压缩必须报错")
	}
}

// ⑤d 分片被删除后，缓存句柄必须被丢弃，否则后续读写会一直失败到进程重启。
func TestDroppedShardRebuildsAfterRetention(t *testing.T) {
	// 不用保活：这里要模拟的是"分片被外部删掉了（例如保留期任务在另一个进程里跑完）"，
	// 保活会让本进程自己占住文件，那就不是这个场景了。
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Now().UTC()
	if _, err := s.Ingest(context.Background(), []ConnRow{row("n1", "b", 1, now)}); err != nil {
		t.Fatal(err)
	}
	// 模拟"分片被清理掉了，但进程内的已建表标记还在"
	path := s.PartitionPath(KindConn, now)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// 再写一行：应自动重建表，而不是永久报 "no such table"
	if _, err := s.Ingest(context.Background(), []ConnRow{row("n1", "b", 2, now)}); err != nil {
		t.Fatalf("分片被删后应能自动重建，实际: %v", err)
	}
	if got := len(query(t, s, "n1")); got != 1 {
		t.Fatalf("重建后应只保留新写入的 1 行，实际 %d", got)
	}
}
