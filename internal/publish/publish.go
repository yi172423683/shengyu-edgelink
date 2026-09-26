// Package publish 实现配置发布流水线（需求 §五）。
//
// 流水线的每一步都必须回答一个具体问题，缺一步就会出现一类事故：
//
//	① 候选配置生成   —— 规则改了，渲染出来是什么？
//	② 语法与冲突检查 —— HAProxy 自己能接受吗？（不自己写语法检查器）
//	③ 保存旧版本     —— 失败了往哪里退？
//	④ 原子替换       —— 换的时候会不会读到半个文件？
//	⑤ 平滑 reload    —— 现有连接会不会被掐断？
//	⑥ 应用结果验证   —— 端口真的起来了吗？还是"命令成功了但服务没起"？
//	⑦ 失败自动回滚   —— 而且回滚本身也要验证，不能"回滚了但没成功"还报成功。
//
// 第 ⑥ 步之后还有一个容易混的判断题：**源站本来就不可用** 不是发布失败。
// 把它报成失败会让运维去回滚一个本来正确的配置；报成成功又掩盖了业务已经断了。
// 因此这里单独用 ReleaseAppliedUnhealthy 表达。
package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shengyu/edgelink/internal/dataplane"
	"github.com/shengyu/edgelink/internal/haproxy"
	"github.com/shengyu/edgelink/internal/id"
	"github.com/shengyu/edgelink/internal/model"
	"github.com/shengyu/edgelink/internal/store"
)

// OriginProbe 判断"源站业务本身是否可用"。
//
// 抽成接口的原因：真实环境需要多位置探测（需求 §十二.5），而发布流水线只需要一个结论。
// 把探测策略与发布流程解耦，才能在不改发布逻辑的前提下把探测换成外部多节点探测。
type OriginProbe interface {
	// ProbeOrigins 返回 (可用数, 不可用数, 细节说明)。
	ProbeOrigins(ctx context.Context, routes []model.RouteView) (up int, down int, details []string)
}

// TCPOriginProbe 是最小可用的源站探测：只做 TCP 连通性。
//
// 它的结论**只能说"TCP 能连上"**，不能说"业务正常"——需求 §七 明确要求
// "不能把 TCP 连接成功显示为业务完全正常"。因此它的细节说明里会写清这一点。
type TCPOriginProbe struct {
	Timeout time.Duration
}

func (p TCPOriginProbe) ProbeOrigins(ctx context.Context, routes []model.RouteView) (int, int, []string) {
	to := p.Timeout
	if to <= 0 {
		to = 3 * time.Second
	}
	d := net.Dialer{Timeout: to}
	up, down := 0, 0
	var details []string
	for _, rv := range routes {
		if !rv.Enabled {
			continue
		}
		addr := net.JoinHostPort(rv.Route.OriginHost, strconv.Itoa(rv.Route.OriginPort))
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			down++
			details = append(details, fmt.Sprintf("业务 %s：源站 %s TCP 连接失败（%v）", rv.Route.BusinessID, addr, err))
			continue
		}
		_ = c.Close()
		up++
		details = append(details, fmt.Sprintf("业务 %s：源站 %s TCP 可连接（仅证明端口可达，不代表业务正常）", rv.Route.BusinessID, addr))
	}
	return up, down, details
}

// Pipeline 发布流水线。一个管理实例对一台数据面只应有一个 Pipeline，
// 因为它内部持有该数据面的发布锁。
//
// **LocalNodeID 是必填的，而且它不是"元信息"而是安全边界**：
// 本实例的 Applier 只能操作它所在的那台机器。若允许对任意 nodeID 发布，
// 一次写错 nodeID 就会把**另一台节点**的期望状态（可能是空的）应用到本机，
// 直接把本机正在服务的监听拆掉 —— 这是"误操作导致业务中断"的典型路径。
// 远程节点必须由该节点上的 agent 执行，管理面只下发意图。
type Pipeline struct {
	Store    *store.Store
	Applier  dataplane.Applier
	Defaults haproxy.Defaults
	Probe    OriginProbe
	// VersionsRoot 版本目录的父目录。
	VersionsRoot string
	// LocalNodeID 本实例（本机数据面）对应的节点 ID。
	LocalNodeID string

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	clock func() time.Time
	// sleep 是等待实现，默认 time.Sleep；测试可以注入"即时返回"以避免真等 30 秒。
	sleep func(time.Duration)
}

// New 构造流水线。
//
// 关于 def.ConfigDir：保持为占位符（haproxy.ConfigDirToken）不动。
// 渲染器写进配置里的允许清单路径是 `{CONFIGDIR}/sni_allow_443.lst`，
// 由本包在**写入版本目录时**替换成该版本的绝对路径。
// 为什么不直接写 current 路径：平滑 reload 后老 worker 仍在服务，
// 它读的必须是**它自己那一版**的清单；若指向 current，新版本一发布
// 老 worker 就会读到新清单 —— 这正是"平滑更新时现有长连接仍要按原规则服务"的反面。
func New(st *store.Store, ap dataplane.Applier, versionsRoot string, def haproxy.Defaults) *Pipeline {
	if def.ConfigDir == "" {
		def.ConfigDir = haproxy.ConfigDirToken
	}
	return &Pipeline{
		Store:        st,
		Applier:      ap,
		Defaults:     def,
		Probe:        TCPOriginProbe{},
		VersionsRoot: versionsRoot,
		locks:        map[string]*sync.Mutex{},
		clock:        time.Now,
		sleep:        time.Sleep,
	}
}

// SetClock 注入时钟（测试用）。
func (p *Pipeline) SetClock(f func() time.Time) {
	if f != nil {
		p.clock = f
	}
}

// SetLocalNode 绑定本实例负责的节点。必须在服务启动时调用；
// 未绑定就调用发布会被拒绝（宁可拒绝也不能猜）。
func (p *Pipeline) SetLocalNode(nodeID string) { p.LocalNodeID = nodeID }

// ErrRemoteNode 表示请求的节点不是本实例所在的那台机器。
var ErrRemoteNode = errors.New("publish: 该节点不由本实例管理")

// checkLocalNode 是发布/回滚的前置守卫。
//
// 三道判断，缺一不可：
//  1. 未绑定本机节点 —— 配置缺失，拒绝（不能默认"就当是本机"）；
//  2. 请求的节点不是本机 —— 拒绝（防止把别的节点的期望状态应用到本机）；
//  3. 本地节点记录不存在 —— 拒绝（数据不一致，先修数据再发布）。
func (p *Pipeline) checkLocalNode(nodeID string) error {
	if p.LocalNodeID == "" {
		return fmt.Errorf("%w：本实例尚未绑定本机节点，为避免误操作其它节点，拒绝执行发布", ErrRemoteNode)
	}
	if nodeID != p.LocalNodeID {
		return fmt.Errorf("%w：本实例只管理节点 %s，收到的是 %s。"+
			"远程节点的发布必须由该节点上的 agent 执行，管理面只下发意图",
			ErrRemoteNode, p.LocalNodeID, nodeID)
	}
	if _, err := p.Store.GetNode(nodeID); err != nil {
		return fmt.Errorf("publish: 本机节点 %s 在平台侧无记录: %w", nodeID, err)
	}
	return nil
}

// VersionsDir 返回本机**指定版本**的目录。
//
// 目录里带节点 ID：不同节点的 v1 必须是两个不同的目录。
// 只按版本号分目录会让两台节点互相覆盖对方的"第 1 版"，
// 而"历史版本不可变"正是回滚能成立的前提。
func (p *Pipeline) VersionsDir(version int) string {
	node := p.LocalNodeID
	if node == "" {
		node = "_unbound"
	}
	return filepath.Join(p.VersionsRoot, "versions", node, "v"+strconv.Itoa(version))
}

// nodeLock 取该节点的发布锁。
//
// 需求 §五要求"使用发布锁和版本号，避免并发覆盖"。
// 这里用进程内互斥 + 数据库里的 running 发布记录双重保护：
// 前者挡住同一实例的并发请求，后者挡住"另一个管理实例同时在推同一台节点"
// （需求 §十二.4：同一业务在任一时刻只能有一个自动调度控制者）。
func (p *Pipeline) nodeLock(nodeID string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.locks[nodeID]
	if !ok {
		m = &sync.Mutex{}
		p.locks[nodeID] = m
	}
	return m
}

// EnsureNoRunningRelease 检查是否有未结束的发布，避免并发覆盖。
func (p *Pipeline) EnsureNoRunningRelease(nodeID string) error {
	rels, _, err := p.Store.ListReleases(nodeID, 5, 0)
	if err != nil {
		return err
	}
	for _, r := range rels {
		if r.Status == store.ReleaseRunning {
			return fmt.Errorf("%w: 节点 %s 上已有正在进行中的发布（%s，开始于 %s），请等待完成",
				store.ErrConflict, nodeID, r.ID, r.StartedAt.Format(time.RFC3339))
		}
	}
	return nil
}

