package store

import (
	"database/sql"
	"fmt"
)

// 本文件是**元数据库**的向前迁移。
//
// 为什么必须有它：schema.go 用的是 `CREATE TABLE IF NOT EXISTS`，
// 对**已经存在**的表完全不起作用。一旦给某张表加了列，老库不会自动获得该列，
// 而代码里的 INSERT/SELECT 已经按新结构写了 —— 结果是升级后第一次写节点/版本就报
// "no such column"，而报错点离"你改了表结构"这件事很远，很难查。
//
// 规则（和 logstore/shard.go 一样）：
//   - 每加一列，必须在 migrate.go 里登记一次；
//   - 迁移必须幂等（可以反复执行）；
//   - 不删列、不改类型 —— 数据留在那里比"看起来干净"更重要。
//
// 日志分片的迁移在 logstore/shard.go，两者刻意分开：
// 元数据库是强一致的配置数据，日志分片是可丢弃的时序数据，风险等级不同。
func (s *Store) migrate() error {
	steps := []struct {
		table  string
		column string
		decl   string
		why    string
	}{
		{"nodes", "health_detail", "TEXT NOT NULL DEFAULT ''",
			"区分 Agent / HAProxy / 业务路径三个健康维度（评审 F10）"},
		{"config_versions", "object_names", "TEXT NOT NULL DEFAULT '{}'",
			"保存该版本的不可变对象快照（backend→业务、backend→源站），供历史日志按版本富化（评审 F11）"},
	}
	for _, st := range steps {
		if err := s.addColumnIfMissing(st.table, st.column, st.decl); err != nil {
			return fmt.Errorf("store: 迁移 %s.%s（%s）失败: %w", st.table, st.column, st.why, err)
		}
	}
	return nil
}

func (s *Store) addColumnIfMissing(table, column, decl string) error {
	has, err := columnExists(s.db, table, column)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + decl); err != nil {
		return err
	}
	return nil
}

func columnExists(d *sql.DB, table, column string) (bool, error) {
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
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
