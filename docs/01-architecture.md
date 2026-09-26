# 01 · 总体架构

> 交付物代号：**shengyu-edgelink**（自有品牌名可改，代码内所有品牌/域名/路径均为配置项，不硬编码）
> 文档状态：设计基线 v1.0
> 适用范围：第一版「我方管理员后台 + 自持中转节点」

---

## 1. 业务边界（写进代码注释与界面文案的硬约束）

| 项 | 是 | 否 |
|---|---|---|
| 服务器归属 | 全部中转节点由我方持有，root 权限在我方 | 不交付服务器、配置文件、后台权限给客户 |
| 客户动作 | 只改自己业务域名的 DNS 解析 | 不做客户私有化部署、不做软件授权/防破解、不做计费、不做客户自助注册 |
| 承诺 | 提供真实状态、真实日志、可诊断、可回滚 | **不承诺永久不被封、不承诺固定加速比例、不承诺零中断** |
| 第一版范围 | 我方管理员后台（单管理实例 + 多节点 agent） | 通配符域名规则、HTTP/3、ECH 真实域名识别、内网穿透 |

**「连接数 / 请求数 / 流量」术语定义（界面上必须区分）**

| 术语 | 含义 | 数据来源 | 采样方式 |
|---|---|---|---|
| 当前连接数 | 此刻未关闭的 TCP 会话数（含长连接） | HAProxy stats socket `show stat` 的 `scur`（frontend/backend/server 三级） | 轮询即时值，**不依赖连接结束日志** |
| 连接速率 | 单位时间新建会话数 | stats `rate`（10s 滑动）+ 我们自己按采样差分计算的 1m/5m | 轮询 |
| 累计连接数 | 进程启动至今的会话总数 | stats `stot` | 轮询（注意：reload 后按进程重置或累加，见 §7.4） |
| 流量 | 字节数，分 `in`/`out` 方向 | stats `bin`/`bout`（frontend 视角：客户端侧收发） | 轮询 |
| 请求数 | HTTP 事务数 | **TCP 透传模式下不适用，界面显示「不适用（TCP 模式）」** | — |

⚠️ 关键区分：**连接数 ≠ 请求数 ≠ 流量**。TCP 透传下 HAProxy 不解析 HTTP，没有「请求数」这个概念；界面不得用连接数冒充请求数。

---

## 2. 组件与部署形态

```
                         ┌──────────────────────── 我方管理面 ────────────────────────┐
                         │                                                            │
   管理员浏览器 ──HTTPS──▶│  relay-mgrd (Go, 单实例)                                   │
                         │   ├─ Web API + 静态前端 (非 root 运行, user: shengyu-mgr)  │
                         │   ├─ 发布编排器 publish orchestrator (发布锁 + 版本号)      │
                         │   ├─ 日志接收 / 分片落盘 (logstore)                        │
                         │   ├─ 指标接收 / 时序分片 (metricstore)                     │
                         │   ├─ 诊断引擎 diagnose (规则分类，不臆测)                   │
                         │   ├─ DNS 调度控制器 dnsctl (单控制者租约)                   │
                         │   ├─ 探测执行器 probe (外部多位置)                          │
                         │   └─ SQLite: 业务配置 + 审计 (小)                          │
                         │      独立分片存储: 连接日志 / 指标 / 探测 (大)              │
                         └───────┬───────────────────────────┬────────────────────────┘
                                 │ mTLS (HTTP/2)             │ mTLS
                                 │ 双向证书认证               │
                    ┌────────────▼───────────┐   ┌───────────▼────────────┐
                    │ 节点 A (relay-agentd)  │   │ 节点 B (relay-agentd)  │
                    │  ├─ 日志接收 unix dgram │   │  ...                   │
                    │  ├─ 本地 spool 缓冲     │   │                        │
                    │  ├─ stats socket 轮询   │   │                        │
                    │  ├─ 系统指标采集 /proc  │   │                        │
                    │  └─ 节点侧探测          │   │                        │
                    │  relay-helper (root,    │   │                        │
                    │   socket-activated)     │   │                        │
                    │  ├─ haproxy -c 校验     │   │                        │
                    │  ├─ 原子发布 + 版本目录 │   │                        │
                    │  └─ systemctl reload    │   │                        │
                    │  shengyu-edgelink-haproxy (2.8)  │   │  shengyu-edgelink-haproxy (2.8) │
                    │   ├─ :443 SNI 透传      │   │                        │
                    │   └─ :<port> TCP 端口映射│  │                        │
                    └────────────────────────┘   └────────────────────────┘
```

