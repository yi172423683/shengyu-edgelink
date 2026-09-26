# 03 · 日志采集与实时指标设计（重点实现）

> 需求 §六 / §七 / §八 的落地设计。三条硬约束贯穿全篇：
> **① 日志链路任何环节故障都不得阻塞转发；② 没有的数据标「不可用」，绝不伪造；③ 实时在线连接不依赖会话结束日志。**

---

## 1. 端到端链路

```
┌─ 节点 ─────────────────────────────────────────────────────────────────────────┐
│                                                                               │
│  HAProxy 2.8 ──unix dgram──▶ /run/shengyu-edgelink/log.sock                            │
│  (log-format = JSON)                ▲                                         │
│                                     │ 由 systemd socket 单元持有 inode          │
│                                     │ → agent 重启也不换 inode（关键）          │
│                              ┌──────┴──────────┐                              │
│                              │ relay-agentd    │                              │
│                              │  收包 goroutine │ ← 只做 append，永不阻塞网络   │
│                              └──────┬──────────┘                              │
│                                     ▼                                         │
│                       /var/lib/shengyu-edgelink/spool/conn/seg-*.ndjson                │
│                       （有界：默认 1 GiB / 8 段滚动，满了丢最旧并计数）          │
│                                     │                                         │
│                              ┌──────┴──────────┐                              │
│                              │ 发送器（独立）   │ 读 spool → 批次 → mTLS POST  │
│                              └──────┬──────────┘  mgr 不可用 → 原地退避重试     │
└─────────────────────────────────────┼─────────────────────────────────────────┘
                                      │ POST /agent/v1/logs  (mTLS, NDJSON+gzip)
                                      ▼
┌─ 管理面 ──────────────────────────────────────────────────────────────────────┐
│  relay-mgrd                                                                    │
│   摄入 → 解析(encoding/json) → 富化(be_name→业务/路由/源站) → 批量事务写分片    │
│        → 缺口检测(seq) → 丢日志事件 → 分片滚动/压缩/配额清理                    │
│                                                                               │
│   页面查询 ◀── logstore（只打开时间范围内的分片，分页 + 时间范围硬限制）          │
└───────────────────────────────────────────────────────────────────────────────┘
```

---

## 2. HAProxy 侧配置

### 2.1 日志目标

```
global
    # 社区版特性，不使用企业版
    log /run/shengyu-edgelink/log.sock format raw local0
    maxconn 20000
    master-worker                 # -Ws 平滑 reload 基础
    expose-fd listeners           # 允许监听 socket 在 master/worker 间传递
    stats socket /run/shengyu-edgelink/haproxy.sock mode 660 level operator expose-fd listeners
    stats timeout 10s
    nbthread 4
    hard-stop-after 30m           # 优雅 reload 时老 worker 的兜底退出时间
    # ...
```

| 项 | 取值 | 理由 / 陷阱 |
|---|---|---|
| 日志目标 | unix **datagram** socket | 不引入 rsyslog 依赖；比 UDP 环回更可信 |
| `format raw` | 不加 syslog 头 | 解析更简单；**但解析器仍要能容忍 rfc3164 头**（防御性，见 §5.1） |
| socket inode 归属 | **systemd socket 单元** `shengyu-log.socket` | 若由 agent 自己 bind，agent 一重启 inode 就变，HAProxy 缓存的连接会指向失效 inode → **静默丢日志** |
| socket 权限 | `SocketMode=0660 SocketGroup=shengyu` | `shengyu`(agent) 与 `shengyu-edgelink-haproxy` 同组，收/发都对 |
| socket 缓冲 | `SocketReceiveBuffer=67108864` | agent 短暂重启时的缓冲，配合 SO_RXQ_OVFL 计数 |
| 启动顺序 | haproxy unit `Requires=shengyu-log.socket shengyu-agentd.service` `After=...` | HAProxy 启动时 socket 必须已存在，否则该 log target 被禁用且只在启动日志里抱怨一句 |

### 2.2 日志格式：**JSON，转义交给 HAProxy 的转换器**

需求 §六「若使用 JSON，必须正确处理字符串转义」——我们**选择 JSON，并把转义交给 HAProxy 的 `json()` 转换器 + Go 的 `encoding/json`**，全程不手写转义逻辑。

**唯一的转义写法是 `json(utf8s)`，且必须写在 `%[...]` 的方括号里面**：