// Result 一次发布/回滚的结果。
type Result struct {
	ReleaseID   string                 `json:"release_id"`
	NodeID      string                 `json:"node_id"`
	Action      store.ReleaseAction    `json:"action"`
	Version     int                    `json:"version"`
	PrevVersion int                    `json:"prev_version"`
	Status      store.ReleaseStatus    `json:"status"`
	Phase       string                 `json:"phase"`
	Message     string                 `json:"message"`
	NoChange    bool                   `json:"no_change"`
	Verify      dataplane.VerifyResult `json:"verify"`
	// VerifyWaitedMS / VerifyAttempts 说明"新版本就绪等了多久、试了几次"。
	// 它是"reload 是异步的"这件事的可观测证据：等待时间突增通常意味着
	// HAProxy 变慢或配置变大，而不是发布失败。
	VerifyWaitedMS int64 `json:"verify_waited_ms"`
	VerifyAttempts int   `json:"verify_attempts"`
	DurationMS     int64 `json:"duration_ms"`
	OriginUp       int   `json:"origin_up"`
	OriginDown     int   `json:"origin_down"`

	// RolledBack / NoRollbackNeeded / RollbackNote / ManualFix 是「失败之后怎么办」的四件套。
	//
	// 界面必须能回答三个问题：现在跑的是哪一版？刚才那次改动回滚了没有？
	// 如果连回滚本身都失败了，我下一步该敲什么？
	// 少了任何一条，运维在发布失败时只能靠猜，而猜错方向通常意味着更长的故障时间。
	//
	// ⚠️ 纪律（曾经在这里翻过车，见 rollback_verify_test.go 与 docs/08）：
	//
	//	**"回滚文件恢复成功"不等于"回滚验证成功"。**
	//
	// 曾经有一次：上层调用时把 nodeID 传成了空串，于是"回滚结果验证"被整段跳过，
	// 而结果里仍然写着"已自动回滚到第 3 版**并验证通过**"。运维据此收工，
	// 而线上到底在跑什么其实没人确认过。所以现在：
	//
	//   - 空 nodeID 一律**硬失败**（RollbackCodeNodeIDMissing），不退回"当作本机"；
	//   - 只有 RollbackVerified=true 才允许在 RollbackNote 里出现"验证通过"；
	//   - RollbackVerifyRan 单独记录"验证到底跑了没有"，界面不必从散文里推断。
	//
	// RolledBack 与 NoRollbackNeeded **都必须以验证为据**：
	//   - RolledBack：确实发生了一次切换，**且**结果通过验证；
	//   - NoRollbackNeeded：本次改动根本没生效，且**已确认**原版本仍在正常运行。
	// 只有这两个之一为 true，界面才允许说"安全、无需人工介入"。
	// 单纯"命令返回 0"不足以推出任何一个：`systemctl reload` 返回 0 只说明信号已送达，
	// HAProxy 绑定失败时会保留旧 worker 并继续服务，此时磁盘版本标记却已经是新的了。
	RolledBack       bool     `json:"rolled_back"`
	NoRollbackNeeded bool     `json:"no_rollback_needed,omitempty"`
	RollbackNote     string   `json:"rollback_note,omitempty"`
	ManualFix        []string `json:"manual_fix,omitempty"`

	// Warnings 是"这次动作本身是成功的，但有一件事你必须知道"的清单。
	//
	// 目前唯一的来源是**版本目录里清单之外的多余文件**（边界见 docs/08 §10）：
	// 它们不会被发布到 current/（写入以清单为准），因此不影响正确性；
	// 但它们是"这个目录在清单生成之后被改过"的唯一信号，所以必须说出来 ——
	// 显示在页面上、并且写进审计（页面刷新就没了，审计才是几天后排查的依据）。
	//
	// 为什么是 warning 而不是 error：这类文件常见于运维手工备份
	// （cp haproxy.cfg haproxy.cfg.orig）。把它当硬错误会在**事故中挡住回滚** ——
	// 而回滚恰好是最不能拖延的时候。只有"配置**真的引用**了清单外的文件"才拒绝：
	// 那种文件运行时会按绝对路径被读到、真的生效（见 haproxy.ScanCitedFiles）。
	Warnings []string `json:"warnings,omitempty"`

	// StatsSocketPrune 是发布成功后"顺手打扫"的结果：清理了哪些无主的旧统计套接字、
	// 保留了哪些及原因、哪些没删掉。**它不影响发布结论** —— 清理失败只是少打扫了一次卫生，
	// 所以它的任何问题都只进 Warnings，绝不改变 Status。
	StatsSocketPrune *dataplane.StatsSocketPrune `json:"stats_socket_prune,omitempty"`

	// RollbackVerified 表示"对回滚目标版本的验证确实执行了，并且通过了"。
	//
	// 与 RolledBack 的区别只在**有没有发生过切换**：
	//   - no_rollback_needed 时 RollbackVerified 同样是 true（那一版确实被验证过在跑），
	//     但 RolledBack 为 false —— 没有切换过，就不该让运维去找一个不存在的抖动窗口。
	RollbackVerified bool `json:"rollback_verified"`
	// RollbackRestored 表示目标版本的一套文件已经完整回到 current/（逐字节一致）。
	// 它是"已恢复但未验证"这个中间状态的唯一依据：文件回来了 ≠ 它真的在跑。
	RollbackRestored bool `json:"rollback_restored"`
	// RollbackVerifyRan 表示验证**实际执行过**（而不是被跳过）。
	RollbackVerifyRan bool `json:"rollback_verify_ran"`
	// RollbackOutcome 回滚结论，界面按它选措辞：
	// verified / restored_not_verified / verify_failed / failed / not_needed。
	RollbackOutcome string `json:"rollback_outcome,omitempty"`
	// RollbackCode 失败时的机器可读原因码：
	// node_id_missing / rollback_verification_not_run / rollback_verification_failed /
	// rollback_restore_failed / rollback_no_target。
	RollbackCode string `json:"rollback_code,omitempty"`
	// RollbackChecks 逐条判据（文件集合 / 版本标记 / 统计套接字 / 监听归属），给运维看"哪一条没成立"。
	RollbackChecks []string `json:"rollback_checks,omitempty"`
}

// 回滚失败时的机器可读原因码。界面与自动化脚本都按它分支，
// 因此必须是稳定字面量，而不是中文描述（中文会随措辞调整而变）。
const (
	// RollbackCodeNodeIDMissing 没有拿到节点 ID。
	// 没有它就无法确定"要确认哪台机器上的哪一版"，因此**一律拒绝**，
	// 而不是退回到"当作本机"—— 猜错机器的代价是把别处正在服务的监听拆掉。
	RollbackCodeNodeIDMissing = "node_id_missing"
	// RollbackCodeVerificationNotRun 文件已恢复，但"是否真的在跑"没有被验证
	// （缺状态快照、拿不到运行状态、数据面不支持文件级核对等）。
	RollbackCodeVerificationNotRun = "rollback_verification_not_run"
	// RollbackCodeVerifyFailed 验证真的执行了，但没通过。
	RollbackCodeVerifyFailed = "rollback_verification_failed"
	// RollbackCodeRestoreFailed 连文件都没恢复成目标版本。
	RollbackCodeRestoreFailed = "rollback_restore_failed"
	// RollbackCodeNoTarget 没有可回退的历史版本（当前仍是基线 v0）。
	RollbackCodeNoTarget = "rollback_no_target"
)

// 回滚结论（Result.RollbackOutcome）。界面按它选措辞，不允许自己推断。
const (
	// RollbackOutcomeVerified 文件已完整恢复 + 四项判据全部成立。
	RollbackOutcomeVerified = "verified"
	// RollbackOutcomeNotVerified 文件已恢复，但验证没有执行（不宣称安全）。
	RollbackOutcomeNotVerified = "restored_not_verified"
	// RollbackOutcomeVerifyFailed 验证执行了且未通过。
	RollbackOutcomeVerifyFailed = "verify_failed"
	// RollbackOutcomeFailed 文件都没恢复成功。
	RollbackOutcomeFailed = "failed"
	// RollbackOutcomeNotNeeded 改动未生效，且已确认原版本仍在正常运行。
	RollbackOutcomeNotNeeded = "not_needed"
	// RollbackOutcomeNotApplied 改动还没下发到数据面就失败了（渲染/写目录/语法检查）。
	// 与 not_needed 的区别：前者是"已经动过数据面又确认没生效"，
	// 这个是"根本没碰过数据面"—— 连版本标记都不曾改变。
	RollbackOutcomeNotApplied = "not_applied"
)

// rollbackResult 一次"回滚 / 确认原版仍在跑"的可核验结论。
//
// 把它做成结构而不是 (string, bool)：用布尔表达不了"已恢复但未验证"这个中间状态，
// 而那恰恰是最需要说清楚的一个状态 —— 文件回来了、但没人确认它真的在跑。
type rollbackResult struct {
	NodeID    string
	Restored  bool // 目标版本的一套文件已完整回到 current/
	VerifyRan bool // 验证实际执行过
	Verified  bool // 验证通过
	Code      string
	Note      string
	Checks    []string
	// Warnings 与"回滚是否成功"无关，是"回滚成功了，但目标版本目录有问题要你知道"。
	// 目前唯一来源：目标版本目录里有清单之外的多余文件（不阻断，见 docs/08 §10）。
	Warnings []string
}

// outcome 把结论映射成界面用的字面量。
func (r rollbackResult) outcome(noRollbackNeeded bool) string {
	switch {
	case noRollbackNeeded:
		return RollbackOutcomeNotNeeded
	case r.Verified:
		return RollbackOutcomeVerified
	case !r.Restored:
		return RollbackOutcomeFailed
	case !r.VerifyRan:
		return RollbackOutcomeNotVerified
	default:
		return RollbackOutcomeVerifyFailed
	}
}

