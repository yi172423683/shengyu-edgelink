# 04 · HAProxy 版本与字段兼容性基线

> 需求 §二.8 要求：**选择并明确受支持的 HAProxy 版本与 Linux 发行版，验证使用的日志字段和配置语法，不混用商业版特性。**
> 本文档就是那份基线，并且把「验证」做成可执行的工具而不是口头承诺。

---

## 1. 受支持的运行环境

| 组合 | 发行版 | HAProxy | 来源 | 支持状态 | 结论 |
|---|---|---|---|---|---|
| **A（推荐基线）** | Ubuntu 24.04 LTS (noble) | **2.8.x**（`apt` 官方源，如 2.8.5+security） | 发行版仓库 | ✅ 正式支持 | **生产默认**，LTS 到 2029（OS）/2028（HAProxy 2.8 LTS） |
| B（保守基线） | Debian 12 (bookworm) | **2.6.x**（`apt` 官方源） | 发行版仓库 | ✅ 支持 | 适用于客户已有 Debian 12 环境；语法是 2.8 的子集 |
| C（精确锁定） | Ubuntu 24.04 / Debian 12 | 指定 2.8.x（源码编译，`make TARGET=linux-glibc USE_OPENSSL=1 USE_PCRE2=1 USE_SYSTEMD=1`） | 上游源码 | ✅ 支持 | 需要"一个字节都不许变"的场景；同时产出版本清单 |
| D | Ubuntu 22.04 LTS | 2.4.x（官方源）/ 自行装 2.8 | — | ⚠️ 不推荐 | 2.4 太老，部分 log-format 变量行为差异大 |
| E | 任何发行版 | **3.x**（3.0/3.1/3.2 等） | — | 🔶 未验证 | **v1 声明不支持**。若未来要支持，必须重跑 §6 的全部验证并更新本文档 |
| F | 任何发行版 | HAProxy Enterprise / HAPEE 包 | 商业仓库 | ❌ 明确不用 | 见 §4 |

**硬门槛（agent 启动自检，不满足则节点标记为"环境不受支持"并拒绝发布）**

1. `haproxy -vv` 可执行且能解析出主版本号；
2. 主版本 ∈ {2.6, 2.7, 2.8}（2.7 是过渡版，允许但不推荐）；
3. `haproxy -vv` 里 OpenSSL 支持存在（`Built with OpenSSL` 且非 `no`）；
4. **功能探针**（见 §5）全部通过 —— 这一条才是真正的门槛，版本号只是快速筛选。

---

## 2. 我们依赖的 HAProxy 能力及其最低版本

| 能力 | 用途 | 引入版本（社区版） | 在我们基线中的状态 |
|---|---|---|---|
| `mode tcp` + `tcp-request inspect-delay` | SNI 透传的前置条件 | 远古 | ✅ |
| `req.ssl_sni` sample fetch | 读 ClientHello 里的 SNI。手册 7.3 的正式名是 **`req.ssl_sni`**；`req_ssl_sni` 是早期写法，仍被接受但手册已标注 "deprecated"，故渲染器不再产出它 | 1.5 | ✅ |
| `tcp-request content set-var(txn.<name>)` | **握手期把 SNI 存入会话变量** | `set-var` 1.9 / tcp-request content 上下文 2.0 | ✅ 2.6+ 可用 |
| `req_ssl_hello_type` | 判定是不是 TLS ClientHello | 1.5 | ✅ |
| `-m str -i -f <file>` ACL（从文件加载精确模式） | 未配置 SNI 的拒绝清单 | 远古 | ✅ |
| 自定义 `log-format` + 任意 sample fetch | 结构化日志 | 1.6+ | ✅ |
| `json(utf8s)` 转换器 | 日志字段转义。**2.8 里不存在 `json_escape` 这个转换器**；正确的是 `json([input-code])`，取 `utf8s`（永不失败、丢弃非法字符）。选 `utf8s` 而非 `utf8` 的原因见 docs/07 §1.3 | 1.9+（`json(...)`） | ✅ |
| `log <unix-socket-path> format raw` | 直投 unix datagram socket | `format` 选项 2.0+ | ✅ |
| `master-worker`（`-Ws`） | 平滑 reload（新 worker 继承监听 socket） | 1.8（`-W`）/ 稳定于 2.0+ | ✅ |
| ~~`expose-fd listeners`~~ | **不使用**。手册 5.1 原文 "This option is only usable with the stats socket"，它不是 global 关键字；且在 master-worker 模式下已被内部 socketpair 取代。曾误写在 global 段，评审 F01 已删除 | — | ❌ 明确不用 |
| `hard-stop-after` | reload 后老 worker 的最长存活（防泄漏） | 1.9 | ✅ |
| `stats socket ... level operator` | 只读统计接口，不开放写操作 | 1.6+ | ✅ |
| `show stat` / `show info`（CLI） | 指标采集 | 1.6+ | ✅ |
| `nbthread` | 多线程 | 1.8+ | ✅ |
| `http-reuse` / `maxqueue` / `timeout queue` | 排队与连接复用（TCP 模式主要用 `maxqueue`） | 1.6+ | ✅ |
| `tcp-check`（`check` + `tcp-check connect/send/expect`） | 源站健康探测 | 1.5+ | ✅ |
| `option log-health-checks` | 把健康检查结果也写日志 | 1.6+ | ✅ |
| graceful reload 保持存量连接 | 需求 §五硬要求 | 1.8+ | ✅ |

