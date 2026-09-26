# 02 · 数据模型

> 目标：把「业务配置 + 审计」与「海量连接日志 + 指标」在**存储层就分开**（需求 §二.4）。
> 约定：**所有 SQL 不写数据库方言函数**（时间一律由 Go 传参），以便后续换 PostgreSQL。

---

## 1. 存储布局

| 存储 | 路径 | 内容 | 体量特征 | 备份策略 |
|---|---|---|---|---|
| 配置库 | `/var/lib/shengyu-edgelink/db/config.db` | 客户/业务/路由/节点/版本/审计/令牌/DNS 配置/切换记录/设置 | 小（MB 级） | 每日全量 + WAL 归档 |
| 连接日志 | `/var/lib/shengyu-edgelink/logs/conn/YYYY/MM/DD/HH.db` | HAProxy 连接日志（小时分片） | 大 | 不备份（可重新产生） |
| 指标 | `/var/lib/shengyu-edgelink/logs/metric/YYYY/MM/DD/HH.db` | 节点/代理/系统指标（小时分片） | 中 | 不备份 |
| 探测 | `/var/lib/shengyu-edgelink/logs/probe/YYYY/MM/DD/HH.db` | 健康探测结果（小时分片） | 中 | 不备份 |
| 丢日志事件 | `config.db` 的 `log_loss_events` 表 | 采集丢失/延迟/配额事件 | 小 | 随配置库 |

分片文件名固定 `HH.db`（UTC 小时），**查询只打开时间范围内的分片**，从物理上保证「不扫全历史」。

---

## 2. 配置库 DDL（`migrations/001_init.sql`）

### 2.1 身份与权限

```sql
CREATE TABLE admin_users (
  id            TEXT PRIMARY KEY,           -- usr_<26位>
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,              -- argon2id
  role          TEXT NOT NULL,              -- owner | operator | auditor
  totp_secret   TEXT,                       -- 预留
  disabled      INTEGER NOT NULL DEFAULT 0,
  created_at    TEXT NOT NULL,              -- RFC3339 UTC，由 Go 传入
  updated_at    TEXT NOT NULL
);

CREATE TABLE admin_sessions (
  id           TEXT PRIMARY KEY,            -- session id（Cookie 里只放这个）
  user_id      TEXT NOT NULL REFERENCES admin_users(id),
  csrf_token   TEXT NOT NULL,
  client_ip    TEXT NOT NULL,
  user_agent   TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL,
  expires_at   TEXT NOT NULL,
  revoked_at   TEXT
);
CREATE INDEX idx_sessions_user ON admin_sessions(user_id, expires_at);

CREATE TABLE admin_api_tokens (            -- 供自动化/CI 调用，可选
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  token_hash  TEXT NOT NULL UNIQUE,        -- SHA-256
  scopes      TEXT NOT NULL,               -- JSON 数组
  created_at  TEXT NOT NULL,
  expires_at  TEXT,
  revoked_at  TEXT
);
```

### 2.2 客户与业务

