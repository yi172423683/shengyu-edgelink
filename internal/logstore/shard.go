package logstore

import (
	"database/sql"
	"fmt"
	"strings"
)

// 本文件负责分片表结构的**向前迁移**。
//
// 为什么必须有迁移：分片是长期存在的文件（保留 7 天，可配置更长），
// `CREATE TABLE IF NOT EXISTS` 对**已经存在**的旧分片完全没有作用。
// 一旦改了表结构（例如给幂等键加 boot_id），旧分片就会继续用旧结构，
// 而写入方按新结构 INSERT —— 结果是新日志全部写入失败，且只在跨小时那一刻才暴露。
// 这类问题必须靠显式迁移解决，不能靠"反正是新库"。

// migrateConnShard 把旧版 conn_log 分片补齐到当前结构。
//
// 幂等：可以反复执行。
//
// 迁移清单（每加一列就要在这里登记，否则旧分片会在跨小时那一刻才开始写入失败）：
//
//	boot_id          幂等键的一部分：重启后的新连接不再被误判成重复（评审 F07）
//	bytes_up         上行字节（%U：客户端→源站）
//	bytes_down       下行字节（%B：源站→客户端）——由旧列 bytes_in 回填
//	origin_reported  源站地址是 HAProxy 实报还是平台反查
func migrateConnShard(d *sql.DB) error {
	// 旧分片没有 boot_id：补列并给默认值 ''。
	// 历史行的 boot_id 都是空串，而旧结构下 (node_id, seq) 本来就唯一，
	// 所以补完后 (node_id, boot_id, seq) 依然唯一，重建索引不会冲突。
	if err := addColumnIfMissing(d, "boot_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumnIfMissing(d, "bytes_up", "INTEGER"); err != nil {
		return err
	}
	if err := addColumnIfMissing(d, "bytes_down", "INTEGER"); err != nil {
		return err
	}
	if err := addColumnIfMissing(d, "origin_reported", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := backfillBytesDown(d); err != nil {
		return err
	}
	return ensureSeqIndex(d)
}

// addColumnIfMissing 幂等加列。已经存在就直接返回，不报错。
func addColumnIfMissing(d *sql.DB, column, decl string) error {
	has, err := hasColumn(d, "conn_log", column)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := d.Exec(`ALTER TABLE conn_log ADD COLUMN ` + column + ` ` + decl); err != nil {
		return fmt.Errorf("添加 %s 列失败: %w", column, err)
	}
	return nil
}

// backfillBytesDown 把旧列的字节数搬到正确的新列上。
//
// 这一条不能省：旧版本把 %B（源站→客户端，下行）写进了名为 bytes_in 的列。
// 新版本按方向拆成 bytes_up/bytes_down。若只加列不回填，历史日志的下行字节
// 会变成"不可用" —— 那和"真的没测到"无法区分，等于把已有数据丢掉。
// 方向语义依据见 haproxy.Registry 的 up/down 两条（手册 8.2.6 别名表）。
func backfillBytesDown(d *sql.DB) error {
	hasLegacy, err := hasColumn(d, "conn_log", "bytes_in")
	if err != nil {
		return err
	}
	if !hasLegacy {
		return nil
	}
	_, err = d.Exec(`UPDATE conn_log SET bytes_down = bytes_in
                     WHERE bytes_down IS NULL AND bytes_in IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("回填下行字节失败: %w", err)
	}
	return nil
}

// ensureSeqIndex 确保幂等索引的列组合是 (node_id, boot_id, seq)。
//
// 不能只靠 CREATE UNIQUE INDEX IF NOT EXISTS：索引已存在时它什么都不做，
// 于是旧分片会一直用着 (node_id, seq) —— 重启后新日志依然会被当成重复丢弃。
func ensureSeqIndex(d *sql.DB) error {
	cols, err := indexColumns(d, "ux_conn_seq")
	if err != nil {
		return err
	}
	want := []string{"node_id", "boot_id", "seq"}
	if equalFold(cols, want) {
		return nil
	}
	if _, err := d.Exec(`DROP INDEX IF EXISTS ux_conn_seq`); err != nil {
		return fmt.Errorf("删除旧幂等索引失败: %w", err)
	}
	if _, err := d.Exec(`CREATE UNIQUE INDEX ux_conn_seq ON conn_log(node_id, boot_id, seq)`); err != nil {
		return fmt.Errorf("重建幂等索引失败: %w", err)
	}
	return nil
}

func hasColumn(d *sql.DB, table, column string) (bool, error) {
	rows, err := d.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// indexColumns 返回某个索引包含的列名（按索引内的顺序）。
func indexColumns(d *sql.DB, index string) ([]string, error) {
	rows, err := d.Query(`PRAGMA index_info(` + index + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type col struct {
		seq  int
		name string
	}
	var cols []col
	for rows.Next() {
		var (
			seqno int
			cid   int
			name  string
		)
		if err := rows.Scan(&seqno, &cid, &name); err != nil {
			return nil, err
		}
		cols = append(cols, col{seq: seqno, name: name})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// PRAGMA index_info 已按顺序返回，但仍按 seqno 排一次以防驱动实现差异。
	for i := 1; i < len(cols); i++ {
		for j := i; j > 0 && cols[j-1].seq > cols[j].seq; j-- {
			cols[j-1], cols[j] = cols[j], cols[j-1]
		}
	}
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, c.name)
	}
	return out, nil
}

func equalFold(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}
