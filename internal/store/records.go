package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/shengyu/edgelink/internal/model"
)

// ============================== 配置版本（需求 §五） ==============================

// ConfigVersionStatus 配置版本的生命周期状态。
//
// 刻意区分 candidate / validated / active / failed / rolled_back / superseded：
// 只用一个 "ok" 会让运维无法回答「现在线上到底跑的哪一版」。
type ConfigVersionStatus string

const (
	CfgCandidate  ConfigVersionStatus = "candidate"   // 已生成，未验证
	CfgValidated  ConfigVersionStatus = "validated"   // 语法/冲突检查通过
	CfgActive     ConfigVersionStatus = "active"      // 已生效（reload 成功且验证通过）
	CfgFailed     ConfigVersionStatus = "failed"      // 应用失败
	CfgRolledBack ConfigVersionStatus = "rolled_back" // 已被回滚
	CfgSuperseded ConfigVersionStatus = "superseded"  // 已被更高版本取代（历史）
)

// ConfigVersion 一条配置版本记录。
type ConfigVersion struct {
	ID                string              `json:"id"`
	NodeID            string              `json:"node_id"`
	Version           int                 `json:"version"`
	ContentHash       string              `json:"content_hash"`
	Status            ConfigVersionStatus `json:"status"`
	DirPath           string              `json:"dir_path"`
	ExpectedListeners []model.Listener    `json:"expected_listeners"`
	RouteCount        int                 `json:"route_count"`
	Note              string              `json:"note"`
	CreatedBy         string              `json:"created_by"`
	CreatedAt         time.Time           `json:"created_at"`
	ActivatedAt       time.Time           `json:"activated_at"`
	Error             string              `json:"error"`
}

// NextVersion 返回该节点下一个可用版本号（单调递增，永不回退）。
//
// 为什么不允许复用版本号：平滑 reload 期间新旧 worker 会同时写日志，
// 日志里带的是各自渲染期的版本号。若版本号可复用，日志就无法区分是谁写的。
func (s *Store) NextVersion(nodeID string) (int, error) {
	var maxV sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(version) FROM config_versions WHERE node_id=?`, nodeID).Scan(&maxV)
	if err != nil {
		return 0, fmt.Errorf("store: 查询节点最大版本号: %w", err)
	}
	if !maxV.Valid {
		return 1, nil
	}
	return int(maxV.Int64) + 1, nil
}

func (s *Store) CreateConfigVersion(cv *ConfigVersion) error {
	if cv.Status == "" {
		cv.Status = CfgCandidate
	}
	cv.CreatedAt = s.now()
	_, err := s.db.Exec(`INSERT INTO config_versions(
		id,node_id,version,content_hash,status,dir_path,expected_listeners,
		route_count,note,created_by,created_at,activated_at,error)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		cv.ID, cv.NodeID, cv.Version, cv.ContentHash, string(cv.Status), cv.DirPath,
		toJSON(cv.ExpectedListeners, "[]"), cv.RouteCount, cv.Note, cv.CreatedBy,
		ts(cv.CreatedAt), ts(cv.ActivatedAt), cv.Error)
	return wrapUnique(err, "创建配置版本")
}

func (s *Store) UpdateConfigVersionStatus(nodeID string, version int, status ConfigVersionStatus, errMsg string) error {
	var activated any
	if status == CfgActive {
		activated = ts(s.now())
	} else {
		activated = ""
	}
	res, err := s.db.Exec(`UPDATE config_versions SET status=?,error=?,
		activated_at=CASE WHEN ?<>'' THEN ? ELSE activated_at END
		WHERE node_id=? AND version=?`,
		string(status), errMsg, activated, ts(s.now()), nodeID, version)
	if err != nil {
		return fmt.Errorf("store: 更新配置版本状态: %w", err)
	}
	return mustAffect(res, "配置版本")
}

func (s *Store) GetConfigVersion(nodeID string, version int) (*ConfigVersion, error) {
	row := s.db.QueryRow(`SELECT id,node_id,version,content_hash,status,dir_path,
		expected_listeners,route_count,note,created_by,created_at,activated_at,error
		FROM config_versions WHERE node_id=? AND version=?`, nodeID, version)
	return scanConfigVersion(row)
}