// Publish 执行一次完整发布。
func (p *Pipeline) Publish(ctx context.Context, nodeID, actor, note string) (*Result, error) {
	if err := p.checkLocalNode(nodeID); err != nil {
		return nil, err
	}
	lk := p.nodeLock(nodeID)
	lk.Lock()
	defer lk.Unlock()

	started := p.clock()
	res := &Result{NodeID: nodeID, Action: store.ActionPublish}

	if err := p.EnsureNoRunningRelease(nodeID); err != nil {
		return nil, err
	}

	prevVer, err := p.Applier.ActiveVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("publish: 读取当前生效版本失败: %w", err)
	}
	res.PrevVersion = prevVer

	nextVer, err := p.Store.NextVersion(nodeID)
	if err != nil {
		return nil, err
	}
	res.Version = nextVer

	// ① 候选配置生成
	state, err := p.Store.DesiredStateForNode(nodeID, nextVer)
	if err != nil {
		return nil, fmt.Errorf("publish: 组装期望状态失败: %w", err)
	}
	rendered, err := haproxy.Render(*state, p.Defaults)
	if err != nil {
		return nil, fmt.Errorf("publish: 渲染配置失败: %w", err)
	}

	// 内容没变就不用重新发布：重复发布会白白污染版本历史，
	// 让"第 N 版到底改了什么"变得无法回答。
	if existing, ferr := p.Store.FindConfigVersionByHash(nodeID, rendered.ContentHash); ferr == nil {
		res.NoChange = true
		res.Phase = "no_change"
		res.Status = store.ReleaseSucceeded
		res.Message = fmt.Sprintf("配置内容与第 %d 版完全一致，未产生新版本", existing.Version)
		res.DurationMS = p.clock().Sub(started).Milliseconds()
		_ = p.Store.WriteAudit(&store.AuditEntry{
			ID: id.New("aud"), Actor: actor, Action: "config.publish.noop",
			TargetType: "node", TargetID: nodeID, Summary: res.Message, Result: "ok",
			Detail: fmt.Sprintf("content_hash=%s", rendered.ContentHash),
		})
		return res, nil
	}

	rel := &store.Release{
		ID: id.New("rel"), NodeID: nodeID, Action: store.ActionPublish,
		FromVersion: prevVer, ToVersion: nextVer, Status: store.ReleaseRunning,
		Phase: "render", Actor: actor,
	}
	if err := p.Store.CreateRelease(rel); err != nil {
		return nil, err
	}
	res.ReleaseID = rel.ID

	// ② 写入版本目录（保存这份候选的**字节**，供日后原样回滚与比对）
	if err := p.writeVersionDir(nextVer, rendered, state); err != nil {
		// 这一步还没碰数据面：当前跑的仍是第 prevVer 版，所以**不是**回滚，而是"从未应用"。
		res.RolledBack = false
		res.RollbackOutcome = RollbackOutcomeNotApplied
		res.RollbackNote = fmt.Sprintf("配置尚未下发给数据面，当前仍运行第 %d 版，无需回滚", prevVer)
		return p.fail(rel, res, "write", fmt.Errorf("publish: 写入版本目录失败: %w", err), prevVer, actor)
	}

	// 版本目录自查：清单之外的多余文件**不阻断**发布，但必须记 warning + 审计 + 页面提示。
	// 放在这里（写完目录、还没碰数据面）是为了：无论后面发布成功还是失败，
	// 这条信息都已经落在结果里了。
	p.warnFilesOutsideManifest(res, nextVer, nodeID, actor)

	cfgVer := &store.ConfigVersion{
		ID: id.New("cfgver"), NodeID: nodeID, Version: nextVer,
		ContentHash: rendered.ContentHash, Status: store.CfgCandidate,
		DirPath: p.VersionsDir(nextVer), ExpectedListeners: rendered.ExpectedListeners,
		RouteCount: len(state.Routes), Note: note, CreatedBy: actor,
	}
	if err := p.Store.CreateConfigVersion(cfgVer); err != nil {
		return p.fail(rel, res, "write", err, prevVer, actor)
	}
	// 写入这一版的对象快照（backend→业务 / backend→源站）。
	//
	// 这一步让"历史日志按当时版本富化"成为可能（评审 F11）：
	// 版本行写后不改，所以它天然是不可变快照；日志里带着 ver，
	// 解析时按 ver 取快照即可还原"这条连接当时连的是哪个源站"。
	// 快照写失败**不阻断发布**（转发本身不依赖它），但必须留痕 ——
	// 否则将来会发现"某一天的日志源站是空的"而查不出原因。
	if err := p.Store.SetConfigVersionObjectNames(nodeID, nextVer, rendered.ObjectNames); err != nil {
		_ = p.Store.WriteAudit(&store.AuditEntry{
			ID: id.New("aud"), Actor: actor, Action: "config.snapshot_failed",
			TargetType: "node", TargetID: nodeID, Result: "error",
			Summary: fmt.Sprintf("第 %d 版的日志富化快照写入失败（发布继续）", nextVer),
			Detail:  err.Error(),
		})
	}

	// ③ 语法与冲突检查（交给 HAProxy 自己判断）
	_ = p.Store.UpdateReleasePhase(rel.ID, "validate")
	res.Phase = "validate"
	cfgPath := filepath.Join(p.VersionsDir(nextVer), "haproxy.cfg")
	if err := p.Applier.Validate(ctx, cfgPath); err != nil {
		// 语法/冲突检查在**下发之前**：此时数据面一行都没改，谈不上回滚。
		// 明确写"无需回滚"而不是把 rolled_back 置 true ——
		// 后者会让运维以为发生过一次切换，从而去找根本不存在的抖动窗口。
		res.RolledBack = false
		res.RollbackOutcome = RollbackOutcomeNotApplied
		res.RollbackNote = fmt.Sprintf("语法检查在下发之前失败，数据面一行未改，当前仍运行第 %d 版，无需回滚", prevVer)
		_ = p.Store.UpdateConfigVersionStatus(nodeID, nextVer, store.CfgFailed, err.Error())
		return p.fail(rel, res, "validate", err, prevVer, actor)
	}
	_ = p.Store.UpdateConfigVersionStatus(nodeID, nextVer, store.CfgValidated, "")

	// ④⑤ 原子替换 + 平滑 reload
	_ = p.Store.UpdateReleasePhase(rel.ID, "reload")
	res.Phase = "reload"
	applyErr := p.Applier.Apply(ctx, dataplane.ApplyRequest{
		State: *state, Version: nextVer, ConfigDir: p.VersionsDir(nextVer),
		ConfigPath: cfgPath, PreviousVersion: prevVer,
	})
	if applyErr != nil {
		// 应用失败：如果数据面没能自己回退，我们在这里兜一次回滚并**验证回滚结果**。
		rb, skipped := p.rollbackIfNeeded(ctx, nodeID, prevVer)
		p.recordRollback(res, rb, skipped, nodeID, actor)
		msg := applyErr.Error()
		if rb.Note != "" {
			msg += "；" + rb.Note
		}
		_ = p.Store.UpdateConfigVersionStatus(nodeID, nextVer, store.CfgFailed, applyErr.Error())
		return p.fail(rel, res, "reload", errors.New(msg), prevVer, actor)
	}

	// ⑥ 应用结果验证（**带等待**，见 waitVerify 的说明）
	_ = p.Store.UpdateReleasePhase(rel.ID, "verify")
	res.Phase = "verify"
	vr, verr, waited, attempts := p.waitVerify(ctx, dataplane.VerifyRequest{
		State: *state, Version: nextVer, ProbeTimeout: dataplane.DefaultVerifyTimeout,
	})
	res.Verify = vr
	res.VerifyWaitedMS = waited.Milliseconds()
	res.VerifyAttempts = attempts
	if verr != nil || !vr.OK {
		detail := ""
		if verr != nil {
			detail = verr.Error()
		} else {
			detail = dataplane.DescribeVerify(vr)
		}
		if waited > 0 {
			detail += fmt.Sprintf("（已等待 %.1f 秒、重试 %d 次，仍未就绪）", waited.Seconds(), attempts)
		}
		rb := p.rollback(ctx, prevVer, nodeID)
		p.recordRollback(res, rb, false, nodeID, actor)
		msg := "应用后验证未通过：" + detail
		if rb.Note != "" {
			msg += "；" + rb.Note
		}
		_ = p.Store.UpdateConfigVersionStatus(nodeID, nextVer, store.CfgRolledBack, msg)
		return p.fail(rel, res, "verify", errors.New(msg), prevVer, actor)
	}

	// 验证通过：配置确实生效了。接下来判断源站业务本身是否健康 ——
	// 这是"发布成功但客户业务本来就断了"与"发布失败"的分水岭。
	status := store.ReleaseSucceeded
	msg := fmt.Sprintf("第 %d 版已生效，%d 个监听全部就绪", nextVer, len(vr.Listeners))
	if p.Probe != nil {
		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		up, down, details := p.Probe.ProbeOrigins(probeCtx, state.Routes)
		cancel()
		res.OriginUp, res.OriginDown = up, down
		if down > 0 {
			status = store.ReleaseAppliedUnhealthy
			msg += fmt.Sprintf("；但 %d 条业务的源站当前不可达（配置本身已生效，这不是发布失败）", down)
		}
		if len(details) > 0 {
			_ = p.Store.WriteAudit(&store.AuditEntry{
				ID: id.New("aud"), Actor: actor, Action: "config.publish.probe",
				TargetType: "node", TargetID: nodeID,
				Summary: fmt.Sprintf("发布后源站探测：%d 可用 / %d 不可用", up, down),
				Result:  "ok", Detail: joinLines(details),
			})
		}
	}

	_ = p.Store.UpdateConfigVersionStatus(nodeID, nextVer, store.CfgActive, "")
	// 把此前处于 active 的版本标记为"已被取代"。
	// 只改状态、**不删记录**：历史版本要能一键回滚，也要能回答"上周跑的是哪一版"。
	if _, err := p.Store.DB().Exec(
		`UPDATE config_versions SET status=? WHERE node_id=? AND version<>? AND status=?`,
		string(store.CfgSuperseded), nodeID, nextVer, string(store.CfgActive)); err != nil {
		// 历史版本标记失败不影响本次发布结果，但要留痕，避免静默
		_ = p.Store.WriteAudit(&store.AuditEntry{
			ID: id.New("aud"), Actor: actor, Action: "config.mark_superseded_failed",
			TargetType: "node", TargetID: nodeID, Result: "error", Detail: err.Error(),
		})
	}
	if err := p.Store.SetRouteAppliedVersion(nodeID, nextVer); err != nil {
		return p.fail(rel, res, "post", err, prevVer, actor)
	}

	// 期望版本由发布流程推出 —— 这是"后台显示期望版本 vs 节点实际版本"的来源
	n, err := p.Store.GetNode(nodeID)
	if err == nil {
		n.ExpectedVersion = nextVer
		_ = p.Store.UpdateNode(n)
	}

	// 顺手打扫：清理"已确认无进程使用"的旧统计套接字。
	//
	// 为什么放在验证通过**之后**：此刻新 worker 已就绪（判据③要求它的 stats socket 可连）；
	// 旧 worker 若还在排空，它的套接字仍会应答 ⇒ 被"有 worker 在应答就保留"这条规则保护住。
	// 为什么结果只进 warning：这是打扫卫生，不该影响任何一次成功发布的结果。
	p.pruneStatsSockets(res, nodeID, actor)

	res.Status, res.Message = status, msg
	res.DurationMS = p.clock().Sub(started).Milliseconds()
	_ = p.Store.FinishRelease(rel.ID, status, "done", shortResult(status), msg)
	_ = p.Store.WriteAudit(&store.AuditEntry{
		ID: id.New("aud"), Actor: actor, Action: "config.publish",
		TargetType: "node", TargetID: nodeID,
		Summary: fmt.Sprintf("发布第 %d 版（上一版 %d）：%s", nextVer, prevVer, shortResult(status)),
		Result:  string(status), Detail: msg,
	})
	return res, nil
}

