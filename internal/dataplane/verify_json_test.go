package dataplane

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shengyu/edgelink/internal/model"
)

// 发布结果里的 `verify.listeners` 是**嵌套**结构，而且它的 JSON 键名必须稳定。
//
// 真机上发现的缺陷（docs/08 §9.3）：`ListenerStatus` 当初只有 Frontend 带了 json 标签，
// 其余字段按 Go 默认规则序列化成 `Expected` / `Bound` / `OwnedByUs` / `Note`。
// 结果：
//   - 界面里 `l.addr` 永远读到 undefined → 发布成功的气泡显示"监听：undefined:undefined"；
//   - 我自己的验收脚本按 `l["expected"]["addr"]` 取值也全读到 None，于是"9443 不在监听列表里"
//     这条断言变成了空断言（永远成立）。
//
// 两个"读到空值却都不报错"的地方叠在一起，就成了一条**看起来验证过、其实什么都没验**的路径。
// 所以这里把 JSON 形状钉死：键名必须是 snake_case，地址必须是 expected.addr。
func TestVerifyResultJSONShapeIsStable(t *testing.T) {
	res := VerifyResult{
		OK:               true,
		DataplaneVersion: 7,
		VersionMarkerOK:  true,
		StatsSocketOK:    true,
		StatsSocketPath:  "/run/shengyu-edgelink/stats-v7.sock",
		ListenersOK:      true,
		Listeners: []ListenerStatus{{
			Expected:  model.Listener{Addr: "0.0.0.0", Port: 8443},
			Frontend:  "fe_sni_8443",
			Bound:     true,
			OwnedByUs: true,
		}},
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)

	// 必须存在的键（snake_case）
	for _, must := range []string{
		`"ok"`, `"dataplane_version"`, `"version_marker_ok"`, `"stats_socket_ok"`,
		`"stats_socket_path"`, `"listeners_ok"`, `"listeners"`,
		`"frontend"`, `"bound"`, `"owned_by_us"`,
		`"expected"`, `"addr"`, `"port"`,
	} {
		if !strings.Contains(got, must) {
			t.Errorf("VerifyResult 的 JSON 应含 %s，实际 %s", must, got)
		}
	}
	// 绝不能出现 Go 默认的驼峰键名 —— 界面与脚本都按 snake_case 取值
	for _, forbidden := range []string{`"Expected"`, `"Bound"`, `"OwnedByUs"`, `"Note"`, `"VersionMarkerOK"`} {
		if strings.Contains(got, forbidden) {
			t.Errorf("不该出现 Go 默认键名 %s（界面/脚本按 snake_case 取值，读不到就会静默变成 undefined）：%s",
				forbidden, got)
		}
	}
	// 地址必须是**嵌套**的：expected.addr —— 直接按 l.addr 取会一直是空
	var back struct {
		Listeners []struct {
			Expected struct {
				Addr string `json:"addr"`
				Port int    `json:"port"`
			} `json:"expected"`
			OwnedByUs bool `json:"owned_by_us"`
		} `json:"listeners"`
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Listeners) != 1 {
		t.Fatalf("应能解出 1 条监听，实际 %d", len(back.Listeners))
	}
	if back.Listeners[0].Expected.Addr != "0.0.0.0" || back.Listeners[0].Expected.Port != 8443 {
		t.Fatalf("expected.addr/port 解析不对：%+v", back.Listeners[0].Expected)
	}
	if !back.Listeners[0].OwnedByUs {
		t.Fatalf("owned_by_us 解析不对：%+v", back.Listeners[0])
	}
}
