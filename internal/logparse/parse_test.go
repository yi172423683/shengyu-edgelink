package logparse

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/shengyu/edgelink/internal/haproxy"
)

type fakeResolver map[string][2]string

func (f fakeResolver) BusinessForBackend(be string) (string, string, bool) {
	if v, ok := f[be]; ok {
		return v[0], v[1], true
	}
	return "", "", false
}

func (f fakeResolver) OriginForBackend(be string) (string, int, bool) {
	if _, ok := f[be]; ok {
		return "198.51.100.7", 8443, true
	}
	return "", 0, false
}

const goodLine = `{"ts":"22/Sep/2026:15:04:05.123","node":"nd_ABC","ver":42,"ci":"203.0.113.9",` +
	`"cp":"51820","fi":"203.0.113.10","fp":"443","sni":"a.example.com","be":"bk_bza1","srv":"s1",` +
	`"tq":"0","tc":"12","tt":"3456","up":"8192","down":"16384","tsc":"--","rc":"0","bq":"0"}`

// onlyHID 判断：除了「HAProxy 原生连接 ID 不可用」(hid) 之外，没有任何数据字段不可用。
// hid 是恒定存在的（社区版就是没有原生连接 ID），所以它不该被当成"这行有问题"。
func onlyHID(t *testing.T, rec Record) {
	t.Helper()
	for _, u := range rec.Unavailable {
		if u != "hid" {
			t.Fatalf("除 hid 外不应有不可用字段，实际 %v", rec.Unavailable)
		}
	}
}

func TestParseGoodLine(t *testing.T) {
	rec, err := Parse([]byte(goodLine), fakeResolver{"bk_bza1": {"biz_A1", "rt_1"}})
	if err != nil {
		t.Fatalf("应解析成功: %v", err)
	}
	if !rec.ParseOK {
		t.Fatalf("ParseOK 应为 true，ParseErr=%s", rec.ParseErr)
	}
	if rec.SNI != "a.example.com" {
		t.Fatalf("SNI 解析错误: %q", rec.SNI)
	}
	if rec.ClientIP != "203.0.113.9" {
		t.Fatalf("客户端 IP 错误: %q", rec.ClientIP)
	}
	if rec.ClientPort == nil || *rec.ClientPort != 51820 {
		t.Fatalf("客户端端口错误: %v", rec.ClientPort)
	}
	if rec.ConfigVersion != 42 {
		t.Fatalf("配置版本错误: %d", rec.ConfigVersion)
	}
	if rec.SessionMS == nil || *rec.SessionMS != 3456 {
		t.Fatalf("会话时长错误: %v", rec.SessionMS)
	}
	if rec.BytesUp == nil || *rec.BytesUp != 8192 || rec.BytesDown == nil || *rec.BytesDown != 16384 {
		t.Fatalf("字节数错误: up=%v down=%v", rec.BytesUp, rec.BytesDown)
	}
	if rec.BusinessID != "biz_A1" || rec.RouteID != "rt_1" {
		t.Fatalf("富化失败: biz=%s rt=%s", rec.BusinessID, rec.RouteID)
	}
	if rec.OriginAddr != "198.51.100.7" || rec.OriginPort == nil || *rec.OriginPort != 8443 {
		t.Fatalf("源站反查失败: %s:%v", rec.OriginAddr, rec.OriginPort)
	}
	if rec.AcceptTS.IsZero() || rec.AcceptTS.Year() != 2026 {
		t.Fatalf("时间解析失败: %v", rec.AcceptTS)
	}
	onlyHID(t, rec)
}

// ★ 最重要的一条：`-` 必须翻译成「不可用」，绝不能变成 0。
func TestDashMeansUnavailableNotZero(t *testing.T) {
	line := `{"ts":"22/Sep/2026:15:04:05.123","node":"nd_ABC","ver":42,"ci":"203.0.113.9",` +
		`"cp":"-","fi":"203.0.113.10","fp":"-","sni":"-","be":"bk_bza1","srv":"-",` +
		`"tq":"-","tc":"-","tt":"-","up":"-","down":"-","tsc":"-","rc":"-","bq":"-"}`
	rec, err := Parse([]byte(line), nil)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if rec.ClientPort != nil {
		t.Fatalf("不可用的客户端端口必须是 nil，实际 %d", *rec.ClientPort)
	}
	if rec.QueueMS != nil || rec.ConnectMS != nil || rec.SessionMS != nil || rec.BytesUp != nil || rec.BytesDown != nil {
		t.Fatal("不可用的耗时/字节必须是 nil（不能是 0）")
	}
	if rec.SNI != "" {
		t.Fatalf("不可用的 SNI 应为空，实际 %q", rec.SNI)
	}
	want := []string{"tq", "tc", "tt", "up", "down", "sni", "cp", "fp", "srv", "tsc", "rc", "bq"}
	got := strings.Join(rec.Unavailable, ",")
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("不可用清单应包含 %s，实际 %v", w, rec.Unavailable)
		}
	}
}

