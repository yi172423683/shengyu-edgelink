// Package logparse 把 HAProxy 写出来的一行 JSON 日志解析成结构化记录。
//
// 三条铁律（需求 §六）：
//  1. **不手写转义**：HAProxy 端用 json(utf8s) 转换器，平台端用 encoding/json；
//  2. **区分「0」与「不可用」**：HAProxy 对取不到的 sample 输出裸 `-`，解析后必须是
//     NULL/不可用，绝不能变成 0 —— 否则页面会把"没测到"显示成"零流量"；
//  3. **坏行不丢**：解析失败要保留原始行（parse_ok=0），否则诊断时看不到证据。
package logparse

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// haproxyDateLayout 对应 HAProxy 日志别名 `%T` 的输出格式（如 22/Sep/2026:15:04:05.123）。
//
// 注意：不要因为有"日期"两个字就把它和 sample fetch 混为一谈 —— `accept_date`
// **不是**合法的 sample fetch（`%[accept_date]` 会让 haproxy -c 直接报错）。
// `%T` 别名输出的就是 accept date，且是 GMT；渲染器统一用它，详见
// internal/haproxy/spec.go 中 ts 字段的说明与 docs/07 §1.2。
const haproxyDateLayout = "02/Jan/2006:15:04:05.000"

// Unavailable 是 HAProxy 对「该 sample 取不到值」的输出标记。
const Unavailable = "-"

// Resolver 把 HAProxy 对象名反解成平台对象 ID。
//
// 基础接口只按"当前配置"解析；**跨版本正确性**由下面的 VersionedResolver 提供。
// 之所以拆成两个接口而不是直接改签名：基础接口是"最小可用能力"，
// 而按版本快照解析需要访问 config_versions，不是所有调用场景都有（例如单测）。
type Resolver interface {
	// BusinessForBackend 返回 backend 名对应的 (businessID, routeID)。
	BusinessForBackend(beName string) (businessID, routeID string, ok bool)
	// OriginForBackend 返回 backend 名对应源站的 (host, port)。
	OriginForBackend(beName string) (host string, port int, ok bool)
}

// VersionedResolver 按**日志所属的配置版本**解析（评审 F11）。
//
// 为什么必须按版本解析：一条长连接可能在新配置发布之后才结束，
// 它的日志里 ver 是旧版本号、be 是旧 backend 名。若用"当前配置"反查，
// 这条日志会被归到**新**源站上 —— 而它实际连的是旧源站。排障时这会把结论指向错误的对象
// （"源站 B 有问题"，其实日志描述的是源站 A 的连接）。
//
// 实现方应当查 config_versions 里那一版**不可变**的快照，而不是当前表。
type VersionedResolver interface {
	Resolver
	// BusinessForBackendAt 按配置版本解析业务归属；版本快照缺失时返回 ok=false，
	// 调用方据此退回 BusinessForBackend，绝不猜。
	BusinessForBackendAt(cfgVersion int, beName string) (businessID, routeID string, ok bool)
	// OriginForBackendAt 按配置版本解析源站。
	OriginForBackendAt(cfgVersion int, beName string) (host string, port int, ok bool)
}

// Record 是落到 conn_log 分片表里的一行。
// 指针类型表示「可能不可用」：nil = 不可用，非 nil = 真实测到的值（含 0）。
type Record struct {
	AcceptTS      time.Time
	NodeID        string
	BusinessID    string
	RouteID       string
	ConfigVersion int
	ConnID        string

	ClientIP   string
	ClientPort *int
	EntryAddr  string
	EntryPort  *int
	SNI        string

	BeName     string
	SrvName    string
	OriginAddr string
	OriginPort *int
	// OriginReported 为 true 表示源站地址来自 HAProxy 自己在日志里报的目标地址，
	// 而不是平台按配置反查的结果。两者冲突时以**报的**为准：
	// 配置说的是"应该连哪里"，日志报的是"实际连到了哪里"。
	OriginReported bool

	QueueMS   *int64 // HAProxy TCP 日志的 Tw：等待连接的总时间（含排队），非纯排队
	ConnectMS *int64 // Tc
	SessionMS *int64 // Tt
	// BytesUp / BytesDown 的方向语义见 haproxy.Registry 的 up/down 两条说明
	//（评审 F11：这两个方向曾经标反）。
	BytesUp   *int64 // 别名 %U = bytes_uploaded = from client to server
	BytesDown *int64 // 别名 %B = bytes_read     = from server to client

	TermCode   string // HAProxy 原始终止状态码
	TermReason string // 原始错误串（若该版本可用）

	Retries  *int
	QueueMax *int // bq：队列峰值

	ParseOK     bool
	ParseErr    string
	RawLine     string
	Unavailable []string // 本行哪些字段被标记为不可用（供界面逐字段提示）
}

