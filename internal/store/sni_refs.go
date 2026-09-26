package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrSNIEntryInUse 表示该共享 SNI 入口仍被业务路由引用，不允许删除。
//
// 单独一个错误值（而不是 ErrConflict）是因为上层要给出**不同的**处置建议：
// 冲突是"改个参数再试"，而这个是"先去删业务或改挂入口" —— 混在一起运维会试错很久。
var ErrSNIEntryInUse = errors.New("store: 共享入口仍被业务路由引用")

// DeleteSNIEntry 删除一个共享 SNI 入口。
//
// **先校验引用、再删除，且两步在同一个事务里。** 这不是洁癖：
// 如果先查（没引用）后删（两条语句），中间那一刻有人建了一条路由，
// 删完就会留下**悬空引用** —— 而悬空引用是本平台最难查的一类故障：
// 渲染器按入口 ID 索引，找不到就把那条路由**静默丢掉**，
// 于是"发布成功但业务不转发"。真机上已经因为缺这个接口吃过一次事故
// （验收只能改库、改库绕过了所有一致性检查，见 docs/08 §8）。
//
// 守卫放在 store 而不是 handler：handler 可以被绕过（批量脚本、未来的其它调用方），
// 而"不允许留下悬空引用"是数据完整性约束，属于存储层的职责。
func (s *Store) DeleteSNIEntry(entryID string) error {
	return s.Tx(func(tx *sql.Tx) error {
		var refs int
		if err := tx.QueryRow(`SELECT count(*) FROM routes WHERE sni_entry_id=?`, entryID).
			Scan(&refs); err != nil {
			return fmt.Errorf("store: 统计入口引用数失败: %w", err)
		}
		if refs > 0 {
			return fmt.Errorf("%w: 仍被 %d 条业务路由引用%s —— "+
				"请先删除这些业务，或把它们改挂到别的入口，再删除本入口",
				ErrSNIEntryInUse, refs, s.sniEntryRefNames(tx, entryID))
		}
		res, err := tx.Exec(`DELETE FROM sni_entries WHERE id=?`, entryID)
		if err != nil {
			return fmt.Errorf("store: 删除 SNI 入口: %w", err)
		}
		return mustAffect(res, "共享入口")
	})
}

// sniEntryRefNames 返回引用该入口的业务名（用于报错里点名，最长 3 个）。
//
// 为什么要点名："仍被 1 条业务路由引用"会让运维去翻整个业务列表自己找；
// 直接说出业务名，通常一眼就知道该动哪一条。
func (s *Store) sniEntryRefNames(tx *sql.Tx, entryID string) string {
	rows, err := tx.Query(`SELECT b.name FROM routes r JOIN businesses b ON b.id=r.business_id
		WHERE r.sni_entry_id=? ORDER BY b.name LIMIT 3`, entryID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err == nil && n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return ""
	}
	suffix := ""
	if len(names) == 3 {
		suffix = " 等"
	}
	return "（" + strings.Join(names, "、") + suffix + "）"
}

// DanglingRef 一条悬空引用：某处指向了不存在的记录。
type DanglingRef struct {
	// From 来源，形如 `route/rt_xxx`、`sni_entry/sni_xxx`、`business/biz_xxx`。
	From string `json:"from"`
	// Kind 缺失记录的类型：sni_entry / business / node。
	Kind string `json:"kind"`
	// MissingID 被引用但不存在的记录 ID。
	MissingID string `json:"missing_id"`
	// Detail 一句人话，说明"这个引用是从哪来的、会造成什么"。
	Detail string `json:"detail"`
}

// FindDanglingReferences 扫描"指向不存在记录的引用"。
//
// 为什么值得单列一个自检，而不是等发布时报 SNI_ENTRY_MISSING：
// 悬空引用一旦进入渲染，那条路由会被**静默丢掉**（渲染器按入口 ID 索引），
// 结果是"发布成功但业务不转发" —— 最难查的一类故障。
// 目前唯一能发现它的时机是"发布"，而那时运维往往已经在改另一件事了
// （真机事故：改库删入口留下悬空引用，之后**每次**发布都被前置校验拒成 422，
// 而原因只有到"发布失败"那一刻才暴露 —— 见 docs/08 §8）。
//
// routes.business_id / node_id 都有外键（且 Open 里 PRAGMA foreign_keys=ON），
// 所以正常情况下不会悬空；这里仍然检查它们，是为了发现"外键开启之前写入的历史脏数据"。
// routes.sni_entry_id **没有外键** —— 这正是最需要盯的一条。
func (s *Store) FindDanglingReferences() ([]DanglingRef, error) {
	queries := []struct {
		kind   string
		detail string
		sql    string
	}{
		{
			kind: "sni_entry",
			detail: "路由引用的 SNI 共享入口不存在：该路由在渲染时会被**静默丢掉**，" +
				"表现为「发布成功但业务不转发」。请删除这条路由，或把它改挂到一个存在的入口。",
			sql: `SELECT r.id, r.sni_entry_id FROM routes r
				LEFT JOIN sni_entries e ON e.id=r.sni_entry_id
				WHERE r.sni_entry_id<>'' AND e.id IS NULL`,
		},
		{
			kind: "business",
			detail: "路由引用的业务不存在（外键开启前写入的脏数据）：" +
				"这条路由没有归属的业务，发布时不会产生任何转发规则。建议删除该路由。",
			sql: `SELECT r.id, r.business_id FROM routes r
				LEFT JOIN businesses b ON b.id=r.business_id
				WHERE b.id IS NULL`,
		},
		{
			kind:   "node",
			detail: "路由引用的节点不存在：该路由不会被任何节点渲染，也不会被心跳上报覆盖。建议删除该路由。",
			sql: `SELECT r.id, r.node_id FROM routes r
				LEFT JOIN nodes n ON n.id=r.node_id
				WHERE n.id IS NULL`,
		},
		{
			kind: "node",
			detail: "共享入口挂在不存在的节点上：它不会被渲染，但会一直占用该端口号，" +
				"导致在同一节点上重新创建同端口入口时冲突。建议删除该入口。",
			sql: `SELECT e.id, e.node_id FROM sni_entries e
				LEFT JOIN nodes n ON n.id=e.node_id
				WHERE n.id IS NULL`,
		},
		{
			kind:   "node",
			detail: "业务的主节点不存在：该业务不会被下发到任何节点。请改选一个存在的节点。",
			sql: `SELECT b.id, b.primary_node_id FROM businesses b
				LEFT JOIN nodes n ON n.id=b.primary_node_id
				WHERE b.primary_node_id<>'' AND n.id IS NULL`,
		},
	}

	var out []DanglingRef
	for _, q := range queries {
		rows, err := s.db.Query(q.sql)
		if err != nil {
			return nil, fmt.Errorf("store: 悬空引用自检失败（%s）: %w", q.kind, err)
		}
		for rows.Next() {
			var id, missing string
			if err := rows.Scan(&id, &missing); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: 读取悬空引用行失败: %w", err)
			}
			out = append(out, DanglingRef{
				From: id, Kind: q.kind, MissingID: missing, Detail: q.detail,
			})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("store: 遍历悬空引用结果失败: %w", err)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].From < out[j].From
	})
	return out, nil
}