// Rollback 一键回滚到指定历史版本（需求 §五）。
//
// 回滚同样是"一次发布"：它也要走验证，也要在读不到目标版本时明确报错，
// 而不是把 current 指过去就宣布成功。
func (p *Pipeline) Rollback(ctx context.Context, nodeID string, target int, actor, reason string) (*Result, error) {
	if err := p.checkLocalNode(nodeID); err != nil {
		return nil, err
	}
	lk := p.nodeLock(nodeID)
	lk.Lock()
	defer lk.Unlock()

	started := p.clock()
	res := &Result{NodeID: nodeID, Action: store.ActionRollback, Version: target}

	if err := p.EnsureNoRunningRelease(nodeID); err != nil {
		return nil, err
	}
	prevVer, err := p.Applier.ActiveVersion(ctx)
	if err != nil {
		return nil, err
	}
	res.PrevVersion = prevVer

	targetDir := p.VersionsDir(target)
	if _, err := os.Stat(filepath.Join(targetDir, "haproxy.cfg")); err != nil {
		return nil, fmt.Errorf("publish: 回滚目标第 %d 版不存在或已不可读: %w", target, err)
	}
	cfgVer, err := p.Store.GetConfigVersion(nodeID, target)
	if err != nil {
		return nil, fmt.Errorf("publish: 第 %d 版在平台侧无记录（拒绝回滚到来源不明的配置）: %w", target, err)
	}

	rel := &store.Release{
		ID: id.New("rel"), NodeID: nodeID, Action: store.ActionRollback,
		FromVersion: prevVer, ToVersion: target, Status: store.ReleaseRunning,
		Phase: "rollback", Actor: actor,
	}
	if err := p.Store.CreateRelease(rel); err != nil {
		return nil, err
	}
	res.ReleaseID = rel.ID

	state, note := p.stateFor(nodeID, target)

	cfgPath := filepath.Join(targetDir, "haproxy.cfg")
	if err := p.Applier.Validate(ctx, cfgPath); err != nil {
		return p.fail(rel, res, "validate", err, prevVer, actor)
	}
	if err := p.Applier.Apply(ctx, dataplane.ApplyRequest{
		State: *state, Version: target, ConfigDir: targetDir,
		ConfigPath: cfgPath, PreviousVersion: prevVer,
	}); err != nil {
		return p.fail(rel, res, "reload", err, prevVer, actor)
	}
	vr, verr := p.Applier.Verify(ctx, dataplane.VerifyRequest{
		State: *state, Version: target, ProbeTimeout: dataplane.DefaultVerifyTimeout,
	})
	res.Verify = vr
	// 目标版本目录里有"清单之外的多余文件"：**不阻断**回滚，但要说出来 + 写审计。
	p.warnFilesOutsideManifest(res, target, nodeID, actor)
	if verr != nil || !vr.OK {
		detail := dataplane.DescribeVerify(vr)
		if verr != nil {
			detail = verr.Error()
		}
		return p.fail(rel, res, "verify", errors.New("回滚后验证未通过："+detail), prevVer, actor)
	}
	// 回滚成功：把"验证确实执行过"这件事一并记在结果里，
	// 界面上的"已回滚"才有依据（而不是靠 reload 命令返回 0）。
	res.RollbackVerifyRan = true
	res.RollbackVerified = true
	res.RollbackRestored = true
	res.RollbackOutcome = RollbackOutcomeVerified
	res.RollbackChecks = []string{
		fmt.Sprintf("版本标记：current/VERSION=%d（期望 %d）—— %s", vr.DataplaneVersion, target, boolWord(vr.VersionMarkerOK)),
		fmt.Sprintf("统计套接字：%s —— %s", vr.StatsSocketPath, boolWord(vr.StatsSocketOK)),
		fmt.Sprintf("监听归属：%d 个期望监听 —— %s", len(vr.Listeners), boolWord(vr.ListenersOK)),
	}

	_ = p.Store.UpdateConfigVersionStatus(nodeID, target, store.CfgActive, "")
	// 被覆盖掉的那一版标为已回滚，而不是"失败"——它当时是成功的。
	_ = p.Store.UpdateConfigVersionStatus(nodeID, prevVer, store.CfgRolledBack, "被回滚到第 "+strconv.Itoa(target)+" 版")
	// 注意：回滚**不修改**业务表里的源站等字段 —— 业务表反映的是"当前期望"，
	// 回滚改的是"节点上跑什么"。两者不一致是回滚的正常状态，
	// 界面必须能表达这件事（期望版本 != 实际生效版本），而不是偷偷把业务也改回去。
	_ = p.Store.SetRouteAppliedVersion(nodeID, target)
	if n, err := p.Store.GetNode(nodeID); err == nil {
		n.ExpectedVersion = target
		_ = p.Store.UpdateNode(n)
	}

	res.Status = store.ReleaseSucceeded
	res.Message = fmt.Sprintf("已回滚到第 %d 版（原 %s）", target, cfgVer.ContentHash[:minInt(8, len(cfgVer.ContentHash))])
	if note != "" {
		res.Message += "；注意：" + note
	}
	res.DurationMS = p.clock().Sub(started).Milliseconds()
	_ = p.Store.FinishRelease(rel.ID, store.ReleaseSucceeded, "done", "rolled_back", res.Message+"；原因："+reason)
	_ = p.Store.WriteAudit(&store.AuditEntry{
		ID: id.New("aud"), Actor: actor, Action: "config.rollback",
		TargetType: "node", TargetID: nodeID,
		Summary: fmt.Sprintf("回滚到第 %d 版（原因：%s）", target, reason),
		Result:  "ok", Detail: res.Message,
	})
	return res, nil
}

