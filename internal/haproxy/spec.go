package haproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// 1. 指令白名单
//
// 需求 §二.8「不混用商业版特性」。这个白名单是**闭合集合**：渲染器只允许产出这里列出的
// 顶层关键字。任何模板被误改而引入未登记指令时，渲染测试会立刻失败 —— 这是机械保障，
// 而不是"大家记得别加商业指令"的口头约定。
// ---------------------------------------------------------------------------

// allowedSections 允许出现的大段。
var allowedSections = map[string]bool{
	"global":   true,
	"defaults": true,
	"frontend": true,
	"backend":  true,
}

// allowedDirectives 允许出现的指令（key = 大段名，value = 该段内允许的指令首关键字）。
var allowedDirectives = map[string]map[string]bool{
	"global": {
		"log": true, "maxconn": true, "nbthread": true,
		// 注意：这里**没有** expose-fd。
		// 手册 5.1 明确它"only usable with the stats socket"，不是 global 关键字；
		// 而且在 master-worker 模式下已不需要（评审 F01）。把它从白名单里去掉之后，
		// 任何"顺手加一行 expose-fd listeners 到 global"的改动都会被自检拦下。
		"master-worker": true,
		"stats":         true, "hard-stop-after": true,
		// user / group：让 HAProxy 的 worker 进程降权到指定用户（不是"权限交给 systemd"
		// 就不管了）。手册 3.11 原文：
		//     user <user name>
		//       Similar to "uid" but uses the UID of user name <user name> from /etc/passwd.
		//     group <group name>
		//       Similar to "gid" but uses the GID of group name <group name> from /etc/group.
		// 之前这里被显式禁掉（"权限由 systemd 管，配置文件里不再降权"），
		// 结果是：如果服务以 root 启动，**所有进程都会长期以 root 跑**。
		// 现在两层都做：unit 侧不用 root（见 deploy/shengyu-edgelink-haproxy.service），
		// 配置侧再写一次 —— 万一有人手工以 root 起 haproxy，worker 仍会降权。
		"user": true, "group": true,
		"chroot": false, // 明确标 false：不 chroot，避免与 unix socket 路径冲突（保留以便将来评审）
	},
	"defaults": {
		"mode": true, "log": true, "option": true,
		"timeout": true, "retries": true,
		"maxconn": true, "default-server": true,
	},
	"frontend": {
		"bind": true, "mode": true, "log": true, "log-format": true,
		"tcp-request": true, "use_backend": true, "default_backend": true,
		"timeout": true, "maxconn": true, "option": true,
		"description": true, "default-server": true,
		"#": true, // 注释
	},
	"backend": {
		"mode": true, "log": true, "server": true, "timeout": true,
		"option": true, "balance": true, "maxconn": true, "description": true,
		// http-check：HTTP 健康探测的请求与期望（手册 4.2 标注 backend 可用）。
		// 它只影响探测，不影响业务传输。
		"http-check": true,
		// 注意：这里**没有** maxqueue。它不是 backend 关键字，而是 server 行参数
		//（手册 5.2 的 "Server and default-server options"）。评审 F09 指出的正是
		// "把 per-server 限制写到了 backend 层"，所以去掉它让同类错误再也过不了自检。
		"default-server": true,
		"#":              true,
	},
}

// forbiddenSubstrings 是商业版/不稳定特性的硬禁用词。即使误加也会被拦住。
var forbiddenSubstrings = []string{
	"hapee", "fusion", "dataplane", "data-plane", "deviceatlas",
	"quic", "http3", "http/3", "ech_args", "easy-ssl", "stick-table",
	"peers", "lua-load", "spoe", "ring", "modsecurity", "waf-mod",
}