// wire 严格对应渲染器生成的 JSON 键。
//
// 注意：**除了 ver 之外全部是字符串**，因为 HAProxy 对取不到的 sample 输出裸 `-`，
// 数字位置出现 `-` 会让整行 JSON 非法（见 docs/04 T15）。
type wire struct {
	TS   string `json:"ts"`
	Node string `json:"node"`
	Ver  *int   `json:"ver"`

	CI  string `json:"ci"`
	CP  string `json:"cp"`
	FI  string `json:"fi"`
	FP  string `json:"fp"`
	SNI string `json:"sni"`
	BE  string `json:"be"`
	SRV string `json:"srv"`

	TQ   string `json:"tq"`
	TC   string `json:"tc"`
	TT   string `json:"tt"`
	UP   string `json:"up"`
	DOWN string `json:"down"`

	// 用指针：需要区分"这条日志的格式里根本没有这个字段"（旧格式）与
	// "有字段但取不到值"（值是多出来的 `-`）。前者不该报"不可用"——
	// 那会让每一条历史日志都挂上一个假的"字段不可用"，把真正的缺失淹没掉。
	OIP   *string `json:"oip"`
	OPORT *string `json:"oport"`

	TSC string `json:"tsc"`
	ERR string `json:"err"`
	RC  string `json:"rc"`
	BQ  string `json:"bq"`

	// 可选字段（实测通过后才开启）
	UID string `json:"uid"`
}

