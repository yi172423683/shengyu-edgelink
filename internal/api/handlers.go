package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shengyu/edgelink/internal/dataplane"
	"github.com/shengyu/edgelink/internal/haproxy"
	"github.com/shengyu/edgelink/internal/id"
	"github.com/shengyu/edgelink/internal/logparse"
	"github.com/shengyu/edgelink/internal/logstore"
	"github.com/shengyu/edgelink/internal/model"
	"github.com/shengyu/edgelink/internal/netutil"
	"github.com/shengyu/edgelink/internal/publish"
	"github.com/shengyu/edgelink/internal/store"
	"github.com/shengyu/edgelink/internal/validate"
)

// ============================== 管理台元信息 ==============================

// ProductName 产品中文名。
//
// 界面抬头、安装脚本打印、systemd 描述必须同源，否则会出现
// "安装包叫 A、网页叫 B"的品牌漂移，而这类漂移在客户现场非常扎眼。
const ProductName = "盛愈边缘网关"

// handleMeta 给内置管理台一份"这台是什么、现在处于哪个阶段"的元信息。
//
// 存在的理由：向导要判断"是不是首次进入（还没有任何已发布业务）"，
// 这个判断**不能放在前端** —— 前端只知道当前这一次会话，
// 换个浏览器就会被重新引导一遍向导，而数据其实早就配好了。
// effectiveAppliedVersion 决定界面「当前运行版本」显示哪个数。
//
// 优先级：**数据面自己报的版本 > 节点表里 agent 上报的版本**。
//
// 为什么不取两者的大值（旧写法就是 `if v > applied { applied = v }`）：
// **回滚会让生效版本变小**，取 max 的结果是"回滚成功了，页面却仍显示回滚前那个更大的版本号"。
// 真机实测（见 docs/08 回滚 UI 验收 §6C）：v9 发布失败并自动回滚到 v3 之后，
// 节点表里 agent 上报的 9 还没被下一次心跳（20 秒一轮）刷新，
// 界面就显示"当前运行版本 v9"，而机器上跑的其实是 v3 ——
// 运维正低头看失败面板的那一刻，看到的恰好是错的版本号。
//
// 为什么以数据面为准是安全的：ActiveVersion 读的是 current/VERSION，
// 也就是 HAProxy 下一次 reload / 重启会加载的那份配置的标记，
// 它是"这台机器到底在跑哪一版"的唯一权威来源（首次发布成功后它也从 0 变成 1，
// 因此不会出现"发布成功却被踢回向导"的老问题）。
// 只有在读不到时才回退到 agent 上报值 —— 宁可显示一个可能偏旧的值，也不显示空。
func effectiveAppliedVersion(ctx context.Context, dp dataplane.Applier, reported int) int {
	if dp == nil {
		return reported
	}
	v, err := dp.ActiveVersion(ctx)
	if err != nil {
		return reported
	}
	return v
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.Store.ListNodes()
	if err != nil {
		mapStoreErr(w, err, "节点")
		return
	}
	_, totalBiz, err := s.Store.ListBusinesses(store.BusinessFilter{Limit: 1})
	if err != nil {
		mapStoreErr(w, err, "业务")
		return
	}

	var node map[string]any
	applied := 0
	if len(nodes) > 0 {
		n := nodes[0]
		// 「当前运行版本」以**数据面自己报的版本**为准，见 effectiveAppliedVersion 的说明。
		reported := n.AppliedVersion
		var dp dataplane.Applier
		if s.Pipeline != nil {
			dp = s.Pipeline.Applier
		}
		applied = effectiveAppliedVersion(r.Context(), dp, reported)
		node = map[string]any{
			"id":               n.ID,
			"name":             n.Name,
			"public_ipv4":      n.PublicIPv4,
			"public_ipv6":      n.PublicIPv6,
			"applied_version":  applied,
			"expected_version": n.ExpectedVersion,
			"version_skewed":   n.ExpectedVersion != applied,
			"haproxy_version":  n.HAProxyVersion,
			"health":           n.Health,
		}
	}

	// 首次初始化判定：必须**真的发布过**（生效版本 > 基线 0）且存在业务。
	// 只看"有没有业务"是不够的：建了业务但还没发布时，用户刷新页面会被踢回向导，
	// 可他明明正准备点发布。
	initialized := totalBiz > 0 && applied > haproxy.BaselineVersion

	listen := s.Config.ListenAddr
	if listen == "" {
		// 兜底：由 cmd 层注入，注入缺失时用当前访问地址，至少不会显示空。
		listen = r.Host
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"product":        ProductName,
		"version":        s.Config.Version,
		"listen_addr":    listen,
		"health_path":    "/api/health",
		"initialized":    initialized,
		"business_count": totalBiz,
		"node_count":     len(nodes),
		"node":           node,
		// TLS 能力矩阵：透传是**唯一已实现**的模式。
		//
		// 终止模式尚未实现，所以这里明确给 supported=false + 说明，
		// 界面据此把它渲染成"未开放"，而不是一个可选项。
		// 把没实现的功能显示为可用，比干脆不做这个功能更糟 ——
		// 用户会真的去点，然后得到一个看不懂的失败。
		"tls": map[string]any{
			"passthrough": map[string]any{
				"supported": true,
				"label":     "TLS 透传（证书在源站）",
				"note":      "平台不终止 TLS、不接触证书：客户端与你的源站直接完成握手，证书由源站自己持有和续期。",
			},
			"terminate": map[string]any{
				"supported": false,
				"label":     "TLS 终止（平台代申请证书）",
				"note":      "尚未实现：当前版本不会申请也不会续期任何证书。请继续使用透传模式，证书留在源站。",
			},
			"default_mode": string(model.ModeSNITLS),
		},
	})
}

// ============================== 总览 ==============================

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.Store.ListNodes()
	if err != nil {
		mapStoreErr(w, err, "节点")
		return
	}
	customers, err := s.Store.ListCustomers("")
	if err != nil {
		mapStoreErr(w, err, "客户")
		return
	}
	bizs, totalBiz, err := s.Store.ListBusinesses(store.BusinessFilter{Limit: 500})
	if err != nil {
		mapStoreErr(w, err, "业务")
		return
	}

	// 版本偏移：期望版本 != 实际生效版本，说明"推了但没生效"，这是运维最需要立刻看到的状态。
	type nodeBrief struct {
		ID              string             `json:"id"`
		Name            string             `json:"name"`
		Health          model.NodeHealth   `json:"health"`
		AppliedVersion  int                `json:"applied_version"`
		ExpectedVersion int                `json:"expected_version"`
		VersionSkewed   bool               `json:"version_skewed"`
		LastHeartbeat   time.Time          `json:"last_heartbeat"`
		HAProxyVersion  string             `json:"haproxy_version"`
		Caps            model.Capabilities `json:"capabilities"`
	}
	briefs := make([]nodeBrief, 0, len(nodes))
	online := 0
	for _, n := range nodes {
		if n.Health == model.NodeOnline {
			online++
		}
		briefs = append(briefs, nodeBrief{
			ID: n.ID, Name: n.Name, Health: n.Health,
			AppliedVersion: n.AppliedVersion, ExpectedVersion: n.ExpectedVersion,
			VersionSkewed: n.VersionSkewed(), LastHeartbeat: n.LastHeartbeat,
			HAProxyVersion: n.HAProxyVersion, Caps: n.Capabilities,
		})
	}

	// 最近发布
	rels, _, _ := s.Store.ListReleases("", 10, 0)
	audits, _, _ := s.Store.ListAudit(store.AuditFilter{Limit: 10})

	// 日志摄入健康（数据面 → 攒批 → 落盘）。
	//
	// 这一项存在的唯一理由是**把静默失败变可见**：这条链路任何一环丢弃都只累加计数器，
	// 既不报错也不影响转发，于是界面上"查不到日志"会被解释成"这段时间没有连接" ——
	// 而真实原因可能是写盘失败或缓冲溢出。排查时这是最费时间的一类误判。
	// nil 表示本实例未启用本机日志摄入（例如纯远程节点管理模式）。
	var ingestInfo map[string]any
	if s.Ingest != nil {
		is := s.Ingest.IngestStats()
		ingestInfo = map[string]any{
			"ingested":     is.Ingested,
			"duplicated":   is.Duplicated,
			"parse_errors": is.ParseErrs,
			"dropped":      is.Dropped,
		}
		if is.BootID != "" {
			ingestInfo["boot_id"] = is.BootID
		}
		if is.LastError != "" {
			ingestInfo["last_error"] = is.LastError
		}
		var bufferDrops int64
		if s.Batch != nil {
			bs := s.Batch.Stats()
			bufferDrops = bs.BufferDrops
			ingestInfo["buffer_drops"] = bs.BufferDrops
			ingestInfo["pending"] = bs.Pending
			ingestInfo["flushes"] = bs.Flushes
			if bs.LastError != "" {
				ingestInfo["last_error"] = bs.LastError
			}
		}
		if is.Dropped > 0 || bufferDrops > 0 {
			ingestInfo["warning"] = fmt.Sprintf(
				"有 %d 条日志在写入阶段被丢弃、%d 条因缓冲溢出被丢弃 —— 这些连接的记录不可恢复。"+
					"请先确认日志库目录可写、磁盘未满；同时注意「查不到日志」很可能正是它们造成的，"+
					"而不是「这段时间没有连接」。",
				is.Dropped, bufferDrops)
		}
	}

	// 日志用量（需求 §八：容量控制要可见）
	var usage []logstore.KindUsage
	for _, k := range logstore.AllKinds() {
		if u, err := s.Logs.Usage(k); err == nil {
			usage = append(usage, u)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"nodes":              briefs,
		"nodes_online":       online,
		"nodes_total":        len(nodes),
		"customers_total":    len(customers),
		"businesses_total":   totalBiz,
		"businesses_enabled": countEnabled(bizs),
		"recent_releases":    rels,
		"recent_audit":       audits,
		"log_usage":          usage,
		"log_ingest":         ingestInfo,
		"server_time":        s.now(),
		"disclaimer": "本页所有数字均来自节点实时上报与真实日志，不含任何模拟数据。" +
			"「连接数」是当前在线连接；「请求数」不适用于 TCP 层转发（本平台不解析载荷）；" +
			"「流量」是已转发的字节数。",
	})
}