// rollbackIfNeeded：数据面 Apply 失败后，确认它是否已经退回旧版本；没退就补一次。
//
// nodeID **必须非空**，而且它不是一个可选的"元信息"参数：没有它就无法确定
// "要确认哪台机器上的哪一版"。曾经因为这里允许空串（底层悄悄退回"当作本机"），
// 导致回滚结果的验证被整段跳过，而对外仍然宣称"已回滚并验证通过"。
// 现在空 nodeID 一律硬失败并给出 node_id_missing —— 宁可拒绝，也不给一句没有依据的"安全"。
//
// 返回 (回滚结论, 是否"改动压根没生效所以无需回滚")。
//
// 三种结果的语义必须严格区分，因为它们对运维的动作完全不同：
//
//	① 回滚了且验证通过        → 现在跑的是旧版，安全；
//	② 改动没生效、旧版仍在跑  → 与 ① 结果等价（都安全），但**没有发生过切换**，
//	                           把 ① 的说法套上去会让运维去找一个根本不存在的抖动窗口；
//	③ 以上都不是              → 必须报"回滚失败/未验证"并给人工命令。
//
// 两个结论都**只能由验证得出**：`systemctl reload` 返回 0 不代表配置生效，
// 所以"旧版还在跑"这句话同样要拿去验证一遍（文件集合 + 版本标记 + 统计套接字 + 监听归属）。
func (p *Pipeline) rollbackIfNeeded(ctx context.Context, nodeID string, prevVer int) (rb rollbackResult, noRollbackNeeded bool) {
	// 入口守卫：空 nodeID 直接拒绝，绝不留到下面某一层去"猜"。
	if strings.TrimSpace(nodeID) == "" {
		return rollbackResult{
			NodeID: nodeID,
			Code:   RollbackCodeNodeIDMissing,
			Checks: []string{"node_id 为空 —— 拒绝执行回滚；也不退回「当作本机」（猜错机器会把别处正在服务的监听拆掉）"},
			Note: "回滚未执行（node_id_missing）：没有拿到节点 ID，无法确定要确认哪台机器上的第 " +
				strconv.Itoa(prevVer) + " 版。已保持现状，请人工按下方步骤处理",
		}, false
	}
	if prevVer <= 0 {
		return rollbackResult{
			NodeID: nodeID,
			Code:   RollbackCodeNoTarget,
			Checks: []string{"上一版本号为 0（基线不监听任何端口），无回退目标"},
			Note:   "此前从未成功发布过（当前仍是基线 v0，不监听任何端口），没有可回退的版本",
		}, false
	}
	// 数据面自己已经退回旧版（磁盘标记仍是旧版）时走这里。
	// 但"磁盘标记写着旧版"不等于"旧版真的在跑"，所以这句结论同样要验证。
	if cur, err := p.Applier.ActiveVersion(ctx); err == nil && cur == prevVer {
		confirm := p.inspectRollbackTarget(ctx, nodeID, prevVer, true)
		if confirm.Verified {
			confirm.Note = fmt.Sprintf("已确认数据面仍在正常运行第 %d 版：本次改动没有生效，无需回滚", prevVer)
			return confirm, true
		}
		// 确认不了 ⇒ **绝不能**报"无需回滚"就收工。
		// 版本标记与真实运行状态不一致，正是最需要人工看一眼的情况。
		if confirm.Code == "" {
			confirm.Code = RollbackCodeVerificationNotRun
		}
		if confirm.Code != RollbackCodeNodeIDMissing {
			confirm.Note = fmt.Sprintf("本次改动未生效，但**无法确认**第 %d 版仍在正常运行（%s）——"+
				"版本标记与实际运行状态可能不一致，请人工确认转发是否正常", prevVer, confirm.Note)
		}
		return confirm, false
	}
	return p.rollback(ctx, prevVer, nodeID), false
}

// inspectRollbackTarget 确认"第 version 版**确实在运行**"，并给出可核验的逐条判据。
//
// **四条判据全部成立才算验证通过**，这正是"回滚是否真的成功"的全部条件：
//
//	① 文件集合：current/ 与第 N 版目录逐字节一致
//	   （配置正文、允许清单、meta.json、state.json、manifest.json 一套都换回来了）
//	② 版本标记：current/VERSION == N
//	③ 统计套接字：能连上 stats-vN.sock（该版 worker 真的在跑，而不是只剩磁盘标记）
//	④ 监听归属：期望的 地址:端口 上确实由第 N 版配置的 frontend 提供
//
// 另外一条**前置**：nodeID 必须非空且必须是本实例绑定的那个节点。
// 空 nodeID 时不猜、不退回本机 —— 直接判"无法验证"。
//
// 缺任何一条都返回 Verified=false，并按"缺在哪一步"给出 RollbackCode，
// 让界面能分开说"已恢复但未验证"（文件回来了但没确认在跑）与"回滚验证失败"（确认了但没通过）。
//
// requireSnapshot 控制"没有该版状态快照时怎么办"：
//   - true（平台自己发起的自动回滚、以及"无需回滚"结论）：**必须**有快照。
//     没有就报"验证没有执行" —— 因为此时平台要说的是"安全、无需人工介入"，
//     而这个结论不能建立在"拿当前业务配置顶替旧版期望"的猜测上。
//   - false（运维在管理台点"回滚"）：允许退回当前期望状态（沿用既有行为），
//     但会在判据里写明来源，让人知道这次的期望监听集合是从哪来的。
func (p *Pipeline) inspectRollbackTarget(ctx context.Context, nodeID string, version int, requireSnapshot bool) rollbackResult {
	rb := rollbackResult{NodeID: nodeID}
	ver := strconv.Itoa(version)

	// 前置 ①：nodeID 非空。
	if strings.TrimSpace(nodeID) == "" {
		rb.Code = RollbackCodeNodeIDMissing
		rb.Checks = append(rb.Checks, "node_id 为空 —— 无法确定要确认哪台机器上的第 "+ver+" 版")
		rb.Note = "无法验证（node_id_missing）：没有拿到节点 ID"
		return rb
	}
	// 前置 ②：必须就是本实例绑定的那台机器。
	if p.LocalNodeID != "" && nodeID != p.LocalNodeID {
		rb.Code = RollbackCodeNodeIDMissing
		rb.Checks = append(rb.Checks, fmt.Sprintf("节点不匹配：请求 %s，本实例只管理 %s", nodeID, p.LocalNodeID))
		rb.Note = fmt.Sprintf("无法验证（node_id_missing）：本实例只管理节点 %s，不接受对 %s 的回滚验证",
			p.LocalNodeID, nodeID)
		return rb
	}

	// 判据 ①：文件集合。只有"以文件为准"的数据面能回答这条（HAProxy 可以）。
	if fi, ok := p.Applier.(dataplane.FileSetInspector); ok {
		diff, derr := fi.CompareCurrentToVersion(ctx, p.VersionsDir(version))
		switch {
		case derr != nil:
			rb.Code = RollbackCodeVerificationNotRun
			rb.Checks = append(rb.Checks, "文件集合：核对失败（"+derr.Error()+"）")
			rb.Note = "无法验证（rollback_verification_not_run）：文件集合核对失败，无法确认配置正文已恢复"
			return rb
		case diff.OK():
			rb.Restored = true
			src := "版本清单 " + haproxy.ManifestFileName
			if diff.ManifestMissing {
				src = "目录列举（该版无清单，可能是升级前发布的）"
			}
			rb.Checks = append(rb.Checks,
				fmt.Sprintf("文件集合：current/ 与第 %d 版目录逐字节一致（%d 个文件，依据 %s）", version, diff.Same, src))
			// 目标版本目录里有"清单之外的多余文件"：**不阻断回滚**（评审定的边界，
			// 这类文件不会进入 current/，而当硬错误会在事故中挡住回滚），
			// 但必须让运维看见 —— 它是"这个目录被手工改过"的唯一信号。
			if len(diff.VersionDirExtra) > 0 {
				rb.Checks = append(rb.Checks, fmt.Sprintf(
					"额外文件：第 %d 版目录里有 %d 个未纳入清单的文件（%s）—— 不进入 current/，不影响本次回滚",
					version, len(diff.VersionDirExtra), strings.Join(diff.VersionDirExtra, ", ")))
				rb.Warnings = append(rb.Warnings, versionDirExtraWarning(version, diff.VersionDirExtra))
			}
		default:
			rb.Code = RollbackCodeRestoreFailed
			rb.Checks = append(rb.Checks, "文件集合：与第 "+ver+" 版目录不一致 —— "+diff.Describe())
			rb.Note = fmt.Sprintf("回滚未成功：current/ 与第 %d 版目录不一致（%s）", version, diff.Describe())
			return rb
		}
	} else {
		// 不是以文件为准的数据面（例如纯 Go 测试替身）：这条判据**不适用**，
		// 而不是"没通过"。用"目标状态已被应用"作为等价前提。
		rb.Restored = true
		rb.Checks = append(rb.Checks, "文件集合：不适用（该数据面不以文件为准，无 current/ 目录可比对）")
	}

	// 判据 ②③④ 需要"这一版当时期望的监听集合"。
	if p.Store == nil {
		rb.Code = RollbackCodeVerificationNotRun
		rb.Checks = append(rb.Checks, "状态来源：不可用（本实例没有平台库连接）")
		rb.Note = "无法验证（rollback_verification_not_run）：读不到第 " + ver + " 版的状态快照"
		return rb
	}
	st, snapErr := p.LoadStateSnapshot(version)
	snapSource := "第 " + ver + " 版状态快照"
	if snapErr != nil {
		if !requireSnapshot {
			if fb, why := p.stateFor(nodeID, version); fb != nil && fb.Node.ID != "" {
				st, snapSource = fb, "当前期望状态（第 "+ver+" 版没有状态快照："+snapErr.Error()+"）"
				if why != "" {
					snapSource += "；" + why
				}
			}
		}
		if st == nil {
			rb.Code = RollbackCodeVerificationNotRun
			rb.Checks = append(rb.Checks, "状态来源：不可用（"+snapErr.Error()+"）")
			rb.Note = "无法验证（rollback_verification_not_run）：第 " + ver +
				" 版缺少状态快照，无法按该版期望的监听集合核对 frontend 归属"
			return rb
		}
	}
	rb.Checks = append(rb.Checks, "状态来源："+snapSource)

	// 判据 ②③④：交给数据面回答（版本标记 / 统计套接字 / 监听归属）。
	vr, verr := p.Applier.Verify(ctx, dataplane.VerifyRequest{
		State: *st, Version: version, ProbeTimeout: dataplane.DefaultVerifyTimeout,
	})
	rb.VerifyRan = true
	sock := vr.StatsSocketPath
	if sock == "" {
		sock = "（未报告路径）"
	}
	owned := 0
	for _, l := range vr.Listeners {
		if l.OwnedByUs {
			owned++
		}
	}
	if verr != nil {
		rb.Code = RollbackCodeVerifyFailed
		rb.Checks = append(rb.Checks, "验证执行出错："+verr.Error())
		rb.Note = fmt.Sprintf("回滚结果验证未通过：%v", verr)
		return rb
	}
	rb.Checks = append(rb.Checks,
		fmt.Sprintf("版本标记：current/VERSION=%d（期望 %d）—— %s", vr.DataplaneVersion, version, boolWord(vr.VersionMarkerOK)),
		fmt.Sprintf("统计套接字：%s —— %s", sock, boolWord(vr.StatsSocketOK)),
		fmt.Sprintf("监听归属：%d/%d 个期望监听已确证由第 %d 版的 frontend 提供 —— %s",
			owned, len(vr.Listeners), version, boolWord(vr.ListenersOK)),
	)
	if !vr.VersionMarkerOK || !vr.StatsSocketOK || !vr.ListenersOK {
		rb.Code = RollbackCodeVerifyFailed
		problems := strings.Join(vr.Problems, "；")
		if problems == "" {
			problems = dataplane.DescribeVerify(vr)
		}
		rb.Note = fmt.Sprintf("回滚结果验证未通过：%s", problems)
		return rb
	}
	rb.Verified = true
	rb.Note = fmt.Sprintf("已自动回滚到第 %d 版并验证通过（文件集合、版本标记、统计套接字、监听归属四项判据全部成立）", version)
	return rb
}

