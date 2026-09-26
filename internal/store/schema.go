package store

// schemaSQL 是管理面元数据库的完整 DDL（幂等）。
//
// 设计原则（对应需求 §二.4：不把海量连接日志、时序指标写进业务 SQLite）：
//   - 本库只存**配置与审计**：客户、节点、业务、路由、配置版本、发布记录、账号、会话、令牌；
//   - 连接日志与指标走 logstore 的小时分片，物理隔离，本库不建对应表；
//   - 所有时间统一用 TEXT + RFC3339Nano（UTC），SQLite 无原生时间类型，
//     用固定宽度格式保证字符串排序 == 时间排序，索引才有效。
//
// 关于 JSON 列：domains / capabilities / healthcheck 等是**天然列表或结构**，
// 拆表会带来大量 join 且无查询价值（我们从不按"域名列表里的第 n 项"检索）。
// 需要检索的维度（business_id、node_id、enabled）都单独成列并建索引。
const schemaSQL = `
-- ============ 元信息 ============
CREATE TABLE IF NOT EXISTS meta (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);

-- ============ 账号与会话（需求 §九） ============
-- 口令用 PBKDF2-HMAC-SHA256（见 internal/auth），存 salt + iterations 以便日后提升强度。
CREATE TABLE IF NOT EXISTS users (
  id            TEXT PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  salt          TEXT NOT NULL,
  iterations    INTEGER NOT NULL,
  role          TEXT NOT NULL DEFAULT 'admin',
  enabled       INTEGER NOT NULL DEFAULT 1,
  ip_allowlist  TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL,
  last_login_at TEXT NOT NULL DEFAULT ''
);

-- 会话令牌只存哈希：库被读走也无法直接用 token 登录。
CREATE TABLE IF NOT EXISTS sessions (
  id         TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash TEXT NOT NULL UNIQUE,
  csrf_hash  TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  last_seen  TEXT NOT NULL,
  client_ip  TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ix_sessions_expires ON sessions(expires_at);

-- 节点注册令牌：一次性、可撤销、可轮换（需求 §九）。
CREATE TABLE IF NOT EXISTS agent_tokens (
  id         TEXT PRIMARY KEY,
  node_id    TEXT NOT NULL,
  token_hash TEXT NOT NULL UNIQUE,
  note       TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  used_at    TEXT NOT NULL DEFAULT '',
  revoked_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ix_agent_tokens_node ON agent_tokens(node_id);

-- 节点长期凭据：注册令牌消费成功后派生，用于后续心跳/拉配置/上报日志。
--
-- 为什么要和 agent_tokens 分开：
--   - 注册令牌是**一次性**的，用完必须废掉（否则泄露一个就能注册任意节点）；
--   - 长期凭据要能单独轮换与撤销（节点被回收时只需撤销这一条）；
--   - 两者生命周期完全不同，混在一张表里迟早会有人把注册令牌当长期凭据用。
CREATE TABLE IF NOT EXISTS node_credentials (
  id         TEXT PRIMARY KEY,
  node_id    TEXT NOT NULL,
  cred_hash  TEXT NOT NULL UNIQUE,
  note       TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  revoked_at TEXT NOT NULL DEFAULT '',
  last_used  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ix_node_cred_node ON node_credentials(node_id);

-- ============ 客户 / 节点 ============
CREATE TABLE IF NOT EXISTS customers (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  contact    TEXT NOT NULL DEFAULT '',
  remark     TEXT NOT NULL DEFAULT '',
  enabled    INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS nodes (
  id               TEXT PRIMARY KEY,
  name             TEXT NOT NULL,
  group_name       TEXT NOT NULL DEFAULT '',
  agent_endpoint   TEXT NOT NULL DEFAULT '',
  public_ipv4      TEXT NOT NULL DEFAULT '',
  public_ipv6      TEXT NOT NULL DEFAULT '',
  region           TEXT NOT NULL DEFAULT '',
  agent_version    TEXT NOT NULL DEFAULT '',
  haproxy_version  TEXT NOT NULL DEFAULT '',
  applied_version  INTEGER NOT NULL DEFAULT 0,
  expected_version INTEGER NOT NULL DEFAULT 0,
  last_heartbeat   TEXT NOT NULL DEFAULT '',
  health           TEXT NOT NULL DEFAULT 'unknown',
  health_detail    TEXT NOT NULL DEFAULT '',
  capabilities     TEXT NOT NULL DEFAULT '{}',
  enabled          INTEGER NOT NULL DEFAULT 1,
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL
);

-- 共享 SNI 入口（需求 §三 模式 A：多域名共享一个入口 IP:443）
CREATE TABLE IF NOT EXISTS sni_entries (
  id         TEXT PRIMARY KEY,
  node_id    TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  bind_addr  TEXT NOT NULL DEFAULT '',
  bind_port  INTEGER NOT NULL,
  enabled    INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_sni_entry_bind ON sni_entries(node_id, bind_addr, bind_port);

-- ============ 业务与路由 ============
CREATE TABLE IF NOT EXISTS businesses (
  id                 TEXT PRIMARY KEY,
  customer_id        TEXT NOT NULL REFERENCES customers(id),
  name               TEXT NOT NULL,
  remark             TEXT NOT NULL DEFAULT '',
  mode               TEXT NOT NULL,
  enabled            INTEGER NOT NULL DEFAULT 1,
  connect_timeout_ms INTEGER NOT NULL DEFAULT 0,
  client_timeout_ms  INTEGER NOT NULL DEFAULT 0,
  server_timeout_ms  INTEGER NOT NULL DEFAULT 0,
  maxconn            INTEGER NOT NULL DEFAULT 0,
  queue_limit        INTEGER NOT NULL DEFAULT 0,
  healthcheck        TEXT NOT NULL DEFAULT '{}',
  primary_node_id    TEXT NOT NULL DEFAULT '',
  backup_node_id     TEXT NOT NULL DEFAULT '',
  domains            TEXT NOT NULL DEFAULT '[]',
  created_at         TEXT NOT NULL,
  updated_at         TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_biz_customer ON businesses(customer_id);
CREATE INDEX IF NOT EXISTS ix_biz_primary  ON businesses(primary_node_id);
CREATE INDEX IF NOT EXISTS ix_biz_backup   ON businesses(backup_node_id);

CREATE TABLE IF NOT EXISTS routes (
  id                   TEXT PRIMARY KEY,
  business_id          TEXT NOT NULL REFERENCES businesses(id) ON DELETE CASCADE,
  node_id              TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  role                 TEXT NOT NULL,
  mode                 TEXT NOT NULL,
  sni_entry_id         TEXT NOT NULL DEFAULT '',
  entry_addr           TEXT NOT NULL DEFAULT '',
  entry_port           INTEGER NOT NULL DEFAULT 0,
  origin_host          TEXT NOT NULL,
  origin_port          INTEGER NOT NULL,
  origin_sni           TEXT NOT NULL DEFAULT '',
  queue_limit          INTEGER NOT NULL DEFAULT 0,
  maxconn              INTEGER NOT NULL DEFAULT 0,
  enabled              INTEGER NOT NULL DEFAULT 1,
  last_applied_version INTEGER NOT NULL DEFAULT 0,
  created_at           TEXT NOT NULL,
  updated_at           TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_route_biz_node ON routes(business_id, node_id);
CREATE INDEX IF NOT EXISTS ix_route_node ON routes(node_id);
-- TCP 模式下「同一入口地址:端口只能被一条路由占用」，用部分唯一索引在库层面兜底；
-- SNI 模式共享 443，不参与该约束。
CREATE UNIQUE INDEX IF NOT EXISTS ux_route_tcp_entry
  ON routes(node_id, entry_addr, entry_port)
  WHERE mode = 'tcp_port';

-- ============ 配置版本与发布（需求 §五） ============
CREATE TABLE IF NOT EXISTS config_versions (
  id                  TEXT PRIMARY KEY,
  node_id             TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  version             INTEGER NOT NULL,
  content_hash        TEXT NOT NULL,
  status              TEXT NOT NULL,
  dir_path            TEXT NOT NULL DEFAULT '',
  expected_listeners  TEXT NOT NULL DEFAULT '[]',
  route_count         INTEGER NOT NULL DEFAULT 0,
  note                TEXT NOT NULL DEFAULT '',
  object_names        TEXT NOT NULL DEFAULT '{}',
  created_by          TEXT NOT NULL DEFAULT '',
  created_at          TEXT NOT NULL,
  activated_at        TEXT NOT NULL DEFAULT '',
  error               TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_cfgver_node_ver ON config_versions(node_id, version);
CREATE INDEX IF NOT EXISTS ix_cfgver_node_created ON config_versions(node_id, created_at DESC);
-- 同一 hash 在同一节点上只应存在一次：如果重复，说明版本号在无意义地增长。
CREATE INDEX IF NOT EXISTS ix_cfgver_node_hash ON config_versions(node_id, content_hash);

CREATE TABLE IF NOT EXISTS releases (
  id            TEXT PRIMARY KEY,
  node_id       TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  action        TEXT NOT NULL,
  from_version  INTEGER NOT NULL DEFAULT 0,
  to_version    INTEGER NOT NULL DEFAULT 0,
  status        TEXT NOT NULL,
  phase         TEXT NOT NULL DEFAULT '',
  result        TEXT NOT NULL DEFAULT '',
  detail        TEXT NOT NULL DEFAULT '',
  actor         TEXT NOT NULL DEFAULT '',
  started_at    TEXT NOT NULL,
  finished_at   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ix_release_node_started ON releases(node_id, started_at DESC);

-- ============ 审计（需求 §六.C） ============
-- 只记录「做了什么」，绝不记录口令/令牌/私钥。写入前统一过 redact 包。
CREATE TABLE IF NOT EXISTS audit (
  id          TEXT PRIMARY KEY,
  ts          TEXT NOT NULL,
  actor       TEXT NOT NULL DEFAULT '',
  actor_ip    TEXT NOT NULL DEFAULT '',
  action      TEXT NOT NULL,
  target_type TEXT NOT NULL DEFAULT '',
  target_id   TEXT NOT NULL DEFAULT '',
  summary     TEXT NOT NULL DEFAULT '',
  result      TEXT NOT NULL DEFAULT '',
  detail      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ix_audit_ts     ON audit(ts DESC);
CREATE INDEX IF NOT EXISTS ix_audit_target ON audit(target_type, target_id, ts DESC);
CREATE INDEX IF NOT EXISTS ix_audit_action ON audit(action, ts DESC);

-- ============ 探测（需求 §六.B / §十二.5） ============
CREATE TABLE IF NOT EXISTS probes (
  id          TEXT PRIMARY KEY,
  ts          TEXT NOT NULL,
  origin      TEXT NOT NULL DEFAULT '',
  node_id     TEXT NOT NULL DEFAULT '',
  business_id TEXT NOT NULL DEFAULT '',
  target      TEXT NOT NULL DEFAULT '',
  kind        TEXT NOT NULL DEFAULT '',
  ok          INTEGER NOT NULL DEFAULT 0,
  dns_result  TEXT NOT NULL DEFAULT '',
  tcp_ms      INTEGER,
  tls_ms      INTEGER,
  http_status INTEGER,
  total_ms    INTEGER,
  err         TEXT NOT NULL DEFAULT '',
  detail      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ix_probe_biz_ts  ON probes(business_id, ts DESC);
CREATE INDEX IF NOT EXISTS ix_probe_node_ts ON probes(node_id, ts DESC);

-- ============ 密钥（DNS API 凭据等） ============
-- 密文用 AES-256-GCM 加封（见 internal/secrets），主密钥不进库。
CREATE TABLE IF NOT EXISTS secrets (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE,
  kind       TEXT NOT NULL DEFAULT '',
  ciphertext BLOB NOT NULL,
  nonce      BLOB NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

-- ============ 双节点高可用 / DNS 调度（需求 §十二） ============
-- 业务组：把共用同一套调度策略的业务聚在一起（例如一个客户的多个域名同时切）。
CREATE TABLE IF NOT EXISTS business_groups (
  id                TEXT PRIMARY KEY,
  name              TEXT NOT NULL,
  customer_id       TEXT NOT NULL DEFAULT '',
  preferred_node_id TEXT NOT NULL DEFAULT '',
  auto_failover     INTEGER NOT NULL DEFAULT 0,
  auto_failback     INTEGER NOT NULL DEFAULT 0,
  health_up_n       INTEGER NOT NULL DEFAULT 2,
  health_down_n     INTEGER NOT NULL DEFAULT 3,
  recover_observe_s INTEGER NOT NULL DEFAULT 300,
  switch_cooldown_s INTEGER NOT NULL DEFAULT 600,
  enabled           INTEGER NOT NULL DEFAULT 1,
  created_at        TEXT NOT NULL,
  updated_at        TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS business_group_members (
  group_id    TEXT NOT NULL REFERENCES business_groups(id) ON DELETE CASCADE,
  business_id TEXT NOT NULL REFERENCES businesses(id) ON DELETE CASCADE,
  PRIMARY KEY (group_id, business_id)
);

-- DNS 记录绑定：精确到 域名 + RecordId + 记录类型 + 解析线路（需求 §十二.3）。
-- 这是「只切我们管的记录、不误伤其他业务和国内线路」的唯一依据，因此四要素缺一不可。
CREATE TABLE IF NOT EXISTS dns_bindings (
  id            TEXT PRIMARY KEY,
  business_id   TEXT NOT NULL REFERENCES businesses(id) ON DELETE CASCADE,
  group_id      TEXT NOT NULL DEFAULT '',
  provider      TEXT NOT NULL DEFAULT 'aliyun',
  domain        TEXT NOT NULL,
  rr            TEXT NOT NULL DEFAULT '@',
  record_type   TEXT NOT NULL DEFAULT 'A',
  line          TEXT NOT NULL DEFAULT 'default',
  record_id     TEXT NOT NULL DEFAULT '',
  observed_value TEXT NOT NULL DEFAULT '',
  observed_at   TEXT NOT NULL DEFAULT '',
  managed       INTEGER NOT NULL DEFAULT 1,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_dns_binding
  ON dns_bindings(provider, domain, rr, record_type, line);

-- DNS 变更留痕：变更前后值 + API RequestId + 结果（需求 §十二.3）。
CREATE TABLE IF NOT EXISTS dns_changes (
  id           TEXT PRIMARY KEY,
  ts           TEXT NOT NULL,
  binding_id   TEXT NOT NULL DEFAULT '',
  business_id  TEXT NOT NULL DEFAULT '',
  action       TEXT NOT NULL DEFAULT '',
  value_before TEXT NOT NULL DEFAULT '',
  value_after  TEXT NOT NULL DEFAULT '',
  request_id   TEXT NOT NULL DEFAULT '',
  accepted     INTEGER NOT NULL DEFAULT 0,
  status       TEXT NOT NULL DEFAULT '',
  err          TEXT NOT NULL DEFAULT '',
  actor        TEXT NOT NULL DEFAULT '',
  detail       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ix_dnschange_ts ON dns_changes(ts DESC);

-- 切换事件（需求 §十二.6）。
CREATE TABLE IF NOT EXISTS switch_events (
  id             TEXT PRIMARY KEY,
  ts             TEXT NOT NULL,
  business_id    TEXT NOT NULL DEFAULT '',
  group_id       TEXT NOT NULL DEFAULT '',
  from_node_id   TEXT NOT NULL DEFAULT '',
  to_node_id     TEXT NOT NULL DEFAULT '',
  trigger        TEXT NOT NULL DEFAULT '',
  actor          TEXT NOT NULL DEFAULT '',
  reason         TEXT NOT NULL DEFAULT '',
  status         TEXT NOT NULL DEFAULT '',
  evidence       TEXT NOT NULL DEFAULT '',
  config_version INTEGER NOT NULL DEFAULT 0,
  dns_result     TEXT NOT NULL DEFAULT '',
  verified       TEXT NOT NULL DEFAULT '',
  finished_at    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ix_switchevent_ts ON switch_events(ts DESC);

-- 调度控制权租约（需求 §十二.4：同一业务同一时刻只有一个自动调度控制者）。
CREATE TABLE IF NOT EXISTS scheduler_lease (
  scope      TEXT PRIMARY KEY,
  holder     TEXT NOT NULL,
  acquired_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  heartbeat_at TEXT NOT NULL
);
`