func countEnabled(bs []model.Business) int {
	n := 0
	for _, b := range bs {
		if b.Enabled {
			n++
		}
	}
	return n
}

// ============================== 客户 ==============================

func (s *Server) handleListCustomers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.ListCustomers(r.URL.Query().Get("q"))
	if err != nil {
		mapStoreErr(w, err, "客户")
		return
	}
	// 顺带带上每个客户的业务数，避免前端 N+1 查询。
	type row struct {
		model.Customer
		BusinessCount int `json:"business_count"`
	}
	out := []row{}
	for _, c := range rows {
		_, n, _ := s.Store.ListBusinesses(store.BusinessFilter{CustomerID: c.ID, Limit: 1})
		out = append(out, row{Customer: c, BusinessCount: n})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "total": len(out)})
}

type customerReq struct {
	Name    string `json:"name"`
	Contact string `json:"contact"`
	Remark  string `json:"remark"`
	Enabled *bool  `json:"enabled"`
}

func (s *Server) handleCreateCustomer(w http.ResponseWriter, r *http.Request) {
	var req customerReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "bad_request", "")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "客户名称不能为空", "invalid_name", "")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	c := &model.Customer{
		ID: id.New("cus"), Name: strings.TrimSpace(req.Name),
		Contact: req.Contact, Remark: req.Remark, Enabled: enabled,
	}
	if err := s.Store.CreateCustomer(c); err != nil {
		mapStoreErr(w, err, "客户")
		return
	}
	s.audit(r, "customer.create", "customer", c.ID, "新建客户《"+c.Name+"》", "ok", "")
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) handleGetCustomer(w http.ResponseWriter, r *http.Request) {
	c, err := s.Store.GetCustomer(pathID(r))
	if err != nil {
		mapStoreErr(w, err, "客户")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleUpdateCustomer(w http.ResponseWriter, r *http.Request) {
	c, err := s.Store.GetCustomer(pathID(r))
	if err != nil {
		mapStoreErr(w, err, "客户")
		return
	}
	before := *c
	var req customerReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "bad_request", "")
		return
	}
	if strings.TrimSpace(req.Name) != "" {
		c.Name = strings.TrimSpace(req.Name)
	}
	c.Contact, c.Remark = req.Contact, req.Remark
	if req.Enabled != nil {
		c.Enabled = *req.Enabled
	}
	if err := s.Store.UpdateCustomer(c); err != nil {
		mapStoreErr(w, err, "客户")
		return
	}
	s.audit(r, "customer.update", "customer", c.ID, "修改客户《"+c.Name+"》", "ok", diffString(before, *c))
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleDeleteCustomer(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	c, _ := s.Store.GetCustomer(id)
	if err := s.Store.DeleteCustomer(id); err != nil {
		mapStoreErr(w, err, "客户")
		return
	}
	name := id
	if c != nil {
		name = c.Name
	}
	s.audit(r, "customer.delete", "customer", id, "删除客户《"+name+"》", "ok", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ============================== 节点 ==============================

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.Store.ListNodes()
	if err != nil {
		mapStoreErr(w, err, "节点")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nodes, "total": len(nodes)})
}

type nodeReq struct {
	Name          string `json:"name"`
	GroupName     string `json:"group_name"`
	AgentEndpoint string `json:"agent_endpoint"`
	PublicIPv4    string `json:"public_ipv4"`
	PublicIPv6    string `json:"public_ipv6"`
	Region        string `json:"region"`
	Enabled       *bool  `json:"enabled"`
}

func (s *Server) handleCreateNode(w http.ResponseWriter, r *http.Request) {
	var req nodeReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "bad_request", "")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "节点名称不能为空", "invalid_name", "")
		return
	}
	// 入口地址必须是 IP：需求 §四 明确"源站 IP 不接受域名"，
	// 而入口侧同理——用域名做入口会让 DNS 抖动直接影响转发可用性。
	if req.PublicIPv4 != "" && !isIPv4(req.PublicIPv4) {
		writeErr(w, http.StatusBadRequest, "公网 IPv4 格式不正确（只接受 IPv4 字面量）", "invalid_ipv4", req.PublicIPv4)
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	n := &model.Node{
		ID: id.New("node"), Name: strings.TrimSpace(req.Name), GroupName: req.GroupName,
		AgentEndpoint: req.AgentEndpoint, PublicIPv4: req.PublicIPv4, PublicIPv6: req.PublicIPv6,
		Region: req.Region, Enabled: enabled, Health: model.NodeUnknown,
	}
	if err := s.Store.CreateNode(n); err != nil {
		mapStoreErr(w, err, "节点")
		return
	}
	s.audit(r, "node.create", "node", n.ID, "新建节点《"+n.Name+"》", "ok", "")
	writeJSON(w, http.StatusCreated, n)
}

func (s *Server) handleGetNode(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.GetNode(pathID(r))
	if err != nil {
		mapStoreErr(w, err, "节点")
		return
	}
	entries, _ := s.Store.ListSNIEntries(n.ID)
	visited, _, _ := s.Store.ListReleases(n.ID, 5, 0)
	writeJSON(w, http.StatusOK, map[string]any{
		"node": n, "sni_entries": entries, "recent_releases": visited,
	})
}

func (s *Server) handleUpdateNode(w http.ResponseWriter, r *http.Request) {
	n, err := s.Store.GetNode(pathID(r))
	if err != nil {
		mapStoreErr(w, err, "节点")
		return
	}
	before := *n
	var req nodeReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "bad_request", "")
		return
	}
	if req.PublicIPv4 != "" && !isIPv4(req.PublicIPv4) {
		writeErr(w, http.StatusBadRequest, "公网 IPv4 格式不正确", "invalid_ipv4", req.PublicIPv4)
		return
	}
	if strings.TrimSpace(req.Name) != "" {
		n.Name = strings.TrimSpace(req.Name)
	}
	n.GroupName, n.AgentEndpoint = req.GroupName, req.AgentEndpoint
	n.PublicIPv4, n.PublicIPv6, n.Region = req.PublicIPv4, req.PublicIPv6, req.Region
	if req.Enabled != nil {
		n.Enabled = *req.Enabled
	}
	if err := s.Store.UpdateNode(n); err != nil {
		mapStoreErr(w, err, "节点")
		return
	}
	s.audit(r, "node.update", "node", n.ID, "修改节点《"+n.Name+"》", "ok", diffString(before, *n))
	writeJSON(w, http.StatusOK, n)
}

func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if err := s.Store.DeleteNode(id); err != nil {
		mapStoreErr(w, err, "节点")
		return
	}
	// 节点删除后必须撤销其凭据，否则一台被回收的机器仍能上报数据。
	if n, err := s.Store.RevokeNodeCredentials(id); err == nil && n > 0 {
		logf("已撤销节点 %s 的 %d 条凭据", id, n)
	}
	s.audit(r, "node.delete", "node", id, "删除节点", "ok", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleListSNIEntries(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.ListSNIEntries(pathID(r))
	if err != nil {
		mapStoreErr(w, err, "共享入口")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows})
}

type sniEntryReq struct {
	BindAddr string `json:"bind_addr"`
	BindPort int    `json:"bind_port"`
	Enabled  *bool  `json:"enabled"`
}

func (s *Server) handleCreateSNIEntry(w http.ResponseWriter, r *http.Request) {
	nodeID := pathID(r)
	if _, err := s.Store.GetNode(nodeID); err != nil {
		mapStoreErr(w, err, "节点")
		return
	}
	var req sniEntryReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "bad_request", "")
		return
	}
	if req.BindPort < 1 || req.BindPort > 65535 {
		writeErr(w, http.StatusBadRequest, "端口必须在 1-65535 之间", "invalid_port", "")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	addr := req.BindAddr
	if addr == "" || addr == "*" {
		addr = "0.0.0.0"
	}
	if !isIPv4(addr) {
		writeErr(w, http.StatusBadRequest, "绑定地址必须是 IPv4 字面量", "invalid_addr", addr)
		return
	}
	// 创建前做一次系统级端口占用预检（需求：创建前检查端口是否被占用）。
	//
	// 为什么选在这一步、而不是等发布：此刻这条入口**还没有**被 HAProxy 绑定，
	// 所以扫描系统监听不会出现"把本平台自己正在监听的端口当成冲突"的误判。
	// 典型收益：本机 443 已被 xray 之类的程序占用时，创建 443 入口会立刻被拦下，
	// 运维当场就知道该换端口，而不是等到发布后才发现绑定失败再回滚。
	//
	// 读不到 /proc（非 Linux）时 IsPortInUse 返回不占用 —— 判不出来就放行，
	// 最终裁决交给发布后的 Verify + 自动回滚，不在这儿误伤。
	if conflict, inUse, perr := netutil.InspectPort(addr, req.BindPort); perr == nil && inUse {
		// 报错必须带上"被谁占了"，并且查不出进程时要给出排查命令。
		// 只说"端口已被占用"会逼运维自己挨个进程排查，而在 443 常年被占用的机器上
		// 这一步往往是整条接入流程里最耗时的一段。
		writeErr(w, http.StatusConflict,
			conflict.Describe()+"。请换一个端口（例如 8443 / 9443），或先释放该端口后再创建",
			"port_in_use", conflict.InspectCmd)
		return
	}
	e := &model.SNIEntry{ID: id.New("sni"), NodeID: nodeID, BindAddr: addr, BindPort: req.BindPort, Enabled: enabled}
	if err := s.Store.CreateSNIEntry(e); err != nil {
		mapStoreErr(w, err, "共享入口")
		return
	}
	s.audit(r, "sni_entry.create", "node", nodeID,
		fmt.Sprintf("新增共享 SNI 入口 %s:%d", addr, req.BindPort), "ok", "")
	writeJSON(w, http.StatusCreated, e)
}