// GetActiveConfigVersion 取当前生效版本。返回 ErrNotFound 表示该节点从未成功发布过。
func (s *Store) GetActiveConfigVersion(nodeID string) (*ConfigVersion, error) {
	row := s.db.QueryRow(`SELECT id,node_id,version,content_hash,status,dir_path,
		expected_listeners,route_count,note,created_by,created_at,activated_at,error
		FROM config_versions WHERE node_id=? AND status=? ORDER BY version DESC LIMIT 1`,
		nodeID, string(CfgActive))
	return scanConfigVersion(row)
}

// ListConfigVersions 历史版本列表（需求 §五：支持查看历史版本及一键回滚）。
// 注意：这里可以翻历史，但**只翻元数据**；配置正文在节点文件的版本目录里。
func (s *Store) ListConfigVersions(nodeID string, limit, offset int) ([]ConfigVersion, int, error) {
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM config_versions WHERE node_id=?`, nodeID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: 统计配置版本: %w", err)
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id,node_id,version,content_hash,status,dir_path,
		expected_listeners,route_count,note,created_by,created_at,activated_at,error
		FROM config_versions WHERE node_id=? ORDER BY version DESC LIMIT ? OFFSET ?`,
		nodeID, limit, max0(offset))
	if err != nil {
		return nil, 0, fmt.Errorf("store: 查询配置版本: %w", err)
	}
	defer rows.Close()
	out := []ConfigVersion{}
	for rows.Next() {
		cv, err := scanConfigVersion(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("store: 读取配置版本行: %w", err)
		}
		out = append(out, *cv)
	}
	return out, total, rows.Err()
}

// FindConfigVersionByHash 查找该节点上是否已经存在内容完全相同的版本。
// 命中则无需重新发布 —— 这能挡住"点两次保存产生两个版本号"的无意义增长。
func (s *Store) FindConfigVersionByHash(nodeID, hash string) (*ConfigVersion, error) {
	row := s.db.QueryRow(`SELECT id,node_id,version,content_hash,status,dir_path,
		expected_listeners,route_count,note,created_by,created_at,activated_at,error
		FROM config_versions WHERE node_id=? AND content_hash=? AND status IN (?,?)
		ORDER BY version DESC LIMIT 1`,
		nodeID, hash, string(CfgActive), string(CfgValidated))
	return scanConfigVersion(row)
}

func scanConfigVersion(sc interface{ Scan(...any) error }) (*ConfigVersion, error) {
	var (
		cv        ConfigVersion
		status    string
		listeners string
		createdA  string
		activA    string
	)
	if err := sc.Scan(&cv.ID, &cv.NodeID, &cv.Version, &cv.ContentHash, &status, &cv.DirPath,
		&listeners, &cv.RouteCount, &cv.Note, &cv.CreatedBy, &createdA, &activA, &cv.Error); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("%w: 配置版本", ErrNotFound)
		}
		return nil, err
	}
	cv.Status = ConfigVersionStatus(status)
	fromJSON(listeners, &cv.ExpectedListeners)
	cv.CreatedAt, cv.ActivatedAt = parseTS(createdA), parseTS(activA)
	return &cv, nil
}

// ============================== 发布记录（需求 §五 / §十二.6） ==============================

// ReleaseAction 发布动作。
type ReleaseAction string

const (
	ActionPublish  ReleaseAction = "publish"
	ActionRollback ReleaseAction = "rollback"
	ActionRepair   ReleaseAction = "repair" // 重放当前期望状态（不做规则改动）
)

// ReleaseStatus 发布结果。
type ReleaseStatus string

const (
	ReleaseRunning   ReleaseStatus = "running"
	ReleaseSucceeded ReleaseStatus = "succeeded"
	ReleaseFailed    ReleaseStatus = "failed"
	// ReleaseAppliedUnhealthy：配置应用成功，但源站业务本来就不可用。
	// 这是需求里明确要求区分的情况 —— 不能报"发布失败"，也不能报"一切正常"。
	ReleaseAppliedUnhealthy ReleaseStatus = "applied_origin_unhealthy"
	ReleaseRolledBack       ReleaseStatus = "rolled_back"
)