```sql
CREATE TABLE customers (
  id         TEXT PRIMARY KEY,             -- cus_<26位>
  name       TEXT NOT NULL,
  contact    TEXT NOT NULL DEFAULT '',
  remark     TEXT NOT NULL DEFAULT '',
  enabled    INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX idx_customers_name ON customers(name);

CREATE TABLE businesses (
  id                TEXT PRIMARY KEY,      -- biz_<26位 Crockford Base32，时间有序，永不变
  customer_id       TEXT NOT NULL REFERENCES customers(id),
  name              TEXT NOT NULL,
  remark            TEXT NOT NULL DEFAULT '',
  mode              TEXT NOT NULL,         -- sni_tls | tcp_port
  enabled           INTEGER NOT NULL DEFAULT 1,

  -- 超时（毫秒），NULL 表示用节点默认
  connect_timeout_ms INTEGER,              -- 连接源站的超时
  client_timeout_ms  INTEGER,              -- 客户端空闲超时
  server_timeout_ms  INTEGER,              -- 源站空闲超时

  -- 可选连接/排队限制（NULL = 不限）
  maxconn            INTEGER,
  queue_limit        INTEGER,              -- 对应 HAProxy 的 maxqueue

  -- 可选健康探测（JSON，见 §3.4）
  healthcheck        TEXT NOT NULL DEFAULT '{}',

  primary_node_id   TEXT REFERENCES nodes(id),
  backup_node_id    TEXT REFERENCES nodes(id),   -- 可空；为空则无备用节点

  created_at        TEXT NOT NULL,
  updated_at        TEXT NOT NULL,
  CHECK (mode IN ('sni_tls','tcp_port')),
  CHECK (primary_node_id IS NULL OR backup_node_id IS NULL OR primary_node_id <> backup_node_id)
);
CREATE INDEX idx_biz_customer ON businesses(customer_id);
CREATE INDEX idx_biz_primary  ON businesses(primary_node_id);

-- SNI 模式下一条业务可挂多个域名（精确匹配，不支持通配符 —— 写入前校验）
CREATE TABLE business_domains (
  business_id TEXT NOT NULL REFERENCES businesses(id) ON DELETE CASCADE,
  domain      TEXT NOT NULL,
  created_at  TEXT NOT NULL,
  PRIMARY KEY (business_id, domain)
);
CREATE UNIQUE INDEX idx_domain_unique ON business_domains(domain);
```

> **域名唯一性说明**：`idx_domain_unique` 保证全球唯一（同一域名不允许挂两条业务）。若客户要求同一域名同时挂 A/B 两节点，那是**同一业务的两个 route**，不是两条 domain 记录。跨节点串流由此从数据模型层面被禁止。

### 2.3 节点与节点级入口

```sql
CREATE TABLE nodes (
  id              TEXT PRIMARY KEY,        -- nd_<26位>
  name            TEXT NOT NULL UNIQUE,
  group_name      TEXT NOT NULL DEFAULT 'default',
  agent_endpoint  TEXT NOT NULL,           -- https://<ip>:9443
  public_ipv4     TEXT,                    -- 该节点对外提供服务的 IP（DNS 切换目标）
  public_ipv6     TEXT,
  region          TEXT NOT NULL DEFAULT '',-- 例如 cn-shanghai / hk
  -- agent 侧事实（由 agent 心跳上报，管理面只读）
  agent_version   TEXT NOT NULL DEFAULT '',
  haproxy_version TEXT NOT NULL DEFAULT '',
  applied_version INTEGER NOT NULL DEFAULT 0,   -- 节点当前生效的配置版本
  last_heartbeat  TEXT,
  health          TEXT NOT NULL DEFAULT 'unknown', -- online|offline|degraded
  enabled         INTEGER NOT NULL DEFAULT 1,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL
);

-- SNI 共享入口：一台节点可配多个 SNI 入口端口（默认一个 443）
CREATE TABLE node_sni_entries (
  id         TEXT PRIMARY KEY,             -- sni_<26位>
  node_id    TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  bind_addr  TEXT NOT NULL DEFAULT '0.0.0.0',
  bind_port  INTEGER NOT NULL,
  enabled    INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  UNIQUE (node_id, bind_addr, bind_port),
  CHECK (bind_port BETWEEN 1 AND 65535)
);
```

### 2.4 路由（业务 × 节点 = 一份可发布的转发规则）