// handleDeleteSNIEntry 删除共享 SNI 入口。
//
// 三条纪律：
//
//   - **有业务路由引用就拒绝**。守卫在 store 层（`DeleteSNIEntry` 在一个事务里
//     先数引用再删），因为 handler 是可以被绕过的（批量脚本、未来的其它调用方），
//     而"不允许留下悬空引用"是数据完整性约束。
//     真机事故：改库删入口留下悬空引用 ⇒ 之后**每次**发布都被前置校验拒成 422（docs/08 §8）。
//   - **删成功必须提示重新发布**。删掉入口只改了"期望状态"；不发布的话，
//     界面上的入口列表与节点上跑的东西就不一致了（渲染器会跳过空入口，
//     所以不会残留监听 —— 但"看不到差异"和"没有差异"是两回事）。
//   - **写审计**。删一个入口会改变所有挂在该入口上的业务的可达性，必须留痕。
func (s *Server) handleDeleteSNIEntry(w http.ResponseWriter, r *http.Request) {
	nodeID := pathID(r)
	entryID := r.PathValue("eid")
	if strings.TrimSpace(entryID) == "" {
		writeErr(w, http.StatusBadRequest, "缺少入口 ID", "bad_request", "")
		return
	}
	// 先把这个入口取出来：删除后就查不到它的 地址:端口 了，
	// 而审计与"下一步"提示里都需要一个人类能认出来的标识。
	entries, err := s.Store.ListSNIEntries(nodeID)
	if err != nil {
		mapStoreErr(w, err, "共享入口")
		return
	}
	var target *model.SNIEntry
	for i := range entries {
		if entries[i].ID == entryID {
			target = &entries[i]
			break
		}
	}
	if target == nil {
		writeErr(w, http.StatusNotFound, "该节点上不存在这个共享入口", "not_found", entryID)
		return
	}

	if err := s.Store.DeleteSNIEntry(entryID); err != nil {
		if errors.Is(err, store.ErrSNIEntryInUse) {
			// 409：不是"请求写错了"，而是"当前状态不允许"。运维要做的是先处理那些业务。
			writeErr(w, http.StatusConflict, err.Error(), "sni_entry_in_use", entryID)
			return
		}
		mapStoreErr(w, err, "共享入口")
		return
	}
	label := fmt.Sprintf("%s:%d", target.BindAddr, target.BindPort)
	s.audit(r, "sni_entry.delete", "node", nodeID, "删除共享 SNI 入口 "+label, "ok", "")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"next_step": "入口 " + label + " 已从期望状态中移除，但节点上仍在按上一版配置运行 —— " +
			"请执行「发布」使其生效",
	})
}

// handlePortCheck 创建入口前的端口预检（只查不写）。
//
// 为什么单独开一个接口而不是让前端"先试着创建、看有没有 409"：
// 创建是写操作，失败时虽然会回滚，但会在审计里留下一条"创建失败"，
// 而用户其实只是想先问一句"这个端口能用吗"。
// 预检与创建共用同一套判定（netutil.InspectPort），不存在"预检说能用、创建却失败"的口径分歧。
func (s *Server) handlePortCheck(w http.ResponseWriter, r *http.Request) {
	nodeID := pathID(r)
	if _, err := s.Store.GetNode(nodeID); err != nil {
		mapStoreErr(w, err, "节点")
		return
	}
	var req struct {
		BindAddr string `json:"bind_addr"`
		BindPort int    `json:"bind_port"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "bad_request", "")
		return
	}
	if req.BindPort < 1 || req.BindPort > 65535 {
		writeErr(w, http.StatusBadRequest, "端口必须在 1-65535 之间", "invalid_port", "")
		return
	}
	addr := req.BindAddr
	if addr == "" || addr == "*" {
		addr = "0.0.0.0"
	}
	if !isIPv4(addr) {
		writeErr(w, http.StatusBadRequest, "绑定地址必须是 IPv4 字面量", "invalid_addr", addr)
		return
	}
	conflict, inUse, err := netutil.InspectPort(addr, req.BindPort)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "端口检查失败", "port_check_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"addr":     addr,
		"port":     req.BindPort,
		"in_use":   inUse,
		"conflict": conflict,
	})
}

func (s *Server) handleCreateNodeToken(w http.ResponseWriter, r *http.Request) {
	nodeID := pathID(r)
	if _, err := s.Store.GetNode(nodeID); err != nil {
		mapStoreErr(w, err, "节点")
		return
	}
	ttl := 30 * time.Minute
	var req struct {
		Note       string `json:"note"`
		TTLMinutes int    `json:"ttl_minutes"`
	}
	_ = decodeJSON(r, &req)
	if req.TTLMinutes > 0 {
		ttl = time.Duration(req.TTLMinutes) * time.Minute
	}
	tok, err := s.Store.CreateAgentToken(nodeID, req.Note, ttl)
	if err != nil {
		mapStoreErr(w, err, "注册令牌")
		return
	}
	s.audit(r, "node.token.create", "node", nodeID,
		fmt.Sprintf("签发注册令牌（%s 后过期，一次性）", ttl), "ok", "")
	// 明文只在这一次响应里出现；库里存的是哈希。
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":      tok.Token,
		"node_id":    nodeID,
		"expires_at": tok.ExpiresAt,
		"warning":    "该令牌只显示这一次，请立即填入节点上的 agent 配置。它是一次性的，用过后自动失效。",
	})
}

// ============================== 发布 ==============================

func (s *Server) handlePreviewConfig(w http.ResponseWriter, r *http.Request) {
	nodeID := pathID(r)
	next, err := s.Store.NextVersion(nodeID)
	if err != nil {
		mapStoreErr(w, err, "节点")
		return
	}
	st, err := s.Store.DesiredStateForNode(nodeID, next)
	if err != nil {
		mapStoreErr(w, err, "节点")
		return
	}
	res := validate.State(*st, validate.DefaultOptions())
	rendered, rerr := haproxy.Render(*st, s.Pipeline.Defaults)
	out := map[string]any{
		"version":            next,
		"validation":         res,
		"validation_ok":      res.OK(),
		"expected_listeners": dataplane.ExpectedListeners(*st),
	}
	if rerr != nil {
		out["render_error"] = rerr.Error()
	} else {
		out["content_hash"] = rendered.ContentHash
		out["config"] = string(rendered.Config)
		out["object_names"] = rendered.ObjectNames
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	nodeID := pathID(r)
	var req struct {
		Note string `json:"note"`
	}
	_ = decodeJSON(r, &req)

	// 发布前先做结构化校验：把能在平台侧发现的问题挡在"写文件+reload"之前。
	// 注意：这不是替代 HAProxy 的语法检查，而是更早一层——
	// 域名重复、端口冲突、源站回环这类**语义**问题，HAProxy 自己是看不出来的。
	next, err := s.Store.NextVersion(nodeID)
	if err != nil {
		mapStoreErr(w, err, "节点")
		return
	}
	st, err := s.Store.DesiredStateForNode(nodeID, next)
	if err != nil {
		mapStoreErr(w, err, "节点")
		return
	}
	vr := validate.State(*st, s.validationOptions())
	if !vr.OK() {
		s.audit(r, "config.publish", "node", nodeID, "发布被平台校验拦下", "denied", joinIssues(vr.Issues))
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "配置未通过平台校验，发布已中止（现有转发未受影响）",
			"code":   "validation_failed",
			"issues": vr.Issues,
		})
		return
	}

	// 发布前再查一次端口（需求：发布前再次检查端口；端口冲突不能破坏已有业务）。
	//
	// 关键：这里**不能**扫到系统监听里有端口就判冲突。上一个已发布版本的端口，
	// 此刻正是我们自己的 HAProxy 在监听；把它当成外部占用，会让"改个域名重新发布"
	// 这种最常规的操作被误拦。所以先排除当前生效版本自己的监听，剩下的才是真冲突。
	//
	// 这样冲突时会在**写文件之前**中止（422），一个字节都不动：现有转发零影响。
	if conflict, conflicted := s.externalPortConflicts(nodeID, st); conflicted {
		s.audit(r, "config.publish", "node", nodeID, "发布被端口占用检查拦下", "denied", conflict)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "以下监听端口已被本机其它程序占用，发布已中止（现有转发未受影响）",
			"code":   "port_in_use",
			"detail": conflict,
		})
		return
	}

	actor := ""
	if u, ok := r.Context().Value(ctxUser).(*store.User); ok {
		actor = u.Username
	}
	res, err := s.Pipeline.Publish(r.Context(), nodeID, actor, req.Note)
	if err != nil && res == nil {
		// 非本机节点必须在这里被明确挡回，并留下审计 ——
		// 这是"误操作会导致另一台机器业务中断"的那类请求，即使被拒绝也值得记录。
		if errors.Is(err, publish.ErrRemoteNode) {
			s.audit(r, "config.publish", "node", nodeID,
				"拒绝：该节点不由本实例管理", "denied", err.Error())
			writeErr(w, http.StatusConflict,
				"该节点不由本实例管理，拒绝发布（本机现有转发未受影响）",
				"remote_node_unsupported", err.Error())
			return
		}
		mapStoreErr(w, err, "发布")
		return
	}
	if s.OnPublished != nil && res != nil && res.Status != store.ReleaseFailed && !res.NoChange {
		s.OnPublished(nodeID, res.Version)
	}
	status := http.StatusOK
	if res.Status == store.ReleaseFailed {
		status = http.StatusConflict
	}
	writeJSON(w, status, res)
}

// externalPortConflicts 检查本次期望监听里有没有被「外部进程」占用的端口。
//
// 返回 (说明, 是否冲突)。
//
// 判定口径（这是本函数存在的全部理由）：
//   - 系统监听表里没有该端口        → 不冲突；
//   - 该端口属于**当前已生效版本**自己的监听 → 不冲突（那是本平台的 HAProxy 在听，
//     属于"我要续用它"，不是冲突）；
//   - 其余被 LISTEN 占用的端口      → 冲突（别的程序先占了，例如 xray 占着 443）。
//
// 读不到系统监听表（非 Linux / 权限不足）时返回不冲突 —— 判不出来就让位于
// 发布后的 Verify：它按 frontend 名核对归属，绑定失败还会触发回滚。
// 宁可多放一次，也不能把常规发布拦死。
func (s *Server) externalPortConflicts(nodeID string, st *model.DesiredState) (string, bool) {
	ports, err := netutil.ListeningPorts()
	if err != nil || len(ports) == 0 {
		return "", false
	}
	// 当前已生效版本自己的监听：排除掉，避免把自家人当冲突。
	own := map[string]bool{}
	if st.Node.AppliedVersion > 0 {
		if cur, cerr := s.Store.DesiredStateForNode(nodeID, st.Node.AppliedVersion); cerr == nil {
			for _, l := range dataplane.ExpectedListeners(*cur) {
				own[l.Key()] = true
			}
		}
	}
	var conflicts []string
	for _, l := range dataplane.ExpectedListeners(*st) {
		if own[l.Key()] {
			continue
		}
		if inUse, owners := netutil.PortInUse(l.Addr, l.Port, ports); inUse {
			conflicts = append(conflicts, fmt.Sprintf("%s（已被 %s 占用）", l.Key(), strings.Join(owners, ", ")))
		}
	}
	if len(conflicts) == 0 {
		return "", false
	}
	return "请改用其它端口或先释放这些端口：" + strings.Join(conflicts, "；"), true
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	nodeID := pathID(r)
	var req struct {
		Version int    `json:"version"`
		Reason  string `json:"reason"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "bad_request", "")
		return
	}
	if req.Version <= 0 {
		writeErr(w, http.StatusBadRequest, "必须指定要回滚到的版本号", "invalid_version", "")
		return
	}
	actor := ""
	if u, ok := r.Context().Value(ctxUser).(*store.User); ok {
		actor = u.Username
	}
	res, err := s.Pipeline.Rollback(r.Context(), nodeID, req.Version, actor, req.Reason)
	if err != nil && res == nil {
		if errors.Is(err, publish.ErrRemoteNode) {
			s.audit(r, "config.rollback", "node", nodeID,
				"拒绝：该节点不由本实例管理", "denied", err.Error())
			writeErr(w, http.StatusConflict,
				"该节点不由本实例管理，拒绝回滚（本机现有转发未受影响）",
				"remote_node_unsupported", err.Error())
			return
		}
		mapStoreErr(w, err, "回滚")
		return
	}
	if s.OnPublished != nil && res != nil && res.Status != store.ReleaseFailed {
		s.OnPublished(nodeID, req.Version)
	}
	status := http.StatusOK
	if res.Status == store.ReleaseFailed {
		status = http.StatusConflict
	}
	writeJSON(w, status, res)
}

