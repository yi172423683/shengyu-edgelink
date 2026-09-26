package store

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/shengyu/edgelink/internal/model"
)

// ============================== 业务 ==============================

const bizCols = `id,customer_id,name,remark,mode,enabled,connect_timeout_ms,client_timeout_ms,
	server_timeout_ms,maxconn,queue_limit,healthcheck,primary_node_id,backup_node_id,domains,created_at,updated_at`

func (s *Store) CreateBusiness(b *model.Business) error {
	if strings.TrimSpace(b.ID) == "" {
		return fmt.Errorf("store: 业务 ID 必须由调用方生成")
	}
	now := s.now()
	b.CreatedAt, b.UpdatedAt = now, now
	_, err := s.db.Exec(`INSERT INTO businesses(`+bizCols+`)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		b.ID, b.CustomerID, b.Name, b.Remark, string(b.Mode), boolToInt(b.Enabled),
		b.ConnectTimeoutMS, b.ClientTimeoutMS, b.ServerTimeoutMS, b.MaxConn, b.QueueLimit,
		toJSON(b.HealthCheck, "{}"), b.PrimaryNodeID, b.BackupNodeID, toJSON(b.Domains, "[]"),
		ts(now), ts(now))
	return wrapUnique(err, "创建业务")
}

func (s *Store) UpdateBusiness(b *model.Business) error {
	b.UpdatedAt = s.now()
	res, err := s.db.Exec(`UPDATE businesses SET
		customer_id=?,name=?,remark=?,mode=?,enabled=?,connect_timeout_ms=?,client_timeout_ms=?,
		server_timeout_ms=?,maxconn=?,queue_limit=?,healthcheck=?,primary_node_id=?,
		backup_node_id=?,domains=?,updated_at=? WHERE id=?`,
		b.CustomerID, b.Name, b.Remark, string(b.Mode), boolToInt(b.Enabled),
		b.ConnectTimeoutMS, b.ClientTimeoutMS, b.ServerTimeoutMS, b.MaxConn, b.QueueLimit,
		toJSON(b.HealthCheck, "{}"), b.PrimaryNodeID, b.BackupNodeID, toJSON(b.Domains, "[]"),
		ts(b.UpdatedAt), b.ID)
	if err != nil {
		return wrapUnique(err, "更新业务")
	}
	return mustAffect(res, "业务")
}

func scanBiz(sc interface{ Scan(...any) error }) (*model.Business, error) {
	var (
		b        model.Business
		mode     string
		enabled  int
		hc, dom  string
		createdA string
		updatedA string
	)
	if err := sc.Scan(&b.ID, &b.CustomerID, &b.Name, &b.Remark, &mode, &enabled,
		&b.ConnectTimeoutMS, &b.ClientTimeoutMS, &b.ServerTimeoutMS, &b.MaxConn, &b.QueueLimit,
		&hc, &b.PrimaryNodeID, &b.BackupNodeID, &dom, &createdA, &updatedA); err != nil {
		return nil, err
	}
	b.Mode = model.Mode(mode)
	b.Enabled = intToBool(enabled)
	fromJSON(hc, &b.HealthCheck)
	fromJSON(dom, &b.Domains)
	if b.Domains == nil {
		b.Domains = []string{}
	}
	b.CreatedAt, b.UpdatedAt = parseTS(createdA), parseTS(updatedA)
	return &b, nil
}

func (s *Store) GetBusiness(id string) (*model.Business, error) {
	row := s.db.QueryRow(`SELECT `+bizCols+` FROM businesses WHERE id=?`, id)
	b, err := scanBiz(row)
	if err != nil {
		return nil, scanErr(err, "业务 "+id)
	}
	return b, nil
}

// BusinessFilter 列表筛选条件。全部为空表示不过滤。
type BusinessFilter struct {
	CustomerID string
	NodeID     string // 匹配主或备节点
	Keyword    string // 业务名称/备注模糊
	OnlyEnable bool
	Limit      int
	Offset     int
}

func (s *Store) ListBusinesses(f BusinessFilter) ([]model.Business, int, error) {
	var (
		where []string
		args  []any
	)
	if f.CustomerID != "" {
		where = append(where, "customer_id=?")
		args = append(args, f.CustomerID)
	}
	if f.NodeID != "" {
		where = append(where, "(primary_node_id=? OR backup_node_id=?)")
		args = append(args, f.NodeID, f.NodeID)
	}
	if k := strings.TrimSpace(f.Keyword); k != "" {
		where = append(where, "(name LIKE ? OR remark LIKE ?)")
		args = append(args, "%"+k+"%", "%"+k+"%")
	}
	if f.OnlyEnable {
		where = append(where, "enabled=1")
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM businesses`+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: 统计业务数: %w", err)
	}

	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT ` + bizCols + ` FROM businesses` + clause + ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	rows, err := s.db.Query(q, append(args, limit, max0(f.Offset))...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: 查询业务列表: %w", err)
	}
	defer rows.Close()
	out := []model.Business{}
	for rows.Next() {
		b, err := scanBiz(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("store: 读取业务行: %w", err)
		}
		out = append(out, *b)
	}
	return out, total, rows.Err()
}

// DeleteBusiness 删除业务，级联删除其路由、DNS 绑定与组成员关系。
func (s *Store) DeleteBusiness(id string) error {
	res, err := s.db.Exec(`DELETE FROM businesses WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("store: 删除业务: %w", err)
	}
	return mustAffect(res, "业务")
}