```sql
CREATE TABLE routes (
  id              TEXT PRIMARY KEY,        -- rt_<26位>
  business_id     TEXT NOT NULL REFERENCES businesses(id) ON DELETE CASCADE,
  node_id         TEXT NOT NULL REFERENCES nodes(id),
  role            TEXT NOT NULL,           -- primary | backup
  mode            TEXT NOT NULL,           -- 冗余存储，便于渲染时校验一致性

  sni_entry_id    TEXT REFERENCES node_sni_entries(id),  -- sni_tls 模式必填
  entry_addr      TEXT,                    -- tcp_port 模式必填
  entry_port      INTEGER,                 -- tcp_port 模式必填

  origin_host     TEXT NOT NULL,           -- 源站 IP（不接受域名，见 validate 规则）
  origin_port     INTEGER NOT NULL,
  origin_sni      TEXT,                    -- 可选：向源站发起的 SNI/Host 覆盖（极少用）

  queue_limit     INTEGER,                 -- 覆盖业务级
  maxconn         INTEGER,                 -- 覆盖业务级

  enabled         INTEGER NOT NULL DEFAULT 1,
  last_applied_version INTEGER NOT NULL DEFAULT 0,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL,

  UNIQUE (business_id, node_id),
  CHECK (role IN ('primary','backup')),
  CHECK (
    (mode = 'sni_tls'  AND sni_entry_id IS NOT NULL)
    OR
    (mode = 'tcp_port' AND entry_addr IS NOT NULL AND entry_port IS NOT NULL)
  ),
  CHECK (origin_port BETWEEN 1 AND 65535)
);
CREATE INDEX idx_routes_node ON routes(node_id, enabled);

-- TCP 监听冲突的唯一真理来源：一台节点上 (bind_addr, bind_port) 只能被一个启用的 TCP route 占用
CREATE UNIQUE INDEX idx_tcp_entry_unique
  ON routes(node_id, entry_addr, entry_port)
  WHERE mode = 'tcp_port' AND enabled = 1;
```

**为什么 SNI 模式不需要这个唯一索引**：SNI 入口的 `(addr,port)` 唯一性在 `node_sni_entries` 上，多条 SNI route 共享它，靠 SNI 精确匹配分流 —— 这正是「多域名共享 443」。

### 2.5 版本、发布、锁

```sql
CREATE TABLE config_versions (
  id            TEXT PRIMARY KEY,          -- ver_<26位>
  node_id       TEXT NOT NULL REFERENCES nodes(id),
  version       INTEGER NOT NULL,          -- 单调递增，节点内唯一
  content_hash  TEXT NOT NULL,             -- cfg + allowlist 合并的 SHA-256
  cfg_bytes     INTEGER NOT NULL,
  expected_listeners TEXT NOT NULL,        -- JSON: [{"addr":"0.0.0.0","port":443}, ...]
  object_names  TEXT NOT NULL,             -- JSON: {"frontends":[...],"backends":[...]}
  publish_id    TEXT,
  published_at  TEXT NOT NULL,
  UNIQUE (node_id, version)
);

CREATE TABLE publish_records (
  id            TEXT PRIMARY KEY,          -- pub_<26位>
  node_id       TEXT NOT NULL REFERENCES nodes(id),
  version       INTEGER NOT NULL,
  kind          TEXT NOT NULL,             -- publish | rollback | auto_rollback
  stage         TEXT NOT NULL,             -- render|validate|stage|push|apply|verify|done|failed
  ok            INTEGER NOT NULL DEFAULT 0,
  expected_version INTEGER NOT NULL,
  observed_version INTEGER,                -- 完成后从节点读回
  error         TEXT NOT NULL DEFAULT '',  -- 原始错误文本，不改写
  validate_out  TEXT NOT NULL DEFAULT '',
  verify_detail TEXT NOT NULL DEFAULT '',  -- JSON：端口自检逐项结果
  rollback_performed INTEGER NOT NULL DEFAULT 0,
  rollback_ok   INTEGER,
  duration_ms   INTEGER NOT NULL DEFAULT 0,
  actor_user_id TEXT REFERENCES admin_users(id),
  actor_ip      TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL
);
CREATE INDEX idx_pub_node_time ON publish_records(node_id, created_at DESC);

CREATE TABLE publish_locks (
  node_id     TEXT PRIMARY KEY REFERENCES nodes(id),
  holder      TEXT NOT NULL,               -- pub_<26位>
  acquired_at TEXT NOT NULL,
  expires_at  TEXT NOT NULL                -- 看门狗超时，防死锁
);
```

### 2.6 审计（对应需求 §六.C）