func (s *Server) handleListVersions(w http.ResponseWriter, r *http.Request) {
	nodeID := pathID(r)
	limit, offset := pageParams(r)
	rows, total, err := s.Store.ListConfigVersions(nodeID, limit, offset)
	if err != nil {
		mapStoreErr(w, err, "配置版本")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows, "total": total, "limit": limit, "offset": offset})
}

func (s *Server) handleListReleases(w http.ResponseWriter, r *http.Request) {
	nodeID := pathID(r)
	limit, offset := pageParams(r)
	rows, total, err := s.Store.ListReleases(nodeID, limit, offset)
	if err != nil {
		mapStoreErr(w, err, "发布记录")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows, "total": total, "limit": limit, "offset": offset})
}

func (s *Server) handleNodeStats(w http.ResponseWriter, r *http.Request) {
	nodeID := pathID(r)
	n, err := s.Store.GetNode(nodeID)
	if err != nil {
		mapStoreErr(w, err, "节点")
		return
	}
	out := map[string]any{
		"node_id":          n.ID,
		"health":           n.Health,
		"applied_version":  n.AppliedVersion,
		"expected_version": n.ExpectedVersion,
		"last_heartbeat":   n.LastHeartbeat,
	}
	if s.NodeID == nodeID && s.Pipeline != nil && s.Pipeline.Applier != nil {
		if st, serr := s.Pipeline.Applier.Stats(r.Context()); serr == nil {
			out["stats"] = st
		} else {
			out["stats_error"] = serr.Error()
		}
		if ls, lerr := s.Pipeline.Applier.Listeners(r.Context()); lerr == nil {
			out["listeners"] = ls
		}
	} else {
		out["stats_error"] = "该节点不是本实例所在机器；实时指标由该节点的 agent 采集后上报（见节点上报的 metrics）"
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleNodeClientTraffic(w http.ResponseWriter, r *http.Request) {
	nodeID := pathID(r)
	if _, err := s.Store.GetNode(nodeID); err != nil {
		mapStoreErr(w, err, "节点")
		return
	}
	if s.NodeID != nodeID || s.Pipeline == nil || s.Pipeline.Applier == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "items": []dataplane.ClientTraffic{}, "note": "远程节点尚未上报客户端实时流量"})
		return
	}
	reporter, ok := s.Pipeline.Applier.(dataplane.ClientTrafficReporter)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "items": []dataplane.ClientTraffic{}, "note": "当前数据面不支持客户端实时流量"})
		return
	}
	items, err := reporter.ClientTraffic(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "客户端实时流量暂不可用", "client_traffic_unavailable", err.Error())
		return
	}
	active := items[:0]
	for _, item := range items {
		if item.ActiveConnections > 0 || item.BytesUpRate > 0 || item.BytesDownRate > 0 { active = append(active, item) }
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "at": s.now(), "items": active})
}

// ============================== 业务 ==============================

func (s *Server) handleListBusinesses(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, offset := pageParams(r)
	f := store.BusinessFilter{
		CustomerID: q.Get("customer_id"),
		NodeID:     q.Get("node_id"),
		Keyword:    q.Get("q"),
		OnlyEnable: q.Get("enabled") == "1",
		Limit:      limit, Offset: offset,
	}
	rows, total, err := s.Store.ListBusinesses(f)
	if err != nil {
		mapStoreErr(w, err, "业务")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows, "total": total, "limit": limit, "offset": offset})
}

// businessReq 新增/修改业务的请求体。
//
// 这里只接受**结构化字段**，绝不接受原始 HAProxy 配置或任何形式的命令字符串
// （需求 §四 明确禁止）。想加新能力就加字段，不走"透传配置"这条捷径。
type businessReq struct {
	CustomerID    string   `json:"customer_id"`
	Name          string   `json:"name"`
	Remark        string   `json:"remark"`
	Mode          string   `json:"mode"`
	Enabled       *bool    `json:"enabled"`
	Domains       []string `json:"domains"`
	PrimaryNodeID string   `json:"primary_node_id"`
	BackupNodeID  string   `json:"backup_node_id"`
	OriginHost    string   `json:"origin_host"`
	OriginPort    int      `json:"origin_port"`
	EntryAddr     string   `json:"entry_addr"`
	EntryPort     int      `json:"entry_port"`
	SNIEntryID    string   `json:"sni_entry_id"`

	ConnectTimeoutMS int                `json:"connect_timeout_ms"`
	ClientTimeoutMS  int                `json:"client_timeout_ms"`
	ServerTimeoutMS  int                `json:"server_timeout_ms"`
	MaxConn          int                `json:"maxconn"`
	QueueLimit       int                `json:"queue_limit"`
	HealthCheck      *model.HealthCheck `json:"healthcheck"`
}

// buildBusiness 把请求体转成领域对象，并做全量校验。
func (s *Server) buildBusiness(req businessReq, existing *model.Business) (*model.Business, error) {
	b := &model.Business{}
	if existing != nil {
		*b = *existing
	} else {
		b.ID = id.New("biz")
		b.Enabled = true
	}
	if strings.TrimSpace(req.Name) != "" {
		b.Name = strings.TrimSpace(req.Name)
	}
	b.Remark = req.Remark
	if req.CustomerID != "" {
		b.CustomerID = req.CustomerID
	}
	if req.Mode != "" {
		b.Mode = model.Mode(req.Mode)
	}
	if req.Enabled != nil {
		b.Enabled = *req.Enabled
	}
	if req.Domains != nil {
		b.Domains = normalizeDomains(req.Domains)
	}
	if req.PrimaryNodeID != "" {
		b.PrimaryNodeID = req.PrimaryNodeID
	}
	if req.BackupNodeID != "" {
		b.BackupNodeID = req.BackupNodeID
	}
	if existing == nil || req.ConnectTimeoutMS != 0 {
		b.ConnectTimeoutMS = req.ConnectTimeoutMS
	}
	if existing == nil || req.ClientTimeoutMS != 0 {
		b.ClientTimeoutMS = req.ClientTimeoutMS
	}
	if existing == nil || req.ServerTimeoutMS != 0 {
		b.ServerTimeoutMS = req.ServerTimeoutMS
	}
	if existing == nil || req.MaxConn != 0 {
		b.MaxConn = req.MaxConn
	}
	if existing == nil || req.QueueLimit != 0 {
		b.QueueLimit = req.QueueLimit
	}
	if req.HealthCheck != nil {
		b.HealthCheck = *req.HealthCheck
	} else if existing == nil {
		b.HealthCheck = model.DefaultHealthCheck()
	}
	return b, nil
}