```
%[var(txn.sni),json(utf8s)]     ✅
%[var(txn.sni)] ,json_escape    ❌ 转换器写在外面 = 语法错误
%[var(txn.sni),json_escape]     ❌ 2.8 里没有 json_escape 这个转换器，haproxy -c 直接报 unknown converter
```

> 为什么是 `utf8s` 而不是 `utf8`：SNI 是**客户端可控内容**，构造一个非法 UTF-8 序列就能让
> `json(utf8)` 返回"取不到值"（输出 `-`），于是日志里 SNI 永远为空 —— 攻击者用这种方式就能
> 让日志失去追溯价值。`utf8s` 的语义是"永不失败，只丢掉非法字符"，正是日志该有的行为。
> 详见 docs/07 §1.3（附 2.8 手册原文）。

⚠️ **有两种取值语法，不能混用**：

| 类型 | 写法 | 能否加转换器 | 例子 |
|---|---|---|---|
| 日志别名（log tag） | `%xx`，裸写 | **不能**（手册 8.2.6 明确禁止） | `%ci` `%cp` `%fi` `%fp` `%b` `%s` `%Tw` `%Tc` `%Tt` `%U` `%B` `%si` `%sp` `%ts` `%rc` `%bq` `%T` |
| sample 表达式 | `%[expr]`，转换器在括号内 | 可以 | `%[var(txn.sni),json(utf8s)]` |

一个反复出现的错误是把"字段名"当成 sample fetch 用，例如 `accept_date`：
它在手册里只出现在**日志字段说明**（§8.2.2/§8.2.3 的字段 3），对应的别名是 `%t`（本地时间）
与 `%T`（GMT）。**我们统一用 `%T`** —— 语义就是 accept date，且是 GMT，与日志解析按 UTC
入库的做法一致，跨时区的远程节点不会产生时间漂移。

模式 A（SNI 透传）frontend 的 `log-format` 渲染结果（`node_x` / `7` 是渲染期字面量，
**每版配置写死自己的版本号**）：

```
log-format "{\"ts\":\"%T\",\"node\":\"node_x\",\"ver\":7,\"ci\":\"%ci\",\"cp\":\"%cp\",\"fi\":\"%fi\",\"fp\":\"%fp\",\"sni\":\"%[var(txn.sni),json(utf8s)]\",\"be\":\"%b\",\"srv\":\"%s\",\"tq\":\"%Tw\",\"tc\":\"%Tc\",\"tt\":\"%Tt\",\"up\":\"%U\",\"down\":\"%B\",\"oip\":\"%si\",\"oport\":\"%sp\",\"tsc\":\"%ts\",\"rc\":\"%rc\",\"bq\":\"%bq\"}\n"
```

模式 B（TCP 端口映射）与上相同，只是**没有 `sni`**（TCP 端口转发里客户端不发 SNI，
强行渲染只会得到一列永远为 `-` 的字段）。

> 上面这行是 `haproxy.BuildLogFormat()` 的实际输出形态，**不要手工改写文档里的这行，
> 也不要在别处手抄一份** —— 真实来源是 `internal/haproxy/spec.go` 的 `Registry`。
> 字段增删只改注册表，文档这里跟着失效时要以代码为准。

**字段可用性处理规则（贯穿解析器、UI、导出）**

| HAProxy 输出 | 含义 | 平台处理 |
|---|---|---|
| `-` | 该 sample 取不到值（HAProxy 用 `-` 表示 unavailable） | 解析为 `unavailable`，UI 显示「不可用」，导出留空 |
| 缺 key | 该版本不支持该 fetch | 同上 |
| `""` | 真的取到了空值 | 空字符串 |
| 数值字段为 `-` | 计数不可用 | 存 NULL，不写 0 |

> ⚠️ 这是「不伪造」的落地方式：**`0` 和「不可用」在数据库里是两种不同的值**（`0` vs `NULL`），在 UI 上也是两种不同的展示。

渲染器不写死上面这串格式，而是由 **`internal/haproxy/spec.go` 的字段注册表 `Registry`**
程序化生成（`BuildLogFormat`）；注册表里每个字段带 `Status`（`verified`/`unverified`/`unavailable`）
与 `Expr`，`scripts/verify-log-fields.sh` 的实测结果通过 `ApplyAvailability` 覆盖 `Status`。
（文档历史上曾写作 `internal/haproxy/fieldreg`，实际路径已改为 `internal/haproxy/spec.go`。）