// Parse 解析一行。
//
// 返回的 error 表示这一行不可用；调用方（mgr 摄入）应当**仍然入库**这次调用的 Record
// （ParseOK=false + RawLine 有内容 + ParseErr 有原因），而不是丢掉。
func Parse(line []byte, res Resolver) (Record, error) {
	rec := Record{ParseOK: true}
	// 原始行留证据。截断由调用方按 logstore.raw_line_max_bytes 处理。
	rec.RawLine = string(line)

	body := stripSyslogHeader(line)
	if len(body) == 0 {
		rec.ParseOK = false
		rec.ParseErr = "空行"
		return rec, errors.New("空行")
	}
	// 防御：只处理 JSON 行。
	trimmed := strings.TrimSpace(string(body))
	if !strings.HasPrefix(trimmed, "{") {
		rec.ParseOK = false
		rec.ParseErr = "该版本/该配置下产出的不是 JSON 日志行"
		return rec, errors.New(rec.ParseErr)
	}

	var w wire
	dec := json.NewDecoder(strings.NewReader(trimmed))
	if err := dec.Decode(&w); err != nil {
		rec.ParseOK = false
		rec.ParseErr = "JSON 解析失败: " + err.Error()
		return rec, errors.New(rec.ParseErr)
	}

	var unavail []string
	mark := func(k string) { unavail = append(unavail, k) }

	// 时间：HAProxy 的 accept_date 不是 RFC3339，必须按它自己的格式解析。
	if ts, ok := parseTime(w.TS); ok {
		rec.AcceptTS = ts
	} else if w.TS == "" || w.TS == Unavailable {
		mark("ts")
	} else {
		rec.ParseOK = false
		rec.ParseErr = fmt.Sprintf("时间格式无法识别: %q", w.TS)
		mark("ts")
	}

	rec.NodeID = strOr(w.Node, "", &unavail, "node")
	if w.Ver != nil {
		rec.ConfigVersion = *w.Ver
	} else {
		mark("ver")
	}

	rec.ClientIP = strOr(w.CI, "", &unavail, "ci")
	rec.ClientPort = intOr(w.CP, &unavail, "cp")
	rec.EntryAddr = strOr(w.FI, "", &unavail, "fi")
	rec.EntryPort = intOr(w.FP, &unavail, "fp")
	rec.SNI = strOr(w.SNI, "", &unavail, "sni")
	rec.BeName = strOr(w.BE, "", &unavail, "be")
	rec.SrvName = strOr(w.SRV, "", &unavail, "srv")

	rec.QueueMS = asInt64(w.TQ, &unavail, "tq")
	rec.ConnectMS = asInt64(w.TC, &unavail, "tc")
	rec.SessionMS = asInt64(w.TT, &unavail, "tt")
	// 方向：%U = 客户端→源站（上行），%B = 源站→客户端（下行）。
	// 这两个字段名就是"方向"本身，界面上再也不会出现"到底哪个是哪个"的争论。
	rec.BytesUp = asInt64(w.UP, &unavail, "up")
	rec.BytesDown = asInt64(w.DOWN, &unavail, "down")

	// 终止状态码：`--` 是**合法值**（正常结束），只有单个 `-` 才代表不可用。
	rec.TermCode = rawOrEmpty(w.TSC, &unavail, "tsc")
	rec.TermReason = rawOrEmpty(w.ERR, &unavail, "err")
	rec.Retries = intOr(w.RC, &unavail, "rc")
	rec.QueueMax = intOr(w.BQ, &unavail, "bq")

	// ---- 富化：业务归属与源站 ----
	//
	// 顺序刻意如此（评审 F11）：
	//  1. 先用**HAProxy 实报**的目标地址（%si/%sp）—— 这是"实际连到哪里"的事实；
	//  2. 再用**该日志所属配置版本的不可变快照**反查业务/源站 —— 这是"当时配置写的是什么"；
	//  3. 快照缺失时才退回当前配置，并且不覆盖第 1 步的结果。
	//
	// 只用第 3 步（旧实现）的问题是：跨版本的长连接会被归到新源站上，
	// 排障时结论指向错误的对象。
	if res != nil && rec.BeName != "" {
		biz, rt, ok := resolveBusiness(res, rec.ConfigVersion, rec.BeName)
		if ok {
			rec.BusinessID, rec.RouteID = biz, rt
		}
		if host, port, ok := resolveOrigin(res, rec.ConfigVersion, rec.BeName); ok {
			rec.OriginAddr = host
			rec.OriginPort = &port
		}
	}
	// HAProxy 自己报的目标地址优先级最高：配置是"应该"，日志是"实际"。
	// 旧格式（没有 oip 键）时 w.OIP 为 nil —— 那时不覆盖、也不报"不可用"。
	if w.OIP != nil {
		switch {
		case *w.OIP == Unavailable:
			mark("oip")
		case *w.OIP != "":
			rec.OriginAddr = *w.OIP
			rec.OriginReported = true
			if w.OPORT != nil {
				if p := asInt64(*w.OPORT, &unavail, "oport"); p != nil {
					pi := int(*p)
					rec.OriginPort = &pi
				}
			}
		}
	}

	rec.ConnID = derivedConnID(rec, w.UID)
	if w.UID == Unavailable || w.UID == "" {
		// 用合成 ID 时，在不可用清单里说明它不是 HAProxy 原生 ID。
		unavail = append(unavail, "hid")
	}

	rec.Unavailable = dedup(unavail)
	return rec, nil
}

// resolveBusiness 解析 backend 名对应的业务。
//
// 有 VersionedResolver 且日志带了版本号时，**必须**按版本解析；
// 版本快照里查不到（例如那一版已被清理）才退回"当前配置"。
// 退回时业务归属仍然可信 —— backend 名 = "bk_" + handle(businessID)，
// 而 businessID 永不复用（见 store.nodeResolver 的说明）。
func resolveBusiness(res Resolver, cfgVersion int, beName string) (string, string, bool) {
	if vr, ok := res.(VersionedResolver); ok && cfgVersion > 0 {
		if biz, rt, ok := vr.BusinessForBackendAt(cfgVersion, beName); ok {
			return biz, rt, true
		}
	}
	return res.BusinessForBackend(beName)
}

