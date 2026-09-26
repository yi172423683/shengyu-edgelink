package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shengyu/edgelink/internal/model"
)

// ============================== 客户 ==============================

func (s *Store) CreateCustomer(c *model.Customer) error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("store: 客户名称不能为空")
	}
	now := s.now()
	if c.ID == "" {
		return fmt.Errorf("store: 客户 ID 必须由调用方生成")
	}
	c.CreatedAt, c.UpdatedAt = now, now
	_, err := s.db.Exec(`INSERT INTO customers(id,name,contact,remark,enabled,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?)`,
		c.ID, c.Name, c.Contact, c.Remark, boolToInt(c.Enabled), ts(c.CreatedAt), ts(c.UpdatedAt))
	return wrapUnique(err, "创建客户")
}

func (s *Store) UpdateCustomer(c *model.Customer) error {
	c.UpdatedAt = s.now()
	res, err := s.db.Exec(`UPDATE customers SET name=?,contact=?,remark=?,enabled=?,updated_at=? WHERE id=?`,
		c.Name, c.Contact, c.Remark, boolToInt(c.Enabled), ts(c.UpdatedAt), c.ID)
	if err != nil {
		return wrapUnique(err, "更新客户")
	}
	return mustAffect(res, "客户")
}

func (s *Store) GetCustomer(id string) (*model.Customer, error) {
	var (
		c         model.Customer
		createdAt string
		updatedAt string
		enabled   int
	)
	err := s.db.QueryRow(`SELECT id,name,contact,remark,enabled,created_at,updated_at FROM customers WHERE id=?`, id).
		Scan(&c.ID, &c.Name, &c.Contact, &c.Remark, &enabled, &createdAt, &updatedAt)
	if err != nil {
		return nil, scanErr(err, "客户 "+id)
	}
	c.Enabled = intToBool(enabled)
	c.CreatedAt, c.UpdatedAt = parseTS(createdAt), parseTS(updatedAt)
	return &c, nil
}

// ListCustomers 返回客户列表；q 非空时按名称模糊匹配。
func (s *Store) ListCustomers(q string) ([]model.Customer, error) {
	sqlText := `SELECT id,name,contact,remark,enabled,created_at,updated_at FROM customers`
	var args []any
	if q = strings.TrimSpace(q); q != "" {
		sqlText += ` WHERE name LIKE ?`
		args = append(args, "%"+q+"%")
	}
	sqlText += ` ORDER BY created_at ASC`
	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询客户列表: %w", err)
	}
	defer rows.Close()
	out := []model.Customer{}
	for rows.Next() {
		var (
			c                model.Customer
			createdAt, updAt string
			enabled          int
		)
		if err := rows.Scan(&c.ID, &c.Name, &c.Contact, &c.Remark, &enabled, &createdAt, &updAt); err != nil {
			return nil, fmt.Errorf("store: 读取客户行: %w", err)
		}
		c.Enabled = intToBool(enabled)
		c.CreatedAt, c.UpdatedAt = parseTS(createdAt), parseTS(updAt)
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteCustomer 删除客户。若名下仍有业务则拒绝 —— 静默级联删除会让人以为"业务还在跑"，
// 而实际上转发已经没了，这是最危险的一类误操作。
func (s *Store) DeleteCustomer(id string) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM businesses WHERE customer_id=?`, id).Scan(&n); err != nil {
		return fmt.Errorf("store: 统计客户业务数: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("%w: 客户下仍有 %d 条业务，请先删除或转移业务", ErrConflict, n)
	}
	res, err := s.db.Exec(`DELETE FROM customers WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("store: 删除客户: %w", err)
	}
	return mustAffect(res, "客户")
}

// ============================== 节点 ==============================