// `--` 是 TCP 日志格式里"正常结束"的合法终止码，不能被当成不可用吞掉。
func TestDoubleDashTermCodePreserved(t *testing.T) {
	line := `{"ts":"22/Sep/2026:15:04:05.123","node":"n","ver":1,"ci":"1.2.3.4","cp":"1",` +
		`"fi":"5.6.7.8","fp":"443","sni":"a.example.com","be":"bk_x","srv":"s1",` +
		`"tq":"0","tc":"1","tt":"2","bin":"3","tsc":"--","rc":"0","bq":"0"}`
	rec, _ := Parse([]byte(line), nil)
	if rec.TermCode != "--" {
		t.Fatalf("`--` 应被原样保留，实际 %q", rec.TermCode)
	}
	for _, u := range rec.Unavailable {
		if u == "tsc" {
			t.Fatal("`--` 不应被标记为不可用")
		}
	}
}

// 坏行不能丢：必须保留原文与原因，供诊断回看。
func TestNonJSONLineKeptAsEvidence(t *testing.T) {
	raw := "Sep 22 15:04:05 relay-a haproxy[123]: some legacy line"
	rec, err := Parse([]byte(raw), nil)
	if err == nil {
		t.Fatal("非 JSON 行应返回错误")
	}
	if rec.ParseOK {
		t.Fatal("ParseOK 应为 false")
	}
	if rec.RawLine != raw {
		t.Fatalf("原始行必须保留，实际 %q", rec.RawLine)
	}
	if rec.ParseErr == "" {
		t.Fatal("必须给出失败原因")
	}
}

func TestSyslogHeaderIsStripped(t *testing.T) {
	line := "<134>" + goodLine
	rec, err := Parse([]byte(line), nil)
	if err != nil {
		t.Fatalf("带 syslog 头的行也应能解析: %v", err)
	}
	if rec.SNI != "a.example.com" {
		t.Fatalf("剥离 syslog 头失败: %q", rec.SNI)
	}
}

// 客户端可以把 SNI 构造成任意字节；json_escape + encoding/json 必须保证不串字段。
func TestHostileSNIWithQuotesAndNewline(t *testing.T) {
	// 这是经过 HAProxy json_escape 之后的形态
	line := `{"ts":"22/Sep/2026:15:04:05.123","node":"n","ver":1,"ci":"1.2.3.4","cp":"1",` +
		`"fi":"5.6.7.8","fp":"443","sni":"evil\"\\n{\"ts\":\"fake\",\"be\":\"bk_other\"}",` +
		`"be":"bk_x","srv":"s1","tq":"0","tc":"1","tt":"2","bin":"3","tsc":"--","rc":"0","bq":"0"}`
	rec, err := Parse([]byte(line), nil)
	if err != nil {
		t.Fatalf("恶意 SNI 不应导致整行解析失败: %v", err)
	}
	if rec.BeName != "bk_x" {
		t.Fatalf("backend 字段被注入串改：%q", rec.BeName)
	}
	if !strings.Contains(rec.SNI, "evil") {
		t.Fatalf("SNI 应被完整保留为字符串值：%q", rec.SNI)
	}
}

func TestConnIDIsDerivedNotFaked(t *testing.T) {
	rec, _ := Parse([]byte(goodLine), nil)
	if rec.ConnID == "" {
		t.Fatal("合成连接 ID 不应为空")
	}
	if !strings.Contains(rec.ConnID, "203.0.113.9") {
		t.Fatalf("合成 ID 应包含客户端地址：%q", rec.ConnID)
	}
	// 必须明确标注它不是 HAProxy 原生 ID
	found := false
	for _, u := range rec.Unavailable {
		if u == "hid" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应标注 hid（HAProxy 原生连接 ID 不可用），实际 %v", rec.Unavailable)
	}
}