---

## 3. 明确不使用的特性（**防止无意混入商业版**）

| 不用 | 原因 |
|---|---|
| HAPEE 模块包（WAF/ModSecurity、Bot 防护、速率限制模块等） | 商业授权，部署到客户环境会产生许可风险 |
| Fusion / 官方 Data Plane API 作为控制面 | 商业产品；我们自己的 Go 控制面就是替代品 |
| 商业版 RPM/DEB 仓库（`hapee-*` 包） | 同上 |
| DeviceAtlas 商业数据文件 | 依赖商业许可证 |
| `lua` 载荷（社区版有 Lua，但我们不用） | 减少攻击面与不确定性；同时避免误引商业 Lua 扩展 |
| 标注 `experimental` 的特性：QUIC/HTTP3、ECH 解析 | 需求 §三 已明确不支持 HTTP/3 与 ECH 识别 |
| `option httplog` 处理业务流量 | 我们是 TCP 透传，不解析 HTTP，也就不采集任何请求正文（需求 §八） |

**代码保障**：`internal/haproxy/spec.go` 的指令白名单是**闭合集合**（`allowedDirectives` map，另有 `forbiddenSubstrings` 硬禁用词）。渲染器每次输出都会过一遍 `CheckDirectives`，任何不在白名单里的指令（包括模板被误改引入的）都会让渲染**直接返回 error**，配置根本不会被写出来。这是防止"某天有人手滑加了个商业指令"的机械保障，而不是靠 review 记得。

---

## 4. 交付与许可

| 组件 | 许可 | 我们怎么处理 |
|---|---|---|
| 我们的 Go 后端/前端/agent | 自有品牌，未定（建议私有） | 交付**编译产物**（`relay-mgrd`、`relay-agentd`、`relay-helper`，linux/amd64 + arm64） |
| HAProxy 社区版 | **GPL-2.0-or-later**（含 OpenSSL 链接例外） | **不自带分发**：节点上由发行版 `apt` 安装（我方服务器自用，不构成对外分发）。若某场景需要随包分发，则同目录提供 HAProxy 源码获取方式（`apt-get source haproxy` 或上游 tarball 地址 + 校验和） |
| Go 标准库 | BSD-3-Clause | 保留 `THIRD_PARTY_LICENSES.md` |
| 其余第三方（SQLite 驱动、Vue/Element Plus 等） | 各自 | 由 `scripts/gen-third-party-licenses.sh` 生成清单，随交付物一起出 |

`THIRD_PARTY_LICENSES.md` 由脚本生成，**手动维护的许可清单一定会过期**，所以做成构建步骤。

---

## 5. 功能探针（真正的版本门槛）

`relay-agentd` 启动时和每次发布前可执行一次 **feature probe**：用一份专门探测特性的最小配置跑 `haproxy -c`，把结果落成 `NodeCapabilities`：