### 2.1 允许同机部署

管理面与节点面**允许同机**：`relay-mgrd`、`relay-agentd`、`shengyu-edgelink-haproxy` 三个服务在同一台机器上互不冲突（不同用户、不同端口、不同配置目录）。架构上按「管理面 / 节点面」分层，后续加节点只是多注册一个 agent，管理面代码不变。

### 2.2 单实例 → 多实例演进点（现在就留好缝）

| 组件 | v1 | 演进方向 | 现在必须做对的事 |
|---|---|---|---|
| 配置库 | 单文件 SQLite（WAL） | PostgreSQL | 所有 SQL 走 `store` 接口；不写 SQLite 方言函数（如 `datetime('now')` 一律用 Go 传时间参数） |
| 发布锁 | SQLite 事务行锁 | 分布式锁（etcd/PG advisory lock） | 锁接口抽象为 `publish.Locker`，有 `TryAcquire/Release/Lease` |
| 日志存储 | 小时分片 SQLite | ClickHouse / Parquet+DuckDB | 写入与查询都走 `logstore` 接口，分片路径规则集中在一处 |
| DNS 单控制者 | SQLite 租约行 | etcd lease | `dnsctl.Lease` 接口，含 TTL 与续约语义 |
| 指标 | 小时分片 SQLite | VictoriaMetrics / Prometheus | `metricstore` 接口，写入是 append-only 批 |
| 探测点 | 管理面 + 节点面 | 多地域外部探测集群 | `probe.Executor` 接口，结果带 `location` 维度 |

---

## 3. 进程与权限模型（对应需求 §九）

| 进程 | 运行用户 | 能力 | 明确不能做的事 |
|---|---|---|---|
| `shengyu-edgelink` | `shengyu`（非 root） | 监听 `127.0.0.1:8081`（**默认值，可配**：`install.sh --listen` 或 `-listen`；旧文档写作 `relay-mgrd` / 8443，已过时，以代码为准）、读写 `/var/lib/shengyu-edgelink`（`meta.db` 与日志分片） | 不落 root、不执行 shell |
| `relay-agentd` | `shengyu`（非 root） | 绑定 unix dgram 日志 socket、读 stats socket、读 `/proc`、读写 `/var/lib/shengyu-edgelink/spool`、写 `/var/lib/shengyu-edgelink/staging`、通过 helper socket 请求特权动作 | **不直接写 `/etc/shengyu`**、不 reload 服务、不执行任意命令 |
| `relay-helper` | `root` | systemd socket 激活（`/run/shengyu-edgelink/helper.sock`，root:shengyu 0660），仅接受白名单结构化请求 | **只支持 5 个 op**（见下）；不接受 shell、不接受任意路径、不接受任意 argv |
| `shengyu-edgelink-haproxy` | `shengyu-edgelink-haproxy`（非 root，`NoNewPrivileges=yes`） | 绑定 443 / 业务端口、写 unix dgram 日志 socket、读 stats socket | 不读业务证书（透传模式下无需证书）、不写配置文件 |

### 3.1 helper 的特权动作白名单（**唯一**的特权入口）

请求是 JSON，走 unix socket，`Content-Length` 明确的消息帧，**不经过任何 shell**：

