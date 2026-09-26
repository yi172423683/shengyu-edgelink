package logparse

import (
	"testing"
)

// 本文件是评审 F11 的第一半：**字节方向**。
//
// 依据 HAProxy 2.8 手册 8.2.6 的别名表原文：
//
//	| %U | bytes_uploaded (from client to server) | numeric |
//	| %B | bytes_read     (from server to client) | numeric |
//
// 旧实现把 %B 标成"客户端→中转字节"，也就是把下行说成了上行。
// 用**不对称**的上下行数据来验证，是最不容易自欺欺人的方式：
// 如果两个方向被写反，断言必然失败；如果两边都用同一个值，写反了也看不出来。
func TestByteDirectionsAreNotSwapped(t *testing.T) {
	// 上传 1000、下载 9_000_000：差三个数量级，写反了一眼就能看出来
	line := `{"ts":"22/Sep/2026:15:04:05.123","node":"n1","ver":3,"ci":"203.0.113.9",` +
		`"up":"1000","down":"9000000","tsc":"--"}`
	rec, err := Parse([]byte(line), nil)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if rec.BytesUp == nil || *rec.BytesUp != 1000 {
		t.Fatalf("up 应为 1000（客户端→源站），实际 %v", rec.BytesUp)
	}
	if rec.BytesDown == nil || *rec.BytesDown != 9000000 {
		t.Fatalf("down 应为 9000000（源站→客户端），实际 %v", rec.BytesDown)
	}
	if *rec.BytesUp >= *rec.BytesDown {
		t.Fatal("上下行方向被写反了：上行不应大于下载量（这是把下传说成上行的典型表现）")
	}
}

// 两个方向各自独立地"不可用"：一个能取到、一个取不到时，不能互相污染。
func TestByteDirectionsIndependent(t *testing.T) {
	line := `{"ts":"22/Sep/2026:15:04:05.123","node":"n1","ver":1,"ci":"203.0.113.9",` +
		`"up":"-","down":"4096","tsc":"--"}`
	rec, err := Parse([]byte(line), nil)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if rec.BytesUp != nil {
		t.Fatalf("取不到的字段必须是「不可用」（nil），不能是 0：实际 %v", *rec.BytesUp)
	}
	if rec.BytesDown == nil || *rec.BytesDown != 4096 {
		t.Fatalf("另一个方向不该受牵连，实际 %v", rec.BytesDown)
	}
	found := false
	for _, u := range rec.Unavailable {
		if u == "up" {
			found = true
		}
	}
	if !found {
		t.Fatalf("不可用清单里应列出 up，实际 %v", rec.Unavailable)
	}
}

// HAProxy 实报的目标地址（%si/%sp）优先于平台按配置反查的结果：
// 配置说的是"应该连哪里"，日志报的是"实际连到了哪里"。
type fixedResolver struct {
	biz    string
	host   string
	port   int
	atVers map[int]string
}

func (r fixedResolver) BusinessForBackend(string) (string, string, bool) { return r.biz, "rt_1", true }
func (r fixedResolver) OriginForBackend(string) (string, int, bool)      { return r.host, r.port, true }
func (r fixedResolver) BusinessForBackendAt(int, string) (string, string, bool) {
	return r.biz, "rt_1", true
}
func (r fixedResolver) OriginForBackendAt(v int, _ string) (string, int, bool) {
	if h, ok := r.atVers[v]; ok {
		return h, 8443, true
	}
	return "", 0, false
}

func TestReportedOriginWinsAndVersionSnapshotUsed(t *testing.T) {
	res := fixedResolver{
		biz: "biz_A", host: "203.0.113.1", port: 8443,
		atVers: map[int]string{5: "203.0.113.5", 9: "203.0.113.9"},
	}
	// ① 日志里带了 HAProxy 实报的目标地址 → 以它为准（并标记来源）
	withOIP := `{"ts":"22/Sep/2026:15:04:05.123","node":"n1","ver":5,"ci":"1.2.3.4","be":"bk_x",` +
		`"oip":"203.0.113.5","oport":"9999","tsc":"--"}`
	rec, err := Parse([]byte(withOIP), res)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.OriginReported {
		t.Fatal("带 oip 时必须标记「源站地址来自 HAProxy 实报」，否则界面无法区分事实与反查")
	}
	if rec.OriginAddr != "203.0.113.5" || rec.OriginPort == nil || *rec.OriginPort != 9999 {
		t.Fatalf("实报地址应优先，实际 %s:%v", rec.OriginAddr, rec.OriginPort)
	}

	// ② 没有 oip 时，按**该日志所属版本**的快照反查
	noOIP := `{"ts":"22/Sep/2026:15:04:05.123","node":"n1","ver":9,"ci":"1.2.3.4","be":"bk_x","tsc":"--"}`
	rec2, err := Parse([]byte(noOIP), res)
	if err != nil {
		t.Fatal(err)
	}
	if rec2.OriginReported {
		t.Fatal("没有 oip 时不应声称「实报」")
	}
	if rec2.OriginAddr != "203.0.113.9" {
		t.Fatalf("应按 ver=9 的快照反查源站，实际 %q", rec2.OriginAddr)
	}

	// ③ 快照缺失时**留空**，不许退回"当前配置"去猜（那正是评审指出的错误来源）
	missing := `{"ts":"22/Sep/2026:15:04:05.123","node":"n1","ver":77,"ci":"1.2.3.4","be":"bk_x","tsc":"--"}`
	rec3, err := Parse([]byte(missing), res)
	if err != nil {
		t.Fatal(err)
	}
	if rec3.OriginAddr != "" || rec3.OriginPort != nil {
		t.Fatalf("版本快照缺失时必须留空（界面显示不可用），绝不能退回当前配置：实际 %s:%v",
			rec3.OriginAddr, rec3.OriginPort)
	}
	// 业务归属仍可解析（backend 名与版本无关，见 store.nodeResolver 的说明）
	if rec3.BusinessID != "biz_A" {
		t.Fatalf("业务归属不该受影响，实际 %q", rec3.BusinessID)
	}
}