// Release 一次发布/回滚的完整留痕。
type Release struct {
	ID          string        `json:"id"`
	NodeID      string        `json:"node_id"`
	Action      ReleaseAction `json:"action"`
	FromVersion int           `json:"from_version"`
	ToVersion   int           `json:"to_version"`
	Status      ReleaseStatus `json:"status"`
	Phase       string        `json:"phase"`
	Result      string        `json:"result"`
	Detail      string        `json:"detail"`
	Actor       string        `json:"actor"`
	StartedAt   time.Time     `json:"started_at"`
	FinishedAt  time.Time     `json:"finished_at"`
}

func (s *Store) CreateRelease(r *Release) error {
	r.StartedAt = s.now()
	if r.Status == "" {
		r.Status = ReleaseRunning
	}
	_, err := s.db.Exec(`INSERT INTO releases(id,node_id,action,from_version,to_version,status,phase,result,detail,actor,started_at,finished_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.NodeID, string(r.Action), r.FromVersion, r.ToVersion, string(r.Status),
		r.Phase, r.Result, r.Detail, r.Actor, ts(r.StartedAt), ts(r.FinishedAt))
	return wrapUnique(err, "创建发布记录")
}

func (s *Store) FinishRelease(id string, status ReleaseStatus, phase, result, detail string) error {
	res, err := s.db.Exec(`UPDATE releases SET status=?,phase=?,result=?,detail=?,finished_at=? WHERE id=?`,
		string(status), phase, result, detail, ts(s.now()), id)
	if err != nil {
		return fmt.Errorf("store: 结束发布记录: %w", err)
	}
	return mustAffect(res, "发布记录")
}

func (s *Store) UpdateReleasePhase(id, phase string) error {
	res, err := s.db.Exec(`UPDATE releases SET phase=? WHERE id=?`, phase, id)
	if err != nil {
		return fmt.Errorf("store: 更新发布阶段: %w", err)
	}
	return mustAffect(res, "发布记录")
}

func (s *Store) ListReleases(nodeID string, limit, offset int) ([]Release, int, error) {
	where, args := "", []any{}
	if nodeID != "" {
		where, args = " WHERE node_id=?", []any{nodeID}
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM releases`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: 统计发布记录: %w", err)
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id,node_id,action,from_version,to_version,status,phase,result,detail,actor,started_at,finished_at
		FROM releases`+where+` ORDER BY started_at DESC LIMIT ? OFFSET ?`,
		append(args, limit, max0(offset))...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: 查询发布记录: %w", err)
	}
	defer rows.Close()
	out := []Release{}
	for rows.Next() {
		var (
			r              Release
			action, status string
			startedA, finA string
		)
		if err := rows.Scan(&r.ID, &r.NodeID, &action, &r.FromVersion, &r.ToVersion, &status,
			&r.Phase, &r.Result, &r.Detail, &r.Actor, &startedA, &finA); err != nil {
			return nil, 0, fmt.Errorf("store: 读取发布记录行: %w", err)
		}
		r.Action, r.Status = ReleaseAction(action), ReleaseStatus(status)
		r.StartedAt, r.FinishedAt = parseTS(startedA), parseTS(finA)
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// LastRelease 取最近一次发布，用于总览页显示"最近发布时间"。
func (s *Store) LastRelease(nodeID string) (*Release, error) {
	rows, _, err := s.ListReleases(nodeID, 1, 0)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: 节点 %s 尚无发布记录", ErrNotFound, nodeID)
	}
	return &rows[0], nil
}

// ============================== 审计（需求 §六.C） ==============================

// AuditEntry 一条审计记录。detail 里**禁止**出现口令/令牌/私钥 ——
// 写入前由调用方过一遍 redact 包，这里不做二次兜底（兜底会掩盖漏网字段）。
type AuditEntry struct {
	ID         string
	TS         time.Time
	Actor      string
	ActorIP    string
	Action     string
	TargetType string
	TargetID   string
	Summary    string
	Result     string
	Detail     string
}

func (s *Store) WriteAudit(e *AuditEntry) error {
	if e.TS.IsZero() {
		e.TS = s.now()
	}
	_, err := s.db.Exec(`INSERT INTO audit(id,ts,actor,actor_ip,action,target_type,target_id,summary,result,detail)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		e.ID, ts(e.TS), e.Actor, e.ActorIP, e.Action, e.TargetType, e.TargetID,
		e.Summary, e.Result, e.Detail)
	if err != nil {
		return fmt.Errorf("store: 写审计: %w", err)
	}
	return nil
}