| op | 入参 | helper 内部实现（全部 `exec.Command(固定二进制, 固定 argv...)`） | 返回 |
|---|---|---|---|
| `ValidateConfig` | `{staging_relpath}` | 校验路径必须在 staging 根下（`filepath.Clean` + 前缀检查 + 拒绝符号链接）；`/usr/sbin/haproxy -c -q -f <abs>` | `{ok, exit_code, stdout, stderr, haproxy_version}` |
| `PublishConfig` | `{staging_relpath, version, node_id}` | 1) 再跑一次 `-c`；2) 版本目录落盘 + fsync；3) 原子 symlink 切换；4) `systemctl reload shengyu-edgelink-haproxy`；5) 自检（监听端口 + stats socket）；失败则自动回滚到上一版本并再自检 | `{ok, applied_version, rollback_performed, verify, error}` |
| `Rollback` | `{version}` | 目标版本必须存在于版本目录；symlink 切换 + reload + 自检 | 同上 |
| `GetStatus` | `{what}` ∈ {`haproxy`,`listeners`,`version`,`versions`} | `systemctl show shengyu-edgelink-haproxy --property=...`、读 `/proc/net/tcp{,6}` 匹配 inode、读 `current` symlink | 结构化状态 |
| `ListVersions` | `{}` | 列版本目录 + 元数据 | 版本列表 |

**禁止项（代码层面写死，不靠约定）**：
- 无 `sh -c`、无 `bash -c`、无 `system()`、无字符串拼接成命令；
- 路径必须 `filepath.Clean` 后校验前缀，且 `os.Lstat` 拒绝 symlink；
- `version` 必须是 `^v[0-9]{6,}$`；
- 所有 argv 元素是常量或经白名单校验的值；
- **不提供任何远程 Shell、文件读取、任意 systemctl 动作**。

### 3.2 平台 ↔ Agent 双向认证

- 管理面自带私有 CA（`relay-mgrd` 首次启动生成，私钥 0600，落在 `/var/lib/shengyu-edgelink/pki/mgr-ca.key`）。
- **注册令牌一次性**：`enrollment_tokens` 表存 `token_hash`(SHA-256)、`node_group`、`expires_at`、`used_at`、`revoked_at`。消费用 `UPDATE ... WHERE used_at IS NULL AND revoked_at IS NULL AND expires_at > now` 的**单语句原子消费**，`RowsAffected==1` 才算成功。
- Agent 首次 `enroll`：生成 EC P-256 密钥（私钥永不离开节点）→ 发 CSR + token → 管理面签 `CN=<node_id>` 客户端证书 → agent 写 `/etc/shengyu/pki/{agent.crt,agent.key,ca.crt}`（key 0600，owner `shengyu`）。
- 之后所有 agent → mgr、mgr → agent 的流量都是 mTLS。管理面按证书 CN 授权；吊销走 `revoked_serials` 表，握手时校验。
- 证书自动续期：到期前 1/3 时长的窗口内，agent 用现有 mTLS 调 `/agent/v1/renew`。
- **管理 API 不裸露**：mgrd 只监听一个端口，管理 UI/API 与 agent API 同端口不同路径前缀，用 mTLS 区分；可选 `admin_allow_cidr` 限制管理员来源。stats socket 与 helper socket 都是本机 unix socket，不做网络监听。

---

## 4. 两种接入模式的数据面实现

### 模式 A：TLS SNI 透传（共享入口端口，默认 `:443`）

> **端口不是固定 443**：入口端口取自数据库 `node_sni_entries.bind_port`，范围 **1–65535**。
> 443 只是推荐的默认值（也是 `EnsureDefaultSNIEntry` 会去查找的默认入口），
> 代码里没有"必须是 443"的常量。同机 443 已被别的程序占用时（例如 xray），
> 直接新建一个 8443 / 9443 入口即可，**无需迁移或停止那个程序**。
> 下方示例中的 `443`、`fe_sni_443` 是默认情形，实际会按端口渲染成 `fe_sni_<port>`。

