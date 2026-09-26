package main

import (
	"context"
	"strings"
	"time"

	"github.com/shengyu/edgelink/internal/agent"
	"github.com/shengyu/edgelink/internal/dataplane"
	"github.com/shengyu/edgelink/internal/model"
	"github.com/shengyu/edgelink/internal/store"
)

// localProber 采集本机节点的健康样本（评审 F10）。
//
// 三个维度分别这样判定，判定依据都会写进 Note（界面直接展示，不让运维猜）：
//
//	Agent  本进程在跑 ⇒ online。这一维度看起来"永远在线"，但它必须存在：
//	       它是"管理面能不能看到这台机器"的唯一证据，聚合时必须与内核维度分开，
//	       才能表达"管理面失联但转发正常"这种真实状态。
//	Proxy  真实执行 haproxy -v 的能力探测（Capabilities）。
//	       探测失败 = 转发内核不可用 ⇒ offline；版本不受支持/无 master-worker ⇒ degraded。
//	Path   拿**当前运行的版本**去 Verify：期望监听是否都在、绑定地址是否一致。
//	       不一致 ⇒ degraded（转发可能部分可用），而不是笼统的"离线"。
//	       尚未发布过任何配置（版本 0）时是 unknown —— 不是错误，如实标注。
type localProber struct {
	dp     dataplane.Applier
	st     *store.Store
	nodeID string
}

func newLocalProber(dp dataplane.Applier, st *store.Store, nodeID string, _ options) *localProber {
	return &localProber{dp: dp, st: st, nodeID: nodeID}
}

// Probe 实现 agent.Prober。
func (p *localProber) Probe(ctx context.Context) agent.HeartbeatRecord {
	now := time.Now().UTC()
	rec := agent.HeartbeatRecord{
		NodeID:       p.nodeID,
		AgentVersion: AgentVersion,
		At:           now,
	}
	det := model.NodeHealthDetail{Agent: model.NodeOnline, At: now}
	var notes []string

	caps, err := p.dp.Capabilities(ctx)
	if err != nil {
		det.Proxy = model.NodeOffline
		notes = append(notes, "转发内核探测失败："+err.Error())
	} else {
		rec.Capabilities = caps
		rec.HAProxyVersion = caps.Version
		switch {
		case !caps.Supported:
			det.Proxy = model.NodeDegraded
			notes = append(notes, "HAProxy 版本 "+caps.Version+" 不在受支持范围内")
		case !caps.MasterWorker:
			det.Proxy = model.NodeDegraded
			notes = append(notes, "HAProxy 未启用 master-worker，无法平滑发布")
		default:
			det.Proxy = model.NodeOnline
		}
	}

	ver, verr := p.dp.ActiveVersion(ctx)
	switch {
	case verr != nil:
		// 读不到就**不写版本**：写 0 会让界面显示"实际版本 0"，
		// 看上去像配置被清空了，而事实只是"这一秒没读到"。
		notes = append(notes, "运行版本读取失败："+verr.Error())
	case ver <= 0:
		det.Path = model.NodeUnknown
		notes = append(notes, "尚未发布任何配置")
	default:
		rec.AppliedVersion = ver
		rec.AppliedVersionKnown = true
		det.Path = p.pathHealth(ctx, ver, &notes)
	}

	det.Note = strings.Join(notes, "；")
	rec.Detail = det
	return rec
}

// pathHealth 用"当前运行的版本"核对业务路径。
func (p *localProber) pathHealth(ctx context.Context, ver int, notes *[]string) model.NodeHealth {
	st, err := p.st.DesiredStateForNode(p.nodeID, ver)
	if err != nil {
		*notes = append(*notes, "取期望状态失败："+err.Error())
		return model.NodeUnknown
	}
	vr, err := p.dp.Verify(ctx, dataplane.VerifyRequest{
		State: *st, Version: ver, ProbeTimeout: 2 * time.Second,
	})
	if err != nil {
		*notes = append(*notes, "业务路径核对失败："+err.Error())
		return model.NodeDegraded
	}
	if vr.OK {
		return model.NodeOnline
	}
	*notes = append(*notes, "业务路径不完整："+dataplane.DescribeVerify(vr))
	return model.NodeDegraded
}

var _ agent.Prober = (*localProber)(nil)
