package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/shengyu/edgelink/internal/model"
)

// 本文件是评审 F10 的回归测试：
// 本机节点必须**周期性续报**心跳，否则后台"90 秒没心跳就标离线"会把自己这台
// 正在正常转发的机器标成离线；并且健康必须拆成 agent / 转发内核 / 业务路径三个维度。
//
// 用假时钟驱动，不去 sleep 真实时间。

type sinkRecorder struct {
	mu    sync.Mutex
	got   []HeartbeatRecord
	times []time.Time
}

func (s *sinkRecorder) sink(rec HeartbeatRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, rec)
	s.times = append(s.times, rec.At)
	return nil
}

func (s *sinkRecorder) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

func (s *sinkRecorder) last() HeartbeatRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) == 0 {
		return HeartbeatRecord{}
	}
	return s.got[len(s.got)-1]
}

func TestHeartbeaterTicksWithSimulatedClock(t *testing.T) {
	clock := time.Now().UTC()
	rec := &sinkRecorder{}
	probe := StaticProber{Record: HeartbeatRecord{
		Detail: model.NodeHealthDetail{
			Agent: model.NodeOnline, Proxy: model.NodeOnline, Path: model.NodeOnline,
		},
		AppliedVersion: 7, AppliedVersionKnown: true,
		HAProxyVersion: "2.8.0", AgentVersion: "test",
	}}
	hb := NewHeartbeater("node-1", probe, rec.sink, 20*time.Second)
	hb.SetClock(func() time.Time { return clock })
	hb.SetErrorHandler(func(error) {})

	// 模拟 6 个心跳周期：每次把时钟往前拨 20 秒
	const ticks = 6
	for i := 0; i < ticks; i++ {
		if err := hb.Tick(context.Background()); err != nil {
			t.Fatalf("心跳失败: %v", err)
		}
		clock = clock.Add(20 * time.Second)
	}
	if rec.count() != ticks {
		t.Fatalf("%d 个周期应产生 %d 次心跳，实际 %d", ticks, ticks, rec.count())
	}
	// 心跳时间必须随着周期推进。
	// 注意：这里先把时间片拷出来再放锁 —— 持锁期间去调用也会加锁的方法就是自死锁，
	// 那会让整个测试套件卡到超时（本轮真的踩到过）。
	rec.mu.Lock()
	times := append([]time.Time(nil), rec.times...)
	rec.mu.Unlock()
	for i := 1; i < len(times); i++ {
		if !times[i].After(times[i-1]) {
			t.Fatalf("心跳时间必须持续推进（否则会被判离线）：%v → %v", times[i-1], times[i])
		}
	}
	// 6 个周期共 100 秒 > 90 秒的离线阈值：正因为有续报，才不会被误判离线
	if times[len(times)-1].Sub(times[0]) < 90*time.Second {
		t.Fatalf("测试没有覆盖到 90 秒窗口，实际跨度 %v", times[len(times)-1].Sub(times[0]))
	}
	last := rec.last()
	if last.AppliedVersion != 7 || !last.AppliedVersionKnown {
		t.Fatalf("心跳必须带上实际生效版本，实际 %+v", last)
	}
	if got := last.Detail.Aggregate(); got != model.NodeOnline {
		t.Fatalf("三维全在线时聚合应为 online，实际 %s", got)
	}
}

// 三维健康的聚合规则：三者独立，且"管理面失联但转发正常"不能被报成离线。
func TestHealthDetailAggregation(t *testing.T) {
	cases := []struct {
		name string
		d    model.NodeHealthDetail
		want model.NodeHealth
	}{
		{"全部正常", model.NodeHealthDetail{Agent: model.NodeOnline, Proxy: model.NodeOnline, Path: model.NodeOnline}, model.NodeOnline},
		{"全未知", model.NodeHealthDetail{}, model.NodeUnknown},
		{"内核离线是真事故", model.NodeHealthDetail{Agent: model.NodeOnline, Proxy: model.NodeOffline, Path: model.NodeOffline}, model.NodeOffline},
		{"agent 失联但转发正常", model.NodeHealthDetail{Agent: model.NodeOffline, Proxy: model.NodeOnline, Path: model.NodeOnline}, model.NodeDegraded},
		{"内核在跑但路径不全", model.NodeHealthDetail{Agent: model.NodeOnline, Proxy: model.NodeOnline, Path: model.NodeDegraded}, model.NodeDegraded},
		{"尚未发布配置", model.NodeHealthDetail{Agent: model.NodeOnline, Proxy: model.NodeOnline, Path: model.NodeUnknown}, model.NodeDegraded},
	}
	for _, c := range cases {
		if got := c.d.Aggregate(); got != c.want {
			t.Fatalf("%s：聚合结果应为 %s，实际 %s", c.name, c.want, got)
		}
	}
}

// 未知版本不能被写成 0：0 在界面上会显示成"实际版本 0"，看起来像配置被清空了。
func TestUnknownAppliedVersionIsNotZero(t *testing.T) {
	probe := StaticProber{Record: HeartbeatRecord{
		Detail: model.NodeHealthDetail{Agent: model.NodeOnline, Proxy: model.NodeOnline, Path: model.NodeUnknown},
		// AppliedVersion 保持 0，但 AppliedVersionKnown=false
	}}
	hb := NewHeartbeater("n", probe, func(HeartbeatRecord) error { return nil }, time.Second)
	rec := probe.Probe(context.Background())
	if rec.AppliedVersionKnown {
		t.Fatal("探针没有读到版本时必须标记为未知")
	}
	if err := hb.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}
