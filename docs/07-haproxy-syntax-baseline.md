# 07 · HAProxy 2.8 语法核对记录（逐条对照官方手册）

> 本文档对应评审 **F01 / F09 / F11**，回答同一个问题：
> "渲染出来的配置，凭什么说它在 HAProxy 2.8 上是合法的、且语义与注释一致？"
>
> 结论的前置条件写在最前面：**本机没有 HAProxy，因此这些结论的依据是官方手册原文，
> 不是真实二进制的 `haproxy -c`。** 每条都标了"待真实 HAProxy 验证"，
> 真机验收（Linux + 真实 HAProxy + 最终非 root 身份）完成后再把状态改成"已实测"。

## 0. 依据来源

| 项 | 值 |
|---|---|
| 手册 | HAProxy 2.8 Configuration Manual |
| 获取处 | `https://git.haproxy.org/?p=haproxy-2.8.git;a=blob_plain;f=doc/configuration.txt;hb=HEAD` |
| 本地核对时间 | 2026-09-21 |
| 文件大小 | 1,295,865 字节（完整手册正文） |
| 代码里的基线常量 | `haproxy.SupportedHAProxyVersion = "2.8"` |

为什么不用 `docs.haproxy.org/2.8/configuration.html`：那一页整页加载、体量过大，
抓取时会被截断到第 3 章，`server` 参数表、别名表、sample fetch 表全都拿不到 ——
**拿不到原文就等于没有核对**。因此改为直接取手册源文件正文。

---

## 1. F01：日志表达式与 `global` 段

### 1.1 `%B` 的方向（同时是 F11）

手册 §8.2.2（TCP 日志格式字段说明）原文：

```
  - "bytes_read" is the total number of bytes transmitted from the server to
    the client when the log is emitted.
```

§8.2.6 别名表原文：

```
  |   | %U   | bytes_uploaded       (from client to server)  | numeric     |
  |   | %B   | bytes_read           (from server to client)  | numeric     |
```

**结论**：`%B` 是"源站 → 客户端"（下行），`%U` 才是"客户端 → 源站"（上行）。
原实现把 `%B` 标成"客户端→中转字节"，方向标反了（评审 F11 成立）。

**改法**：拆成两个字段 `up`（`%U`）与 `down`（`%B`），字段名即方向；
数据库由 `bytes_in` 改为 `bytes_up` / `bytes_down`，旧分片按方向迁移
（旧列语义就是下行，见 `internal/logstore/shard.go` 的 `backfillBytesDown`）。

### 1.2 `accept_date` 不是 sample fetch

手册中 `accept_date` 只出现在 §8.2.2 / §8.2.3 的**日志字段说明**里：

```
      3   '[' accept_date ']'                       [06/Feb/2009:12:12:51.443]
  - "accept_date" is the exact date when the connection was received by HAProxy
```

sample fetch 一节的日期类只有 `date` 与 `date_us`，而 `date` 的原文是
"Returns the current date"（**当前时间**，不是会话建立时间）。
因此 `%[accept_date]` 里的 `accept_date` **不是合法的 sample fetch**。

对应别名（§8.2.6）：

```
  |   | %t   | date_time      (with millisecond resolution)  | date        |
  |   | %T   | gmt_date_time                                 | date        |
  |   | %ms  | accept date milliseconds (left-padded with 0) | numeric     |
```

**改法**：时间字段改用别名 `%T`（accept date 的 GMT 形式）。
选 GMT 而不是 `%t`（本地时间）的理由：平台按 UTC 解析入库，
用 GMT 可以避免跨时区的远程节点产生时间漂移。

### 1.3 JSON 转义用 `json(...)`，没有 `json_escape`

手册 §7.3.1（Converters）原文：

```
json([<input-code>])
  Escapes the input string and produces an ASCII output string ready to use as a
  JSON string. ... It can be "ascii", "utf8", "utf8s", "utf8p" or "utf8ps".
   - "utf8s"  : never fails, but removes characters corresponding to errors;
```

全手册中**不存在** `json_escape` 这个转换器。

**改法**：统一用 `json(utf8s)`（`haproxy.JSONConverter`）。
选 `utf8s` 而不是 `utf8`：SNI 是客户端可控内容，构造非法 UTF-8 序列会让
`json(utf8)` 返回"取不到值"，于是日志里 SNI 永远为空 ——
攻击者可以用这种方式让日志失去追溯价值。`utf8s` 的语义是"永不失败，只丢非法字符"。

### 1.4 `expose-fd listeners` 不是 global 关键字

手册 §5.1（Bind options）原文：

```
expose-fd listeners
  This option is only usable with the stats socket. ...
  In master-worker mode, this is not required anymore, the listeners will be
  passed using the internal socketpairs between the master and the workers.
```

