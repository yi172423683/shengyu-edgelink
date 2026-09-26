package api

import (
	"context"
	"errors"
	"testing"

	"github.com/shengyu/edgelink/internal/dataplane"
	"github.com/shengyu/edgelink/internal/model"
)

// appliedVersionStub 只实现「读生效版本」这一个能力。
type appliedVersionStub struct {
	ver int
	err error
}

func (s *appliedVersionStub) Name() string { return "applied-version-stub" }
func (s *appliedVersionStub) Capabilities(context.Context) (model.Capabilities, error) {
	return model.Capabilities{}, nil
}
func (s *appliedVersionStub) Validate(context.Context, string) error { return nil }
func (s *appliedVersionStub) Apply(context.Context, dataplane.ApplyRequest) error {
	return nil
}
func (s *appliedVersionStub) Verify(context.Context, dataplane.VerifyRequest) (dataplane.VerifyResult, error) {
	return dataplane.VerifyResult{}, nil
}
func (s *appliedVersionStub) Listeners(context.Context) ([]model.Listener, error) { return nil, nil }
func (s *appliedVersionStub) Stats(context.Context) (dataplane.Stats, error) {
	return dataplane.Stats{}, nil
}
func (s *appliedVersionStub) ActiveVersion(context.Context) (int, error) {
	return s.ver, s.err
}
func (s *appliedVersionStub) Close() error { return nil }

// 「当前运行版本」必须听数据面的，不能被节点表里那个更大的陈旧上报值盖住。
//
// 这是真机跑出来的：v9 发布失败并自动回滚到 v3 后，页面在最长 20 秒（一轮心跳）
// 里显示"当前运行版本 v9"，而机器上跑的是 v3 —— 而那正是运维盯着失败面板的时刻。
func TestEffectiveAppliedVersionPrefersDataplaneOverStaleReport(t *testing.T) {
	ctx := context.Background()

	// 数据面说 3（回滚后的真实值），节点表里 agent 还报着 9（回滚前采到的，心跳还没刷）
	got := effectiveAppliedVersion(ctx, &appliedVersionStub{ver: 3}, 9)
	if got != 3 {
		t.Fatalf("回滚使生效版本变小，界面必须以数据面为准显示 v3；实际 v%d"+
			"（取大值会让页面在心跳刷新前一直显示回滚前的 v9）", got)
	}

	// 首次发布成功后节点表还是启动时的 0，此时也要以数据面为准
	if got := effectiveAppliedVersion(ctx, &appliedVersionStub{ver: 1}, 0); got != 1 {
		t.Fatalf("数据面报 v1 时应显示 v1（否则刷新页面会被误判成「还没发布过」），实际 v%d", got)
	}

	// 数据面读不到时才回退到上报值：宁可显示可能偏旧的值，也不要显示空
	if got := effectiveAppliedVersion(ctx, &appliedVersionStub{err: errors.New("读取失败")}, 4); got != 4 {
		t.Fatalf("数据面读不到版本时应回退到 agent 上报值 v4，实际 v%d", got)
	}

	// 没有数据面（例如纯只读展示）时同样回退
	if got := effectiveAppliedVersion(ctx, nil, 5); got != 5 {
		t.Fatalf("未注入数据面时应回退到上报值 v5，实际 v%d", got)
	}
}