// ============================== 路由 ==============================

const routeCols = `id,business_id,node_id,role,mode,sni_entry_id,entry_addr,entry_port,
	origin_host,origin_port,origin_sni,queue_limit,maxconn,enabled,last_applied_version,created_at,updated_at`

func (s *Store) CreateRoute(r *model.Route) error {
	now := s.now()
	r.CreatedAt, r.UpdatedAt = now, now
	_, err := s.db.Exec(`INSERT INTO routes(`+routeCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.BusinessID, r.NodeID, string(r.Role), string(r.Mode), r.SNIEntryID,
		r.EntryAddr, r.EntryPort, r.OriginHost, r.OriginPort, r.OriginSNI,
		r.QueueLimit, r.MaxConn, boolToInt(r.Enabled), r.LastAppliedVersion, ts(now), ts(now))
	if err != nil {
		// 明确区分两种冲突，否则运维看到 409 不知道去改什么。
		//
		// 坑：SQLite（含 modernc 驱动）对唯一索引冲突只报**列组合**，不报索引名：
		//   UNIQUE constraint failed: routes.node_id, routes.entry_addr, routes.entry_port
		//   UNIQUE constraint failed: routes.business_id, routes.node_id
		// 所以这里按列组合识别，而不是按 ux_* 索引名（实测确认，见 store_test）。
		if isUniqueViolation(err) {
			msg := strings.ToLower(err.Error())
			switch {
			case strings.Contains(msg, "entry_addr") && strings.Contains(msg, "entry_port"):
				return fmt.Errorf("%w: 入口 %s:%d 已被该节点上另一条 TCP 路由占用",
					ErrConflict, r.EntryAddr, r.EntryPort)
			case strings.Contains(msg, "business_id") && strings.Contains(msg, "node_id"):
				return fmt.Errorf("%w: 该业务在这台节点上已存在路由", ErrConflict)
			default:
				return fmt.Errorf("%w: 路由与已有记录冲突: %v", ErrConflict, err)
			}
		}
		return fmt.Errorf("store: 创建路由: %w", err)
	}
	return nil
}

func (s *Store) UpdateRoute(r *model.Route) error {
	r.UpdatedAt = s.now()
	res, err := s.db.Exec(`UPDATE routes SET
		role=?,mode=?,sni_entry_id=?,entry_addr=?,entry_port=?,origin_host=?,origin_port=?,
		origin_sni=?,queue_limit=?,maxconn=?,enabled=?,updated_at=? WHERE id=?`,
		string(r.Role), string(r.Mode), r.SNIEntryID, r.EntryAddr, r.EntryPort,
		r.OriginHost, r.OriginPort, r.OriginSNI, r.QueueLimit, r.MaxConn,
		boolToInt(r.Enabled), ts(r.UpdatedAt), r.ID)
	if err != nil {
		return wrapUnique(err, "更新路由")
	}
	return mustAffect(res, "路由")
}

// UpsertRouteForNode 幂等地把"业务在某节点上的角色"落库。
// 业务保存时调用：主/备节点可能被改动，需要新建、更新或删除对应路由。
func (s *Store) UpsertRouteForNode(r *model.Route) error {
	existing, err := s.GetRouteByBizNode(r.BusinessID, r.NodeID)
	if err != nil && !IsNotFound(err) {
		return err
	}
	if IsNotFound(err) {
		return s.CreateRoute(r)
	}
	r.ID = existing.ID
	r.CreatedAt = existing.CreatedAt
	r.LastAppliedVersion = existing.LastAppliedVersion
	return s.UpdateRoute(r)
}

func scanRoute(sc interface{ Scan(...any) error }) (*model.Route, error) {
	var (
		r          model.Route
		role, mode string
		enabled    int
		createdA   string
		updatedA   string
	)
	if err := sc.Scan(&r.ID, &r.BusinessID, &r.NodeID, &role, &mode, &r.SNIEntryID,
		&r.EntryAddr, &r.EntryPort, &r.OriginHost, &r.OriginPort, &r.OriginSNI,
		&r.QueueLimit, &r.MaxConn, &enabled, &r.LastAppliedVersion, &createdA, &updatedA); err != nil {
		return nil, err
	}
	r.Role = model.Role(role)
	r.Mode = model.Mode(mode)
	r.Enabled = intToBool(enabled)
	r.CreatedAt, r.UpdatedAt = parseTS(createdA), parseTS(updatedA)
	return &r, nil
}