```
frontend fe_sni_443
    mode tcp
    bind *:443
    # 1) 握手阶段把 SNI 抓进 txn 变量（会话生命周期内可读，不依赖结束时重读 ClientHello）
    tcp-request inspect-delay 5s
    #    用 req.ssl_sni：手册 7.3 的正式名。req_ssl_sni 是同义但已 deprecated 的早期写法。
    tcp-request content set-var(txn.sni) req.ssl_sni
    # 2) 明确只接受 TLS ClientHello，其余直接拒（不产生 backend 连接）
    tcp-request content reject unless { req_ssl_hello_type 1 }
    # 3) 未在允许清单中的 SNI 明确拒绝 —— 清单是渲染出来的独立文件，随版本目录原子切换
    tcp-request content reject unless { var(txn.sni) -m str -i -f /etc/shengyu-edgelink/haproxy/current/sni_allow.lst }
    # 4) 每条业务一条精确匹配的 backend 切换规则
    use_backend bk_b_000001 if { var(txn.sni) -m str -i a.example.com }
    use_backend bk_b_000002 if { var(txn.sni) -m str -i b.example.com }
    log-format <TCP 字段模板，见 docs/03>
    default_backend bk_unmatched_sni    # 兜底：不应到达（第 3 步已拦），仍保留以便日志定位
```

- **不在中转节点终止业务 TLS**：不写 `ssl`、不配 `crt`，全程字节透传。
- **未配置的 SNI 默认拒绝**：两道闸——非 ClientHello 直接 reject；SNI 不在清单直接 reject。
- **不允许按任意外部 SNI 自动解析任意目标**：不存在任何基于 SNI 的动态解析/DNS 查找；`use_backend` 表是发布时静态生成的，SNI 只做**精确等于**匹配（`-m str -i`），无通配、无后缀、无双正则。
- SNI 大小写：RFC 6066 规定 SNI 是 hostname，大小写不敏感，因此用 `-i`。

### 模式 B：TCP 端口映射（不要求 SNI）

```
frontend fe_tcp_<bizid>_<entryport>
    mode tcp
    bind <entry_addr>:<entry_port>
    default_backend bk_tcp_<bizid>
backend bk_tcp_<bizid>
    mode tcp
    server s1 <origin_addr>:<origin_port> check ...（健康探测可配）
```

普通 TCP 应用（不含 TLS、不发 SNI）走这里。

### 4.2 明确不支持的（界面必须显式提示）

| 不支持项 | 界面文案 | 原因 |
|---|---|---|
| 通配符域名规则 | 「当前版本只支持精确域名匹配，不支持 `*.example.com`」 | 精确匹配避免误路由与串流 |
| HTTP/3 (QUIC) | 「不支持 UDP/QUIC 转发」 | 内核需 UDP 且 SNI 在 QUIC 里加密，社区版无法按 SNI 路由 |
| ECH 真实域名识别 | 「若客户端启用 ECH，中转节点看不到真实域名，规则将落到默认拒绝」 | ECH 加密了 ClientHello 内层 SNI，透传模式下无解 |
| 内网穿透 | 「不提供内网穿透/反向建连」 | 边界外功能 |

---

## 5. 配置发布链路（对应需求 §五）