```
/etc/shengyu/probe/probe-<uuid>.cfg
    global
        log /run/shengyu-edgelink/log.sock format raw local0        # 探 unix dgram + format raw
        stats socket /run/shengyu-edgelink/probe-<uuid>.sock mode 660 level operator
        master-worker                                    # 探 master-worker 关键字
        hard-stop-after 30s
        user  shengyu                                    # 探 worker 降权（见 §8）
        group shengyu
    defaults
        mode tcp
        timeout connect 5s
        timeout client  30s
        timeout server  30s
    frontend probe_fe
        bind 127.0.0.1:0                                    # 不实际占用真实端口
        tcp-request inspect-delay 1s
        tcp-request content set-var(txn.sni) req.ssl_sni    # 探 set-var + req.ssl_sni
        tcp-request content reject unless { req_ssl_hello_type 1 }   # 探 hello_type
        log-format "{\"sni\":\"%[var(txn.sni),json(utf8s)]\",\"be\":\"%b\",\"t\":\"%Tw\",\"tsc\":\"%ts\"}"
        default_backend probe_bk
    backend probe_bk
        server s1 127.0.0.1:1 check
```

`haproxy -c -f probe.cfg` 的返回码 + stderr 决定能力矩阵：

| 探针 | 通过则 | 失败则 |
|---|---|---|
| `set-var(txn.sni) req.ssl_sni` | 支持握手期 SNI 捕获 | **拒绝在该节点发布 SNI 业务**（这是硬依赖） |
| `json(utf8s)` 转换器可用 | 结构化日志可用且不会被客户端构造的非法 UTF-8 打空 | 字段标记「不可用」并停用相应字段（不是"降级为不转义"——不转义等于允许日志注入） |
| `log <path> format raw` | 直投 unix socket 可用 | 降级为 `log 127.0.0.1:<port>` UDP 目标 |
| 自定义 `log-format` 编译通过 | 字段模板可用 | 记录具体是哪个字段导致失败 → 对应字段标 `unavailable` |
| `master-worker` | 平滑 reload 可用（监听 fd 经 master 与 worker 间的 socketpair 传递） | 标记「不支持平滑发布」，UI 上该节点发布按钮变灰并说明原因 |
| global 的 `user` / `group` | worker 进程会降权到指定账号 | 标记「转发进程将以 root 长期运行」并拒绝安装（见 §8） |

**每节点的能力矩阵会被 agent 上报并落库**，发布时按能力矩阵做前置校验：比如某节点不支持 SNI 捕获，就永远不允许把 `sni_tls` 业务发布到它上面。

---

## 6. 字段可用性验证工具

`scripts/verify-log-fields.sh`：在真节点上跑，产出 `field-availability.json`。

流程：

1. 用**真实渲染器**生成一份含各字段的测试配置（不是手写配置，保证和线上一致）；
2. 起一个本地自签 TLS 源站；
3. 依次制造 6 种会话，每种都用 `openssl s_client` 或 Go 客户端精确构造：
   - 正常 TLS + SNI + 数据收发 + 正常关闭
   - TLS + SNI + 长连接被 RST 掐断
   - 无 SNI 的 TLS
   - 非 TLS 裸 TCP（应被拒）
   - 源站不可达（连接被拒）
   - 队列堆积（把源站做成慢接收，制造 `bq>0`）
4. 抓取每个会话产生的原始日志行（从 agent 的 spool 里直接读，绕开解析器 → **验证的是 HAProxy 真的写了什么**）；
5. 对每个注册字段断言：键存在？值非 `-`？值合理（比如 `tt>0`）？
6. 输出：
   ```json
   {
     "haproxy_version": "2.8.5",
     "measured_at": "2026-09-21T07:30:00Z",
     "fields": {
       "sni":         {"status":"verified","sample":"a.example.com","notes":"握手期 set-var，会话被 RST 后仍可读"},
       "conn_id":     {"status":"unavailable","notes":"unique_id 在 2.8 上不可用/不保证唯一"},
       "bytes_out":   {"status":"unavailable","notes":"TCP 自定义 log-format 未取到"},
       "tsc":         {"status":"verified","sample":"--"},
       "origin":      {"status":"unverified","notes":"取到代理地址而非源站，改用 routes 反查"}
     }
   }
   ```