func boolWord(ok bool) string {
	if ok {
		return "成立"
	}
	return "**不成立**"
}

// warnCurrentNotTarget 在"回滚没能让目标版本生效"时，把 current/ 实际停在哪一版写进说明。
//
// 真机暴露的场景（docs/08 第 6 节）：回滚目标版本目录不完整时，
// 平台正确地 fail-closed 拒绝回滚 —— 但 current/ 仍停在**未获批准的新版本**上。
// 此时任何一次 `systemctl reload`（甚至一次计划外重启）都会加载那份没验证过的配置。
// 真机实测就是这个结果：`current/VERSION=15`，而正在服务的是 v12 的 worker。
//
// 界面上只说"回滚失败"是不够的：运维必须知道"现在别 reload"。
// 所以这句话里要同时出现三件事：**current/ 现在是第几版**、**它不是哪一版**、**先别 reload**。
func (p *Pipeline) warnCurrentNotTarget(ctx context.Context, target int, note string) string {
	cur, err := p.Applier.ActiveVersion(ctx)
	if err != nil {
		return note + "。**警告：读不到 current/VERSION，无法确认 current/ 现在停在哪一版**" +
			"——在做任何 reload / 重启之前请先人工核对 " + p.currentDir()
	}
	if cur == target {
		return note
	}
	return fmt.Sprintf("%s。**警告：current/ 目前停留在第 %d 版，不是第 %d 版 —— "+
		"在恢复之前请不要执行 systemctl reload / restart，否则会加载第 %d 版这份未通过验证的配置"+
		"**（正在服务的仍是老 worker，所以此刻转发通常还是正常的）",
		note, cur, target, cur)
}

// rollback 把数据面退回指定版本，然后**验证**回滚结果。
//
// ⚠️ 这里有一条不能破的纪律：**只有验证通过才允许说"已回滚"**。
// 说"已回滚"而实际没回滚，比直接说"回滚失败"危害大得多 —— 前者会让运维收工，
// 而线上仍是一份坏配置。所以传空 nodeID 时**直接拒绝**（而不是退回到本机去猜），
// 验证跑不起来时一律按"未成功"处理（进而在界面上给出人工处置命令）。
func (p *Pipeline) rollback(ctx context.Context, target int, nodeID string) rollbackResult {
	rb := rollbackResult{NodeID: nodeID}
	if strings.TrimSpace(nodeID) == "" {
		rb.Code = RollbackCodeNodeIDMissing
		rb.Checks = append(rb.Checks, "node_id 为空 —— 拒绝执行回滚（不退回「当作本机」）")
		rb.Note = "回滚未执行（node_id_missing）：没有拿到节点 ID，无法确定要在哪台机器上回滚到第 " +
			strconv.Itoa(target) + " 版"
		return rb
	}
	if target <= 0 {
		rb.Code = RollbackCodeNoTarget
		rb.Note = "没有可回退的历史版本，已保持现状（转发未受影响）"
		return rb
	}
	targetDir := p.VersionsDir(target)
	cfgPath := filepath.Join(targetDir, "haproxy.cfg")
	if _, err := os.Stat(cfgPath); err != nil {
		rb.Code = RollbackCodeRestoreFailed
		rb.Checks = append(rb.Checks, fmt.Sprintf("第 %d 版配置不可读：%v", target, err))
		rb.Note = p.warnCurrentNotTarget(ctx, target,
			fmt.Sprintf("回滚失败：第 %d 版配置已不可读（%v），转发仍由现有 worker 提供", target, err))
		return rb
	}
	// 回滚必须用**当时那一版的状态快照**，而不是当前业务配置（见 stateFor 的说明）。
	var state model.DesiredState
	if p.Store != nil {
		if st, _ := p.stateFor(nodeID, target); st != nil {
			state = *st
		}
	}
	if err := p.Applier.Apply(ctx, dataplane.ApplyRequest{
		State: state, Version: target, ConfigDir: targetDir, ConfigPath: cfgPath,
	}); err != nil {
		rb.Code = RollbackCodeRestoreFailed
		rb.Checks = append(rb.Checks, "下发第 "+strconv.Itoa(target)+" 版失败："+err.Error())
		rb.Note = p.warnCurrentNotTarget(ctx, target,
			fmt.Sprintf("回滚到第 %d 版失败：%v（转发仍由现有 worker 提供，请人工介入）", target, err))
		return rb
	}
	// Apply 返回 nil 只说明"文件换好了、reload 信号发出去了"。
	// 必须再跑一遍四项判据，否则不能声称"已回滚"。
	check := p.inspectRollbackTarget(ctx, nodeID, target, true)
	if !check.Verified {
		if check.Restored {
			rb.Restored = true
			rb.VerifyRan = check.VerifyRan
			rb.Checks = check.Checks
			rb.Code = check.Code
			if rb.Code == "" {
				rb.Code = RollbackCodeVerificationNotRun
			}
			// 措辞必须严格区分"恢复了但没验证"与"验证了没通过"，
			// 但两种情况都**不允许**出现"并验证通过"。
			if check.VerifyRan {
				rb.Note = p.warnCurrentNotTarget(ctx, target,
					fmt.Sprintf("已把第 %d 版配置放回 current/，但**回滚验证未通过**：%s。请人工确认转发是否正常",
						target, strings.TrimPrefix(check.Note, "回滚结果验证未通过：")))
			} else {
				rb.Note = p.warnCurrentNotTarget(ctx, target,
					fmt.Sprintf("已把第 %d 版配置放回 current/（文件已恢复），但**回滚验证没有执行**：%s。"+
						"「文件恢复成功」不等于「回滚成功」，请人工确认转发是否正常", target, check.Note))
			}
			return rb
		}
		rb.Restored = check.Restored
		rb.VerifyRan = check.VerifyRan
		rb.Checks = check.Checks
		rb.Code = check.Code
		rb.Note = p.warnCurrentNotTarget(ctx, target, check.Note)
		return rb
	}
	return check
}

// recordRollback 把回滚结论写进发布结果。
//
// 字段填法只在这里定义一次：这几项是"界面照着说"的依据，
// 漏填任何一项都会让界面退回到"自己猜"，而界面的猜测正是本次修复要杜绝的东西。
func (p *Pipeline) recordRollback(res *Result, rb rollbackResult, noRollbackNeeded bool, nodeID, actor string) {
	res.RolledBack = rb.Verified && !noRollbackNeeded
	res.NoRollbackNeeded = noRollbackNeeded
	res.RollbackVerified = rb.Verified
	res.RollbackRestored = rb.Restored
	res.RollbackVerifyRan = rb.VerifyRan
	res.RollbackOutcome = rb.outcome(noRollbackNeeded)
	res.RollbackCode = rb.Code
	res.RollbackChecks = rb.Checks
	res.RollbackNote = rb.Note
	// warning 与"回滚是否成功"无关：它说的是"回滚成功了，但目标版本目录被人改过"。
	// 搬到结果里（界面要显示），并且写进审计 —— 页面刷新后审计还在，
	// 而这类信息恰恰是几天后排查"为什么某次回滚不对劲"时最需要的前置事实。
	if len(rb.Warnings) > 0 {
		res.Warnings = append(res.Warnings, rb.Warnings...)
		p.auditWarnings(nodeID, actor, rb.Warnings)
	}
	// 只有"既不安全、也不需要回滚"才是要人工介入的。
	// 判据是 Verified / NoRollbackNeeded 本身，而不是某个中间布尔 ——
	// 曾经这里用"没回滚成功"来判，结果"改动压根没生效"也被算进来，
	// 于是界面在一切都好的情况下要求运维去敲命令。
	if !rb.Verified && !noRollbackNeeded {
		res.ManualFix = p.manualFixCommands(res.PrevVersion)
	}
}