// CheckDirectives 扫描渲染结果，确认没有越界指令。返回所有越界行号与内容。
func CheckDirectives(cfg []byte) []string {
	var bad []string
	section := ""
	for i, raw := range strings.Split(string(cfg), "\n") {
		lineNo := i + 1
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		head := fields[0]
		indent := raw != "" && (raw[0] == ' ' || raw[0] == '\t')
		if !indent {
			if !allowedSections[head] {
				bad = append(bad, fmt.Sprintf("%d: 未知大段 %q", lineNo, trimmed))
				continue
			}
			section = head
			continue
		}
		allowed, ok := allowedDirectives[section]
		if !ok {
			bad = append(bad, fmt.Sprintf("%d: 大段 %q 内的指令 %q 未登记", lineNo, section, head))
			continue
		}
		permitted, exists := allowed[head]
		if !exists {
			bad = append(bad, fmt.Sprintf("%d: 大段 %q 内不允许指令 %q", lineNo, section, head))
			continue
		}
		if !permitted {
			bad = append(bad, fmt.Sprintf("%d: 指令 %q 在 %q 内被显式禁用", lineNo, head, section))
		}
		// server 行要逐参数校验：只看首个词会漏掉 `check timeout 3000ms` 这类
		// 「行首合法、参数非法」的写法（见 checkServerLine 的说明）。
		if head == "server" {
			bad = append(bad, checkServerLine(lineNo, trimmed)...)
		}
		lower := strings.ToLower(scanRegion(trimmed))
		for _, f := range forbiddenSubstrings {
			if strings.Contains(lower, f) {
				bad = append(bad, fmt.Sprintf("%d: 命中禁用特性 %q", lineNo, f))
			}
		}
	}
	return bad
}

// scanRegion 只保留「指令及其直接参数」部分，去掉 ACL 表达式（{...}）与引号字符串。
//
// 为什么要这么做：域名、SNI、路径都可能**合法地**包含像 "ring" 这样的子串
// （例如 ring.example.com），对整行做子串匹配会产生误报，进而让正常业务发不出去。
// 禁用特性（log ring@x、option lua-load、stick-table…）一定出现在引号/花括号之前，
// 所以只扫这一段既够用又不会误伤。
func scanRegion(line string) string {
	cut := len(line)
	for _, c := range []string{"{", "\""} {
		if i := strings.Index(line, c); i >= 0 && i < cut {
			cut = i
		}
	}
	return line[:cut]
}

// ---------------------------------------------------------------------------
// 1b. server 行参数白名单
//
// 为什么单独校验 server 行：检查越界指令时只看了行的**第一个**词，
// 于是 `server s1 1.2.3.4:443 check timeout 3000ms` 这种行能整行通过 ——
// 而 `timeout` 根本不是 server 行的参数（手册 5.2 的 server 参数表里没有它；
// 健康检查超时要用 backend 的 `timeout check`，见评审 F09）。
// 这类错误 100% 会以 `haproxy -c` 报错收场，但错误信息会指向整个文件的行号，
// 排查成本远高于在这里拦住。所以对 server 行做**逐参数**校验。
// ---------------------------------------------------------------------------

// serverParamsNoValue server 行上「自身即完整参数」的关键字。
var serverParamsNoValue = map[string]bool{
	"check": true, "check-ssl": true, "no-check-ssl": true,
	"disabled": true, "backup": true,
	"send-proxy": true, "send-proxy-v2": true, "send-proxy-v2-ssl": true,
	"ssl": true, "tfo": true, "maintenance": true, "non-stick": true,
}

// serverParamsWithValue server 行上「后面跟一个值」的关键字。
var serverParamsWithValue = map[string]bool{
	"inter": true, "fastinter": true, "downinter": true, "rise": true, "fall": true,
	"check-sni": true, "check-alpn": true, "check-port": true, "check-proto": true,
	"verify": true, "ca-file": true, "crt": true, "alpn": true, "sni": true,
	"maxconn": true, "minconn": true, "maxqueue": true, "weight": true,
	"port": true, "addr": true, "proto": true, "slowstart": true, "on-error": true,
	"error-limit": true, "observe": true, "id": true, "pool-low-conn": true,
	"pool-max-conn": true, "pool-purge-delay": true,
}

// checkServerLine 校验一行 server 的参数是否都在白名单里，返回问题描述。
func checkServerLine(lineNo int, trimmed string) []string {
	fields := strings.Fields(trimmed)
	// server <name> <addr>[:port] [params...]
	if len(fields) < 3 {
		return []string{fmt.Sprintf("%d: server 行至少需要 server <名称> <地址>", lineNo)}
	}
	var bad []string
	expectValue := false
	for i, f := range fields {
		if i == 0 || i == 1 || i == 2 {
			continue // server / 名称 / 地址
		}
		if expectValue {
			expectValue = false
			continue
		}
		if serverParamsNoValue[f] {
			continue
		}
		if serverParamsWithValue[f] {
			expectValue = true
			continue
		}
		bad = append(bad, fmt.Sprintf(
			"%d: server 行参数 %q 不在白名单内（新增参数必须先在 spec.go 登记并引用手册依据）", lineNo, f))
	}
	return bad
}