```sql
CREATE TABLE audit_entries (
  id            TEXT PRIMARY KEY,          -- aud_<26位>
  ts            TEXT NOT NULL,
  actor_user_id TEXT,
  actor_name    TEXT NOT NULL DEFAULT '',  -- 冗余存储，用户改名/删除后审计仍可读
  actor_ip      TEXT NOT NULL DEFAULT '',
  action        TEXT NOT NULL,             -- login.success / login.fail / business.create / publish.apply / rollback / node.enroll / dns.update / ...
  target_type   TEXT NOT NULL DEFAULT '',  -- business | node | route | group | user | token | dns_record
  target_id     TEXT NOT NULL DEFAULT '',
  summary       TEXT NOT NULL,             -- 人类可读变更摘要
  detail        TEXT NOT NULL DEFAULT '{}',-- JSON diff（已脱敏）
  result        TEXT NOT NULL,             -- success | failure
  error         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_audit_ts     ON audit_entries(ts DESC);
CREATE INDEX idx_audit_target ON audit_entries(target_type, target_id, ts DESC);
```

**脱敏规则（写入前执行，不是查询时）**：`detail` 的序列化走 `redact.Sanitize()`，键名命中 `password|passwd|secret|token|api_key|access_key|private|authorization|cookie|session|dsn|connection_string` 或值形如私钥 PEM/bearer 的一律替换为 `"<redacted>"`。密码/令牌/私钥**在任何表中都不存明文**（令牌只存 SHA-256）。

### 2.7 注册令牌与吊销

```sql
CREATE TABLE enrollment_tokens (
  id          TEXT PRIMARY KEY,            -- tok_<26位>
  token_hash  TEXT NOT NULL UNIQUE,        -- SHA-256(明文 token)，明文只在创建响应里出现一次
  label       TEXT NOT NULL DEFAULT '',
  node_group  TEXT NOT NULL DEFAULT 'default',
  created_by  TEXT REFERENCES admin_users(id),
  created_at  TEXT NOT NULL,
  expires_at  TEXT NOT NULL,
  used_at     TEXT,
  used_by_node TEXT REFERENCES nodes(id),
  revoked_at  TEXT
);

CREATE TABLE revoked_serials (
  serial_hex  TEXT PRIMARY KEY,
  node_id     TEXT NOT NULL,
  reason      TEXT NOT NULL DEFAULT '',
  revoked_at  TEXT NOT NULL
);
```

### 2.8 双节点高可用与 DNS 调度

