package agent

import (
	"context"
	"log"
	"time"

	"github.com/shengyu/edgelink/internal/model"
)

// 本文件实现**本机节点的周期性心跳**（评审 F10）。
//
// 问题原样：本机节点只在启动时写了一次 LastHeartbeat，而后台维护任务每 30 秒
// 把"超过 90 秒没心跳"的节点标为离线 —— 于是一台正在正常转发流量的机器，
// 在启动约 90~120 秒后会被自己的管理面标成"离线"，误导总览与后续的高可用决策。
//
// 修法有两层，缺一不可：
//
//	① 周期性续报（就是这里）；
//	② 把"健康"拆成 agent / 转发内核 / 业务路径三个维度（model.NodeHealthDetail），
//	   因为一次故障里这三者的答案经常不一致，而处置方式完全不同：
//	       agent 离线 + 内核在线 → 管理面失联而已，转发正常，不需要救火
//	       agent 在线 + 内核离线 → 真事故
//	       路径不完整           → 配置没生效/端口被占，要人看一眼
//
// 探针与写库都通过接口注入：心跳逻辑必须能被"假时钟 + 假探针"完整测试，
// 而不是只能靠"等 90 秒看会不会变红"来验证。

// HeartbeatRecord 一次心跳要写进节点的内容。
type HeartbeatRecord struct {
	NodeID              string
	At                  time.Time
	Detail              model.NodeHealthDetail
	AppliedVersion      int
	AppliedVersionKnown bool
	Capabilities        model.Capabilities
	HAProxyVersion      string
	AgentVersion        string
}

// Prober 采集一次本机健康。
//
// 实现方必须遵守"不确定就说不确定"：读不到版本时把 AppliedVersionKnown 置 false，
// 而不是返回 0 —— 0 会被界面显示成"实际版本 0"，看上去像配置被清空了。
type Prober interface {
	Probe(ctx context.Context) HeartbeatRecord
}

// Heartbeater 周期性把本机健康写回节点记录。
type Heartbeater struct {
	NodeID string
	Every  time.Duration
	Probe  Prober
	// Sink 落库。返回 error 表示这次心跳没写进去（下一次会重试）。
	Sink func(HeartbeatRecord) error

	now   func() time.Time
	onErr func(error)
}

// NewHeartbeater 构造心跳器。Every <= 0 时用 20 秒。
//
// 为什么是 20 秒：离线判定阈值是 90 秒，20 秒的心跳让判定有 4 次以上的容错，
// 同时不给元数据库带来压力（一分钟 3 次 UPDATE）。
func NewHeartbeater(nodeID string, probe Prober, sink func(HeartbeatRecord) error, every time.Duration) *Heartbeater {
	if every <= 0 {
		every = 20 * time.Second
	}
	return &Heartbeater{NodeID: nodeID, Every: every, Probe: probe, Sink: sink, now: time.Now}
}

// SetClock 注入时钟（测试用）。
func (h *Heartbeater) SetClock(f func() time.Time) {
	if f != nil {
		h.now = f
	}
}

// SetErrorHandler 注入错误处理（测试用；默认打日志）。
func (h *Heartbeater) SetErrorHandler(f func(error)) { h.onErr = f }

func (h *Heartbeater) report(err error) {
	if err == nil {
		return
	}
	if h.onErr != nil {
		h.onErr(err)
		return
	}
	log.Printf("本机心跳写入失败（下一周期会重试）: %v", err)
}

// Tick 执行一次心跳。**导出**是为了让测试可以精确控制节奏，
// 而不是 sleep 20 秒再断言。
func (h *Heartbeater) Tick(ctx context.Context) error {
	rec := h.Probe.Probe(ctx)
	if rec.NodeID == "" {
		rec.NodeID = h.NodeID
	}
	if rec.At.IsZero() {
		rec.At = h.now()
	}
	if rec.Detail.At.IsZero() {
		rec.Detail.At = rec.At
	}
	return h.Sink(rec)
}

// Run 周期性心跳，直到 ctx 结束。
func (h *Heartbeater) Run(ctx context.Context) {
	// 启动即报一次：不要等第一个周期过去 ——
	// 那 20 秒里界面上显示的是启动时的旧状态，而"刚启动"恰恰是最需要看清的时刻。
	h.report(h.Tick(ctx))

	t := time.NewTicker(h.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.report(h.Tick(ctx))
		}
	}
}

// StaticProber 是 Heartbeater 的测试替身：固定返回值，不依赖真实数据面。
type StaticProber struct {
	Record HeartbeatRecord
}

// Probe 实现 Prober。
func (s StaticProber) Probe(context.Context) HeartbeatRecord { return s.Record }