### 2.3 SNI 必须在握手阶段捕获（需求 §六硬要求）

```
frontend fe_sni_443
    mode tcp
    bind *:443
    tcp-request inspect-delay 5s
    # ★ 在 ClientHello 可用时立刻把 SNI 写进 txn 变量；会话生命周期内一直可读
    tcp-request content set-var(txn.sni) req.ssl_sni    # 正式名；req_ssl_sni 已 deprecated
    tcp-request content set-var(txn.cid) ...   # 连接关联 ID（见 §2.4）
    tcp-request content reject unless { req_ssl_hello_type 1 }
    tcp-request content reject unless { var(txn.sni) -m str -i -f /etc/shengyu-edgelink/haproxy/current/sni_allow.lst }
    use_backend bk_b00001 if { var(txn.sni) -m str -i a.example.com }
    ...
```

**为什么不直接用 `%[ssl_fc_sni]`**：需求明确要求「不能假设连接结束时还能重新读取 ClientHello」。`ssl_fc_sni` 读的是 SSL 层缓存的 SNI，在会话结束后是否仍可读取决于实现细节；而 `txn.*` 变量是 HAProxy 显式提供、生命周期到会话结束写日志那一刻的载体。**用 txn 变量是唯一在语义上有保证的做法。**

**验证方式**（`scripts/verify-log-fields.sh` 会跑）：建一条真实 TLS 连接 → 握手 → 传数据 → 正常关闭 → 解析日志行断言 `sni` == 客户端实际发送的值；再建一条**长时间保持然后被 RST 掉**的连接，断言 `sni` 依然出现在日志里。

### 2.4 连接关联 ID

HAProxy 社区版没有原生稳定的连接 ID。方案（按优先级降级，**降级时明确标注不可用，不用假 ID 顶替**）：

1. `%[unique_id]`：若该版本提供且语义是「每连接唯一」，直接采用；
2. 否则由**平台在渲染期/解析期合成**（当前做法）：`<node>-<fe>-<accept_ms>-<ci>-<cp>-<fi>-<fp>`。
   这里的时间戳取日志别名 **`%T`**（accept date，GMT，含毫秒）—— 不能用 `%[accept_date]`：
   `accept_date` 不是 sample fetch，那样写会让 `haproxy -c` 直接报错（见 §2.2）。
   社区版 `gpc0` 属于 stick-table 机制，需要额外声明 `table`，我们**不使用**（也是为了避免
   stick-table 带来的内存/eviction 复杂度）：
   `cid = <node>-<fe>-<T 的毫秒部分>-<ci>-<cp>-<fi>-<fp>`
   五元组 + 时间戳在同一毫秒内理论上可能撞（同 IP 同端口同毫秒重连），概率极低；`cid` 的用途是
   "把同一条日志的多个上下文串起来"，**不承担唯一键职责**，所以可接受。
3. 都不行 → `conn_id` 留空，UI 显示「不可用」。

> 说明写清楚：这个 ID 是**平台生成**的关联标识，不是 HAProxy 原生 ID。

---

## 3. Agent 侧接收与缓冲

### 3.1 收包（永不阻塞）

```go
// 伪代码，实际实现见 internal/agent/logrecv
fd3 := activateOrCreate("/run/shengyu-edgelink/log.sock", 0o660)  // systemd LISTEN_FDS 优先
conn := net.FilePacketConn(fd3)                          // unix dgram
setSocketRxqOvfl(conn)                                   // 读 SO_RXQ_OVFL 辅助数据
for {
    n, oob, err := recvmsg(conn, buf)      // 带 oob 读 cmsg
    if ovfl := parseRxqOvfl(oob); ovfl > 0 { dropCounter.add(ovfl) }
    spool.Append(buf[:n])                  // ★ 只做顺序追加写，不解析、不发网络
}
```

关键点：
- **收包 goroutine 只做 append**。解析、富化、组批、发网络全部在另一个 goroutine。这样即使管理面挂了、磁盘慢了，也不会反压到 HAProxy。
- **内核丢包可观测**：`SO_RXQ_OVFL` 的 ancillary data 会告诉我们内核因缓冲满丢了多少个数据报（Linux 特性）。这是**唯一**能拿到 HAProxy→agent 段丢包的方式，因为 **HAProxy 社区版不提供日志发送计数器**（`show info`/`show stat` 里没有日志相关计数）——这一点必须在文档里说清楚，不能假装有。
- spool 写失败（磁盘满/IO 错）：计数 + 写 `log_loss_events(kind=spool_overflow)`，**不重试阻塞**。