```
管理员提交变更
      │
      ▼
[1] 生成候选配置          render.Render(node, desiredState) -> 字节流 + 期望监听端口集
      │                   version = 该节点当前 max(version)+1（在发布事务内取，避免并发覆盖）
      ▼
[2] 静态校验              validate.Validate(): 域名语法/重复规则/监听冲突/目标回环/目标网段黑名单/
      │                   模式必填项/端口范围/超时阈值/健康探测参数
      │                   ✗ 失败 -> 直接返回，不产生版本、不落盘
      ▼
[3] 拿发布锁              publish.Locker.TryAcquire(node_id)
      │                   ✗ 已有发布在跑 -> 返回 409 + 当前发布者，排队（不并发覆盖）
      ▼
[4] 落 staging            mgr 把候选配置放到 node staging（agent 侧 /var/lib/shengyu-edgelink/staging/v<N>.cfg）
      ▼
[5] 节点语法校验          helper.ValidateConfig -> haproxy -c -q -f <staging>
      │                   ✗ 失败 -> 记 publish_record(失败,阶段=validate)，释放锁
      ▼
[6] 保存旧版本            版本目录 versions/<node>/v<N-1>/ 已存在（每版都留 cfg + allowlist + meta.json）
      ▼
[7] 原子替换 + 平滑 reload  helper.PublishConfig:
      │                   tmp 写入 -> fsync -> symlink 原子 rename -> systemctl reload shengyu-edgelink-haproxy
      │                   (HAProxy -Ws master-worker + USR2 平滑 reload，旧连接不中断)
      ▼
[8] 应用结果验证          verify.Apply(): 期望监听端口全部处于 LISTEN 且属于 haproxy 进程 /
      │                   stats socket 可应答 / 期望 frontend 与 backend 名在 show stat 中存在 /
      │                   版本号写入节点状态文件
      │                   ✗ 失败 -> 自动回滚到 v<N-1> + 再次自检 + 结果一并记录
      ▼
[9] 记账                  publish_record: 期望版本 / 节点实际版本 / 阶段结果 / 耗时 / 错误原文 / 操作审计
```

### 5.1 需求条款 → 实现映射

| 需求 | 实现 |
|---|---|
| 使用发布锁和版本号，避免并发覆盖 | `publish_locks` 表行 + `UNIQUE(node_id, version)`；版本号在锁内分配 |
| 验证全部预期监听端口和对应服务 | `verify` 用 `/proc/net/tcp{,6}` 取 LISTEN 项，映射 inode → pid，校验属于 haproxy 且端口集合完全一致（多一个少一个都算失败） |
| 区分「配置发布失败」和「源站业务本来就不可用」 | 发布验证**只验证数据面自身**（端口在听、stats 有 frontend、进程活着）；源站可达性是**独立**的 `probe` 结果，页面上分开呈现，不混为一谈 |
| 应用失败自动恢复旧配置，并验证恢复结果 | helper 内置回滚路径，回滚后同样跑一次 `verify`，两次结果都返回给 mgr |
| 支持查看历史版本及一键回滚 | 版本目录 + `ListVersions` + UI「回滚到此版本」→ helper `Rollback` |
| 常规规则更新不主动中断现有连接 | `-Ws` + `USR2` 平滑 reload：新 worker 继承监听 socket，老 worker 处理完存量连接后退出；**不做** `restart`，**不做** `-sf` 之外的一切粗暴手段 |
| 后台显示期望版本、节点实际版本、最近发布时间 | 节点状态文件 + `publish_record` 最新一条；UI 上两者不一致即高亮 `版本漂移` |
| 不通过 pkill 等方式影响无关进程 | 全链路只用 `systemctl reload shengyu-edgelink-haproxy`（固定 unit 名）；代码中不出现 `kill`/`pkill`/`killall` |

---

## 6. 节点侧韧性（对应需求 §二.7、§五）

```
/etc/shengyu-edgelink/haproxy/
├── current -> versions/node-A/v000042        # 原子切换点
└── versions/node-A/
    ├── v000041/{haproxy.cfg, sni_allow.lst, meta.json}
    └── v000042/{haproxy.cfg, sni_allow.lst, meta.json}
```

- systemd unit 的 `ExecStart` 指向 `current/haproxy.cfg`，**节点重启后 HAProxy 直接用最后有效配置起来**，不依赖管理面。
- `meta.json` 记 `version / published_at / publish_id / expected_listeners`，供 agent 上报「实际版本」。
- 保留最近 N 个版本（默认 20，可配），更老的按需清理。
- 管理面离线：转发不受影响；agent 继续本地收日志进 spool；恢复后补传（带 `seq` 缺口检测）。

