package haproxy

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// 这组测试守的是「nbthread 不再是写死的 4」这件事，以及它的边界。
//
// 为什么值得单独一个文件：线程数是**唯一一个**"合法取值依赖运行机"的渲染参数
// （CPU 数与 HAProxy 编译期 MAX_THREADS）。写死过一次 4，在 2 vCPU 的验收机上
// 直接让 HAProxy 打告警、吞吐数据不可信。把边界钉成测试，比写在注释里可靠。

func TestDefaultNBThreadInRange(t *testing.T) {
	n := DefaultNBThread()
	if n < MinNBThread || n > MaxNBThread {
		t.Fatalf("DefaultNBThread()=%d 越界，应落在 [%d,%d]", n, MinNBThread, MaxNBThread)
	}
	// 默认值的定义就是"CPU 数归一化后的结果"，两者必须一致。
	if want := NormalizeNBThread(runtime.NumCPU()); n != want {
		t.Fatalf("DefaultNBThread()=%d，期望等于 NormalizeNBThread(NumCPU)=%d", n, want)
	}
}

func TestValidateNBThread(t *testing.T) {
	cases := []struct {
		n       int
		wantErr bool
	}{
		{-1, true}, // 负数：非法
		{0, true},  // 0 不是"自动"，自动由 NormalizeNBThread 负责；先归一化再校验
		{1, false}, // 下界
		{2, false},
		{MaxNBThread, false},    // 上界
		{MaxNBThread + 1, true}, // 超上限：必须拒绝，而不是静默夹断
		{1024, true},
	}
	for _, c := range cases {
		err := ValidateNBThread(c.n)
		if c.wantErr && err == nil {
			t.Fatalf("ValidateNBThread(%d) 应报错，却返回 nil", c.n)
		}
		if !c.wantErr && err != nil {
			t.Fatalf("ValidateNBThread(%d) 不应报错: %v", c.n, err)
		}
	}
}

func TestNormalizeNBThread(t *testing.T) {
	cpu := NormalizeNBThread(runtime.NumCPU())
	cases := []struct {
		in   int
		want int
	}{
		{0, cpu},                       // 0 = 自动取 CPU 数
		{-3, cpu},                      // 负数同 0
		{1, 1},                         // 下界保持
		{MaxNBThread, MaxNBThread},     // 上界保持
		{MaxNBThread + 1, MaxNBThread}, // 超限夹到上限（这里是"防御"，主路径由 Validate 拒绝）
		{9999, MaxNBThread},
	}
	for _, c := range cases {
		if got := NormalizeNBThread(c.in); got != c.want {
			t.Fatalf("NormalizeNBThread(%d)=%d，期望 %d", c.in, got, c.want)
		}
	}
	// 归一化后的结果必须一律合法，否则"归一 + 校验"这条链就是断的。
	for _, in := range []int{-100, -1, 0, 1, 2, MaxNBThread, MaxNBThread + 1, 9999} {
		if err := ValidateNBThread(NormalizeNBThread(in)); err != nil {
			t.Fatalf("NormalizeNBThread(%d) 的结果仍不合法: %v", in, err)
		}
	}
}

// 渲染必须真的用这个值 —— 参数接进来了但没写进配置，是最容易漏的一类缺陷。
func TestRenderWritesNBThread(t *testing.T) {
	// 显式值：配置里必须原样出现。
	d := DefaultDefaults()
	d.NBThread = 3
	r, err := Render(sniState(), d)
	if err != nil {
		t.Fatalf("Render 失败: %v", err)
	}
	want := "    nbthread 3\n"
	if !strings.Contains(string(r.Config), want) {
		t.Fatalf("配置里没有 %q；global 段实际为:\n%s", want, globalSection(string(r.Config)))
	}
}

// 零值 Defaults（NBThread=0）也必须能渲染出合法配置，不能渲染出 "nbthread 0"。
func TestRenderZeroNBThreadFallsBackToCPU(t *testing.T) {
	var d Defaults // 零值：模拟调用方没设过线程数
	r, err := Render(sniState(), d)
	if err != nil {
		t.Fatalf("零值 Defaults 渲染失败: %v", err)
	}
	cfg := string(r.Config)
	if strings.Contains(cfg, "nbthread 0") {
		t.Fatalf("渲染出了非法的 nbthread 0:\n%s", globalSection(cfg))
	}
	want := fmt.Sprintf("    nbthread %d\n", DefaultNBThread())
	if !strings.Contains(cfg, want) {
		t.Fatalf("零值应回退到 %q，实际 global 段为:\n%s", want, globalSection(cfg))
	}
}

// 超过上限必须让渲染失败，而不是夹断成 64 后"看起来成功"。
func TestRenderRejectsTooLargeNBThread(t *testing.T) {
	d := DefaultDefaults()
	d.NBThread = MaxNBThread + 1
	if _, err := Render(sniState(), d); err == nil {
		t.Fatalf("nbthread=%d 超过上限 %d，Render 应报错而不是静默夹断", d.NBThread, MaxNBThread)
	}
}

// DefaultDefaults 的线程数必须在合法区间内 —— 它会被 e2e / publish 测试直接拿去用。
func TestDefaultDefaultsNBThreadUsable(t *testing.T) {
	d := DefaultDefaults()
	if err := ValidateNBThread(d.NBThread); err != nil {
		t.Fatalf("DefaultDefaults().NBThread=%d 不合法: %v", d.NBThread, err)
	}
}

// globalSection 截取 global 段，测试失败时给出可读的上下文，而不是刷一整份配置。
func globalSection(cfg string) string {
	start := strings.Index(cfg, "global\n")
	if start < 0 {
		return cfg
	}
	rest := cfg[start:]
	if end := strings.Index(rest, "\ndefaults\n"); end >= 0 {
		return rest[:end]
	}
	if len(rest) > 800 {
		return rest[:800]
	}
	return rest
}
