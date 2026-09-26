package store

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shengyu/edgelink/internal/id"
	"github.com/shengyu/edgelink/internal/logparse"
	"github.com/shengyu/edgelink/internal/model"
)

// nodeResolver 把 HAProxy 对象名（backend / server）反解回平台对象。
//
// 两级解析，先版本快照、再当前配置：
//
//	① 业务归属：backend 名 = "bk_" + handle(businessID)，businessID 随机且永不复用，
//	   所以 handle → businessID 的映射**与配置版本无关**，历史日志的归属不会漂移。
//	   这正是需求 §四"禁止直接用域名拼接内部对象名称"带来的好处之一 ——
//	   若当初用域名拼 backend 名，域名易主后旧日志就会指到新业务上。
//
//	② **源站地址：必须来自该日志所属版本的不可变快照**（评审 F11）。
//	   源站会随配置变更而变。若用当前配置反查，一条"在 v3 建立、在 v5 结束时结束"的长连接
//	   会被归到 v5 的源站上 —— 排障时结论会指向错误的源站。
//	   快照缺失时**留空**（界面显示"不可用"），绝不退回当前配置去猜。
//
// 快照带一层小缓存：日志是持续高频到达的，同一版本会被反复查询，
// 每次查库会把日志摄入变成数据库压力源。
type nodeResolver struct {
	nodeID string
	biz    map[string]string      // handle / bk_handle -> businessID
	route  map[string]string      // businessID -> routeID（该节点上的）
	origin map[string]net.TCPAddr // businessID -> 源站（当前配置，仅用于"当前值"类展示）

	store *Store

	mu      sync.Mutex
	snaps   map[int]*snapshotEntry
	snapErr int
}

type snapshotEntry struct {
	snap *VersionSnapshot
	at   time.Time
}

// snapshotCacheTTL 版本快照的缓存时长。
//
// 为什么不设成"永久缓存"：一个版本行的快照确实不会变（不可变），但**行本身可能被
// 保留策略清理**（历史版本裁剪），永久缓存会让解析器一直用一个已不存在的版本。
// 设 5 分钟是成本与正确性的折中，同时把缓存大小限制在合理范围内。
const snapshotCacheTTL = 5 * time.Minute

// ResolverForNode 构造某节点的日志解析器。
func (s *Store) ResolverForNode(nodeID string) (logparse.Resolver, error) {
	// 1. 全部业务（含停用）的 ID，用于建立 handle 映射。
	rows, err := s.db.Query(`SELECT id FROM businesses`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询业务用于解析: %w", err)
	}
	r := &nodeResolver{
		nodeID: nodeID,
		biz:    map[string]string{},
		route:  map[string]string{},
		origin: map[string]net.TCPAddr{},
		store:  s,
		snaps:  map[int]*snapshotEntry{},
	}
	for rows.Next() {
		var bid string
		if err := rows.Scan(&bid); err != nil {
			rows.Close()
			return nil, err
		}
		h := id.Handle(bid)
		r.biz[h] = bid
		r.biz["bk_"+h] = bid // 同时登记带前缀的形式，简化下面查表
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 2. 该节点上的路由，用于补 routeID 与"当前源站"。
	routes, err := s.ListRoutesByNode(nodeID)
	if err != nil {
		return nil, err
	}
	for _, rt := range routes {
		r.route[rt.BusinessID] = rt.ID
		r.origin[rt.BusinessID] = net.TCPAddr{
			IP:   net.ParseIP(rt.OriginHost),
			Port: rt.OriginPort,
		}
	}
	return r, nil
}

// BusinessForBackend 实现 logparse.Resolver。
func (r *nodeResolver) BusinessForBackend(beName string) (string, string, bool) {
	beName = strings.TrimSpace(beName)
	if beName == "" || beName == logparse.Unavailable {
		return "", "", false
	}
	bid, ok := r.biz[beName]
	if !ok {
		// 未匹配 SNI 的兜底 backend（bk_unmatched_sni_<port>）走到这里，
		// 它**不应该**归属到任何业务 —— 这正是它在诊断页要单独分类的原因。
		return "", "", false
	}
	return bid, r.route[bid], true
}

// OriginForBackend 实现 logparse.Resolver（**当前配置**的源站）。
//
// 只作为"没有版本信息"时的兜底。带版本的日志走 OriginForBackendAt。
func (r *nodeResolver) OriginForBackend(beName string) (string, int, bool) {
	bid, _, ok := r.BusinessForBackend(beName)
	if !ok {
		return "", 0, false
	}
	a, ok := r.origin[bid]
	if !ok {
		return "", 0, false
	}
	host := ""
	if a.IP != nil {
		host = a.IP.String()
	}
	return host, a.Port, true
}

// BusinessForBackendAt 实现 logparse.VersionedResolver。
func (r *nodeResolver) BusinessForBackendAt(cfgVersion int, beName string) (string, string, bool) {
	snap, ok := r.snapshotAt(cfgVersion)
	if !ok {
		return "", "", false
	}
	bid, ok := snap.BackendToBiz[strings.TrimSpace(beName)]
	if !ok || bid == "" {
		return "", "", false
	}
	// routeID 取该业务在当前节点上的路由 ID：路由 ID 与 (业务, 节点) 一一对应，
	// 业务重建路由时会拿到新 ID，届时应以快照里的 backend 名为准去核对 ——
	// 这里取不到就留空，不编造。
	return bid, r.route[bid], true
}

// OriginForBackendAt 实现 logparse.VersionedResolver：按**该日志所属版本**的源站。
func (r *nodeResolver) OriginForBackendAt(cfgVersion int, beName string) (string, int, bool) {
	snap, ok := r.snapshotAt(cfgVersion)
	if !ok {
		return "", 0, false
	}
	hp, ok := snap.BackendOrigin[strings.TrimSpace(beName)]
	if !ok || hp == "" {
		return "", 0, false
	}
	host, portStr, err := net.SplitHostPort(hp)
	if err != nil {
		// 快照里存的是 "host:port"；解析不了说明数据坏了 —— 如实返回"不可用"。
		return "", 0, false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, false
	}
	return host, port, true
}

// snapshotAt 取某版本的快照（带缓存）。
func (r *nodeResolver) snapshotAt(version int) (*VersionSnapshot, bool) {
	if version <= 0 || r.store == nil {
		return nil, false
	}
	now := time.Now()
	r.mu.Lock()
	if e, ok := r.snaps[version]; ok && now.Sub(e.at) < snapshotCacheTTL {
		snap := e.snap
		r.mu.Unlock()
		return snap, snap != nil
	}
	r.mu.Unlock()

	snap, err := r.store.ConfigVersionSnapshot(r.nodeID, version)
	r.mu.Lock()
	if len(r.snaps) > 512 {
		r.snaps = map[int]*snapshotEntry{}
	}
	r.snaps[version] = &snapshotEntry{snap: snap, at: now}
	r.mu.Unlock()
	if err != nil {
		return nil, false
	}
	return snap, true
}

var _ logparse.Resolver = (*nodeResolver)(nil)
var _ logparse.VersionedResolver = (*nodeResolver)(nil)
var _ = model.ModeTCPPort