func TestParseTimeFormats(t *testing.T) {
	for _, s := range []string{"22/Sep/2026:15:04:05.123", "2026-09-22T15:04:05Z", "22/Sep/2026:15:04:05"} {
		if _, ok := parseTime(s); !ok {
			t.Fatalf("时间格式 %q 应能解析", s)
		}
	}
	if _, ok := parseTime("-"); ok {
		t.Fatal("`-` 不应被解析成时间")
	}
}

// ★★ 端到端契约测试：把渲染器真正生成的 log-format 里的表达式全部替换成
// HAProxy 最坏情况下的输出（裸 `-`），生成的行仍必须是合法 JSON。
//
// 这条测试直接守护 docs/04 的 T15：数字字段不加引号会让一行坏数据打穿整条解析链路。
func TestGeneratedLogFormatIsAlwaysValidJSON(t *testing.T) {
	for _, mode := range []string{"sni_tls", "tcp_port"} {
		lf, err := haproxy.BuildLogFormat(haproxy.LogFormatOptions{
			NodeID: "nd_ABC", Version: 42, Mode: mode,
		})
		if err != nil {
			t.Fatalf("BuildLogFormat 失败: %v", err)
		}
		// log-format 里是 \" 转义形式，这里还原成 HAProxy 实际会输出的样子。
		unquoted := strings.ReplaceAll(lf, `\"`, `"`)

		// 把每一个 %[...] / %xx 表达式替换成 `-`（HAProxy 的不可用标记）。
		unavailable := regexp.MustCompile(`%(\[[^\]]*\]|[A-Za-z][A-Za-z0-9]*)`).ReplaceAllString(unquoted, Unavailable)
		unavailable = strings.TrimSuffix(unavailable, `\n`)

		var m map[string]any
		if err := json.Unmarshal([]byte(unavailable), &m); err != nil {
			t.Fatalf("模式 %s：不可用值场景下生成的行不是合法 JSON：%v\n原始行：%s", mode, err, unavailable)
		}

		// 再换一批真实值，确认也能解析且类型符合预期
		real := regexp.MustCompile(`%(\[[^\]]*\]|[A-Za-z][A-Za-z0-9]*)`).ReplaceAllString(unquoted, "x")
		real = strings.TrimSuffix(real, `\n`)
		if err := json.Unmarshal([]byte(real), &m); err != nil {
			t.Fatalf("模式 %s：真实值场景下生成的行不是合法 JSON：%v\n原始行：%s", mode, err, real)
		}

		if mode == "tcp_port" {
			if _, ok := m["sni"]; ok {
				t.Fatal("TCP 端口转发模式的日志里不应有 sni 字段")
			}
		} else {
			if _, ok := m["sni"]; !ok {
				t.Fatal("SNI 模式的日志里必须有 sni 字段")
			}
		}
	}
}

func TestEmptyLine(t *testing.T) {
	rec, err := Parse([]byte("   \n"), nil)
	if err == nil || rec.ParseOK {
		t.Fatal("空行应被判为解析失败")
	}
}

func TestRecordZeroValueDistinction(t *testing.T) {
	// 明确断言：0 与 nil 是两种不同的事实
	zero := `{"ts":"22/Sep/2026:15:04:05.123","node":"n","ver":1,"ci":"1.2.3.4","cp":"1",` +
		`"fi":"5.6.7.8","fp":"443","sni":"a.example.com","be":"bk_x","srv":"s1",` +
		`"tq":"0","tc":"0","tt":"0","up":"0","down":"0","tsc":"--","rc":"0","bq":"0"}`
	rec, _ := Parse([]byte(zero), nil)
	if rec.BytesUp == nil || *rec.BytesUp != 0 {
		t.Fatal("真实的 0 字节必须保留为 0")
	}
	onlyHID(t, rec)
	// accept_date 精确解析（HAProxy 输出的是无时区的本地时间，按字面值解析）
	want := time.Date(2026, 9, 22, 15, 4, 5, 123000000, time.UTC)
	if !rec.AcceptTS.Equal(want) {
		t.Fatalf("时间解析错误：期望 %v，实际 %v", want, rec.AcceptTS)
	}
}