```sql
CREATE TABLE business_groups (
  id                 TEXT PRIMARY KEY,     -- grp_<26位>
  name               TEXT NOT NULL UNIQUE,
  remark             TEXT NOT NULL DEFAULT '',
  preferred_node_id  TEXT REFERENCES nodes(id),   -- 首选节点 A/B
  auto_failover      INTEGER NOT NULL DEFAULT 0,  -- 自动故障切换
  auto_failback      INTEGER NOT NULL DEFAULT 0,  -- 自动回切（默认关）
  fail_threshold     INTEGER NOT NULL DEFAULT 3,  -- 连续失败阈值
  recover_threshold  INTEGER NOT NULL DEFAULT 3,  -- 恢复阈值
  recover_observe_s  INTEGER NOT NULL DEFAULT 600,-- 恢复观察时间(秒)
  cooldown_s         INTEGER NOT NULL DEFAULT 300,-- 切换冷却时间(秒)
  scheduler_mode     TEXT NOT NULL DEFAULT 'manual', -- manual | platform_only
  created_at         TEXT NOT NULL,
  updated_at         TEXT NOT NULL,
  CHECK (scheduler_mode IN ('manual','platform_only'))
);

CREATE TABLE group_members (
  group_id    TEXT NOT NULL REFERENCES business_groups(id) ON DELETE CASCADE,
  business_id TEXT NOT NULL REFERENCES businesses(id) ON DELETE CASCADE,
  PRIMARY KEY (group_id, business_id)
);

-- 普通云解析的被管控记录：精确绑定到 RecordId + 类型 + 解析线路
CREATE TABLE dns_records (
  id                TEXT PRIMARY KEY,      -- dns_<26位>
  group_id          TEXT NOT NULL REFERENCES business_groups(id) ON DELETE CASCADE,
  provider          TEXT NOT NULL,         -- aliyun_alidns | aliyun_gtm(预留)
  domain            TEXT NOT NULL,         -- 例如 a.example.com（主域 example.com 单独存）
  zone              TEXT NOT NULL,         -- example.com
  record_type       TEXT NOT NULL,         -- A | AAAA | CNAME
  provider_record_id TEXT NOT NULL,        -- 阿里云 RecordId
  line              TEXT NOT NULL DEFAULT 'default',  -- 解析线路 RR：default / cn_telecom / ...
  ttl               INTEGER NOT NULL,
  managed           INTEGER NOT NULL DEFAULT 1,       -- 是否由平台接管（关则只读）
  desired_node_id   TEXT REFERENCES nodes(id),        -- 期望指向的节点
  desired_value     TEXT,                              -- 期望记录值（预计算）
  last_seen_value   TEXT,                              -- 平台最后一次读到的值（漂移检测基准）
  last_seen_at      TEXT,
  last_synced_at    TEXT,
  created_at        TEXT NOT NULL,
  updated_at        TEXT NOT NULL,
  UNIQUE (provider, provider_record_id)
);
CREATE INDEX idx_dns_group ON dns_records(group_id);

CREATE TABLE dns_change_log (
  id             TEXT PRIMARY KEY,         -- dnslog_<26位>
  dns_record_id  TEXT NOT NULL REFERENCES dns_records(id),
  ts             TEXT NOT NULL,
  trigger        TEXT NOT NULL,            -- manual | auto_failover | auto_failback | drift_fix | reconcile
  actor_user_id  TEXT,
  before_value   TEXT NOT NULL DEFAULT '',
  before_ttl     INTEGER,
  after_value    TEXT NOT NULL DEFAULT '',
  after_ttl      INTEGER,
  provider_request_id TEXT NOT NULL DEFAULT '',   -- 阿里云 RequestId，原样记录
  provider_code  TEXT NOT NULL DEFAULT '',        -- API 返回 Code
  result         TEXT NOT NULL,                   -- accepted | authoritative_updated | verified | failed | skipped_conflict
  detail         TEXT NOT NULL DEFAULT '{}',
  switch_event_id TEXT
);
CREATE INDEX idx_dnslog_rec ON dns_change_log(dns_record_id, ts DESC);

CREATE TABLE switch_events (
  id             TEXT PRIMARY KEY,         -- sw_<26位>
  group_id       TEXT NOT NULL REFERENCES business_groups(id),
  ts             TEXT NOT NULL,
  from_node_id   TEXT REFERENCES nodes(id),
  to_node_id     TEXT REFERENCES nodes(id),
  trigger        TEXT NOT NULL,            -- manual | auto
  actor_user_id  TEXT,
  reason         TEXT NOT NULL,            -- 人类可读原因
  evidence       TEXT NOT NULL DEFAULT '{}',  -- 切换前探测证据（逐探测点结果快照）
  config_versions TEXT NOT NULL DEFAULT '{}', -- {"nd_A":42,"nd_B":42}
  dns_result     TEXT NOT NULL DEFAULT '{}',
  authoritative_check TEXT NOT NULL DEFAULT '{}', -- 权威 NS 直查结果
  external_verify    TEXT NOT NULL DEFAULT '{}',  -- 外部探测点验证
  retry_count    INTEGER NOT NULL DEFAULT 0,
  manual_intervention INTEGER NOT NULL DEFAULT 0,
  created_at     TEXT NOT NULL
);
CREATE INDEX idx_switch_group ON switch_events(group_id, ts DESC);

CREATE TABLE scheduler_lease (       -- 全局唯一一行，保证任一时刻只有一个自动调度控制者
  name        TEXT PRIMARY KEY,      -- 固定值 'dns_scheduler'
  holder      TEXT NOT NULL,         -- 控制者实例 ID
  acquired_at TEXT NOT NULL,
  expires_at  TEXT NOT NULL,
  heartbeat_at TEXT NOT NULL
);
```