func (s *Server) handleCreateBusiness(w http.ResponseWriter, r *http.Request) {
	var req businessReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "bad_request", "")
		return
	}
	if req.Name == "" || req.CustomerID == "" || req.PrimaryNodeID == "" {
		writeErr(w, http.StatusBadRequest, "客户、业务名称、首选节点为必填项", "missing_field", "")
		return
	}
	b, err := s.buildBusiness(req, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "bad_request", "")
		return
	}
	if err := s.validateBusinessRequest(b, req); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, err)
		return
	}

	if err := s.Store.CreateBusiness(b); err != nil {
		mapStoreErr(w, err, "业务")
		return
	}
	if err := s.upsertRoutes(b, req); err != nil {
		// 业务与路由必须一致：路由建不起来时把业务回滚掉，
		// 否则会留下"业务存在但没有任何路由"的幽灵，界面上看起来一切正常却根本不转发。
		_ = s.Store.DeleteBusiness(b.ID)
		mapStoreErr(w, err, "路由")
		return
	}
	s.audit(r, "business.create", "business", b.ID,
		fmt.Sprintf("接入业务《%s》（客户 %s，模式 %s）", b.Name, b.CustomerID, b.Mode), "ok", "")
	writeJSON(w, http.StatusCreated, map[string]any{"business": b, "next_step": "请到节点页执行「发布」使规则生效"})
}

func (s *Server) handleGetBusiness(w http.ResponseWriter, r *http.Request) {
	b, err := s.Store.GetBusiness(pathID(r))
	if err != nil {
		mapStoreErr(w, err, "业务")
		return
	}
	routes, _ := s.Store.ListRoutesByBusiness(b.ID)
	writeJSON(w, http.StatusOK, map[string]any{"business": b, "routes": routes})
}

func (s *Server) handleUpdateBusiness(w http.ResponseWriter, r *http.Request) {
	existing, err := s.Store.GetBusiness(pathID(r))
	if err != nil {
		mapStoreErr(w, err, "业务")
		return
	}
	var req businessReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "bad_request", "")
		return
	}
	b, err := s.buildBusiness(req, existing)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "bad_request", "")
		return
	}
	if err := s.validateBusinessRequest(b, req); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, err)
		return
	}
	before := *existing
	if err := s.Store.UpdateBusiness(b); err != nil {
		mapStoreErr(w, err, "业务")
		return
	}
	if err := s.upsertRoutes(b, req); err != nil {
		mapStoreErr(w, err, "路由")
		return
	}
	s.audit(r, "business.update", "business", b.ID, "修改业务《"+b.Name+"》", "ok", diffString(before, *b))
	writeJSON(w, http.StatusOK, map[string]any{"business": b, "next_step": "改动尚未生效，请到节点页执行「发布」"})
}

func (s *Server) handleDeleteBusiness(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	b, _ := s.Store.GetBusiness(id)
	if err := s.Store.DeleteBusiness(id); err != nil {
		mapStoreErr(w, err, "业务")
		return
	}
	name := id
	if b != nil {
		name = b.Name
	}
	s.audit(r, "business.delete", "business", id, "删除业务《"+name+"》", "ok", "")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "next_step": "请在相关节点上执行「发布」，使该业务从转发规则中移除",
	})
}

func (s *Server) handleListRoutes(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.ListRoutesByBusiness(pathID(r))
	if err != nil {
		mapStoreErr(w, err, "路由")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows})
}

// handleDNSInstructions 返回该业务接入所需的 DNS 配置说明（新业务向导最后一步）。
func (s *Server) handleDNSInstructions(w http.ResponseWriter, r *http.Request) {
	b, err := s.Store.GetBusiness(pathID(r))
	if err != nil {
		mapStoreErr(w, err, "业务")
		return
	}
	routes, _ := s.Store.ListRoutesByBusiness(b.ID)
	type instruction struct {
		NodeID     string   `json:"node_id"`
		NodeName   string   `json:"node_name"`
		Role       string   `json:"role"`
		RecordType string   `json:"record_type"`
		Domains    []string `json:"domains"`
		TargetHint string   `json:"target"`
		Note       string   `json:"note"`
	}
	// 端口一律从数据库读，绝不写死 443（需求：443 只是默认推荐值，不是系统唯一入口）。
	//
	// 这里曾经写死过 "端口为 443"：用户建了 8443 入口，界面却仍告诉客户去连 443，
	// 属于"配置是对的、指引是错的"—— 最难被发现的一类错误。
	sniPort := map[string]int{} // sniEntryID -> bind_port
	for _, rt := range routes {
		if rt.Mode != model.ModeSNITLS || rt.SNIEntryID == "" {
			continue
		}
		if _, ok := sniPort[rt.SNIEntryID]; ok {
			continue
		}
		entries, lerr := s.Store.ListSNIEntries(rt.NodeID)
		if lerr != nil {
			continue
		}
		for _, e := range entries {
			sniPort[e.ID] = e.BindPort
		}
	}

	var out []instruction
	for _, rt := range routes {
		n, err := s.Store.GetNode(rt.NodeID)
		if err != nil {
			continue
		}
		target := n.PublicIPv4
		note := "把上述域名的 A 记录指向该 IP。"
		if b.Mode == model.ModeSNITLS {
			port := sniPort[rt.SNIEntryID]
			if port == 0 {
				// 取不到入口（数据异常）时退回 443 只是保守兜底，并明确说这不是固定值。
				port = 443
			}
			target = fmt.Sprintf("%s（端口 %d）", n.PublicIPv4, port)
			note = fmt.Sprintf("把上述域名的 A 记录指向该 IP；客户端连接 域名:%d（TLS SNI 透传，不改动你的证书）。"+
				"该端口来自业务的共享入口配置，不是固定 443。", port)
		} else {
			target = fmt.Sprintf("%s（端口 %d）", n.PublicIPv4, rt.EntryPort)
			note = "把客户端指向该 IP:端口。此模式为纯 TCP 转发，不需要 SNI。"
		}
		out = append(out, instruction{
			NodeID: n.ID, NodeName: n.Name, Role: string(rt.Role),
			RecordType: "A", Domains: b.Domains, TargetHint: target, Note: note,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"business_id":  b.ID,
		"mode":         b.Mode,
		"instructions": out,
		"limitations": []string{
			"第一版不支持通配符域名规则（必须逐个子域名配置）。",
			"第一版不支持 HTTP/3 与 ECH；ECH 下无法识别真实域名，此类客户端会被拒绝。",
			"第一版不提供内网穿透能力。",
			"平台不迁移既有连接：修改规则或切换节点时，已建立的连接不会被切断，但也不会被迁移到新目标。",
		},
	})
}

// syncRoutes 依据业务的主/备节点与请求里的源站信息，创建或更新路由。
func (s *Server) upsertRoutes(b *model.Business, req businessReq) error {
	// 先清掉不再需要的节点（例如把备节点换了）
	existing, err := s.Store.ListRoutesByBusiness(b.ID)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	if b.PrimaryNodeID != "" {
		keep[b.PrimaryNodeID] = true
	}
	if b.BackupNodeID != "" {
		keep[b.BackupNodeID] = true
	}
	for _, rt := range existing {
		if !keep[rt.NodeID] {
			if derr := s.Store.DeleteRoute(rt.ID); derr != nil {
				return derr
			}
		}
	}

	roles := []struct {
		nodeID string
		role   model.Role
	}{}
	if b.PrimaryNodeID != "" {
		roles = append(roles, struct {
			nodeID string
			role   model.Role
		}{b.PrimaryNodeID, model.RolePrimary})
	}
	if b.BackupNodeID != "" && b.BackupNodeID != b.PrimaryNodeID {
		roles = append(roles, struct {
			nodeID string
			role   model.Role
		}{b.BackupNodeID, model.RoleBackup})
	}

	entryAddr := req.EntryAddr
	if entryAddr == "" {
		entryAddr = "0.0.0.0"
	}
	for _, rl := range roles {
		if _, err := s.Store.GetNode(rl.nodeID); err != nil {
			return fmt.Errorf("节点 %s 不存在", rl.nodeID)
		}
		// 入口字段按模式取值 —— 这是需求 §三 的核心约束：
		//   SNI 模式：入口 = 该节点上的共享入口（*:443），路由**不能**自带入口地址/端口。
		//             若允许，多域名共享 443 这件事就被破坏了（校验层会直接拦下）。
		//   TCP 模式：入口 = 本路由独占的 地址:端口。
		rtEntryAddr, rtEntryPort := entryAddr, req.EntryPort
		sniID := req.SNIEntryID
		if b.Mode == model.ModeSNITLS {
			rtEntryAddr, rtEntryPort = "", 0
			if sniID == "" {
				// 未指定就找该节点的默认共享入口；找不到就明确报错，
				// 而不是"自动建一个"——那会让运维不知道 443 被谁占了。
				var eerr error
				if sniID, eerr = s.Store.EnsureDefaultSNIEntry(rl.nodeID); eerr != nil {
					return fmt.Errorf("节点 %s：%w", rl.nodeID, eerr)
				}
			}
		} else {
			if rtEntryPort < 1 || rtEntryPort > 65535 {
				return errors.New("TCP 端口转发模式必须指定 1-65535 的入口端口")
			}
			sniID = ""
		}
		rt := &model.Route{
			ID: id.New("rt"), BusinessID: b.ID, NodeID: rl.nodeID, Role: rl.role, Mode: b.Mode,
			SNIEntryID: sniID, EntryAddr: rtEntryAddr, EntryPort: rtEntryPort,
			OriginHost: req.OriginHost, OriginPort: req.OriginPort,
			QueueLimit: req.QueueLimit, MaxConn: req.MaxConn, Enabled: b.Enabled,
		}
		if err := s.Store.UpsertRouteForNode(rt); err != nil {
			return err
		}
	}
	return nil
}

// validateBusinessRequest 做语义校验并把问题整理成界面可直接展示的结构。
//
// 分两层：
//  1. 单条业务自身的字段校验（validate.Business）
//  2. 跨业务的冲突校验（域名是否已被别的业务占用）——这一条必须查库
func (s *Server) validateBusinessRequest(b *model.Business, req businessReq) *validationError {
	res := validate.Business(*b, b.Domains, s.validationOptions())

	// 域名跨业务占用检测：同一节点、同一 SNI 入口上同一个域名若被两条业务声明，
	// SNI 路由会出现"最后一条赢"。不同入口端口可以复用同一域名。
	if len(b.Domains) > 0 {
		owner := map[string]string{}
		all, _, err := s.Store.ListBusinesses(store.BusinessFilter{Limit: 500})
		if err == nil {
			for _, other := range all {
				if other.ID == b.ID {
					continue
				}
				routes, routeErr := s.Store.ListRoutesByBusiness(other.ID)
				if routeErr != nil {
					continue
				}
				for _, route := range routes {
					if route.NodeID != req.PrimaryNodeID || route.Mode != model.ModeSNITLS || route.SNIEntryID == "" {
						continue
					}
					for _, d := range other.Domains {
						owner[route.SNIEntryID+"\x00"+strings.ToLower(d)] = other.ID
					}
				}
			}
		}
		res.Merge(validate.DomainsAcrossBusinessesForEntry(b.ID, req.SNIEntryID, b.Domains, owner))
	}

	if !res.OK() {
		return &validationError{
			Message: "业务配置未通过校验",
			Code:    "validation_failed",
			Issues:  res.Issues,
		}
	}
	return nil
}

// validationError 是给界面直接展示的校验失败结构。
type validationError struct {
	Message string           `json:"error"`
	Code    string           `json:"code"`
	Issues  []validate.Issue `json:"issues"`
}

func (e *validationError) Error() string {
	return fmt.Sprintf("%s（%d 项）", e.Message, len(e.Issues))
}

// ============================== 日志与诊断 ==============================

func (s *Server) handleLogFields(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"fields": haproxy.FieldsForUI(),
		"notes": []string{
			"「不可用」表示该版本 HAProxy 不提供这个字段，或被明确关闭；它不是 0。",
			"「等待时间」是 HAProxy 的 Tw：等待连接的总时间（含排队），不是纯排队时间。",
			"TCP 会话日志只有会话结束后才完整，因此实时流量与在线连接请以「实时指标」为准。",
		},
	})
}