7. 该文件放回 `internal/haproxy/fieldreg/field-availability.json`，**UI 与导出据此决定显示真实值还是「不可用」**。

> 这一步是把需求里那句「以固定版本文档为准验证字段可用性；没有的数据标记为不可用，不伪造」变成一件**有产物、可复现**的事。

---

## 7. 已知陷阱清单（踩过或必须防的）

| # | 陷阱 | 后果 | 我们的对策 |
|---|---|---|---|
| T1 | 监听 socket 归属 agent → agent 重启换 inode | HAProxy 继续往失效 inode 发日志，**静默丢日志** | socket 由 systemd `shengyu-log.socket` 持有，agent 用 socket activation 接管 fd |
| T2 | HAProxy 启动时 unix socket 不存在 | 该 log target 被静默禁用，只有启动日志里一句 warning | haproxy unit `Requires=` 两个 socket/agent 单元；启动后自检 `log target` 是否可用（用 `haproxy -c` 无法检出，改为自检首个连接日志是否到达） |
| T3 | `-sf` 式 reload 未开 master-worker | 监听端口有极短空窗（SO_REUSEPORT 缓解但不彻底） | 强制 `-Ws` + `USR2`，并在探针里校验 `master-worker` 关键字 |
| T4 | 把 `expose-fd listeners` 当 global 关键字写 | `haproxy -c` 直接报 unknown keyword，整个配置发不出去 | 它不是 global 关键字（手册 5.1），且 master-worker 下已不需要。已从白名单移除，写回去会被渲染自检拦下 |
| T5 | `reload` 后计数器归零 | 指标图掉崖，误判为故障 | 按 pid 分组求和 + 纪元标注（见 docs/03 §3.5） |
| T6 | `log-format` 里 `#` 需转义为 `\#` | 配置解析错误或日志被截断 | 渲染器对字面量做 `#` 转义；版本/节点 ID 都是数字/字母，不触发 |
| T7 | SNI 字段含恶意字节（客户端可构造含 TAB/引号/换行的 SNI） | 日志注入 → 解析器看到假字段 | 所有字符串字段走 `json(utf8s)`；解析用 `encoding/json`；转义后仍是无效 JSON 的行走 `parse_ok=0` 并保留原文 |
| T8 | `tcp-request inspect-delay` 太短 | 慢客户端 ClientHello 未收全就被拒 | 默认 5s，可配；低于 1s 时校验器给警告 |
| T9 | `inspect-delay` 太长 + 大量半开连接 | 内存被未决会话吃掉 | `maxconn` + `hard-stop-after` + 校验器限制上限（≤10s） |
| T10 | ACL 模式文件与 cfg 不同步切换 | 出现"配置已生效但允许清单还是旧的"的窗口 | 允许清单放在**版本目录内**，与 cfg 同一 symlink 原子切换 |
| T11 | `timeout client/server` 设得太小 | 长连接被莫名掐断，日志显示 timeout 但业务并无故障 | 校验器强制 ≥ 30s，并对 < 60s 的值给"可能掐断长连接"警告 |
| T12 | `nbthread > 1` 时 stats 的 `rate` 语义变化 | 速率展示偏大 | 见 docs/03 §3.5 的展示规则 |
| T13 | `option log-health-checks` 未开 | 源站探测失败只体现在 stats `check_status`，没有日志可对照 | 显式开启；探测日志与真实用户日志**分开**呈现 |
| T14 | 用 `pkill haproxy` | 误杀无关进程 | 全链路只用 `systemctl reload shengyu-edgelink-haproxy`；代码中禁止出现 kill/pkill/killall（有测试守护） |
| T15 | **数字字段不加引号，而 HAProxy 对取不到的 sample 输出裸 `-`** | 日志行变成 `{"tq":-}` —— **非法 JSON，一行坏数据打穿整个解析器** | 渲染器把**所有** HAProxy 字段（含数字）都放进引号，需要转义的走 `json(utf8s)`；类型语义由字段注册表的 `Numeric` 表达，解析器把 `"-"` 翻译为「不可用（NULL）」而不是 0。有测试用「把表达式替换成 `-` 后仍能 `json.Unmarshal`」守护这条 |
| T16 | 用字符串拼接生成 `log-format` | 转义错误 → 日志被注入/解析错位 | 格式串由字段注册表程序化生成，字面量走 `escapeLiteral`，不做手工拼接 |
| T17 | 只在 systemd 单元里降权，配置里不写 `user`/`group` | 一旦有人手工以 root 起 haproxy（排查、迁移、容器里直接跑），**所有 worker 长期是 root** | 两层都做：unit 侧 `User=shengyu`，配置侧再写 `user`/`group`（见 §8）。程序启动时会校验降权目标账号存在 |