// auditWarnings 把若干条 warning 逐条写进审计。
func (p *Pipeline) auditWarnings(nodeID, actor string, warnings []string) {
	if p.Store == nil {
		return
	}
	for _, w := range warnings {
		_ = p.Store.WriteAudit(&store.AuditEntry{
			ID: id.New("aud"), Actor: actor, Action: "config.warning",
			TargetType: "node", TargetID: nodeID, Result: "warn", Summary: w,
		})
	}
}

// versionDirExtraWarning 把"清单外多余文件"拼成一条稳定的说明。
//
// 抽成函数是为了让发布路径与回滚路径**说同一句话**：
// 同一件事在界面不同位置措辞不同，是排查时最容易被误读的地方。
func versionDirExtraWarning(version int, extra []string) string {
	return fmt.Sprintf("第 %d 版目录里有 %d 个未纳入清单的额外文件（%s）—— "+
		"它们不会被发布到 current/，也不影响转发；但说明该目录在清单生成后被改动过，建议核对后清理",
		version, len(extra), strings.Join(extra, ", "))
}

// readVersionManifest 读某个版本目录的清单文件。
func readVersionManifest(dir string) (*haproxy.VersionManifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, haproxy.ManifestFileName))
	if err != nil {
		return nil, err
	}
	var mf haproxy.VersionManifest
	if err := json.Unmarshal(b, &mf); err != nil {
		return nil, err
	}
	return &mf, nil
}

// warnFilesOutsideManifest 检查某个版本目录里"清单之外的多余文件"，有就记 warning + 审计。
//
// **不阻断任何流程**（评审确定的边界）：这类文件不会进入 current/
// （写入以清单为准，见 dataplane.writeVersionFiles），所以对正确性没有影响；
// 而当硬错误会在事故中挡住回滚 —— 回滚恰好是最不能拖延的时候。
// 它们必须被看见（warning + 审计 + 页面提示），但不是拦路石。
//
// 唯一例外是"配置真的引用了清单外的文件"，那时必须拒绝；
// 那条由数据面在 Apply 时判定（dataplane.HAProxy.publishFiles），
// 因为只有它知道渲染产物里的实际引用路径与版本目录的对应关系。
//
// 读不到清单时安静返回：这一版没按新格式写（升级前发布的旧版本），
// 数据面会按"目录列举"的口径处理，这里不需要也不应该另外报一次。
func (p *Pipeline) warnFilesOutsideManifest(res *Result, version int, nodeID, actor string) {
	if p.Store == nil {
		return
	}
	mf, err := readVersionManifest(p.VersionsDir(version))
	if err != nil {
		return
	}
	extra, _, serr := haproxy.ScanVersionDir(p.VersionsDir(version), mf.Files)
	if serr != nil || len(extra) == 0 {
		return
	}
	w := versionDirExtraWarning(version, extra)
	for _, have := range res.Warnings {
		if have == w {
			return // 同一条不重复堆（外层与内层可能各扫过一次）
		}
	}
	res.Warnings = append(res.Warnings, w)
	_ = p.Store.WriteAudit(&store.AuditEntry{
		ID: id.New("aud"), Actor: actor, Action: "config.version_dir_extra_files",
		TargetType: "node", TargetID: nodeID, Result: "warn", Summary: w,
		Detail: fmt.Sprintf("dir=%s extra=%s", p.VersionsDir(version), strings.Join(extra, ",")),
	})
}

// pruneStatsSockets 清理旧统计套接字，并把结果记进 Result 与审计。
//
// **只告警，不影响发布结论。** 这一条不能反悔：清理是"顺手打扫"，
// 让它能拖垮一次成功发布，就等于把卫生问题升级成可用性问题。
//
// 数据面不提供这条能力时静默跳过 —— 它不是发布流程的一部分，
// 纯 Go 数据面没有 /run 下的套接字，"不适用"不是"失败"。
func (p *Pipeline) pruneStatsSockets(res *Result, nodeID, actor string) {
	pr, ok := p.Applier.(dataplane.StatsSocketPruner)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := pr.PruneStaleStatsSockets(ctx)
	// 一个可清理的对象都没有、也没出错：不必在结果里添噪声。
	if out.Scanned == 0 && len(out.Problems) == 0 {
		return
	}
	res.StatsSocketPrune = &out
	if len(out.Removed) > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"已清理 %d 个无进程使用的旧统计套接字（%s），保留 %d 个",
			len(out.Removed), strings.Join(out.Removed, ", "), len(out.Kept)))
	}
	if len(out.Problems) > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"统计套接字清理有 %d 项未能完成（不影响本次发布，也不影响转发）：%s",
			len(out.Problems), strings.Join(out.Problems, "；")))
	}
	if p.Store == nil {
		return
	}
	// 删的是 /run 下的文件，属于"动过系统资源"，必须留痕 ——
	// 否则事后没人能解释"为什么某个版本的 stats socket 不见了"。
	result := "ok"
	if len(out.Problems) > 0 {
		result = "warn"
	}
	_ = p.Store.WriteAudit(&store.AuditEntry{
		ID: id.New("aud"), Actor: actor, Action: "config.prune_stats_sockets",
		TargetType: "node", TargetID: nodeID, Result: result,
		Summary: out.Note,
		Detail: fmt.Sprintf("removed=%s kept=%s problems=%s",
			strings.Join(out.Removed, ","), strings.Join(out.Kept, ","),
			strings.Join(out.Problems, "; ")),
	})
}

// manualFixCommands 回滚失败时给界面的、可逐行复制执行的人工处置命令。
//
// 之所以把命令写死在这里而不是让界面自由发挥：这条路径是**故障现场**，
// 运维没有时间判断"这个命令在我的环境里对不对"。命令必须与本平台的
// 单元名、版本目录布局一致，所以只能由知道这些细节的这一层来生成。
//
// 命令的第一组必须是"先看清楚现场"：current/ 里到底有哪几个文件、VERSION 是多少、
// 各版本的统计套接字还有没有 worker 应答。没有这三眼，后面的每一步都是在猜。
func (p *Pipeline) manualFixCommands(prevVer int) []string {
	cur := p.currentDir()
	out := []string{
		"# ⚠️ 在确认 current/ 里的内容之前，不要执行 systemctl reload / restart —— " +
			"回滚失败时 current/ 可能停在没通过验证的那一版上（下面第 2 条就是查它）",
		"# 1) 先确认数据面现在到底在跑什么",
		"systemctl status shengyu-edgelink-haproxy",
		"# 2) 看清 current/ 里实际有哪些文件、版本标记是多少（对着第 N 版目录逐个比）",
		"ls -la " + cur + "; cat " + filepath.Join(cur, dataplane.VersionFileName),
		"# 3) 看各版本的统计套接字还有没有 worker 应答（有应答 = 那一版还在跑）",
		"for s in /run/shengyu-edgelink/stats-v*.sock; do printf '%s: ' \"$s\"; " +
			"echo -e 'show info' | timeout 2 socat - UNIX-CONNECT:\"$s\" 2>/dev/null | head -1 || echo '无应答'; done",
	}
	if prevVer > 0 {
		out = append(out,
			fmt.Sprintf("# 4) 用第 %d 版配置做语法检查，确认这一版本身可用", prevVer),
			fmt.Sprintf("haproxy -c -f %s", filepath.Join(p.VersionsDir(prevVer), "haproxy.cfg")),
			fmt.Sprintf("# 5) 语法通过后，在管理台「节点 → 版本历史」对第 %d 版执行「回滚」", prevVer),
			"#    若管理台不可用，可在服务器上让数据面重新读取 current/：",
			"systemctl reload shengyu-edgelink-haproxy",
			"# 6) 最后再对一次：current/ 的文件清单必须与第 "+
				strconv.Itoa(prevVer)+" 版目录完全一致（回滚的判据就是这个）",
			fmt.Sprintf("diff -r %s %s", p.VersionsDir(prevVer), cur),
		)
	} else {
		out = append(out,
			"# 4) 没有可回退的历史版本：请把业务配置改回可用状态后重新发布，",
			"#    或在管理台「节点 → 版本历史」回滚到基线（基线不监听任何端口，会停止转发）",
		)
	}
	return out
}

// currentDir 返回当前生效配置目录的约定路径。
//
// 它只用于生成"给人照着敲"的命令，不参与任何判定 —— 判定一律走数据面自己的实现
// （HAProxy.CurrentDir）。这样即使部署形态变了，命令会错、结论不会错。
func (p *Pipeline) currentDir() string {
	return filepath.Join(p.VersionsRoot, "current")
}