func (s *Server) handleQueryLogs(w http.ResponseWriter, r *http.Request) {
	q, err := parseLogQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "bad_query", "")
		return
	}
	rows, ex, err := s.Logs.QueryConn(r.Context(), q)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "query_failed", err.Error())
		return
	}
	// 若调用方没给时间范围，我们用了默认值 —— 必须**明说**。
	// 界面上一片空白时，最容易被误读成"客户端没发请求"；
	// 把"你其实只查了最近一小时"讲清楚，能省掉大量无谓排查。
	notes := ex.Notes
	if r.URL.Query().Get("from") == "" || r.URL.Query().Get("to") == "" {
		notes = append(notes, fmt.Sprintf(
			"请求未指定完整时间范围，已默认查询最近 %s。查不到记录不能判定客户端没有发起请求。",
			time.Hour))
		ex.Notes = notes
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":   rows,
		"explain": ex,
		"limit":   q.Limit,
		"offset":  q.Offset,
		"hint": "查不到记录**不能**判定客户端没有发起请求：可能是时间范围不对、" +
			"日志尚未上报，或该连接确实未到达本平台。请结合「诊断」页的判断一起看。",
	})
}

func (s *Server) handleTrafficSummary(w http.ResponseWriter, r *http.Request) {
	days := 30
	if raw := r.URL.Query().Get("days"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil { days = n }
	}
	if days < 1 { days = 1 }
	if days > 31 { days = 31 }
	now := s.now()
	q := logstore.Query{From: now.Add(-time.Duration(days) * 24 * time.Hour), To: now,
		BusinessID: r.URL.Query().Get("business_id"), NodeID: r.URL.Query().Get("node_id"),
		SNI: r.URL.Query().Get("sni")}
	buckets, err := s.Logs.TrafficSummary(r.Context(), q)
	if err != nil { writeErr(w, http.StatusBadRequest, err.Error(), "traffic_summary_failed", err.Error()); return }
	var total logstore.TrafficBucket
	for _, b := range buckets { total.Connections += b.Connections; total.BytesUp += b.BytesUp; total.BytesDown += b.BytesDown }
	writeJSON(w, http.StatusOK, map[string]any{
		"from": q.From, "to": q.To, "days": days, "total": total, "daily": buckets,
		"geo": map[string]any{"available": false, "note": "当前版本保留客户端 IP；地区需要配置 GeoIP 数据库后启用。未知地址会保留为未知。"},
	})
}

func (s *Server) handleTrafficClients(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	period := r.URL.Query().Get("period")
	from := dayStart
	switch period {
	case "", "today":
		period = "today"
	case "7d":
		from = dayStart.AddDate(0, 0, -6)
	case "30d":
		from = dayStart.AddDate(0, 0, -29)
	default:
		writeErr(w, http.StatusBadRequest, "period 仅支持 today、7d、30d", "bad_period", "")
		return
	}
	q := logstore.Query{From: from, To: now, NodeID: r.URL.Query().Get("node_id"), SNI: r.URL.Query().Get("sni")}
	items, err := s.Logs.TrafficByClient(r.Context(), q)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "traffic_clients_failed", err.Error())
		return
	}
	var total logstore.ClientTrafficBucket
	for _, item := range items {
		total.Connections += item.Connections
		total.BytesUp += item.BytesUp
		total.BytesDown += item.BytesDown
	}
	writeJSON(w, http.StatusOK, map[string]any{"period": period, "from": from, "to": now, "total": total, "items": items})
}

func parseLogQuery(r *http.Request) (logstore.Query, error) {
	q := logstore.Query{}
	now := time.Now()
	get := r.URL.Query()
	from, err := parseTimeParam(get.Get("from"), now.Add(-1*time.Hour))
	if err != nil {
		return q, fmt.Errorf("from 时间格式不正确: %w", err)
	}
	to, err := parseTimeParam(get.Get("to"), now)
	if err != nil {
		return q, fmt.Errorf("to 时间格式不正确: %w", err)
	}
	q.From, q.To = from, to
	q.BusinessID = get.Get("business_id")
	q.NodeID = get.Get("node_id")
	q.ClientIP = get.Get("client_ip")
	q.SNI = get.Get("sni")
	q.OnlyAnomaly = get.Get("only_anomaly") == "1"
	if v := get.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			q.Limit = n
		}
	}
	if v := get.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			q.Offset = n
		}
	}
	q.BeforeEndTS = get.Get("before_end_ts")
	if v := get.Get("before_id"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			q.BeforeID = n
		}
	}
	return q, nil
}