// AuditFilter 审计查询条件。
type AuditFilter struct {
	Actor      string
	Action     string
	TargetType string
	TargetID   string
	From, To   time.Time
	Limit      int
	Offset     int
}

func (s *Store) ListAudit(f AuditFilter) ([]AuditEntry, int, error) {
	var where []string
	var args []any
	if f.Actor != "" {
		where = append(where, "actor=?")
		args = append(args, f.Actor)
	}
	if f.Action != "" {
		where = append(where, "action=?")
		args = append(args, f.Action)
	}
	if f.TargetType != "" {
		where = append(where, "target_type=?")
		args = append(args, f.TargetType)
	}
	if f.TargetID != "" {
		where = append(where, "target_id=?")
		args = append(args, f.TargetID)
	}
	if !f.From.IsZero() {
		where = append(where, "ts>=?")
		args = append(args, ts(f.From))
	}
	if !f.To.IsZero() {
		where = append(where, "ts<=?")
		args = append(args, ts(f.To))
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit`+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: 统计审计: %w", err)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id,ts,actor,actor_ip,action,target_type,target_id,summary,result,detail
		FROM audit`+clause+` ORDER BY ts DESC LIMIT ? OFFSET ?`, append(args, limit, max0(f.Offset))...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: 查询审计: %w", err)
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var (
			e AuditEntry
			t string
		)
		if err := rows.Scan(&e.ID, &t, &e.Actor, &e.ActorIP, &e.Action, &e.TargetType,
			&e.TargetID, &e.Summary, &e.Result, &e.Detail); err != nil {
			return nil, 0, fmt.Errorf("store: 读取审计行: %w", err)
		}
		e.TS = parseTS(t)
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// PurgeAuditBefore 按保留期清理审计（需求 §八：默认 90 天，可配置）。
func (s *Store) PurgeAuditBefore(cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM audit WHERE ts < ?`, ts(cutoff))
	if err != nil {
		return 0, fmt.Errorf("store: 清理审计: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ---------------------------------------------------------------------------
// 版本对象快照（评审 F11）
// ---------------------------------------------------------------------------

// VersionSnapshot 某个配置版本的对象快照，用于把历史日志富化回**当时**的归属。
//
// 为什么必须存在：config_versions 的行是**一版一行、写后不改**的，
// 因此它天然是不可变的历史快照。日志落地后可能几天后才被追查，
// 而那时业务可能已改过源站、甚至已经回滚 —— 用当前配置去解释旧日志，
// 会把故障指向错误的对象（"源站 B 有问题"，而日志描述的是源站 A 的连接）。
type VersionSnapshot struct {
	BackendToBiz  map[string]string `json:"backend_to_business"`
	BackendOrigin map[string]string `json:"backend_to_origin"`
	Servers       map[string]string `json:"backend_to_server"`
}

// SetConfigVersionObjectNames 把渲染产物的对象快照写入该版本行。
//
// 只允许写一次（写入后不再改动），这正是"不可变"的实现方式：
// 版本号单调递增、正文落在版本目录、快照落在这一行，三者共同构成可回溯的历史。
func (s *Store) SetConfigVersionObjectNames(nodeID string, version int, names any) error {
	_, err := s.db.Exec(`UPDATE config_versions SET object_names=? WHERE node_id=? AND version=?`,
		toJSON(names, "{}"), nodeID, version)
	if err != nil {
		return fmt.Errorf("store: 写入版本对象快照: %w", err)
	}
	return nil
}

// ConfigVersionSnapshot 读取某版本的不可变快照。
// 未找到（例如版本太老、快照功能上线前发布的）返回 ErrNotFound，
// 调用方据此**如实留空**，而不是退回当前配置去猜。
func (s *Store) ConfigVersionSnapshot(nodeID string, version int) (*VersionSnapshot, error) {
	var raw string
	err := s.db.QueryRow(`SELECT object_names FROM config_versions WHERE node_id=? AND version=?`,
		nodeID, version).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读取版本对象快照: %w", err)
	}
	snap := &VersionSnapshot{}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), snap)
	}
	if snap.BackendToBiz == nil && snap.BackendOrigin == nil {
		return nil, ErrNotFound
	}
	return snap, nil
}