**结论**：它不是 global 关键字；且在 master-worker 模式下已不需要。
本平台强制要求 master-worker（启动自检里 `MasterWorker == false` 就拒绝启动）。

**改法**：从 `global` 段**删除**该指令，并把它从指令白名单里移除 ——
这样"顺手加回一行"的改动会被渲染自检直接拦下。

### 1.5 弃用别名

手册 §7.3 同时列出两套名字：

```
req.ssl_hello_type : integer
req_ssl_hello_type : integer (deprecated)
req.ssl_sni : string
req_ssl_sni : string (deprecated)
```

**改法**：改用带点的正式写法 `req.ssl_sni` / `req.ssl_hello_type`。

### 1.6 `stats socket` 的选项

手册 §3.1 原文：

```
stats socket [<address:port>|<path>] [param*]
  ...
  All parameters supported by "bind" lines are supported, for instance to
  restrict access to some users or their access rights.
```

bind 选项里包含 `mode <mode>`（"Sets the octal mode used to define access
permissions on the UNIX socket"）、`user`、`group`、`level`。

**结论**：`stats socket <path> mode 660 level operator user … group …` 合法；
用一个统计套接字做"运行版本可观测"是有手册依据的。

---

## 2. F09：健康探测与业务传输必须分开

### 2.1 `check-ssl` / `check-sni` 只作用于探测

手册 §5.2 原文：

```
check-sni <sni>
  This option allows you to specify the SNI to be used when doing health checks
  over SSL. It is only possible to use a string to set <sni>. If you want to
  set a SNI for proxied traffic, see "sni".

check-ssl
  This option forces encryption of all health checks over SSL, regardless of
  whether the server uses SSL or not for the normal traffic.
```

### 2.2 server 行的 `ssl` / `sni` / `verify` 会改动**业务传输**

```
sni <expression>
  The "sni" parameter evaluates the sample fetch expression, converts it to a
  string and uses the result as the host name sent in the SNI TLS extension to
  the server. ... If you want to set a SNI for health checks, see the
  "check-sni" directive for more details.
```

`verify [none|required]`（server 行）用于校验**源站证书**。

**结论**：把界面上的"TLS 健康检查"渲染成 server 行的
`ssl sni str(...) verify required`，等于让 HAProxy 对源站发起 TLS ——
在 SNI 透传场景里就是"给客户的 TLS 流量再套一层 TLS"，业务直接不可用。
这正是评审 F09 指出的问题。

**改法**：
- 业务传输侧**永不**输出 server 行的 `ssl` / `sni` / `verify`；
- 探测侧按类型渲染 `check-ssl` / `check-sni <sni>`；
- 默认不校验证书（`SkipCertVerify=true`）：透传场景下证书由**客户端**校验，
  中转节点只做活性探测；对自签/内网证书强行 `verify` 会把健康源站判死。
  确需校验时由运维显式打开，并在校验层给出"可能误判"的 warning。

### 2.3 `timeout` 不是 server 行参数

手册 §5.2 的 server 参数表里没有 `timeout`；探测超时属于代理级关键字
`timeout check <timeout>`（backend 可用）。

**改法**：`HealthCheck.TimeoutMS` 渲染成 backend 的 `timeout check <ms>`；
并在 `internal/haproxy/spec.go` 增加 **server 行参数白名单校验**
（`checkServerLine`），让 `check timeout 3000ms` 这类写法在渲染阶段就被拦下。

### 2.4 `maxqueue` / `maxconn` 的落点

```
  maxqueue <maxqueue>
    The "maxqueue" parameter specifies the maximal number of connections which
    will wait in the queue for this server.

  maxconn <maxconn>
    The "maxconn" parameter specifies the maximal number of concurrent
    connections that will be sent to this server.
```

两者都在 §5.2（Server and default-server options）。

**改法**：
- `maxqueue` / `maxconn` 一律渲染到 **server 行**；
- 从 backend 白名单里**移除** `maxqueue`（它不是 backend 关键字）；
- 不再输出 frontend `maxconn` —— frontend 的 maxconn 是"超出即丢弃"，
  server 的 maxconn 是"超出即排队"，两者语义不同却共用一个界面字段，
  会让运维以为在排队、实际在丢连接。

### 2.5 HTTP 探测的正确写法

```
http-check send [meth <method>] [{ uri <uri> | uri-lf <fmt> }] [ver <version>]
                [hdr <name> <fmt>]*
http-check expect ... [status-code <expr>] [!] <match> <pattern>
    <match> ... may be one of "status", "rstatus", "hdr", "fhdr", "string", "rstring"
```