func (s *Store) CreateNode(n *model.Node) error {
	if strings.TrimSpace(n.Name) == "" {
		return fmt.Errorf("store: 节点名称不能为空")
	}
	now := s.now()
	n.CreatedAt, n.UpdatedAt = now, now
	if n.Health == "" {
		n.Health = model.NodeUnknown
	}
	_, err := s.db.Exec(`INSERT INTO nodes(
		id,name,group_name,agent_endpoint,public_ipv4,public_ipv6,region,
		agent_version,haproxy_version,applied_version,expected_version,last_heartbeat,
		health,health_detail,capabilities,enabled,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		n.ID, n.Name, n.GroupName, n.AgentEndpoint, n.PublicIPv4, n.PublicIPv6, n.Region,
		n.AgentVersion, n.HAProxyVersion, n.AppliedVersion, n.ExpectedVersion, ts(n.LastHeartbeat),
		string(n.Health), toJSON(n.HealthDetail, "{}"), toJSON(n.Capabilities, "{}"),
		boolToInt(n.Enabled), ts(now), ts(now))
	return wrapUnique(err, "创建节点")
}

func (s *Store) UpdateNode(n *model.Node) error {
	n.UpdatedAt = s.now()
	res, err := s.db.Exec(`UPDATE nodes SET
		name=?,group_name=?,agent_endpoint=?,public_ipv4=?,public_ipv6=?,region=?,
		agent_version=?,haproxy_version=?,applied_version=?,expected_version=?,
		last_heartbeat=?,health=?,health_detail=?,capabilities=?,enabled=?,updated_at=?
		WHERE id=?`,
		n.Name, n.GroupName, n.AgentEndpoint, n.PublicIPv4, n.PublicIPv6, n.Region,
		n.AgentVersion, n.HAProxyVersion, n.AppliedVersion, n.ExpectedVersion,
		ts(n.LastHeartbeat), string(n.Health), toJSON(n.HealthDetail, "{}"),
		toJSON(n.Capabilities, "{}"), boolToInt(n.Enabled), ts(n.UpdatedAt), n.ID)
	if err != nil {
		return wrapUnique(err, "更新节点")
	}
	return mustAffect(res, "节点")
}

const nodeCols = `id,name,group_name,agent_endpoint,public_ipv4,public_ipv6,region,
	agent_version,haproxy_version,applied_version,expected_version,last_heartbeat,
	health,health_detail,capabilities,enabled,created_at,updated_at`

func scanNode(sc interface{ Scan(...any) error }) (*model.Node, error) {
	var (
		n          model.Node
		health     string
		healthDtl  string
		caps       string
		enabled    int
		hb, ca, ua string
	)
	if err := sc.Scan(&n.ID, &n.Name, &n.GroupName, &n.AgentEndpoint, &n.PublicIPv4, &n.PublicIPv6, &n.Region,
		&n.AgentVersion, &n.HAProxyVersion, &n.AppliedVersion, &n.ExpectedVersion, &hb,
		&health, &healthDtl, &caps, &enabled, &ca, &ua); err != nil {
		return nil, err
	}
	n.Health = model.NodeHealth(health)
	if healthDtl != "" && healthDtl != "{}" {
		fromJSON(healthDtl, &n.HealthDetail)
	}
	n.Enabled = intToBool(enabled)
	n.LastHeartbeat = parseTS(hb)
	n.CreatedAt, n.UpdatedAt = parseTS(ca), parseTS(ua)
	fromJSON(caps, &n.Capabilities)
	return &n, nil
}

func (s *Store) GetNode(id string) (*model.Node, error) {
	row := s.db.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id=?`, id)
	n, err := scanNode(row)
	if err != nil {
		return nil, scanErr(err, "节点 "+id)
	}
	return n, nil
}