func parseTimeParam(s string, def time.Time) (time.Time, error) {
	if strings.TrimSpace(s) == "" {
		return def, nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("无法解析: " + s)
}

// Finding 一条诊断结论。按需求 §七，必须给出四段：事实 / 原始依据 / 可能原因 / 下一步。
type Finding struct {
	Category string   `json:"category"`
	Severity string   `json:"severity"`
	Count    int      `json:"count"`
	Facts    []string `json:"facts"`
	Evidence []string `json:"evidence"`
	Causes   []string `json:"possible_causes"`
	Next     []string `json:"next_steps"`
}

// handleDiagnose 按需求 §七 输出诊断结论。
//
// 最重要的一条纪律：**不能只凭 timeout/reset 就说"被封"**。
// 因此这里的每条结论都只描述"观察到了什么"，把"可能原因"严格放在单独字段里，
// 且必须给出下一步检查建议 —— 让运维能继续查，而不是止步于一个猜测。
func (s *Server) handleDiagnose(w http.ResponseWriter, r *http.Request) {
	q, err := parseLogQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "bad_query", "")
		return
	}
	rows, ex, err := s.Logs.QueryConn(r.Context(), q)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "query_failed", err.Error())
		return
	}

	var findings []Finding

	if len(rows) == 0 {
		findings = append(findings, Finding{
			Category: "no_records",
			Severity: "info",
			Facts: []string{
				fmt.Sprintf("在 %s ~ %s 的范围内、按当前筛选条件，没有查到任何连接记录。",
					q.From.Format(time.RFC3339), q.To.Format(time.RFC3339)),
			},
			Causes: []string{
				"客户端确实没有发起请求；",
				"客户端发起了请求，但没有到达本平台（DNS 未生效、被运营商阻断、对端网络问题）；",
				"请求到达了，但时间范围选得不对；",
				"日志尚未上报或已超出保留期被清理。",
			},
			Next: []string{
				"把时间范围放宽到当前时间点前后各 10 分钟再看一次。",
				"在总览页确认节点的「最后心跳」是最近的 —— 如果心跳也停了，说明日志本来就没上来。",
				"检查 DNS：从公网解析该域名，确认指向的是本平台节点的入口 IP。",
				"以上都正常时，再用真实客户端从目标地区复现一次，并在诊断页按「客户端 IP」筛选。",
			},
		})
	}

	// 按类别聚合
	agg := map[string]*Finding{}
	bump := func(cat, sev, fact, evidence string, causes, next []string) {
		f, ok := agg[cat]
		if !ok {
			f = &Finding{Category: cat, Severity: sev, Causes: causes, Next: next}
			agg[cat] = f
		}
		f.Count++
		f.Facts = appendUnique(f.Facts, fact)
		if evidence != "" {
			f.Evidence = appendUnique(f.Evidence, evidence)
		}
	}

	for _, row := range rows {
		ev := truncateForUI(row.RawLine, 500)
		if !row.ParseOK {
			bump("log_parse_failed", "warning",
				"存在平台无法解析的日志行，这些连接的完整信息不可用。",
				ev,
				[]string{"HAProxy 版本与当前日志格式不匹配（升级过 HAProxy？）",
					"日志行被截断（例如日志采集端有长度限制）",
					"日志本身是合法的，但平台字段定义需要更新"},
				[]string{"用「原始日志」列对比 docs/04 中登记的字段与 HAProxy 实际输出。",
					"确认节点的 HAProxy 版本是否在受支持范围内。"})
			continue
		}
		reason := row.TermReason
		switch {
		case strings.Contains(reason, "未匹配 SNI"):
			bump("sni_not_matched", "warning",
				fmt.Sprintf("有 %d 条连接的 SNI 未命中任何已配置业务，按策略被拒绝。", 1),
				ev,
				[]string{"客户域名还没配进平台，或已配但尚未发布到该节点；",
					"客户端使用的域名与配置的域名大小写/子域不一致；",
					"客户端根本没发 SNI（例如某些 SDK 直接用 IP 连接）。",
				},
				[]string{"确认该域名确实属于某个客户业务，并在业务页核对拼写。",
					"若刚添加，请到节点页执行「发布」并确认版本已生效。",
					"查看本页「未匹配 SNI」过滤后的记录，逐条核对 SNI 字段的实际取值。"})
		case strings.Contains(reason, "源站连接失败"):
			bump("origin_unreachable", "error",
				"中转节点无法连接到源站。这是**源站侧**的问题，不是中转平台没生效。",
				ev,
				[]string{"源站服务未启动或端口未监听；",
					"源站防火墙未放行中转节点的出口 IP；",
					"源站 IP/端口配置写错；",
					"源站所在网络到中转节点之间不通。"},
				[]string{"在该节点上直接测试到源站的连通性，确认是不是全面不通还是偶发。",
					"核对业务里配置的源站 IP 与端口。",
					"确认源站侧已把中转节点的出口 IP 加入白名单。"})
		case strings.Contains(reason, "非 TLS 流量"), strings.Contains(reason, "未发送 SNI"):
			bump("entry_protocol_mismatch", "warning",
				"SNI 入口收到了非 TLS 或不带 SNI 的流量，无法路由。",
				ev,
				[]string{"客户端把明文 HTTP 或普通 TCP 发到了 443；",
					"客户端用 IP 直连而没有设置 ServerName；",
					"该业务本该用 TCP 端口转发模式，却配成了 SNI 模式。"},
				[]string{"确认客户端协议：SNI 模式只适用于 TLS 流量。",
					"若是普通 TCP 应用，请改用 TCP 端口转发模式并分配独立入口端口。"})
		case row.QueueMS != nil && *row.QueueMS > 1000, row.QueueMax != nil && *row.QueueMax > 0:
			bump("queued", "warning",
				"存在排队现象，说明后端连接建立慢或并发已达上限。",
				ev,
				[]string{"源站响应慢导致连接堆积；",
					"业务配置了 maxconn/queue 上限且已被打满；",
					"中转节点自身资源（CPU/带宽）不足。"},
				[]string{"查看实时指标里的当前连接数与队列长度。",
					"核对业务的连接数限制是否设得过小。",
					"检查源站处理能力与中转节点的出口带宽。"})
		case row.TermCode == "SC" || row.TermCode == "SD" || row.TermCode == "SH":
			bump("session_aborted", "info",
				"存在会话在建立阶段即被中断的记录。",
				ev,
				[]string{"客户端提前关闭连接（用户取消、客户端超时设置过短）；",
					"服务端主动拒绝；",
					"中间网络丢包导致握手失败。"},
				[]string{"结合「客户端 IP」筛选，看是否集中在某个来源。",
					"对比成功会话与失败会话的 SNI/源站，找出差异。"})
		}
	}
	for _, f := range agg {
		findings = append(findings, *f)
	}
	sort.Slice(findings, func(i, j int) bool {
		prio := map[string]int{"error": 0, "warning": 1, "info": 2}
		return prio[findings[i].Severity] < prio[findings[j].Severity]
	})

	// 配置层面的诊断：期望版本与实际版本不一致 = 发布没生效
	var cfgFindings []Finding
	nodes, _ := s.Store.ListNodes()
	for _, n := range nodes {
		if q.NodeID != "" && n.ID != q.NodeID {
			continue
		}
		if n.VersionSkewed() {
			cfgFindings = append(cfgFindings, Finding{
				Category: "config_version_skew", Severity: "error", Count: 1,
				Facts: []string{fmt.Sprintf("节点 %s 的期望版本是 v%d，但实际生效版本是 v%d。",
					n.Name, n.ExpectedVersion, n.AppliedVersion)},
				Causes: []string{"发布在 reload 或验证阶段失败并已回滚；",
					"agent 未及时上报最新版本；",
					"有人直接在节点上改过配置或重启过服务。"},
				Next: []string{"到「配置版本」页查看该节点最近一次发布的阶段与错误信息。",
					"必要时执行「发布」重放当前期望状态。"},
			})
		}
		if n.Health == model.NodeOffline {
			cfgFindings = append(cfgFindings, Finding{
				Category: "node_offline", Severity: "error", Count: 1,
				Facts: []string{fmt.Sprintf("节点 %s 已超过心跳超时未上报（最后心跳 %s）。",
					n.Name, n.LastHeartbeat.Format(time.RFC3339))},
				Causes: []string{"agent 进程退出或被停止；", "节点网络不可达；", "节点整机故障。"},
				Next: []string{"确认节点上 agent 服务状态。",
					"注意：agent 离线**不代表转发已停止** —— 数据面会用最后一个有效配置继续服务。"},
			})
		}
	}
	// 悬空引用自检：配置侧最危险的一种脏数据。
	//
	// 放在这一段（与 config_version_skew / node_offline 同级）是因为它属于"配置一致性"，
	// 而不是"日志里观察到的现象"。
	//
	// 为什么不等到发布时再说：悬空引用一旦进入渲染，那条路由会被**静默丢掉**
	//（渲染器按入口 ID 索引），表现是"发布成功但业务不转发"。
	// 真机上它第一次暴露的方式是"**每次**发布都被前置校验拒成 422"，
	// 而那时运维正在改另一件事（见 docs/08 §8）—— 早一步发现，就少一次事故。
	if dangling, derr := s.Store.FindDanglingReferences(); derr != nil {
		cfgFindings = append(cfgFindings, Finding{
			Category: "dangling_check_failed", Severity: "warning", Count: 1,
			Facts:  []string{"悬空引用自检本身没能执行完，因此**无法判断**是否存在不一致的数据。"},
			Causes: []string{"平台元数据库不可读，或表结构与当前版本不匹配。"},
			Next: []string{"查看服务日志里的 store 错误；确认元数据库文件可用且可写。",
				"在此之前，不要把「没报悬空引用」当成「数据是干净的」。"},
		})
	} else if len(dangling) > 0 {
		facts := make([]string, 0, len(dangling))
		for _, d := range dangling {
			facts = append(facts, fmt.Sprintf("%s 引用了不存在的%s（%s）",
				d.From, danglingKindLabel(d.Kind), d.MissingID))
		}
		next := []string{}
		for _, d := range dangling {
			next = appendUnique(next, d.Detail)
		}
		next = append(next,
			"修完数据后**务必发布一次**：只改期望状态而不发布，节点上跑的还是旧配置。",
			"优先用平台接口删除或改挂，不要再直接改库 —— 改库绕过了引用守卫，"+
				"这正是这类脏数据的来源（SNI 入口已支持删除接口）。")
		cfgFindings = append(cfgFindings, Finding{
			Category: "dangling_reference", Severity: "error", Count: len(dangling),
			Facts: facts,
			Causes: []string{
				"有人直接改库删除记录，绕过了引用守卫（例如删掉一个仍被路由引用的 SNI 共享入口）；",
				"外键约束开启之前写入的历史数据；",
				"导入/迁移脚本只写了一部分记录。",
			},
			Next: next,
		})
	}

	findings = append(findings, cfgFindings...)

	writeJSON(w, http.StatusOK, map[string]any{
		"findings":     findings,
		"scanned_rows": len(rows),
		"explain":      ex,
		"classification_note": "以上分类只描述观察到的现象与原始依据。" +
			"**仅凭 timeout 或 reset 不能判定「被封」**，本页也不会给出这种结论。" +
			"「TCP 连接成功」同样不等于「业务正常」，因为平台不解析业务载荷。",
		"export_hint": "导出诊断包会包含：相关日志、指标摘要、配置版本与探测结果；" +
			"并自动剔除密钥、令牌以及不属于所选业务的数据。",
	})
}

// danglingKindLabel 把悬空引用的类型翻译成人话（界面上的事实描述不该出现内部枚举值）。
func danglingKindLabel(kind string) string {
	switch kind {
	case "sni_entry":
		return "SNI 共享入口"
	case "business":
		return "业务"
	case "node":
		return "节点"
	}
	return kind
}

// joinIssues 把校验问题压成一行摘要，用于审计记录。
func joinIssues(issues []validate.Issue) string {
	var parts []string
	for _, i := range issues {
		parts = append(parts, string(i.Severity)+":"+i.Code+":"+i.Message)
	}
	return strings.Join(parts, " | ")
}

func appendUnique(list []string, s string) []string {
	if s == "" {
		return list
	}
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

func truncateForUI(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	limit, offset := pageParams(r)
	from, _ := parseTimeParam(r.URL.Query().Get("from"), time.Time{})
	to, _ := parseTimeParam(r.URL.Query().Get("to"), time.Time{})
	f := store.AuditFilter{
		Actor:      r.URL.Query().Get("actor"),
		Action:     r.URL.Query().Get("action"),
		TargetType: r.URL.Query().Get("target_type"),
		TargetID:   r.URL.Query().Get("target_id"),
		From:       from, To: to, Limit: limit, Offset: offset,
	}
	rows, total, err := s.Store.ListAudit(f)
	if err != nil {
		mapStoreErr(w, err, "审计")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows, "total": total})
}