func (s *Store) GetRoute(id string) (*model.Route, error) {
	row := s.db.QueryRow(`SELECT `+routeCols+` FROM routes WHERE id=?`, id)
	r, err := scanRoute(row)
	if err != nil {
		return nil, scanErr(err, "路由 "+id)
	}
	return r, nil
}

func (s *Store) GetRouteByBizNode(bizID, nodeID string) (*model.Route, error) {
	row := s.db.QueryRow(`SELECT `+routeCols+` FROM routes WHERE business_id=? AND node_id=?`, bizID, nodeID)
	r, err := scanRoute(row)
	if err != nil {
		return nil, scanErr(err, fmt.Sprintf("业务 %s 在节点 %s 上的路由", bizID, nodeID))
	}
	return r, nil
}

func (s *Store) ListRoutesByNode(nodeID string) ([]model.Route, error) {
	rows, err := s.db.Query(`SELECT `+routeCols+` FROM routes WHERE node_id=? ORDER BY id ASC`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询节点路由: %w", err)
	}
	defer rows.Close()
	out := []model.Route{}
	for rows.Next() {
		r, err := scanRoute(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 读取路由行: %w", err)
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *Store) ListRoutesByBusiness(bizID string) ([]model.Route, error) {
	rows, err := s.db.Query(`SELECT `+routeCols+` FROM routes WHERE business_id=? ORDER BY role ASC`, bizID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询业务路由: %w", err)
	}
	defer rows.Close()
	out := []model.Route{}
	for rows.Next() {
		r, err := scanRoute(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 读取路由行: %w", err)
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *Store) DeleteRoute(id string) error {
	res, err := s.db.Exec(`DELETE FROM routes WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("store: 删除路由: %w", err)
	}
	return mustAffect(res, "路由")
}

// SetRouteAppliedVersion 发布成功后回写"这条路由随哪一版生效"。
// 用于回答运维最常问的问题：**「我改的这条规则，现在到底生效了没有？」**
func (s *Store) SetRouteAppliedVersion(nodeID string, version int) error {
	_, err := s.db.Exec(`UPDATE routes SET last_applied_version=? WHERE node_id=?`, version, nodeID)
	if err != nil {
		return fmt.Errorf("store: 回写路由生效版本: %w", err)
	}
	return nil
}

// ============================== 渲染输入组装 ==============================

// DesiredStateForNode 组装渲染 HAProxy 配置所需的完整输入。
//
// 这是**唯一**把 DB 里的分散行拼成「节点期望状态」的地方，
// 保证管理面预览、发布、agent 拉取三条路径看到的是同一份数据。
func (s *Store) DesiredStateForNode(nodeID string, version int) (*model.DesiredState, error) {
	n, err := s.GetNode(nodeID)
	if err != nil {
		return nil, err
	}
	entries, err := s.ListSNIEntries(nodeID)
	if err != nil {
		return nil, err
	}
	routes, err := s.ListRoutesByNode(nodeID)
	if err != nil {
		return nil, err
	}
	st := &model.DesiredState{Node: *n, SNIEntries: entries, Version: version, PublishedAt: s.now()}
	for _, r := range routes {
		b, err := s.GetBusiness(r.BusinessID)
		if err != nil {
			return nil, fmt.Errorf("store: 路由 %s 引用的业务不存在: %w", r.ID, err)
		}
		view := model.RouteView{
			Route:               r,
			BusinessName:        b.Name,
			Mode:                b.Mode,
			Domains:             append([]string(nil), b.Domains...),
			ConnectTimeoutMS:    b.ConnectTimeoutMS,
			ClientTimeoutMS:     b.ClientTimeoutMS,
			ServerTimeoutMS:     b.ServerTimeoutMS,
			EffectiveMaxConn:    pickInt(r.MaxConn, b.MaxConn),
			EffectiveQueueLimit: pickInt(r.QueueLimit, b.QueueLimit),
			HealthCheck:         b.HealthCheck,
			Enabled:             r.Enabled && b.Enabled,
		}
		st.Routes = append(st.Routes, view)
	}
	return st, nil
}

// EnabledBusinessForNode 只返回启用的业务（供探测/展示用）。
func (s *Store) EnabledBusinessForNode(nodeID string) ([]model.Business, error) {
	rows, err := s.db.Query(`SELECT `+bizCols+` FROM businesses b
		WHERE b.enabled=1 AND EXISTS(SELECT 1 FROM routes r WHERE r.business_id=b.id AND r.node_id=? AND r.enabled=1)
		ORDER BY b.id ASC`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询节点启用业务: %w", err)
	}
	defer rows.Close()
	out := []model.Business{}
	for rows.Next() {
		b, err := scanBiz(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 读取业务行: %w", err)
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

func pickInt(primary, fallback int) int {
	if primary != 0 {
		return primary
	}
	return fallback
}

func max0(i int) int {
	if i < 0 {
		return 0
	}
	return i
}

var _ = sql.ErrNoRows