func (s *Store) ListNodes() ([]model.Node, error) {
	rows, err := s.db.Query(`SELECT ` + nodeCols + ` FROM nodes ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询节点列表: %w", err)
	}
	defer rows.Close()
	out := []model.Node{}
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 读取节点行: %w", err)
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

func (s *Store) DeleteNode(id string) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM routes WHERE node_id=?`, id).Scan(&n); err != nil {
		return fmt.Errorf("store: 统计节点路由: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("%w: 节点上仍有 %d 条路由，请先删除业务或更换节点", ErrConflict, n)
	}
	res, err := s.db.Exec(`DELETE FROM nodes WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("store: 删除节点: %w", err)
	}
	return mustAffect(res, "节点")
}

// HeartbeatInput agent 心跳上报的字段（需求 §五：区分「配置发布失败」与「源站不可用」，
// 所以心跳里同时带"我实际生效的版本"和"我探测到的数据面状态"）。
type HeartbeatInput struct {
	NodeID         string
	AgentVersion   string
	HAProxyVersion string
	AppliedVersion int
	DataplaneOK    bool
	Capabilities   model.Capabilities
}

// RecordHeartbeat 更新节点在线状态。注意这里**不会**改 expected_version ——
// 那是发布流水线推出来的，agent 无权自证"已经是我期望的版本"。
func (s *Store) RecordHeartbeat(in HeartbeatInput) error {
	now := s.now()
	health := model.NodeOnline
	if !in.DataplaneOK {
		// agent 活着但数据面不健康：这不是离线，是降级。区分开才能正确诊断。
		health = model.NodeDegraded
	}
	res, err := s.db.Exec(`UPDATE nodes SET
		last_heartbeat=?,health=?,agent_version=?,haproxy_version=?,
		applied_version=?,capabilities=?,updated_at=?
		WHERE id=?`,
		ts(now), string(health), in.AgentVersion, in.HAProxyVersion,
		in.AppliedVersion, toJSON(in.Capabilities, "{}"), ts(now), in.NodeID)
	if err != nil {
		return fmt.Errorf("store: 记录心跳: %w", err)
	}
	return mustAffect(res, "节点")
}

// NodeHealthInput 一次健康采样的完整内容（同机节点与远程 agent 共用）。
type NodeHealthInput struct {
	NodeID         string
	AgentVersion   string
	HAProxyVersion string
	AppliedVersion int
	// AppliedVersionKnown 为 false 时**不覆盖** applied_version。
	//
	// 为什么要这个开关：同机节点若暂时连不上统计套接字（HAProxy 正在重启），
	// ActiveVersion 会返回 0。若照写，界面会显示"实际版本 0"，
	// 看上去像"配置被清空了"，而事实只是"这一秒没读到"。心跳绝不能把不确定写成确定。
	AppliedVersionKnown bool
	Capabilities        model.Capabilities
	Detail              model.NodeHealthDetail
}

// RecordNodeHealth 记录一次健康采样：心跳时间 + 三维健康 + 版本 + 能力。
//
// 这是评审 F10 的正解：本机节点的 LastHeartbeat 必须**周期性续报**，
// 否则后台的"90 秒没心跳就标离线"会把自己这台正在正常转发的机器标成离线，
// 进而误导总览与后续的高可用决策。
func (s *Store) RecordNodeHealth(in NodeHealthInput) error {
	now := s.now()
	detail := in.Detail
	if detail.At.IsZero() {
		detail.At = now
	}
	agg := detail.Aggregate()
	if !in.AppliedVersionKnown {
		_, err := s.db.Exec(`UPDATE nodes SET
			last_heartbeat=?,health=?,health_detail=?,agent_version=?,haproxy_version=?,
			capabilities=?,updated_at=?
			WHERE id=?`,
			ts(now), string(agg), toJSON(detail, "{}"), in.AgentVersion, in.HAProxyVersion,
			toJSON(in.Capabilities, "{}"), ts(now), in.NodeID)
		if err != nil {
			return fmt.Errorf("store: 记录健康采样: %w", err)
		}
		return nil
	}
	res, err := s.db.Exec(`UPDATE nodes SET
		last_heartbeat=?,health=?,health_detail=?,agent_version=?,haproxy_version=?,
		applied_version=?,capabilities=?,updated_at=?
		WHERE id=?`,
		ts(now), string(agg), toJSON(detail, "{}"), in.AgentVersion, in.HAProxyVersion,
		in.AppliedVersion, toJSON(in.Capabilities, "{}"), ts(now), in.NodeID)
	if err != nil {
		return fmt.Errorf("store: 记录健康采样: %w", err)
	}
	return mustAffect(res, "节点")
}

// MarkStaleNodes 把超过 timeout 未心跳的节点标为离线。
// 由管理面的定时任务调用；不依赖 agent 主动上报"我要下线了"（那种事永远不会发生）。
func (s *Store) MarkStaleNodes(timeout time.Duration) (int64, error) {
	cutoff := s.now().Add(-timeout)
	res, err := s.db.Exec(`UPDATE nodes SET health=?, updated_at=?
		WHERE enabled=1 AND health<>? AND last_heartbeat<>'' AND last_heartbeat < ?`,
		string(model.NodeOffline), ts(s.now()), string(model.NodeOffline), ts(cutoff))
	if err != nil {
		return 0, fmt.Errorf("store: 标记离线节点: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ============================== 共享 SNI 入口 ==============================

func (s *Store) CreateSNIEntry(e *model.SNIEntry) error {
	now := s.now()
	e.CreatedAt = now
	_, err := s.db.Exec(`INSERT INTO sni_entries(id,node_id,bind_addr,bind_port,enabled,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?)`,
		e.ID, e.NodeID, e.BindAddr, e.BindPort, boolToInt(e.Enabled), ts(now), ts(now))
	return wrapUnique(err, "创建 SNI 入口（同一节点同一 地址:端口 只能有一个）")
}

func (s *Store) ListSNIEntries(nodeID string) ([]model.SNIEntry, error) {
	rows, err := s.db.Query(`SELECT id,node_id,bind_addr,bind_port,enabled,created_at
		FROM sni_entries WHERE node_id=? ORDER BY bind_port ASC`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询 SNI 入口: %w", err)
	}
	defer rows.Close()
	out := []model.SNIEntry{}
	for rows.Next() {
		var (
			e        model.SNIEntry
			enabled  int
			createdA string
		)
		if err := rows.Scan(&e.ID, &e.NodeID, &e.BindAddr, &e.BindPort, &enabled, &createdA); err != nil {
			return nil, fmt.Errorf("store: 读取 SNI 入口行: %w", err)
		}
		e.Enabled = intToBool(enabled)
		e.CreatedAt = parseTS(createdA)
		out = append(out, e)
	}
	return out, rows.Err()
}

// EnsureDefaultSNIEntry 保证节点上存在一处默认共享入口（*:443），返回其 ID。
// 新增 SNI 业务时若客户没显式选入口，用它兜底 —— 但**不隐式创建**：
// 必须先由管理员为该节点配置入口，否则说明这个节点还没准备好承载 SNI 业务。
func (s *Store) EnsureDefaultSNIEntry(nodeID string) (string, error) {
	entries, err := s.ListSNIEntries(nodeID)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.BindPort == 443 && (e.BindAddr == "" || e.BindAddr == "0.0.0.0") {
			return e.ID, nil
		}
	}
	return "", fmt.Errorf("%w: 节点 %s 上没有 *:443 共享入口，请先在节点管理里创建", ErrNotFound, nodeID)
}

// mustAffect 把"影响 0 行"翻译成 ErrNotFound。
// 少了这层，客户端会收到 200 但数据根本没改（典型的假成功）。
func mustAffect(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return nil // 驱动不支持时不做判断，避免误报
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	return nil
}

// IsNotFound 供上层做 errors.Is 语义判断。
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsConflict 供上层做 errors.Is 语义判断。
func IsConflict(err error) bool { return errors.Is(err, ErrConflict) }