// ---------------------------------------------------------------------------
// 2. 日志字段注册表
//
// 需求 §六「以固定版本文档为准验证字段可用性；没有的数据标记为不可用，不伪造」。
// 每个字段带：
//   - Expr         : 在 HAProxy log-format 里的表达式
//   - Key          : 输出 JSON 的键名
//   - DefaultOn    : 是否默认开启（未实测前保守关闭）
//   - Status       : 可用性状态，由 scripts/verify-log-fields.sh 的产物覆盖
//   - Note         : 不确定性说明（写进界面与交付文档）
// ---------------------------------------------------------------------------

// FieldStatus 字段可用性。
type FieldStatus string

const (
	// StatusVerified 已在真实 HAProxy 上实测确认。
	StatusVerified FieldStatus = "verified"
	// StatusUnverified 语法合法但未实测（默认关闭，实测后开启）。
	StatusUnverified FieldStatus = "unverified"
	// StatusUnavailable 该版本取不到，界面显示「不可用」。
	StatusUnavailable FieldStatus = "unavailable"
	// StatusDerived 不由 HAProxy 提供，由平台反查配置得出。
	StatusDerived FieldStatus = "derived"
)

// JSONConverter 生成合法 JSON 字符串所用的转换器。
//
// 依据 HAProxy 2.8 配置手册（"7.3.1. Converters"）：
//
//	json([<input-code>])
//	    Escapes the input string and produces an ASCII output string ready to
//	    use as a JSON string. ... It can be "ascii", "utf8", "utf8s", "utf8p"
//	    or "utf8ps".
//	    - "utf8s" : never fails, but removes characters corresponding to errors;
//
// 两点必须写清楚（上一版这里是错的，评审 F01）：
//
//  1. 2.8 里**没有** `json_escape` 这个转换器。写 `%[x,json_escape]` 会让 HAProxy
//     在 `haproxy -c` 阶段直接报"unknown converter"，整个配置发不出去。
//  2. 用 `json(utf8s)` 而不是 `json(utf8)`：SNI 是**客户端可控内容**，
//     构造一个非法 UTF-8 序列就能让 `json(utf8)` 返回"取不到值"（输出 `-`），
//     于是日志里 SNI 永远为空 —— 攻击者可以用这种方式让日志失去追溯价值。
//     "utf8s" 的语义是"永不失败，只丢掉非法字符"，正是日志该有的行为。
const JSONConverter = "json(utf8s)"

// Field 日志字段定义。
//
// ⚠️ HAProxy 的两种取值语法（写错了 `haproxy -c` 会直接报错，但更常见的是"语法通过、
// 语义悄悄变了"，所以这里用字段显式区分，不允许调用方自己拼字符串）：
//
//	① 日志别名（log tag/alias）：如 %ci / %cp / %b / %s / %Tw / %U / %B / %ts / %rc
//	   —— 手册 8.2.6 的别名表里每一项都写明"不能加转换器"，所以只有在
//	   "值天然安全"（数字、IP、平台自有对象名、HAProxy 自己的日期串）时才用。
//	② sample 表达式：如 var(txn.sni) / fc_err_str
//	   —— 必须写成 %[<expr>] 包裹，转换器写在括号**里面**：%[var(txn.sni),json(utf8s)]
//
// 历史错误（评审 F01）有两处，都在这里被结构性挡掉：
//
//	✗ 把 `,json_escape` 拼在 `%[accept_date]` 外面 —— 转换器位置错、名字也不存在；
//	✗ 用了 `accept_date` 这个"sample 表达式" —— 它不是 sample fetch。
//	  手册里 accept_date 只出现在**日志格式字段说明**（8.2.2/8.2.3 的字段 3），
//	  对应的别名是 `%t`（本地时间，含毫秒）与 `%T`（GMT）。sample fetch 一节里
//	  日期类只有 `date`/`date_us`，而那是"当前时间"，不是会话建立时间。
//	  所以这里改用别名 `%T`：语义是 accept date，且是 GMT —— 与我们按 UTC 解析
//	  入库的做法一致，跨时区的远程节点不会产生时间漂移。
type Field struct {
	Key    string `json:"key"`
	Header string `json:"header"` // 中文表头
	// Expr 是日志别名（含 %）或裸 sample 表达式（不含 %[]）。
	Expr string `json:"expr"`
	// Sample 为 true 表示 Expr 是 sample 表达式，需要 %[...] 包裹。
	Sample bool `json:"sample"`
	// Escape 为 true 表示套 JSONConverter（要求 Sample=true）。
	Escape bool `json:"escape"`
	// Literal 非空表示这是渲染期字面量（node / ver），与 HAProxy 无关。
	Literal string `json:"literal"`
	// Numeric 是给解析器与表结构用的类型提示：**不影响日志格式的写法**
	//（所有 HAProxy 提供的值都带引号，见 BuildLogFormat 的说明）。
	Numeric   bool        `json:"numeric"`
	DefaultOn bool        `json:"default_on"`
	Status    FieldStatus `json:"status"`
	Note      string      `json:"note"`
}