### 3.2 spool 格式与轮转

- 目录：`/var/lib/shengyu-edgelink/spool/conn/seg-000001.ndjson` …（默认 8 段，每段 128 MiB，总 1 GiB，可配）
- 内容：**agent 已注入节点侧已知信息后的 JSON 行**（原样 HAProxy JSON 再包一层，减少 mgr 反查开销）：
  ```json
  {"v":1,"seq":1048576,"node":"nd_...","recv_ts":"...","raw":"{...HAProxy 原始 JSON...}"}
  ```
  注意 `raw` 是**字符串**，所以 HAProxy 的引号/反斜杠由 Go 的 JSON 编码器原样转义 —— 这里也**不手写转义**。
- 段满 → 关闭当前段、开新段；超过总配额 → 删最旧段并 `log_loss_events(kind=spool_overflow)`。
- 发送成功且 mgr 已 ack 的段才删除；发送中崩溃用 `seq` 去重（mgr 侧按 `(node_id, seq)` 幂等写入：分片表加 `UNIQUE(node_id, seq)`? 连接日志一行一个 seq，因此在 conn_log 上加 `seq INTEGER` + `UNIQUE(node_id,seq)`；重复投递直接 `INSERT OR IGNORE`）。

### 3.3 发送与退避

| 情况 | 行为 |
|---|---|
| mgr 返回 200 | 记 ack，推进发送位点，段可删 |
| mgr 返回 4xx（鉴权失败/证书吊销） | 指数退避，写 `log_loss_events`，页面告警「节点上报被拒」 |
| mgr 超时/连不上 | 指数退避（1s→2s→…→60s 封顶），spool 继续堆积 |
| spool 满 | 丢最旧 + 计数（**绝不反向阻塞 HAProxy**） |

### 3.4 指标采集（与日志解耦）

| 来源 | 采集内容 | 周期 |
|---|---|---|
| HAProxy stats socket | `show stat` CSV（逐 frontend/backend/server：`scur,qcur,qmax,smax,slim,stot,bin,bout,rate,rate_max,status,check_status,check_code,check_duration,qtime,ctime,rtime,ttime,lastsess,addr,type,pxname,svname`）+ `show info`（`CurrConns,CumConns,ConnRate,SessRate,BytesIn,BytesOut,Uptime,Idle_pct,Nbthread,Pid`） | 5s（可配） |
| 实时心跳 | `scur/qcur/rate/bin/bout` 的即时快照（**独立于日志**，用于「当前连接数」） | 5s，随心跳一起发 |
| `/proc/stat`,`/proc/loadavg`,`/proc/meminfo`,`/proc/net/dev`,`/proc/diskstats`,`statfs` | CPU、负载、内存、网卡收发包与字节、磁盘 IO 与容量 | 10s（可配） |
| `/proc/net/tcp`,`/proc/net/tcp6` | 监听端口集合 → 发布验证与「端口在听吗」 | 按需（发布后立刻 + 每 60s） |

**⚠️ reload 计数器陷阱（必须处理，否则指标会莫名其妙归零）**

HAProxy 社区版的 `stot/bin/bout` 是**进程级**计数。平滑 reload 后新 worker 从 0 开始，老 worker 继续服务存量连接。因此：
- 采集时必须按 `pid` 分组（`show stat` 的 `pid` 列）；
- 平台展示「累计值」时，对同一 `(pxname,svname)` 的多个 pid **求和**，而不是取最后一行；
- 每次 reload 视为一次「计数器纪元」切换，在图上加标注，避免用户误以为流量掉了；
- 若节点上报时 pid 消失（老 worker 退出），其累计值要**归档累加**到节点级总量，否则总量会倒退。

### 3.5 HAProxy 指标字段语义（避免误解）

| 字段 | 含义 | 单节点内是否累加 |
|---|---|---|
| `scur` | 当前会话数 | 多个 pid **求和** |
| `qcur` | 当前排队请求数 | 求和 |
| `smax` / `qmax` | 历史峰值 | 取 **max** |
| `stot` | 累计会话数 | 求和（跨纪元需归档累加） |
| `rate` | 最近 10s 平均会话建立速率 | **求和会偏大** → 只在单 pid 稳定时展示，跨 pid 时显示「合计」并在 tooltip 说明 |
| `bin`/`bout` | 累计字节（frontend 视角：客户端侧收发） | 求和 |
| `qtime/ctime/rtime/ttime` | 最近 1024 次请求的平均耗时(ms) | 取**加权平均**，不能简单求和 |