---

## 8. 转发进程的运行身份（worker 降权）

要求很直接：**不允许有任何长期以 root 运行的转发进程**。这里把依据与落地写清楚，
因为"配了 unit 就算降权了"是一个经不起追问的结论。

### 8.1 HAProxy 在 master-worker 下到底谁降权

源码 `src/haproxy.c` 的调用点：

```c
if ((global.mode & (MODE_MWORKER | MODE_DAEMON)) == 0)
        set_identity(argv[0]);
```

意思是：**master-worker 模式下 `set_identity()` 不会在 master 上执行**，只有 fork 出来的
worker 进程会在后面各自调用它切到 `user`/`group`。所以配置里的 `user`/`group`
管的是 worker（也就是真正处理流量的那些进程），master 的身份由 systemd 单元决定。

推论：**两层都要做，缺一层就不成立** ——

| unit 身份 | global 有 user/group | worker 实际身份 | 结论 |
|---|---|---|---|
| `User=shengyu` | 有 | shengyu | ✅ 两层叠加，符合 |
| `User=shengyu` | 无 | shengyu（继承自 master） | ✅ 仍符合，但依赖 unit 没被改坏 |
| `User=root` | 有 | shengyu（worker 自己降） | ⚠️ master 长期是 root，不合格 |
| `User=root` | 无 | **root** | ❌ 明令禁止 |

### 8.2 为什么"非 root 启动 + 配置里再写 user/group"是安全的

担心点在于：非 root 时 haproxy 再调 `setuid()`/`setgid()` 会不会失败？
答案是不会，源码 `src/linuxcap.c` 的处理链路决定了这一点：

```c
int prepare_caps_for_setuid(int from_uid, int to_uid) {
        if (from_uid != 0)
                return 0;          /* 非 root：整个能力处理直接跳过 */
        ...
}
```

非 root 启动时能力保留/切换整段跳过，随后 `set_identity()` 里的
`setgid(自己)` / `setuid(自己)` 必然成功；补充组丢不掉只产生一条 `ha_warning`，不是致命错误。

### 8.3 落地位置

- 配置侧：`internal/haproxy/render.go` 依据 `Defaults.RunUser/RunGroup` 输出 global 的 `user`/`group`；
  命令行 `-haproxy-user` / `-haproxy-group` 可覆盖，默认 `shengyu`。
- unit 侧：`deploy/shengyu-edgelink-haproxy.service` 用 `User=shengyu` + `CapabilityBoundingSet=CAP_NET_BIND_SERVICE`
  （只保留"绑 443 之类特权端口"这一项能力）。
- 启动侧：`cmd/shengyu-edgelink/main.go` 的 `checkRunIdentity()` 在启动时校验：
  ① 降权目标账号/组必须存在（否则 haproxy 启动会失败）；
  ② 以 root 运行且未配置降权目标 ⇒ **直接拒绝启动**，不留到验收时才发现。

### 8.4 验收命令

```bash
# 每一行都必须显示 shengyu，包括 master（pid 1 子进程里最老的那个）
ps -o user=,pid=,ppid=,args= -C haproxy
# 配置里确实写了降权
grep -E '^\s+(user|group)\s+' /etc/shengyu-edgelink/haproxy/current/haproxy.cfg
```

> 本节结论目前来自 HAProxy 2.8 源码阅读，**尚未在真实 HAProxy 上实测**；
> 实测后请把结果回填到这里并改状态（与 docs/07 的处理方式一致）。