// Registry 默认字段集。顺序即日志里 JSON 键的顺序（便于肉眼比对原始行）。
//
// 每一行的来源都在 HAProxy 2.8 手册 8.2.6 的别名表中可查（别名 = 可直接写在
// log-format 里、且**不能加转换器**），或 7.3 的 sample fetch（需 %[...] 包裹）。
var Registry = []Field{
	{Key: "ts", Header: "会话建立时间", Expr: "%T", DefaultOn: true, Status: StatusUnverified,
		Note: "accept date（会话被 HAProxy 接收的时刻），GMT。TCP 日志在会话结束时才产生，" +
			"所以平台另外单独记录落盘时间 end_ts —— 分片与查询都按 end_ts，两个时间不可混用。" +
			"用 %T（GMT）而不是 %t（本地时间），是为了跨时区的远程节点不产生时间漂移。"},
	{Key: "node", Header: "节点 ID", Literal: "node", DefaultOn: true, Status: StatusVerified,
		Note: "渲染期字面量，不依赖 HAProxy 能力。"},
	{Key: "ver", Header: "配置版本", Literal: "ver", Numeric: true, DefaultOn: true, Status: StatusVerified,
		Note: "渲染期字面量。平滑 reload 期间新旧 worker 各写自己的版本，因此这个值精确反映「服务该连接的 worker 用的是哪版配置」——" +
			"日志里的 business/源站归属就按这个版本查不可变快照，而不是按当前配置。"},
	{Key: "cid", Header: "连接关联 ID", DefaultOn: false, Status: StatusDerived,
		Note: "HAProxy 社区版没有原生稳定连接 ID。平台用 (节点, accept_date, 客户端 IP:端口, 入口 IP:端口) 合成关联标识，" +
			"仅用于把同一条日志的上下文串起来，不承担唯一键职责。"},
	// 以下用日志别名：值天然安全（IP、端口、平台自有对象名、数字、HAProxy 的日期串）。
	{Key: "ci", Header: "客户端 IP", Expr: "%ci", DefaultOn: true, Status: StatusVerified},
	{Key: "cp", Header: "客户端端口", Expr: "%cp", Numeric: true, DefaultOn: true, Status: StatusVerified},
	{Key: "fi", Header: "入口地址", Expr: "%fi", DefaultOn: true, Status: StatusUnverified,
		Note: "别名 %fi = frontend_ip（accepting address），即客户端所连的入口地址。"},
	{Key: "fp", Header: "入口端口", Expr: "%fp", Numeric: true, DefaultOn: true, Status: StatusUnverified},
	// 以下用 sample 表达式：需要 %[...] 包裹，且要转义。
	{Key: "sni", Header: "SNI", Expr: "var(txn.sni)", Sample: true, Escape: true, DefaultOn: true, Status: StatusUnverified,
		Note: "★ 客户端可控内容。握手期由 req.ssl_sni 写入 txn 变量，会话生命周期内保持可读；" +
			"必须经 " + JSONConverter + " 转义，否则可被构造成日志注入。"},
	{Key: "be", Header: "backend", Expr: "%b", DefaultOn: true, Status: StatusVerified,
		Note: "值为 bk_<handle>（平台自己生成的对象名，字符集受控），平台据此反查业务 ID。"},
	{Key: "srv", Header: "server", Expr: "%s", DefaultOn: true, Status: StatusVerified},
	{Key: "tq", Header: "等待时间(ms)", Expr: "%Tw", Numeric: true, DefaultOn: true, Status: StatusUnverified,
		Note: "TCP 日志格式的 Tw 是「等待连接的总时间」（含排队），不是纯排队时间，界面表头如实写「等待」。"},
	{Key: "tc", Header: "源站连接耗时(ms)", Expr: "%Tc", Numeric: true, DefaultOn: true, Status: StatusUnverified},
	{Key: "tt", Header: "会话时长(ms)", Expr: "%Tt", Numeric: true, DefaultOn: true, Status: StatusUnverified},
	// ⚠️ 方向语义曾经标反（评审 F11）。手册 8.2.6 别名表原文：
	//     | %U | bytes_uploaded (from client to server) | numeric |
	//     | %B | bytes_read     (from server to client) | numeric |
	// 并把这两条写进表头与字段名，杜绝"上行下行看反"导致的误判
	//（最典型的误判是「上行流量很小 ⇒ 客户端没在传数据」，而实际那正是下行）。
	{Key: "up", Header: "客户端→中转字节(上行)", Expr: "%U", Numeric: true, DefaultOn: true, Status: StatusUnverified,
		Note: "见上：%U = bytes_uploaded = from client to server。"},
	{Key: "down", Header: "中转→客户端字节(下行)", Expr: "%B", Numeric: true, DefaultOn: true, Status: StatusUnverified,
		Note: "见上：%B = bytes_read = from server to client。若实测与手册不符，本列必须改为「不可用」，不允许猜。"},
	{Key: "oip", Header: "源站地址(HAProxy 实报)", Expr: "%si", DefaultOn: true, Status: StatusUnverified,
		Note: "别名 %si = server_IP (target address)：HAProxy **实际连接**的源站地址。" +
			"它优先于平台按配置反查的结果 —— 反查只能说明「配置里写的是什么」，" +
			"而这个字段说明「这条连接真的连到了哪里」。"},
	{Key: "oport", Header: "源站端口(HAProxy 实报)", Expr: "%sp", Numeric: true, DefaultOn: true, Status: StatusUnverified,
		Note: "别名 %sp = server_port (target address)。"},
	{Key: "tsc", Header: "HAProxy 终止状态", Expr: "%ts", DefaultOn: true, Status: StatusUnverified,
		Note: "别名 %ts = termination_state（`--` 表示正常结束，是合法值）。" +
			"若实测为其它含义或不可用，则本列显示「不可用」。"},
	{Key: "rc", Header: "重试次数", Expr: "%rc", Numeric: true, DefaultOn: true, Status: StatusUnverified},
	{Key: "bq", Header: "队列峰值", Expr: "%bq", Numeric: true, DefaultOn: true, Status: StatusUnverified,
		Note: "后端队列峰值，不是平均值。"},

	// ---- 以下默认关闭：语法合法但未实测，或语义有歧义，实测通过后再开启 ----
	{Key: "err", Header: "原始错误串", Expr: "fc_err_str", Sample: true, Escape: true, DefaultOn: false, Status: StatusUnverified,
		Note: "客户端可控/系统原文，必须转义。诊断区分「源站连不上」与「客户端异常断开」的关键，开启前必须实测。"},
	{Key: "uid", Header: "HAProxy 连接 ID", Expr: "unique_id", Sample: true, Escape: true, DefaultOn: false, Status: StatusUnverified,
		Note: "别名 %ID = unique-id（string）。若实测该版本确实唯一且可用，可替代平台合成的 cid。"},
}