### 2.9 参数与采集状态

```sql
CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
-- 关键默认值：
--   retention.conn_days            = 7
--   retention.audit_days           = 90
--   retention.metric_days          = 7
--   retention.probe_days           = 14
--   quota.conn_bytes               = 21474836480   (20 GiB)
--   quota.metric_bytes             = 4294967296    (4 GiB)
--   quota.probe_bytes              = 2147483648    (2 GiB)
--   compress.partitions            = true
--   logstore.raw_line_keep         = true
--   logstore.raw_line_max_bytes    = 1024
--   ingest.batch_max_rows          = 5000
--   ingest.batch_max_bytes         = 4194304

CREATE TABLE ingest_cursors (        -- 采集连续性与缺口检测（对应需求 §八「避免静默丢失」）
  node_id      TEXT NOT NULL REFERENCES nodes(id),
  stream       TEXT NOT NULL,        -- conn | metric | probe
  last_seq     INTEGER NOT NULL DEFAULT 0,
  last_recv_at TEXT,
  gap_count    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (node_id, stream)
);

CREATE TABLE log_loss_events (
  id        TEXT PRIMARY KEY,
  ts        TEXT NOT NULL,
  node_id   TEXT NOT NULL,
  stream    TEXT NOT NULL,
  kind      TEXT NOT NULL,   -- dgram_rxq_ovfl | spool_overflow | seq_gap | parse_error | disk_quota | ingest_lag
  count     INTEGER NOT NULL DEFAULT 1,
  detail    TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX idx_loss_ts ON log_loss_events(ts DESC);
```

---

## 3. 连接日志分片 DDL（`logs/conn/YYYY/MM/DD/HH.db`）

```sql
CREATE TABLE conn_log (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  ts             TEXT    NOT NULL,     -- 会话结束时间（UTC RFC3339，毫秒）
  accept_ts      TEXT    NOT NULL,     -- 会话建立时间（同一会话与 ts 可能差很久 → 长连接诊断关键）
  node_id        TEXT    NOT NULL,
  business_id    TEXT    NOT NULL,     -- 从 be_name 反解，解析失败则 'unknown'
  route_id       TEXT    NOT NULL,
  config_version INTEGER NOT NULL,     -- 由渲染期字面量写入 log-format → 精确到"服务该连接的 worker 用的哪版配置"
  conn_id        TEXT    NOT NULL DEFAULT '',  -- HAProxy 无原生连接ID，用 采样序号+启动ID 组合，不可用时置空

  client_ip      TEXT    NOT NULL,
  client_port    INTEGER NOT NULL,
  entry_addr     TEXT    NOT NULL DEFAULT '',
  entry_port     INTEGER NOT NULL DEFAULT 0,
  sni            TEXT    NOT NULL DEFAULT '',  -- ★ 握手期捕获，见 docs/03 §3

  be_name        TEXT    NOT NULL DEFAULT '',
  srv_name       TEXT    NOT NULL DEFAULT '',
  origin_addr    TEXT    NOT NULL DEFAULT '',
  origin_port    INTEGER NOT NULL DEFAULT 0,

  queue_ms       INTEGER,              -- %Tq 排队时间
  connect_ms     INTEGER,              -- %Tc 源站 TCP 连接耗时
  session_ms     INTEGER,              -- %Tt 会话总时长
  bytes_in       INTEGER,              -- 客户端→中转（明确方向：in = 来自客户端）
  bytes_out      INTEGER,              -- 中转→客户端
  term_code      TEXT    NOT NULL DEFAULT '',  -- HAProxy 原始终止状态码（%tsc），不可用则空
  term_reason    TEXT    NOT NULL DEFAULT '',  -- 原始错误串（fc_err_str / sc_err_str）
  retries        INTEGER,              -- %rc
  queue_max      INTEGER,              -- %bq 曾经到达的最大队列深度（不是平均）

  parse_ok       INTEGER NOT NULL DEFAULT 1,
  parse_err      TEXT    NOT NULL DEFAULT '',
  raw_line       TEXT    NOT NULL DEFAULT '',  -- 截断到 logstore.raw_line_max_bytes
  ingest_batch   TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX idx_conn_ts      ON conn_log(ts DESC);
CREATE INDEX idx_conn_biz     ON conn_log(business_id, ts DESC);
CREATE INDEX idx_conn_client  ON conn_log(client_ip, ts DESC);
CREATE INDEX idx_conn_sni     ON conn_log(sni, ts DESC);
CREATE INDEX idx_conn_ver     ON conn_log(config_version);
CREATE INDEX idx_conn_origin  ON conn_log(origin_addr, origin_port, ts DESC);
```