---

## 4. 摄入协议（agent → mgr）

```
POST /agent/v1/logs            (mTLS, Content-Encoding: gzip, Content-Type: application/x-ndjson)
X-Shengyu-Edgelink-Node: nd_...
X-Shengyu-Edgelink-Stream: conn
X-Shengyu-Edgelink-Batch: <batch_id>

{"v":1,"seq":1000,"recv_ts":"...","raw":"{...}"}
{"v":1,"seq":1001,"recv_ts":"...","raw":"{...}"}

→ 200 {"accepted":500,"duplicated":0,"parse_errors":0,"last_seq":1499,"lag_ms":820}
```

- **幂等**：分片表 `UNIQUE(node_id, seq)` + `INSERT OR IGNORE`；重传不产生重复。
- **缺口检测**：mgr 维护 `ingest_cursors.last_seq`；若本批 `min_seq > last_seq+1` → 写 `log_loss_events(kind=seq_gap, count=缺口数)`，页面出现「日志缺失」告警，而不是静默。
- **延迟**：`lag_ms = now - max(recv_ts)`；超过阈值（默认 30s）写 `kind=ingest_lag`。
- **解析失败**：计入 `parse_errors`，**原始行照旧入库**（`parse_ok=0`，`raw_line` 保留），保证诊断时能看到原始日志。

---

## 5. 解析与富化

### 5.1 解析

```go
type rawLine struct {  // 严格对应 §2.2 的 JSON 键
    Ts     string  `json:"ts"`
    Node   string  `json:"node"`
    Ver    int     `json:"ver"`
    Cid    string  `json:"cid"`
    Ci     string  `json:"ci"`
    Cp     *int    `json:"cp"`     // 指针 = 区分「0」与「不可用」
    Fi     string  `json:"fi"`
    Fp     *int    `json:"fp"`
    Sni    string  `json:"sni"`
    Be     string  `json:"be"`
    Srv    string  `json:"srv"`
    Origin string  `json:"origin"`
    Tq     *int64  `json:"tq"`
    Tc     *int64  `json:"tc"`
    Tt     *int64  `json:"tt"`
    Bin    *int64  `json:"bin"`
    Bout   *int64  `json:"bout"`
    Tsc    string  `json:"tsc"`
    Err    string  `json:"err"`
    Rc     *int    `json:"rc"`
    Bq     *int    `json:"bq"`
}
```

- 用 `encoding/json` 反序列化 → **转义问题交给标准库**，不手写。
- 防御性处理：若行首出现 `<PRI>`（syslog 头）先剥离；若整行不是 JSON（版本差异/人工干扰），走 `parse_ok=0` 分支并保留原文。
- `"-"` → 置 `nil`（不可用）。`""` → 保留为空字符串。**两者语义不同，不可合并。**

### 5.2 富化

| 输出列 | 来源 | 失败时 |
|---|---|---|
| `business_id` / `route_id` | 启动时把 `bk_<bizshort>` → 业务/路由的内存映射加载好（随发布刷新）；解析 `be` 得到 | `'unknown'`，且计入 `parse_errors` |
| `origin_addr`/`origin_port` | 优先用日志里的 `origin`；取不到则用 `route_id` 反查 routes 表快照 | NULL |
| `entry_addr`/`entry_port` | 日志 `fi`/`fp`；取不到则按 frontend 名反查 | NULL |
| `client_port` | `cp` | NULL |

> 富化用的是**节点发布时的配置快照**（`config_versions.object_names` + routes），所以历史日志的归属关系不会因为后面改了配置而漂移。

---

## 6. 查询与分页（对应需求 §八）

| 约束 | 实现 |
|---|---|
| 必须分页 | `limit` 默认 50、**上限 200**；`offset` 上限 10000；超过则要求用 keyset（`before_ts`+`before_id`）滚动 |
| 限制时间范围 | 单次查询时间跨度**上限 24h**（可配）；默认 1h |
| 不扫全历史 | **物理隔离**：查询前由 `logstore.PartitionsFor(from,to)` 算出相关小时分片，只 `ATTACH` 这些文件 |
| 必须有过滤条件 | 全量无过滤查询拒绝执行，要求至少给 `business_id` / `node_id` / `client_ip` / `sni` 之一，或时间范围 ≤ 1h |
| 归档分片 | >48h 的分片是 `.db.zst`；查询命中时按需解压到 `/var/lib/shengyu-edgelink/cache/`（LRU，默认 2 GiB），未命中且解压失败 → 明确报「该时段已归档，正在解压」而不是空结果 |

