package main

import (
	"strings"
	"testing"

	"github.com/shengyu/edgelink/internal/dataplane"
)

// 这条测试钉住的是一条**产品约束**，不是实现细节：
// 生产程序只能用 HAProxy 社区版转发。仓库里那个纯 Go 的转发实现（internal/testplane）
// 是测试辅助，不能被 -dataplane 选起来，也不能被当成"HAProxy 不可用时的降级方案"。
//
// 之所以要专门写测试：这种约束一旦被"顺手加个选项"破坏，不会有任何功能报错，
// 只会在某天有人用它在生产上转发客户业务时才暴露 —— 那时已经晚了。
func TestDataplaneKindOnlyAllowsHAProxy(t *testing.T) {
	// 允许的取值
	for _, k := range []string{"", dataplane.HAProxyName} {
		if err := checkDataplaneKind(k); err != nil {
			t.Errorf("%q 应当被接受，实际报错: %v", k, err)
		}
	}

	// 必须拒绝的取值。gorelay 单独列出并把理由写在测试里，避免日后被"顺手放开"。
	for _, k := range []string{"gorelay", "goRelay", "testplane", "haproxy2", "nginx", "none"} {
		err := checkDataplaneKind(k)
		if err == nil {
			t.Fatalf("%q 必须被拒绝：生产只允许 HAProxy", k)
		}
		if !strings.Contains(err.Error(), dataplane.HAProxyName) {
			t.Errorf("%q 的拒绝信息里应指明只支持 %s，实际: %v", k, dataplane.HAProxyName, err)
		}
	}

	// gorelay 的拒绝信息必须额外说明"它是什么、为什么不能用"，
	// 否则运维看到"不支持的数据面 gorelay"只会去翻文档找一个并不存在的开关。
	err := checkDataplaneKind("gorelay")
	if err == nil {
		t.Fatal("gorelay 必须被拒绝")
	}
	if !strings.Contains(err.Error(), "仅供自动化测试") {
		t.Errorf("gorelay 的拒绝信息必须说明它仅供测试使用，实际: %v", err)
	}
}

func TestResourceProfiles(t *testing.T) {
	lite, err := resolveResourceProfile("lite")
	if err != nil {
		t.Fatal(err)
	}
	if lite.Name != "lite" || lite.MaxConn != 4000 || lite.MaxOpenShards != 2 || lite.BatchMaxBuffer != 3000 {
		t.Fatalf("轻量档位参数异常: %+v", lite)
	}
	standard, err := resolveResourceProfile("standard")
	if err != nil {
		t.Fatal(err)
	}
	if standard.Name != "standard" || standard.MaxConn != 20000 || standard.MaxOpenShards != 8 {
		t.Fatalf("标准档位参数异常: %+v", standard)
	}
	if _, err := resolveResourceProfile("unknown"); err == nil {
		t.Fatal("未知资源档位必须被拒绝")
	}
}

func TestApplyResourceProfileFeedsRendererDefaults(t *testing.T) {
	o := options{profile: "lite", haproxyNBThread: 1}
	if err := applyResourceProfile(&o); err != nil {
		t.Fatal(err)
	}
	d := renderDefaults(o)
	if d.MaxConn != 4000 {
		t.Fatalf("轻量档位应把 HAProxy maxconn 设为 4000，实际 %d", d.MaxConn)
	}
}