// FieldByKey 按 key 取字段。
func FieldByKey(k string) (Field, bool) {
	for _, f := range Registry {
		if f.Key == k {
			return f, true
		}
	}
	return Field{}, false
}

// AvailableFields 返回"当前应该写进日志"的字段集合：
// 默认开启 + 非 unavailable + 非 derived。
func AvailableFields() []Field {
	var out []Field
	for _, f := range Registry {
		if !f.DefaultOn {
			continue
		}
		if f.Status == StatusUnavailable || f.Status == StatusDerived {
			continue
		}
		out = append(out, f)
	}
	return out
}

// ApplyAvailability 用实测产物覆盖字段状态（docs/04 §6 的 field-availability.json）。
func ApplyAvailability(raw []byte) error {
	var doc struct {
		Fields map[string]struct {
			Status FieldStatus `json:"status"`
			Note   string      `json:"notes"`
			Enable bool        `json:"enable"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("解析字段可用性文件失败: %w", err)
	}
	for i := range Registry {
		if m, ok := doc.Fields[Registry[i].Key]; ok {
			Registry[i].Status = m.Status
			if m.Note != "" {
				Registry[i].Note = m.Note
			}
			if m.Enable {
				Registry[i].DefaultOn = true
			}
			if m.Status == StatusUnavailable {
				Registry[i].DefaultOn = false
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 3. log-format 生成
// ---------------------------------------------------------------------------

// LogFormatOptions 生成 log-format 所需的一切。
type LogFormatOptions struct {
	NodeID  string
	Version int
	// Mode 用于决定是否包含 SNI 字段（TCP 端口转发没有 SNI 概念）。
	Mode Mode
}

// Mode 是本包内部对 model.Mode 的别名，避免 spec.go 依赖 model 包。
type Mode = string

// BuildLogFormat 生成 HAProxy 的 log-format 字符串（含 JSON 转义，含结尾 \n）。
//
// 关键点：
//   - 所有 HAProxy 提供的字段值都是**带引号的字符串**，并统一经过 json_escape 转换器，
//     **转义交给 HAProxy**；
//   - 平台侧用 encoding/json 反序列化，**不手写转义**；
//   - 字面量里的 `#` 必须转义为 `\#`（HAProxy 的注释起始符），`"` 转义为 `\"`。
//
// ⚠️ 为什么连数字字段也要加引号（踩过的坑）：
//
//	HAProxy 对一个取不到值的 sample 会输出裸的 `-`。如果数字字段不加引号，日志行会变成
//	`{"tq":-}` —— **这是非法 JSON，整个解析器会被一行坏数据打穿**。
//	把数字也放在引号里，`"-"` 是合法字符串，解析器再把它翻译成「不可用（NULL）」。
//	类型语义由 Field.Numeric 表达（供解析器与表结构使用），而不是靠 JSON 的类型。
func BuildLogFormat(opt LogFormatOptions) (string, error) {
	var sb strings.Builder
	sb.WriteString(`{`)
	first := true
	for _, f := range AvailableFields() {
		// TCP 端口转发模式没有 SNI，强行写会得到一堆不可用值。
		if f.Key == "sni" && opt.Mode == "tcp_port" {
			continue
		}
		if !first {
			sb.WriteString(",")
		}
		first = false

		sb.WriteString(`\"`)
		sb.WriteString(f.Key)
		sb.WriteString(`\":`)

		expr, err := fieldExpr(f, opt)
		if err != nil {
			return "", err
		}
		// 只有渲染期自己生成的版本号是真正的 JSON 数字（它永远存在，不会变成 '-'）。
		if f.Literal == "ver" {
			sb.WriteString(expr)
			continue
		}
		sb.WriteString(`\"`)
		sb.WriteString(expr)
		sb.WriteString(`\"`)
	}
	sb.WriteString(`}`)
	return sb.String(), nil
}

func fieldExpr(f Field, opt LogFormatOptions) (string, error) {
	switch f.Literal {
	case "node":
		return escapeLiteral(opt.NodeID), nil
	case "ver":
		return fmt.Sprintf("%d", opt.Version), nil
	}
	if f.Expr == "" {
		return "", errors.New("字段 " + f.Key + " 既无 Expr 也无 Literal")
	}
	if !f.Sample {
		// 日志别名：原样使用，不能套转换器（手册 8.2.6：别名不能附加转换器，
		// 套了就是 `haproxy -c` 报错的语法问题）。
		if f.Escape {
			return "", fmt.Errorf("字段 %s 是日志别名，不能套转换器 —— 请改用等价的 sample 表达式", f.Key)
		}
		return f.Expr, nil
	}
	// sample 表达式：转换器必须写在括号**里面**。
	if f.Escape {
		return "%[" + f.Expr + "," + JSONConverter + "]", nil
	}
	return "%[" + f.Expr + "]", nil
}

// escapeLiteral 转义渲染期字面量里的 HAProxy 元字符。
func escapeLiteral(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, `#`, `\#`)
	return s
}

// FieldsForUI 返回界面要展示的字段清单（含「不可用」的），供前端渲染日志表头与说明。
func FieldsForUI() []Field {
	return append([]Field(nil), Registry...)
}