### 3.1 字段可用性矩阵（**不可用的字段显示「不可用」，绝不伪造**）

图例：✅ 高置信（社区版 2.8 有文档支撑）｜🔶 待实测确认（由 `scripts/verify-log-fields.sh` 在真节点上验证）｜❌ 不可用

| 需求字段 | 表列 | HAProxy 来源 | 状态 | 说明 |
|---|---|---|---|---|
| 时间 | `ts` / `end_ts` | 别名 **`%T`**（accept date，GMT，含毫秒） + 平台记录的落盘时间 | ✅ | TCP 日志在会话结束时才产生，两个时间都要留，且**不可混用**：分片与查询按 `end_ts`，`ts` 只表示「HAProxy 什么时候接住这条连接」。历史上这里写成 `%[accept_date]` —— 它不是 sample fetch，`haproxy -c` 会直接报错（见 docs/07 §1.2） |
| 节点 ID | `node_id` | 渲染期字面量写入 `log-format` | ✅ | 不依赖 HAProxy 能力 |
| 业务 ID | `business_id` | `%b`（backend 名 `bk_<bizid>`）反解 | ✅ | 命名规则见 §4 |
| 配置版本 | `config_version` | 渲染期字面量写入 `log-format` | ✅ | reload 期间新旧 worker 各写自己的版本 → 语义精确 |
| 连接关联 ID | `conn_id`（`cid`） | HAProxy 无原生稳定连接 ID | 🔶 | 方案：平台合成 `<node>-<fe>-<%T 毫秒>-<ci>-<cp>-<fi>-<fp>`；否则置空并在 UI 标注「不可用」。社区版 `gpc0` 属 stick-table 机制（需额外 `table` 声明），不使用 |
| 客户端 IP/端口 | `client_ip`/`client_port` | `%ci` / `%cp` | ✅ | |
| 入口地址/端口 | `entry_addr`/`entry_port` | `%fi` / `%fp` | 🔶 | 需确认 `%fi` 在 TCP 自定义 log-format 下取的是"客户端所连的目的地址" |
| 握手期 SNI | `sni` | `%[var(txn.sni),json(utf8s)]`（由 **`req.ssl_sni`** 在握手期 set-var） | 🔶 | 变量在会话生命周期内保持；实测确认「会话结束后仍可读」。`req_ssl_sni` 是已弃用的早期别名，不再产出；转义必须用 `json(utf8s)`（2.8 没有 `json_escape`） |
| 实际 backend/server | `be_name`/`srv_name` | `%b` / `%s` | ✅ | |
| 目标地址 | `origin_addr`/`origin_port` | `%[dst]` / `%[dst_port]`，或由 routes 表反查 | ✅(反查) / 🔶(直采) | **兜底方案**：用 `srv_name`→routes 反查真实源站，一定可得 |
| 排队时间 | `queue_ms` | `%Tq` | ✅ | |
| 源站 TCP 连接耗时 | `connect_ms` | `%Tc` | ✅ | |
| 会话时长 | `session_ms` | `%Tt` | ✅ | |
| 传输字节（带方向） | `bytes_in`/`bytes_out` | `%B`（读取字节）+ `%[bout]`？ | 🔶 | TCP 日志格式默认只给 `%B`（客户端→服务端）。**方向必须显式标注**，缺失的一侧显示「不可用」 |
| 原始终止状态码 | `term_code` | `%tsc` | 🔶 | 需确认 TCP 自定义 log-format 下可用；不可用则留空 |
| 错误原文 | `term_reason` | `%[fc_err_str]` / `%[sc_err_str]` / `%[bc_err_str]` | 🔶 | 诊断靠它区分「源站连不上」与「客户端异常断开」 |
| 重试次数 | `retries` | `%rc` | ✅ | |
| 队列信息 | `queue_max` | `%bq`（后端队列峰值） | ✅ | |
| 实时未结束长连接 | — | **不来自日志**，来自 stats socket `scur`/`qcur` | ✅ | 需求 §六.D 明确要求 |