---

## 7. 诊断（需求 §七）

### 7.1 分类规则表

| 分类码 | 触发条件（全部基于事实） | 已观察事实示例 | 下一步建议 |
|---|---|---|---|
| `NO_RECORDS` | 时间窗内该业务 0 行 | 「查询区间内未找到该业务的连接记录」 | 检查 DNS 是否已生效；检查客户端是否真的在发起连接；检查节点上报是否正常（出现 `LOG_LOSS`/`ingest_lag` 时本结论**不成立**） |
| `SNI_UNMATCHED` | 有记录且 `sni` ∉ 允许清单；或 frontend reject 计数上升 | 「收到 SNI=xxx 的 ClientHello，不在本业务允许清单内」+ 原始行 | 核对域名拼写/是否启用了 ECH/是否走了错误端口 |
| `ORIGIN_CONNECT_FAILED` | `err` 命中 `sc_err`/`srv` 相关串，或 `tc` 为空且 `tsc` 指向服务端 | 「源站 <ip:port> TCP 连接失败」+ 原始错误串 | 从该节点 ping/curl 源站；确认源站未限制该节点 IP |
| `QUEUE_LIMITED` | `bq>0` 或 `qcur>0` 持续，或 `err` 命中 `qlimit`/`no server available` | 「队列峰值 N，超过阈值」 | 调大 `maxconn`/`queue_limit`；检查源站是否变慢 |
| `SESSION_TIMEOUT_ABNORMAL` | `tt` ≈ 配置超时且 `err` 命中 timeout/`CD`/`SD` | 「会话时长 Xms 与配置的客户端超时一致后终止」 | 业务侧是否有长空闲连接需求；评估是否需要调大超时 |
| `PUBLISH_FAILED` | 时间窗内 `publish_records.ok=0` | 「版本 v42 发布失败于阶段 verify」+ 校验输出 | 看 verify_detail 逐端口结果；必要时回滚 |
| `PROBE_FAILED` | 时间窗内探测失败 | 「探测点 <location> 到 <节点IP>:443 的 TLS 握手失败（SNI=...）」 | 确认是节点侧还是源站侧（对比两节点探测） |
| `CONFIG_VERSION_SKEW` | 节点 `applied_version` ≠ `expected_version` | 「期望 v43，节点实际 v42」 | 重发或查看 agent 状态 |
| `LOG_LOSS` | 时间窗内有 `log_loss_events` | 「内核 socket 缓冲丢弃 N 条 / 序列缺口 M 条」 | 说明日志不完整，**其他分类结论需打折** |
| `DNS_DRIFT` | `last_seen_value` ≠ 平台期望值 | 「记录被外部改动为 X，平台期望 Y」 | 确认是否人工改动，决定是否接管 |

### 7.2 展示结构（每条异常固定四段，顺序不可变）

```
① 已观察到的事实      —— 只有可验证的陈述，不带推测
② 相关原始日志/状态码  —— 原始行 + HAProxy 原始终止码，可展开
③ 可能的原因          —— 列多条，按可能性排序，每条标注「支持它的证据」
④ 下一步检查建议      —— 具体命令/具体页面，不给空话
```

### 7.3 明确禁止的结论（代码里以常量注释+测试守护）

- ❌ 「已被封」——**永不输出**。`timeout` / `reset` 只能说明「会话被异常终止」，不能说明原因。
- ❌ 「业务完全正常」——TCP 连接成功 ≠ 业务可用（可能源站应用已崩）。
- ❌ 把业务探测的成功**当作**真实用户请求日志。探测结果单独一张表、单独一段 UI、明确标「平台探测」。
- ❌ 用节点→源站探测**冒充**东南亚用户端体验。探测结果必须带 `location` 与 `probe_kind`，并有一句固定文案：「本结果来自 <location> 的 <kind> 探测，不代表其他地区真实用户体验」。

### 7.4 脱敏诊断包导出

导出为 zip：