**改法**：HTTP/HTTPS 探测渲染成 backend 的
`option httpchk` + `http-check send uri <path> hdr Host <域名>` + `http-check expect status <code>`；
不再使用 server 行上的 `proto http` / `uri`（都不是 server 参数）。

---

## 3. 逐条核对表

| # | 指令/表达式 | 出处 | 状态 |
|---|---|---|---|
| 1 | `global: log <path> format raw local0` | §3.1 `log` | 待真实 HAProxy 验证 |
| 2 | `global: maxconn` / `nbthread` | §3.1 | 待验证 |
| 3 | `global: master-worker` | §3.1 | 待验证 |
| 4 | `global: stats socket <path> mode/level/user/group` | §3.1 + §5.1 | 待验证 |
| 5 | `global: stats timeout` / `hard-stop-after` | §3.1 | 待验证 |
| 6 | `defaults: mode tcp / log global / option log-health-checks` | §4.2 | 待验证 |
| 7 | `defaults: retries / timeout connect,client,server,queue,check,tunnel` | §4.2、§4.2 `timeout` | 待验证 |
| 8 | `frontend: bind` / `log-format` / `description` / `maxconn` | §4.2 | 待验证 |
| 9 | `frontend: tcp-request inspect-delay` | §4.2 | 待验证 |
| 10 | `tcp-request content set-var(txn.sni) req.ssl_sni` | §7.3 `req.ssl_sni` | 待验证 |
| 11 | `tcp-request content reject unless { req.ssl_hello_type 1 }` | §7.3 | 待验证 |
| 12 | `tcp-request content reject unless { var(txn.sni) -m str -i -f <file> }` | §7.3、§7.1 | 待验证 |
| 13 | `use_backend ... if { var(txn.sni) -m str -i <域名> }` | §4.2 `use_backend` | 待验证 |
| 14 | `backend: option httpchk` / `http-check send` / `http-check expect` | §4.2、§4.3 | 待验证 |
| 15 | `backend: timeout check` | §4.2 `timeout` | 待验证 |
| 16 | `server s1 <ip>:<port> check [inter/rise/fall/check-ssl/check-sni/maxconn/maxqueue]` | §5.2 | 待验证 |
| 17 | 日志别名 `%T %ci %cp %fi %fp %b %s %Tw %Tc %Tt %U %B %si %sp %ts %rc %bq` | §8.2.6 | 待验证 |
| 18 | 转换器 `json(utf8s)` | §7.3.1 | 待验证 |
| 19 | ~~`global: expose-fd listeners`~~ | §5.1（仅 stats socket 可用） | **已删除** |
| 20 | ~~`%[accept_date]`~~ | §7.3（无此 fetch） | **已替换为 `%T`** |
| 21 | ~~`json_escape`~~ | §7.3.1（无此转换器） | **已替换为 `json(utf8s)`** |
| 22 | ~~server 行 `ssl` / `sni` / `verify`~~ | §5.2（作用于业务传输） | **已移除** |
| 23 | ~~server 行 `timeout` / `uri` / `proto http`~~ | §5.2（非 server 参数） | **已移除** |
| 24 | ~~backend `maxqueue`~~ | §5.2（server 参数） | **已移到 server 行** |

**第 1–18 项的"待验证"如何收口**：Linux 真机验收时，对**每一种界面可选组合**
（模式 A/B × 健康探测 tcp/tls/http/https × 有/无域名）生成配置，
逐个跑：

```bash
haproxy -c -f /etc/shengyu-edgelink/haproxy/current/haproxy.cfg     # 语法
haproxy -vv | head -5                                      # 版本与编译选项
```

并把每种组合的真实日志行与表头逐字段对照（特别是 `%T / %U / %B / %si / %sp`）。
在拿到这些命令输出之前，**任何"生产 HAProxy 路径已验证"的说法都不成立** ——
这也是评审对交付说明的判断里唯一一条"不成立"的结论。

---

## 4. 未做与风险

- **未在真实 HAProxy 上跑过任何一条**：本机（Windows 开发机）没有 HAProxy，
  渲染器里的**指令白名单 + server 行参数白名单**都只是机械自检，
  不能替代 `haproxy -c`。这是当前最大的未验证面。
- `%si` / `%sp`（target address）在 TCP 模式下是否可用、在什么条件下输出 `-`，
  需实测确认；若实测不可用，界面会显示"不可用"（按字段可用性机制）而不是 0。
- `%T` 是否确为 accept date（而非日志写出行时刻）需实测确认：
  办法是制造一个"建立后挂 60 秒再断开"的连接，比较日志里的 `%T` 与断开时刻。
- `hard-stop-after` 决定 reload 后老 worker 的最长存活时间，因此
  "长连接不会被 reload 打断"这句话的准确说法是
  **"最长不中断时间 = hard-stop-after（默认 30 分钟，可配置）"**。