// waitVerify 反复验证，直到"新版本真的就绪"或超过上限。
//
// 为什么必须等（评审要求补的三项之一：reload 异步就绪）：
//
//	`systemctl reload` 是**异步**的 —— 它把信号交给 master 就返回，新 worker 还要
//	解析配置、接管监听 fd、打开自己那一版的统计套接字。这中间有一段几百毫秒到几秒的窗口，
//	期间"新版本"既没起来、旧版本还在排空。若这里只验证一次，一次正常但稍慢的 reload
//	就会被判成"验证未通过"，然后触发一次**完全没有必要的回滚** ——
//	运维看到的是"发布失败并已回滚"，而实际上那一版配置是好的。
//
// 上限取得比"能容忍的最慢 reload"稍宽：超过它就说明不是"还没就绪"，
// 而是真的有问题（配置无法生效），那时回滚才是正确的。
func (p *Pipeline) waitVerify(ctx context.Context, req dataplane.VerifyRequest) (dataplane.VerifyResult, error, time.Duration, int) {
	deadline := p.clock().Add(ReloadReadyTimeout)
	started := p.clock()
	var (
		last    dataplane.VerifyResult
		lastErr error
	)
	attempts := 0
	for {
		attempts++
		last, lastErr = p.Applier.Verify(ctx, req)
		if lastErr == nil && last.OK {
			return last, nil, p.clock().Sub(started), attempts
		}
		if !p.clock().Before(deadline) {
			return last, lastErr, p.clock().Sub(started), attempts
		}
		// 校验是否还有必要继续等：ctx 被取消（例如运维点了取消）时立刻返回。
		if cerr := ctx.Err(); cerr != nil {
			return last, cerr, p.clock().Sub(started), attempts
		}
		p.sleep(VerifyPollInterval)
	}
}

// ReloadReadyTimeout 发布后等待新版本就绪的上限。
//
// 30 秒的依据：HAProxy 解析配置并接管监听通常远小于 1 秒；给到 30 秒是为了容纳
// "节点负载很高""配置很大""磁盘很慢"这几种现场情况 —— 宁可多等一会儿，
// 也不要因为"慢"而回滚一个正确的版本。
const ReloadReadyTimeout = 30 * time.Second

// VerifyPollInterval 就绪轮询间隔。
const VerifyPollInterval = 500 * time.Millisecond

// SetSleeper 注入等待实现（测试用：把 500ms 的轮询变成即时返回）。
func (p *Pipeline) SetSleeper(f func(time.Duration)) {
	if f != nil {
		p.sleep = f
	}
}

func (p *Pipeline) writeVersionDir(version int, r *haproxy.Rendered, st *model.DesiredState) error {
	dir := p.VersionsDir(version)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	// 把 ConfigDir 占位符替换成该版本的绝对路径（见 New 的说明）。
	// 用文件的**绝对**路径而不是相对路径：HAProxy reload 时工作目录不一定是配置目录。
	absDir, err := filepath.Abs(dir)
	if err != nil {
		absDir = dir
	}
	cfg := strings.ReplaceAll(string(r.Config), haproxy.ConfigDirToken, filepath.ToSlash(absDir))
	if err := os.WriteFile(filepath.Join(dir, "haproxy.cfg"), []byte(cfg), 0o640); err != nil {
		return err
	}
	// 一边写一边记文件清单：清单必须是"刚刚真的写下了哪些文件"的**副产物**，
	// 而不是另算一遍 —— 另算一遍就有算错的可能，而清单一旦失真，
	// 发布时的清理与回滚时的核对都会跟着错。
	files := []string{"haproxy.cfg"}
	allowNames := make([]string, 0, len(r.Allowlists))
	for name := range r.Allowlists {
		allowNames = append(allowNames, name)
	}
	sort.Strings(allowNames)
	for _, name := range allowNames {
		// 文件名来自渲染器内部（不是用户输入），但仍做一次目录穿越检查：
		// 这是"任何要拼进路径的东西都要校验"这条规则的一次具体落实。
		if filepath.Base(name) != name || name == "" {
			return fmt.Errorf("允许清单文件名不合法: %q", name)
		}
		if err := os.WriteFile(filepath.Join(dir, name), r.Allowlists[name], 0o640); err != nil {
			return err
		}
		files = append(files, name)
	}
	meta := haproxy.MetaFile{
		Version: version, NodeID: st.Node.ID, PublishedAt: p.clock(),
		ContentHash: r.ContentHash, ExpectedListeners: r.ExpectedListeners,
		ObjectNames: r.ObjectNames, RendererVersion: haproxy.RendererVersion,
	}
	b, err := jsonMarshalIndent(meta)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o640); err != nil {
		return err
	}
	files = append(files, "meta.json")
	// state.json 保存这一版的**完整结构化状态**。
	//
	// 为什么必须存它（而不是回滚时重新从库里读当前状态）：
	// 回滚的语义是"回到当时那一版"。若用当前业务配置去套旧版本号，
	// 对于"配置以文件为准"的数据面（HAProxy）恰好能工作，
	// 但对于"以结构化状态为准"的数据面（例如测试替身 internal/testplane）就会**回滚了却什么都没变** ——
	// 这是一个只在特定实现下才暴露的 bug，所以必须把状态当作版本的一部分固化下来。
	// 顺带的好处：排查时能直接看到"这一版当时期望的是什么"。
	//
	// 另外：回滚后的核对**要求**这份快照存在 —— 没有它就无法按"这一版当时期望的监听集合"
	// 去核对 frontend 归属，拿当前业务配置顶替会得出与事实不符的结论。
	sb, err := jsonMarshalIndent(st)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, StateFileName), sb, 0o640); err != nil {
		return err
	}
	files = append(files, StateFileName)

	// 最后写文件清单。**放在最后**是因为它必须覆盖上面全部文件；
	// 它自身不列进 Files（见 haproxy.VersionManifest 的说明），读方按
	// Files ∪ {manifest.json} 还原完整集合。
	sort.Strings(files)
	mb, err := jsonMarshalIndent(haproxy.VersionManifest{
		Version: version, NodeID: st.Node.ID, ContentHash: r.ContentHash,
		RendererVersion: haproxy.RendererVersion, CreatedAt: p.clock(),
		Files: files,
	})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, haproxy.ManifestFileName), mb, 0o640)
}

// StateFileName 版本目录里保存结构化期望状态的文件名。
const StateFileName = "state.json"

// LoadStateSnapshot 读取某一版保存下来的期望状态。
//
// 找不到快照时返回 ErrNoSnapshot，调用方据此决定是否退回到"当前期望状态"
// 并**明确告知用户**（而不是静默用一个可能不对的状态去回滚）。
var ErrNoSnapshot = errors.New("publish: 该版本没有状态快照")

func (p *Pipeline) LoadStateSnapshot(version int) (*model.DesiredState, error) {
	path := filepath.Join(p.VersionsDir(version), StateFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: 第 %d 版", ErrNoSnapshot, version)
		}
		return nil, err
	}
	var st model.DesiredState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("publish: 第 %d 版状态快照损坏: %w", version, err)
	}
	return &st, nil
}

// stateFor 返回回滚到某版本时应使用的期望状态，并说明来源。
func (p *Pipeline) stateFor(nodeID string, version int) (*model.DesiredState, string) {
	if st, err := p.LoadStateSnapshot(version); err == nil {
		return st, ""
	}
	// 老版本目录可能没有 state.json（升级前发布的）。退回当前期望状态，
	// 但**必须留话**：这时的回滚只保证"配置文本回到旧版"，
	// 对以状态为准的数据面不保证行为回到旧版。
	st, err := p.Store.DesiredStateForNode(nodeID, version)
	if err != nil {
		return &model.DesiredState{Node: model.Node{ID: nodeID}}, "无法读取该版本的状态快照与当前期望状态"
	}
	return st, fmt.Sprintf("第 %d 版没有状态快照（可能是升级前发布的），"+
		"已退回按当前业务配置重建状态：配置文件会回到旧版，但以结构化状态为准的数据面行为可能与当时不同", version)
}

// fail 统一处理失败收尾：写发布记录、写审计，并把结果带回给调用方。
func (p *Pipeline) fail(rel *store.Release, res *Result, phase string, err error, prevVer int, actor string) (*Result, error) {
	res.Phase = phase
	res.Status = store.ReleaseFailed
	res.Message = err.Error()
	_ = p.Store.FinishRelease(rel.ID, store.ReleaseFailed, phase, phase+"_failed", err.Error())
	_ = p.Store.WriteAudit(&store.AuditEntry{
		ID: id.New("aud"), Actor: actor, Action: "config.publish." + phase,
		TargetType: "node", TargetID: rel.NodeID,
		Summary: fmt.Sprintf("发布第 %d 版在 %s 阶段失败", rel.ToVersion, phase),
		Result:  "error", Detail: err.Error(),
	})
	return res, fmt.Errorf("发布失败（阶段 %s）：%w", phase, err)
}

func shortResult(s store.ReleaseStatus) string {
	switch s {
	case store.ReleaseSucceeded:
		return "succeeded"
	case store.ReleaseAppliedUnhealthy:
		return "applied_origin_unhealthy"
	case store.ReleaseRolledBack:
		return "rolled_back"
	case store.ReleaseFailed:
		return "failed"
	}
	return string(s)
}

func joinLines(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "\n"
		}
		out += s
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
