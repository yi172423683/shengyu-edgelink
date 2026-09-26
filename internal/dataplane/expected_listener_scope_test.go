package dataplane

import (
	"sort"
	"testing"

	"github.com/shengyu/edgelink/internal/haproxy"
	"github.com/shengyu/edgelink/internal/model"
)

// 「应当监听哪些端口」这件事只能有一个口径。
//
// 真机实测（docs/08 第 6 节）：先用 API 建了一个 9443 的 SNI 入口、但业务没建成功
// （该入口因此是"空"的），随后发布 —— 渲染器按设计**跳过**空入口
// （haproxy.Render 里 `if len(rs) == 0 { continue }`，注释写明"空入口不下发"），
// 而 dataplane.ExpectedListeners 却把 9443 算进了"应当监听"，
// 于是 Verify 判 "0.0.0.0:9443 未监听"：发布白等 30.1 秒（重试 61 次）后失败，
// 并触发一次完全没有必要的回滚。
//
// 这类"两个包各算一遍"的口径分歧，单看任何一边都发现不了 ——
// 只有把两边放在一起对拍才看得见。本文件就是那个对拍。

// baseState 造一个有 8443 入口（含一条业务）+ 一个空 9443 入口 + 一条 TCP 路由的状态。
func baseState() model.DesiredState {
	return model.DesiredState{
		Node:    model.Node{ID: "node_x", Name: "对拍节点"},
		Version: 3,
		SNIEntries: []model.SNIEntry{
			{ID: "sni_8443", NodeID: "node_x", BindAddr: "0.0.0.0", BindPort: 8443, Enabled: true},
			{ID: "sni_9443", NodeID: "node_x", BindAddr: "0.0.0.0", BindPort: 9443, Enabled: true},
			{ID: "sni_9444", NodeID: "node_x", BindAddr: "0.0.0.0", BindPort: 9444, Enabled: false},
		},
		Routes: []model.RouteView{
			{
				Route: model.Route{ID: "rt_1", BusinessID: "biz_1", NodeID: "node_x",
					Mode: model.ModeSNITLS, SNIEntryID: "sni_8443", OriginHost: "1.1.1.1", OriginPort: 443,
					Enabled: true},
				BusinessName: "SNI 业务", Mode: model.ModeSNITLS,
				Domains: []string{"a.example.com"}, Enabled: true,
			},
			{
				Route: model.Route{ID: "rt_2", BusinessID: "biz_2", NodeID: "node_x",
					Mode: model.ModeTCPPort, EntryAddr: "0.0.0.0", EntryPort: 19001,
					OriginHost: "1.1.1.1", OriginPort: 19002, Enabled: true},
				BusinessName: "TCP 业务", Mode: model.ModeTCPPort, Enabled: true,
			},
		},
	}
}

// 空入口不应当被要求监听（渲染器就不下发它）。
func TestExpectedListenersSkipsEmptySNIEntry(t *testing.T) {
	st := baseState()
	got := map[string]bool{}
	for _, l := range ExpectedListeners(st) {
		got[l.Key()] = true
	}
	if !got["0.0.0.0:8443"] {
		t.Fatalf("有业务的 8443 入口必须在期望监听里，实际 %v", got)
	}
	if got["0.0.0.0:9443"] {
		t.Fatalf("**空的** 9443 入口不该被要求监听（渲染器按设计不下发它），"+
			"否则每次发布都会白等 30 秒后假失败并回滚。实际 %v", got)
	}
	if got["0.0.0.0:9444"] {
		t.Fatalf("未启用的入口更不该被要求监听，实际 %v", got)
	}
	if !got["0.0.0.0:19001"] {
		t.Fatalf("TCP 路由的入口必须在期望监听里，实际 %v", got)
	}
	// frontend 归属表也必须用同一套过滤，否则两张表会对不上号。
	fes := expectedFrontends(st)
	if _, ok := fes["0.0.0.0:9443"]; ok {
		t.Fatalf("frontend 归属表也不该给空入口留位置，实际 %v", fes)
	}
	if fes["0.0.0.0:8443"] != haproxy.SNIFrontendName(8443) {
		t.Fatalf("8443 的 frontend 名应为 %s，实际 %q", haproxy.SNIFrontendName(8443), fes["0.0.0.0:8443"])
	}
}

// 与渲染器的 ExpectedListeners **逐项对拍**：两者的键集合必须完全相同。
//
// 渲染器那份是"它真的会生成哪些监听"的权威答案（它就是在写 frontend 的循环里 append 的），
// 所以拿它对拍能一次性发现所有口径分歧。
func TestExpectedListenersAgreeWithRenderer(t *testing.T) {
	st := baseState()
	rendered, err := haproxy.Render(st, haproxy.DefaultDefaults())
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}

	keys := func(ls []model.Listener) []string {
		var out []string
		for _, l := range ls {
			// 渲染器存的是原始 BindAddr（可能是 "" 或 "*"），必须用同一套规范化，
			// 否则会把"同一个监听的不同写法"误判成差异。
			norm := model.Listener{Addr: NormalizeBindAddr(l.Addr), Port: l.Port}
			out = append(out, norm.Key())
		}
		sort.Strings(out)
		return out
	}
	want := keys(rendered.ExpectedListeners) // 渲染器口径
	got := keys(ExpectedListeners(st))       // 验证口径

	if len(want) == 0 {
		t.Fatal("渲染器应当至少产出一个监听，用例本身无效")
	}
	if len(want) != len(got) {
		t.Fatalf("两个口径的监听数量不一致：渲染器 %v / 验证 %v", want, got)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("两个口径不一致：渲染器 %v / 验证 %v", want, got)
		}
	}
	// 反证：本用例必须真的覆盖到"空入口"这件事，否则它证明不了什么。
	if len(rendered.ExpectedListeners) == len(st.SNIEntries)+1 {
		t.Fatalf("用例失效：期望的监听数（%d）与「每个入口都算」的写法相同，"+
			"说明空入口没有被构造出来", len(rendered.ExpectedListeners))
	}
}

// 入口被停用后，它的监听应当立刻从期望里消失（与渲染器同步）。
func TestExpectedListenersFollowsEntryEnabledFlag(t *testing.T) {
	st := baseState()
	st.SNIEntries[0].Enabled = false // 关掉 8443
	got := map[string]bool{}
	for _, l := range ExpectedListeners(st) {
		got[l.Key()] = true
	}
	if got["0.0.0.0:8443"] {
		t.Fatalf("停用的入口不该被要求监听，实际 %v", got)
	}
	// 渲染器也必须同意
	rendered, err := haproxy.Render(st, haproxy.DefaultDefaults())
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range rendered.ExpectedListeners {
		if NormalizeBindAddr(l.Addr) == "0.0.0.0" && l.Port == 8443 {
			t.Fatal("渲染器与验证口径不一致：渲染器仍在期待 8443")
		}
	}
}