> 🔶 项的处理：`internal/haproxy/fieldreg` 里每个字段带 `Status` 与 `Expr`，`scripts/verify-log-fields.sh` 产出的 `field-availability.json` 会覆盖状态；UI 与导出对 `unavailable` 字段统一显示「不可用」，CSV 导出留空而不是 0。

---

## 4. HAProxy 对象命名规则（**禁止用域名拼接**）

| 对象 | 规则 | 例子 |
|---|---|---|
| SNI frontend | `fe_sni_<bind_port>` | `fe_sni_443` |
| TCP frontend | `fe_tcp_<bizshort>` | `fe_tcp_b7k3m9q2` |
| backend | `bk_<bizshort>` | `bk_b7k3m9q2` |
| server | `s1` | `s1` |
| SNI 允许清单文件 | `sni_allow.lst`（版本目录内） | — |
| 版本目录 | `versions/<node_id>/v%06d` | `versions/nd_7F2K.../v000042` |

`bizshort` = 业务 ID 去掉 `biz_` 前缀后转小写、只保留 `[a-z0-9]`、截断 16 位。**域名从不出现在任何对象名里** —— 域名只出现在 `use_backend` 的条件表达式与 `sni_allow.lst` 中。

> 双节点场景下 `bizshort` 相同（同一业务在 A/B 上 backend 同名），便于对比两节点的 stats 与日志。

---

## 5. 分片与保留策略参数

| 类别 | 分片粒度 | 默认保留 | 磁盘配额 | 超限动作 | 压缩 |
|---|---|---|---|---|---|
| 连接日志 | 1 小时 | 7 天（可配） | 20 GiB（可配） | 删最旧分片 → 记 `log_loss_events(disk_quota)` → 页面告警 | 关分片后 `zstd` 压缩为 `.db.zst` |
| 指标 | 1 小时 | 7 天 | 4 GiB | 同上 | 同上 |
| 探测 | 1 小时 | 14 天 | 2 GiB | 同上 | 同上 |
| 审计 | 在主库 | 90 天（可配） | — | 按 `ts` 删除，**删除前记一条审计** | — |

保留期与配额都可在界面改（`settings` 表），改完立即生效（清理任务下一轮读取新值）。

---

## 6. 数据流概览

```
HAProxy ──unix dgram──▶ agent 收包 goroutine ──▶ 本地 spool（追加写，有界）
                                                    │
                                    agent 发送器 ◀──┘（按批次，带 seq）
                                                    │ mTLS POST /agent/v1/logs
                                                    ▼
                              mgrd 摄入 ──▶ 解析+富化 ──▶ logstore 按 ts 落入小时分片
                                                    │
                                                    ├─▶ 缺口检测 ──▶ ingest_cursors / log_loss_events
                                                    └─▶ 审计无关（连接日志不含操作）
```

指标路径独立：agent 轮询 stats socket + 读 `/proc` → 批次上报 → `metric/YYYY/MM/DD/HH.db`。
**实时在线连接不经过日志**：agent 把 `show stat` 的 `scur/qcur/rate/bin/bout` 做成心跳的一部分高频上报（默认 5s），页面「当前连接数」直接用这个数。