// resolveOrigin 解析 backend 名对应的源站。
//
// 与业务归属不同，源站**必须**来自版本快照：源站地址会随配置变更而变，
// 用当前值去解释旧日志会把故障指向错误的源站（评审 F11）。
// 因此这里查不到快照时**不退回当前配置**，宁可留空让界面显示"不可用"。
func resolveOrigin(res Resolver, cfgVersion int, beName string) (string, int, bool) {
	if vr, ok := res.(VersionedResolver); ok && cfgVersion > 0 {
		if h, p, ok := vr.OriginForBackendAt(cfgVersion, beName); ok {
			return h, p, true
		}
		return "", 0, false
	}
	return res.OriginForBackend(beName)
}

// derivedConnID 生成连接关联标识。
//
// HAProxy 社区版没有原生稳定连接 ID，所以这里**明确是平台合成**的标识，
// 只用于把同一条日志的上下文串起来，不承担唯一键职责。
func derivedConnID(rec Record, hproxyID string) string {
	if hproxyID != "" && hproxyID != Unavailable {
		return hproxyID
	}
	cp := ""
	if rec.ClientPort != nil {
		cp = strconv.Itoa(*rec.ClientPort)
	}
	fp := ""
	if rec.EntryPort != nil {
		fp = strconv.Itoa(*rec.EntryPort)
	}
	ts := ""
	if !rec.AcceptTS.IsZero() {
		ts = strconv.FormatInt(rec.AcceptTS.UnixMilli(), 10)
	}
	return strings.Join([]string{rec.NodeID, ts, rec.ClientIP, cp, rec.EntryAddr, fp}, "|")
}

// stripSyslogHeader 容错：如果 HAProxy 用了 rfc3164/rfc5424 格式，先剥掉 <PRI> 与时间戳前缀。
// 我们配置的是 `format raw`，但运维现场被改过配置是常态，容错比严格更实用。
func stripSyslogHeader(line []byte) []byte {
	b := trimSpace(line)
	if len(b) > 0 && b[0] == '<' {
		if i := indexByte(b, '>'); i > 0 && i <= 4 {
			b = trimSpace(b[i+1:])
		}
	}
	return b
}

func trimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && (b[start] == ' ' || b[start] == '\t' || b[start] == '\r' || b[start] == '\n') {
		start++
	}
	end := len(b)
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\r' || b[end-1] == '\n') {
		end--
	}
	return b[start:end]
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func parseTime(s string) (time.Time, bool) {
	if s == "" || s == Unavailable {
		return time.Time{}, false
	}
	if t, err := time.Parse(haproxyDateLayout, s); err == nil {
		return t, true
	}
	// 容错：某些配置/版本会输出 RFC3339，或没有毫秒
	for _, l := range []string{time.RFC3339Nano, time.RFC3339, "02/Jan/2006:15:04:05"} {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// strOr 处理字符串字段：`-` => 空并标记不可用；其余原样返回（包括空串）。
func strOr(v, def string, unavail *[]string, key string) string {
	if v == Unavailable {
		*unavail = append(*unavail, key)
		return def
	}
	return v
}

// rawOrEmpty 与 strOr 相同，但语义强调"这是原始值，不做加工"。
// 用于终止状态码：`--` 是合法值，必须原样保留。
func rawOrEmpty(v string, unavail *[]string, key string) string {
	if v == Unavailable {
		*unavail = append(*unavail, key)
		return ""
	}
	return v
}

func asInt64(v string, unavail *[]string, key string) *int64 {
	if v == "" || v == Unavailable {
		*unavail = append(*unavail, key)
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		// 值存在但解析不了 —— 这是"数据异常"而不是"没法测"，两者都要如实反映。
		*unavail = append(*unavail, key)
		return nil
	}
	return &n
}

func intOr(v string, unavail *[]string, key string) *int {
	n := asInt64(v, unavail, key)
	if n == nil {
		return nil
	}
	i := int(*n)
	return &i
}

func dedup(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