---

## 7. 关键技术决策（ADR 摘要）

| # | 决策 | 备选 | 理由 |
|---|---|---|---|
| A1 | 转发内核统一 HAProxy **社区版 2.8 LTS** | Nginx stream / HAProxy 企业版 | 需求指定；社区版足够（SNI 透传、stats socket、平滑 reload 全是社区特性）；明确不用企业版特性（`stick-table` 的 `peers` 同步、runtime API 的写操作、`lua` 的商业模块等） |
| A2 | Go 写管理面与 Agent | Python / Java | 单二进制交付、交叉编译方便、并发模型适合流式日志与长连接 |
| A3 | **自研 root helper（socket 激活）** 作为唯一特权入口 | agent 直接以 root 跑 / sudoers 通配 | 需求 §九 要求 Web 服务非 root；sudoers 通配等于给 root shell；白名单 helper 可审计、可测试 |
| A4 | 配置 + 审计用 **SQLite**，连接日志/指标用**独立小时分片 SQLite** | 全塞一个 SQLite / 直接上 ClickHouse | 需求 §二.4 明确禁止把海量连接日志写业务 SQLite；分片方案 v1 零外部依赖，且天然满足「查询不扫全历史」与「按分区清理」 |
| A5 | 日志通道用 **unix datagram socket → agent** | rsyslog → 文件 → 采集器 / HAProxy `ring` | 无额外守护进程、无磁盘文件竞争；但**必须**解决「接收端阻塞会拖累 HAProxy」的问题 → agent 收包 goroutine 只做 append 到 spool，永不阻塞 |
| A6 | 发布验证**只验数据面自身**，源站可用性单独探测 | 把「转发是否通」并入发布验证 | 需求 §五明确要求区分；否则源站抖动会导致误回滚 |
| A7 | SNI 用 `set-var(txn.sni)` 在握手期捕获 | 日志里直接用 `%[ssl_fc_sni]` | 需求 §六 明确要求抓进会话生命周期变量；txn 变量在会话结束写日志时仍可读，语义最稳 |
| A8 | DNS 调度优先评估 **阿里云 GTM**，普通解析作为受控降级 | 只做普通解析程序化切换 | 需求 §十二.3 明确要求；普通解析无健康切换能力，程序化切换受 TTL 与解析器缓存限制，必须如实说明 |
| A9 | 自动调度控制器**单实例租约** | A/B 各自改 DNS | 需求 §十二.4 明确禁止多控制者 |
| A10 | 诊断结论只输出「已观察事实 + 原始日志 + 可能原因 + 下一步」，**不输出「已被封」** | 规则直接下结论 | 需求 §七明确禁止 |

---

## 8. 与需求章节的对应索引

| 需求章节 | 落点 |
|---|---|
| 一 业务边界 | §1、界面文案常量 |
| 二 技术方案 | §2、§3、ADR A1–A3、docs/04（版本基线） |
| 三 两种接入模式 | §4 |
| 四 业务管理字段 | docs/02 §2、`internal/model`、`internal/validate` |
| 五 配置发布可靠性 | §5、`internal/publish` |
| 六 日志与实时指标 | docs/03、`internal/logparse`、`internal/logstore`、`internal/metrics` |
| 七 诊断界面 | docs/03 §7、`internal/diagnose` |
| 八 日志容量控制 | docs/03 §8、`internal/logstore/retention.go` |
| 九 管理安全 | §3、`internal/pki`、`internal/helper` protobuf/JSON 协议 |
| 十 界面 | docs/05 §4、`web/` |
| 十一 必验场景 | docs/05 §5 验收矩阵 |
| 十二 双节点高可用 + 阿里云 DNS | docs/06 全篇 |