// handleRetention 返回各日志流的占用、磁盘水位与**将要执行**的维护计划。
//
// 评审 F12 的要点是"不要因为结构里有字段就宣称功能已完成"，所以这里刻意把
// 三件事分开呈现，让运维一眼能看出哪些是真在生效的：
//
//	usage      当前占用（分片数 / 字节）
//	policy     本次生效的策略（含容量上限与磁盘水位 —— 不再是只有保留天数）
//	plan       按该策略算出来的动作（delete / compress），只读预览
//	disk       日志根目录所在文件系统的可用空间
//
// 真正执行由后台维护任务按同一份策略调用 ApplyMaintenance ——
// 计划与执行是同一个纯函数算出来的，不会出现"预览说会删、实际没删"。
func (s *Server) handleRetention(w http.ResponseWriter, r *http.Request) {
	type kindUsage struct {
		Kind   string                     `json:"kind"`
		Usage  logstore.KindUsage         `json:"usage"`
		Policy logstore.RetentionPolicy   `json:"policy"`
		Plan   []logstore.RetentionAction `json:"plan"`
	}
	pol := logstore.DefaultRetentionPolicy()
	free, freeErr := s.Logs.FreeBytes()
	if freeErr != nil {
		free = -1
	}
	out := []kindUsage{}
	for _, k := range logstore.AllKinds() {
		u, err := s.Logs.Usage(k)
		if err != nil {
			continue
		}
		plan, _, _ := s.Logs.PlanMaintenance(k, pol, s.now(), free)
		out = append(out, kindUsage{Kind: string(k), Usage: u, Policy: pol, Plan: plan})
	}
	disk := map[string]any{"free_bytes": free}
	if freeErr != nil {
		disk["error"] = freeErr.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kinds": out,
		"disk":  disk,
		"note": "保留期、容量上限（QuotaBytes）与磁盘水位（MinFreeBytes）三者同时生效，先到先清理。" +
			"到期的整点分片会被压缩归档（.db.gz，逐字节校验后才删原文件），仍可查询（查询时自动解压）。" +
			"所有删除与压缩动作都会出现在 plan 里，不会静默丢数据。",
	})
}

// ============================== 节点 Agent 面 ==============================

func (s *Server) handleAgentRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token        string `json:"token"`
		AgentVersion string `json:"agent_version"`
		Hostname     string `json:"hostname"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "bad_request", "")
		return
	}
	nodeID, err := s.Store.ConsumeAgentToken(req.Token)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "注册令牌无效", "token_invalid", err.Error())
		return
	}
	cred, err := s.Store.IssueNodeCredential(nodeID, "register from "+req.Hostname)
	if err != nil {
		mapStoreErr(w, err, "节点凭据")
		return
	}
	_ = s.Store.WriteAudit(&store.AuditEntry{
		ID: id.New("aud"), Actor: "agent:" + nodeID, ActorIP: clientIP(r),
		Action: "agent.register", TargetType: "node", TargetID: nodeID,
		Summary: "节点使用一次性注册令牌完成注册", Result: "ok",
		Detail: "agent_version=" + req.AgentVersion + " hostname=" + req.Hostname,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"node_id":    nodeID,
		"credential": cred,
		"warning":    "该凭据只显示这一次，请写入 /etc/shengyu/agent.conf 并设置 0600 权限。",
	})
}

func (s *Server) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
	nodeID, _ := r.Context().Value(ctxToken).(string)
	var req struct {
		AgentVersion   string             `json:"agent_version"`
		HAProxyVersion string             `json:"haproxy_version"`
		AppliedVersion int                `json:"applied_version"`
		DataplaneOK    bool               `json:"dataplane_ok"`
		Capabilities   model.Capabilities `json:"capabilities"`
		Stats          *dataplane.Stats   `json:"stats"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "bad_request", "")
		return
	}
	err := s.Store.RecordHeartbeat(store.HeartbeatInput{
		NodeID: nodeID, AgentVersion: req.AgentVersion, HAProxyVersion: req.HAProxyVersion,
		AppliedVersion: req.AppliedVersion, DataplaneOK: req.DataplaneOK, Capabilities: req.Capabilities,
	})
	if err != nil {
		mapStoreErr(w, err, "心跳")
		return
	}
	// 返回期望版本，让 agent 知道"管理面要你跑哪一版"，
	// 从而自行决定是否要主动拉配置（这样平台离线也不影响转发）。
	var expected int
	var desired bool
	if n, gerr := s.Store.GetNode(nodeID); gerr == nil {
		expected = n.ExpectedVersion
		desired = n.ExpectedVersion != req.AppliedVersion
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "expected_version": expected, "need_pull": desired,
		"server_time": s.now(),
	})
}

func (s *Server) handleAgentDesired(w http.ResponseWriter, r *http.Request) {
	nodeID, _ := r.Context().Value(ctxToken).(string)
	next, err := s.Store.NextVersion(nodeID)
	if err != nil {
		mapStoreErr(w, err, "节点")
		return
	}
	st, err := s.Store.DesiredStateForNode(nodeID, next)
	if err != nil {
		mapStoreErr(w, err, "节点")
		return
	}
	rendered, rerr := haproxy.Render(*st, s.Pipeline.Defaults)
	out := map[string]any{
		"version":            next,
		"expected_listeners": dataplane.ExpectedListeners(*st),
		"routes":             st.Routes,
		"sni_entries":        st.SNIEntries,
		"server_time":        s.now(),
	}
	if rerr == nil {
		out["content_hash"] = rendered.ContentHash
		out["config"] = string(rendered.Config)
		out["allowlists"] = rendered.Allowlists
		out["object_names"] = rendered.ObjectNames
	}
	writeJSON(w, http.StatusOK, out)
}

// MaxAgentLogRows 单次上报允许的最大行数。
//
// 有上限是必须的：没有上限时，一个被攻陷（或被写错）的 agent 可以一次 POST 一个
// 几百万行的数组，把管理进程的内存吃光。上限设 5000 是"一次上报 5000 行日志"，
// 对正常 agent（每秒几十行、攒批上报）绰绰有余。
const MaxAgentLogRows = 5000

// handleAgentLogs 接收节点上报的连接日志。
//
// **协议要求 `boot_id`（采集端本次启动的标识），它是幂等键的一部分。**
//
// 为什么它是必需的、而不是可选的（评审 F07 的远程侧）：
//
//	日志表的幂等键是 (node_id, boot_id, seq)，写入用 INSERT OR IGNORE。
//	agent 的 seq 只存在内存里，进程一重启就从 1 重新计数。
//	如果上报里不带 boot_id（或被填成空串），重启后的一整段日志会与重启前的序号相撞，
//	被当成"重复记录"**静默丢弃** —— 转发一切正常，但日志永久少了一段。
//
// 这也是一个**协议版本升级点**：旧 agent 不带 boot_id，这里明确拒绝并说明原因，
// 而不是"容忍它、顺便把日志丢了"。宁可让 agent 升级，也不要一份看起来正常、
// 实则缺段的日志。
func (s *Server) handleAgentLogs(w http.ResponseWriter, r *http.Request) {
	nodeID, _ := r.Context().Value(ctxToken).(string)
	var req struct {
		BootID string        `json:"boot_id"`
		Rows   []agentLogRow `json:"rows"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "bad_request", "")
		return
	}
	bootID := strings.TrimSpace(req.BootID)
	if bootID == "" {
		writeErr(w, http.StatusBadRequest,
			"上报缺少 boot_id：采集端启动标识是幂等键的一部分，缺少它会导致重启后的日志被误判为重复而丢弃",
			"missing_boot_id",
			"请在请求体的 boot_id 字段填入本次采集进程的启动标识（每次启动换新值，重启后保持不变）")
		return
	}
	if len(req.Rows) > MaxAgentLogRows {
		writeErr(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("单次上报最多 %d 行，本次 %d 行；请分批上报", MaxAgentLogRows, len(req.Rows)),
			"too_many_rows", "")
		return
	}
	res, err := s.Store.ResolverForNode(nodeID)
	if err != nil {
		mapStoreErr(w, err, "节点配置快照")
		return
	}
	rows := make([]logstore.ConnRow, 0, len(req.Rows))
	parseErrs := 0
	for _, ar := range req.Rows {
		rec, perr := logparse.Parse([]byte(ar.Line), res)
		if perr != nil {
			// 解析失败**仍然入库**：丢掉的日志就是运维事后查不到的日志。
			// ParseOK=false + RawLine 保留原文，诊断页会单独列出这类行。
			parseErrs++
		}
		if rec.NodeID == "" {
			rec.NodeID = nodeID
		}
		rows = append(rows, logstore.ConnRow{
			BootID: bootID, Seq: ar.Seq, RecvTS: s.now(), Rec: rec,
		})
	}
	ing, err := s.Logs.Ingest(r.Context(), rows)
	if err != nil {
		mapStoreErr(w, err, "日志入库")
		return
	}
	// 让 agent 知道哪些已落库，从而安全地推进本地位点。
	//
	// 注意 duplicated 的含义：**同 boot_id + 同 seq** 才会被判为重复。
	// agent 据此可以放心重传（网络抖动导致"其实已到达但没收到响应"时不会产生重复行）。
	writeJSON(w, http.StatusOK, map[string]any{
		"accepted": ing.Accepted, "duplicated": ing.Duplicated,
		"parse_errors": parseErrs, "newest_seq": ing.NewestSeq,
		"partitions": ing.Partitions, "boot_id": bootID,
	})
}

type agentLogRow struct {
	Seq  int64  `json:"seq"`
	Line string `json:"line"`
}

// ============================== 小工具 ==============================

func pageParams(r *http.Request) (int, int) {
	limit, offset := 0, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			offset = n
		}
	}
	return limit, offset
}

func normalizeDomains(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

func isIPv4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil
}

// diffString 生成变更摘要（供审计）。
//
// 用反射逐字段比对，只列出**发生变化的字段名**，不记录字段值。
// 理由：审计表要长期保留（90 天）且可能被导出给第三方看，
// 记录完整前后值等于把客户数据整份抄进审计；而"改了什么字段"已经足够追责与回溯。
func diffString(before, after any) string {
	bv, av := reflect.ValueOf(before), reflect.ValueOf(after)
	if bv.Kind() != reflect.Struct || av.Kind() != reflect.Struct || bv.Type() != av.Type() {
		return ""
	}
	t := bv.Type()
	var changed []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() || f.Name == "UpdatedAt" || f.Name == "CreatedAt" {
			continue // 时间戳每次都会变，列出来是噪声
		}
		if !reflect.DeepEqual(bv.Field(i).Interface(), av.Field(i).Interface()) {
			changed = append(changed, f.Name)
		}
	}
	if len(changed) == 0 {
		return ""
	}
	return "变更字段: " + strings.Join(changed, ", ")
}

var _ = context.Background