| 文件 | 内容 | 脱敏 |
|---|---|---|
| `manifest.json` | 导出时间、导出人、时间范围、业务 ID、平台版本、各节点 agent/haproxy/配置版本、脱敏声明 | — |
| `conn.jsonl` | 筛选后的连接日志（列白名单） | 客户端 IP **默认脱敏**为 `1.2.3.x`（可选保留全量，需显式勾选并记审计） |
| `metrics_summary.json` | 该业务在时间窗内的指标摘要（峰值/均值/总量） | — |
| `probes.jsonl` | 探测结果 | — |
| `publish.json` | 发布/回滚记录 + verify 明细 | 去掉 actor_ip |
| `versions.json` | 版本历史与内容 hash | — |
| `README.txt` | 说明文件与限制声明 | — |

**硬性排除**（导出前断言，断言失败则中止导出）：
- 任何 `admin_users` / `admin_sessions` / `admin_api_tokens` / `enrollment_tokens` / DNS 凭据；
- 私钥/令牌/密码字段（`redact` 包统一处理）；
- 其他客户的业务数据：**逐行校验 `business_id` ∈ 允许集合**，不满足即中止（防止过滤条件写错导致跨客户泄露）。

---

## 8. 容量控制（需求 §八）

| 策略 | 默认 | 可配 | 实现 |
|---|---|---|---|
| 连接日志保留 | 7 天 | ✅ | 分片目录按日期扫描，超期删除 |
| 审计保留 | 90 天 | ✅ | 按 `ts` 删除，删除动作自身记审计 |
| 指标保留 | 7 天 | ✅ | 同上 |
| 探测保留 | 14 天 | ✅ | 同上 |
| 磁盘上限 | conn 20GiB / metric 4GiB / probe 2GiB | ✅ | 超限先删最旧分片，再记 `log_loss_events(disk_quota)` 并在总览页告警 |
| 压缩 | 分片封盘 48h 后 `zstd` | ✅ | 孤儿临时文件启动时清理 |
| 轮转 | 小时分片天然轮转 | 粒度固定 | 跨小时自动新建 |
| 丢弃可观测 | — | — | 四类事件：`dgram_rxq_ovfl` / `spool_overflow` / `seq_gap` / `parse_error`，全部进 `log_loss_events` 并在页面呈现 |
| 不采集正文 | — | — | TCP 模式不解析载荷、不配 `capture`、不用 `option httplog` 处理业务流量；代码中无任何 payload 落盘路径 |
| 日志故障不影响转发 | — | — | dgram + spool 全异步；spool 满只丢弃 |

---

## 9. 验收方法（对应需求 §六「验收完整链路」）

`scripts/e2e-verify.sh`（在真节点上跑，输出可粘贴的验收报告）：

| # | 步骤 | 断言 |
|---|---|---|
| 1 | 后端起一个真实 TLS 源站（自签证书），发布一条 SNI 业务 | 发布成功、`verify` 端口全通过 |
| 2 | `openssl s_client -servername a.example.com -connect <节点IP>:443` 并收发数据后正常关闭 | 日志行出现：`sni=a.example.com`、`be=bk_...`、`srv=s1`、`tt>0`、`bin/bout` 有值或明确为不可用 |
| 3 | 建一条**保持 90s 未关闭**的连接，比较两个时间点的 `show stat scur` | `scur` 在连接未结束时已反映该连接（**证明实时性不依赖结束日志**） |
| 4 | 发送 `SNI=unknown.example.com` | 连接被拒；日志出现该 SNI 且可定位（`SNI_UNMATCHED`） |
| 5 | 把源站停掉，再连 | 日志出现 `ORIGIN_CONNECT_FAILED` 且带原始错误串；页面显示真实错误；发布验证**不会**因此回滚 |
| 6 | 触发一次规则修改并 reload，期间保持一条长连接 | 长连接不断；日志里新旧 `ver` 都出现（证明版本语义准确）；计数器纪元切换被标注 |
| 7 | 灌 200 万行日志 | 分片正常滚动；查询 2 秒内返回；`log_loss_events` 为空或数值与实际一致 |
| 8 | 停 agent 5 分钟再启动 | 转发不受影响；恢复后补传；`dgram_rxq_ovfl` 计数与缺口可对账 |
| 9 | 把日志分区配额改成 1MiB | 观察到 `disk_quota` 事件，转发仍正常，页面出现告警而非静默 |
| 10 | 导出诊断包 | 用 `grep` 断言包内**不含**任何密钥/令牌/其他客户 business_id |
