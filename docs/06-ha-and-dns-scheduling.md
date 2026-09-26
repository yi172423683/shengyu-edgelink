# 06 · 双节点高可用与阿里云 DNS 调度

> 部署前提：**所有中转节点由我方管理**；至少两台 HAProxy 中转节点 A/B。
> ⚠️ 本设计实现的是**中转层高可用**，**不代表国内源站也已高可用**。这句话要出现在界面和交付文档里。

---

## 1. 节点配置同步（需求 §十二.1）

### 1.1 数据模型

一条业务绑定**主、备两个节点**，两节点**都提前部署相同的业务路由并持续提供服务**（不是"待机节点"，是热备双活的数据面——DNS 指向谁，谁在承接真实流量；另一台也处于可服务状态）。

| 关注点 | 实现 |
|---|---|
| 一个业务绑定主/备 | `businesses.primary_node_id` / `backup_node_id`；`routes` 表按 `(business_id, node_id)` 存两份，`role` 区分 |
| 两节点提前部署相同路由 | 发布流程对两节点**分别渲染、分别校验、分别发布**；`bizshort` 在两节点上相同（backend 同名），便于对比 |
| 分别记录版本/发布结果/健康状态 | `routes.last_applied_version` + 每节点 `config_versions` + `publish_records`；`nodes.applied_version`；健康状态来自探测（§5） |
| 备用缺配置或探测不通过时**不得静默切流** | 切换前强制前置校验（§5.4），任一不满足 → **拒绝切换 + 高声告警**，绝不"试一下" |
| 备用应能承接故障后的预期负载 | 容量前置检查：B 近期 `smax`/CPU/带宽 vs A 当前负载 → 余量不足则拒绝自动切换（可配为"警告但允许"） |

### 1.2 一致性校验（每次发布后自动跑）

```
对每个 (business, role∈{primary,backup})：
  1. 该节点存在 route 且 enabled
  2. route.last_applied_version == node.applied_version（否则版本漂移）
  3. 两节点的渲染结果在"业务语义"上等价 —— 对比规范化后的规则指纹：
     {mode, domains(sorted), origin_host, origin_port, timeouts, maxconn, queue_limit, healthcheck}
     SNI 模式下入口端口必须相同（同一 SNI 入口）；TCP 模式下入口端口允许不同（IP 本来就不同）
  4. 该节点对该业务的探测健康（§5）
输出：per-route 的三态：SYNCED / SKEWED / MISSING
```

`MISSING` 或 `SKEWED` 的业务，界面在"备用节点"列显示红色，且这类业务**被排除在自动切换候选之外**。

---

## 2. 业务组配置（需求 §十二.2）

```yaml
业务组: 电商线路A
  preferred_node: A                 # 首选节点 A/B
  auto_failover: true               # 自动故障切换
  auto_failback: false              # 自动回切（默认关）
  fail_threshold: 3                 # 连续失败阈值（连续 N 次探测失败）
  recover_threshold: 3              # 恢复阈值（连续 N 次成功）
  recover_observe_s: 600            # 恢复观察时间：满足恢复阈值后还要稳定观察这么久
  cooldown_s: 300                   # 切换冷却：两次切换之间至少间隔
  max_switches_per_hour: 3          # 防抖硬上限，超过则停止自动切换并告警
  scheduler_mode: platform_only     # manual | platform_only（见 §4 GTM 说明）
  域名与线路范围:
    - domain: a.example.com
      records: [{type: A, line: default}, {type: AAAA, line: default}]
    - domain: b.example.com
      records: [{type: A, line: default}, {type: A, line: cn_telecom}]
  members: [biz_xxx, biz_yyy]        # 组内业务，一起切换
```

> **线路范围必须显式声明**：只有列在 `records` 里的 `(域名, 类型, 线路)` 三元组会被平台修改，其余一律不碰。这是"不误改国内解析线路或其他业务记录"的机械保障。

---

## 3. DNS 调度适配层（需求 §十二.3）

### 3.1 接口

```go
type Provider interface {
    Name() string
    // 只读：按 (zone, rr, type, line) 精确查，返回 RecordId 与全部属性
    GetRecord(ctx context.Context, q RecordQuery) (*Record, error)
    // 只读：列出该 RR 下所有类型/线路，用于 A/AAAA/CNAME 关系检查
    ListRelatedRecords(ctx context.Context, q RecordQuery) ([]*Record, error)
    // 写：必须携带完整属性（值、TTL、线路、类型、优先级），避免误重置未请求修改的字段
    UpdateRecord(ctx context.Context, r *Record, newValue string, newTTL int) (*UpdateResult, error)
    // 能力探测：账号版本、TTL 下限、配额、所需权限是否具备
    ProbeCapabilities(ctx context.Context, zone string) (*Capabilities, error)
}

type UpdateResult struct {
    Accepted            bool   // API 已接受（Code=200）
    RequestID           string // 阿里云 RequestId，原样记录
    Code                string
    Message             string
    BeforeValue         string
    AfterValue          string
}
```

### 3.2 自动容灾的路线选择（需求：优先评估 GTM）

| 方案 | 机制 | 优点 | 缺点 / 风险 | 结论 |
|---|---|---|---|---|
| **阿里云 GTM**（全局流量管理） | GTM 自带多地址池 + 主备/负载均衡 + 健康检查 + 自动切换；解析侧返回 GTM 的 CNAME | 切换在阿里云侧完成，不依赖我们平台的存活；专业容灾 | 额外费用；**配置能力有限**（探测模板/切换阈值以产品为准）；最细探测间隔与容灾粒度受限；无法做"按业务自定义观察窗口"；接入后**平台不得再自行改 DNS**（否则双控制者冲突）；需要确认账号已开通及实例规格 | **优先评估**，作为首选方案；接入前出可行性报告（探测间隔、最小失败次数、TTL、配额、费用、是否支持我们需要的线路） |
| **平台自研调度 + 普通云解析 API** | 平台探测 → 调 `UpdateDomainRecord` 改记录值 | 策略完全可控（阈值/观察/冷却/按业务差异化）；不额外付费 | 切换速度受 **TTL + 递归解析器缓存** 限制；**不具备"权威侧健康切换"能力**，本质是"我们主动改记录"；要求平台自身高可用 | **作为受控降级方案**，并且**必须如实告知客户切换生效时间不确定** |

> **不假设普通云解析自身具备健康切换能力。** 普通云解析就是一张静态表，它不会替你判断源站死活。这句话要写进交付说明。

**GTM 接入时的一致性约束**：`business_groups.scheduler_mode` 设为 `platform_only` 时，平台只做**只读**巡检（对比 GTM 状态与我们的探测结论）并展示，**绝不调用任何 DNS 写接口**。二者不会同时改同一条记录。

### 3.3 接入前置检查（需求：接入前明确账号版本、TTL 下限、配额和所需权限）

`GET /api/dns/accounts/:id/capabilities` → 输出：

| 检查项 | 怎么查 | 输出 |
|---|---|---|
| 账号版本 | 云解析实例列表接口（返回版本字段） | 免费版 / 企业标准版 / 企业旗舰版 |
| **TTL 下限** | **实测**：在专用探测子域上依次尝试写 TTL=1/60/600/3600，记录被接受的最小值 | 例如「本账号最小 TTL = 600s」 |
| 单域记录数配额 | 实例信息里的记录数上限 + 当前用量 | 「100/1000」 |
| 解析线路支持 | 尝试读取线路列表 / 逐线路查询 | 支持哪些线路值 |
| RAM 权限是否齐备 | 逐接口 dry-run：`DescribeDomainRecords` / `DescribeDomainRecordInfo` / `UpdateDomainRecord` | 「缺少 alidns:UpdateDomainRecord」 |
| GTM 是否开通 | GTM 实例列表接口 | 未开通 / 实例规格 |

**所需 RAM 最小权限策略**（写进部署文档，直接可粘贴）：

```json
{
  "Version": "1",
  "Statement": [
    { "Effect": "Allow", "Action": ["alidns:DescribeDomains","alidns:DescribeDomainRecords","alidns:DescribeDomainRecordInfo"], "Resource": ["acs:alidns:*:*:domain/*"] },
    { "Effect": "Allow", "Action": ["alidns:UpdateDomainRecord"], "Resource": ["acs:alidns:*:*:domain/*"] },
    { "Effect": "Allow", "Action": ["gtm:DescribeInstance","gtm:DescribeGtmInstances","gtm:DescribeGtmInstance"], "Resource": ["*"] }
  ]
}
```
（`gtm:*` 为只读，用于可行性评估；若确定用 GTM 由阿里云侧切换，则再按需加 GTM 的写权限，且平台侧仍保持 `platform_only` 只读。）

**密钥只在服务端安全保存**：AK/SK 存在服务端密钥存储（v1：加密字段 + 主密钥来自环境变量/文件权限 0600；演进：KMS/Vault），**任何接口都不回显 SK**，审计里只记 AccessKeyId 后 4 位。

### 3.4 普通 DNS API 模式的十条硬规则

| # | 规则 | 实现 |
|---|---|---|
| 1 | **精确绑定**域名 + RecordId + 记录类型 + 解析线路 | 一切写操作只针对 `dns_records` 里存的 `(provider, provider_record_id, record_type, line)` 四元组；不接受"按域名模糊改" |
| 2 | **保留未请求修改的属性** | 写之前先 `GetRecord` 拿到完整属性（RR/Type/Value/TTL/Line/Priority），把**未请求修改的字段原值回填**到 `UpdateDomainRecord` 请求里；写完再读回来逐字段比对，断言"只有 Value（和显式要求的 TTL）变了" |
| 3 | 不误改国内解析线路或其他业务记录 | 线路范围来自业务组的显式声明（§2）；写操作按 RecordId 精确命中；写后校验其余记录逐字段不变（有测试） |
| 4 | 检查 A/AAAA/CNAME 实际接入关系 | 切换前 `ListRelatedRecords`：若该 RR 同时存在 A 与 AAAA，则**必须一起改**，或（若目标节点无 IPv6）**明确拒绝切换**并提示"AAAA 仍指向故障节点"；若存在 CNAME，则说明该名字已委派给别处 → 拒绝并提示 |
| 5 | 最小权限 RAM 身份 | §3.3 策略 |
| 6 | 密钥只在服务端安全保存 | §3.3 |
| 7 | 记录变更前后值、API RequestId、返回结果 | `dns_change_log` 的 `before_value/before_ttl/after_value/after_ttl/provider_request_id/provider_code/result` |
| 8 | **修改前检查外部变更** | 写之前 `GetRecord`，若当前值 ≠ `last_seen_value`（说明有人手工改过）→ **立即停止覆盖** + 写 `switch_events.manual_intervention` + 告警 `DNS_DRIFT` + 把该组状态切到 `SUSPENDED`（需人工确认后才恢复自动调度） |
| 9 | **API 超时后先读实际状态再决定重试** | 超时不等于失败。流程固定为：超时 → `GetRecord` 重读 → 值已是目标值 ⇒ 视为成功并记 `result=verified`；仍是旧值 ⇒ 才重试（指数退避，上限次数） |
| 10 | 区分三级状态 | `api_accepted`（Code=200+RequestId）→ `authoritative_updated`（**直查权威 NS**，多台权威都查）→ `resolver_observed`（多公共解析器观察）。界面**分别显示**，并且永不宣称"所有客户端已完成切换" |

### 3.5 权威判定与观察

```
权威确认：从 zone 的 NS 记录拿到权威服务器列表 → 对每台 dig @<ns> <rr> <type> +short
          全部返回新值 ⇒ authoritative_updated = true
          注意：权威可能有 Anycast/多台不同步，需给一个等待窗口（默认 60s，可配）
解析器观察：对配置的公共解析器（114.114.114.114 / 223.5.5.5 / 8.8.8.8 等，可配，可加境外点）
          分别查询并记录结果与耗时 ⇒ resolver_observed 明细
平台结论：只用上面两项事实，不做"已全量生效"的推断
```

---

## 4. 调度控制权（需求 §十二.4）

| 要求 | 实现 |
|---|---|
| 任一时刻只有一个自动调度控制者 | `scheduler_lease` 单行表 + `name='dns_scheduler'` 主键；`TryAcquire` 用 `INSERT ... ON CONFLICT DO UPDATE ... WHERE expires_at < ?` 的**条件更新**原子拿锁；持有人每 10s 续约 |
| 不允许 A/B 各自独立改同一条 DNS 记录 | 写 DNS 的能力**只在管理面**；节点 agent **没有任何 DNS 权限**（也不持有 DNS 凭据） |
| 用 GTM 时平台不另行冲突调度 | `scheduler_mode=platform_only` ⇒ 平台只读巡检（§3.2） |
| 自研自动调度运行在独立控制节点，不依赖任一转发节点存活 | 调度器是 mgr 内的组件，mgr 独立部署（可与节点同机，但**不依赖**任何节点）；单点问题由租约 + 部署两台候选 mgr 解决（v1 单 mgr 也可接受，只要它不在 A/B 上） |
| 控制平台故障不得中断现有转发 | 转发数据面完全在节点本地（`current` 配置 + haproxy 进程），mgr 挂了只影响"调度与观测"，转发继续 |
| 页面显示自动切换控制者及其健康 | 总览页固定显示：控制者实例 ID、租约到期时间、上次心跳、是否健康 |

---

## 5. 健康判定（需求 §十二.5）

### 5.1 三层区分（**绝不合并**）

| 层 | 判据 | 数据来源 | 在界面上的呈现 |
|---|---|---|---|
| L1 Agent 在线 | mTLS 心跳未超时（默认 30s） | agent 心跳 | 节点卡片「Agent: 在线/离线」 |
| L2 HAProxy 运行 | 进程存在 + stats socket 可应答 + 期望监听端口都在 LISTEN | helper `GetStatus` + `/proc/net/tcp{,6}` | 节点卡片「HAProxy: 运行/停止/降级」 |
| L3 业务实际可访问 | 探测链：DNS → TCP → TLS(带正确 SNI) → 可选 HTTP 状态 | `probe` | 业务卡片「路径健康: A ✅ / B ✅」 |

**必须区分这三层。** `Agent 在线` ≠ `HAProxy 运行` ≠ `业务可访问`；界面上三者是三个独立指示灯，任何一个亮绿都不代表其他两个。

### 5.2 探测必须直连指定节点（需求硬要求）

```
❌ 错误做法：解析业务域名 → 连接 → 结果只反映"当前 DNS 指向的那台"
✅ 正确做法：
     probe.TLSTarget{
        DialIP:   <A 或 B 的 public_ip>,     // 显式指定节点 IP
        DialPort: <入口端口>,
        SNI:      <业务域名>,                 // 正确的业务域名/SNI
        Host:     <业务域名>,                 // 可选 HTTP 探测用
        Timeout:  5s,
     }
```
- 实现方式：自定义 `DialContext` 或 `TLSConfig.ServerName`，**不经过业务 DNS** 解析；
- 反向校验：一次探测要连 A，就**必须**确保目标 IP 是 A —— 探测结果里记录 `resolved_ip`，若不等于期望 IP 则整条结果作废并记 `probe_error`（防止 DNS 缓存/代理把探测悄悄导到 B）；
- DNS 解析本身**也探测**（`dns_resolve` 类型），但它的结论只用于展示"客户当前会走到哪台"，**不参与** A/B 路径健康判定。

### 5.3 多位置探测

| 探测点 | 位置 | 覆盖 | 局限（必须标注） |
|---|---|---|---|
| `mgr` | 管理面所在机房 | 中转层全局健康 | 不代表客户端地区 |
| `node-A` / `node-B` | 各节点自身 | 节点到源站的可达性 | **节点→源站 ≠ 东南亚用户端体验** |
| `external-*` | 可选的外部探测点（v1 允许配置为"我方其他服务器/客户指定机器"） | 更接近真实客户端 | 未覆盖的地区要**明确显示"无探测点"**，不能假装 |

探测结果一律带 `location` 与 `probe_kind`；界面固定文案：
> 「本结果来自 ⟨location⟩ 的 ⟨probe_kind⟩ 探测，不代表其他地区真实用户体验。」

### 5.4 判定与防抖

```
每个 (business, node) 维护一个滑动窗口（长度 = fail_threshold + recover_threshold + 余量）：

路径健康 = OK   当且仅当： 该节点至少 1 个探测点在当前轮成功
                        且 最近连续成功次数 >= recover_threshold   （进入 OK 需要门槛）
路径健康 = DOWN 当且仅当： 该节点**所有**探测点连续失败次数 >= fail_threshold
                        （单个探测点异常不改变结论 —— 需求 §十二.5「不因单个探测点一次失败就立即切换」）

切换决策（仅当该组 auto_failover=true 且本实例持有租约）：

  if 活跃节点路径 DOWN:
        if 备用节点路径 != OK:            ⇒ 告警「备用路径不健康」，不切换      (H4)
        if 备用配置 MISSING/SKEWED:        ⇒ 告警「备用无有效配置」，不切换      (H4)
        if 备用容量不足:                   ⇒ 告警「备用余量不足」，不自动切（可配为允许）
        if 处于 cooldown 内:               ⇒ 等待，记 skip 原因
        if 本小时切换数 >= max_switches_per_hour:
                                            ⇒ 告警「切换过于频繁，已停止自动调度」，不切换  (防震荡)
        if 两个节点都 DOWN:                ⇒ 只告警「两路径均失败」，不切换      (H5)
        else                              ⇒ 执行切换（§6）
  if 活跃节点 = B 且 A 恢复:
        if auto_failback = false:          ⇒ 不回切（默认）
        if auto_failback = true:           ⇒ 需 A 连续成功 >= recover_threshold
                                              且 已稳定观察 >= recover_observe_s 才回切   (H6)
```

**「双路径都失败」的处理（需求 §十二.5）**：告警，并且**不反复切换**（进入 `BOTH_DOWN` 状态，停止自动动作，只看守与告警）。恢复条件：至少一条路径达到 `OK` 门槛。

**「同一国内源站同时不可达」的处理（需求 §十二.5）**：
切换完成后的验证若显示"经由 B 也连不到源站" → 结论是**「中转层已切换，但业务仍不可用：源站 <ip:port> 从 B 也不可达」**，**不得**显示为"已恢复"。判定依据是探测链里的 L3 结果，而不是"DNS 改了就算好"。

**不凭 `timeout`/`reset` 判「被封」**（需求 §十二.5 + §七）：这两个现象只能支持"会话异常终止"这个结论。`internal/diagnose` 里没有任何能输出"被封"结论的代码路径（有测试守护）。

---

## 6. 切换执行流程与日志（需求 §十二.6）

```
[1] 决策            记 switch_events(reason, evidence = 各探测点当前快照)
[2] 前置校验        备用配置版本 ✔ / 备用路径探测 ✔ / 备用容量 ✔ / 无 DNS 漂移 ✔
                    ✗ 任一失败 → 记录并中止（触发 fail / skip 原因）
[3] 读取现状        provider.GetRecord → 与 last_seen_value 比对（外部变更检测）
[4] 执行变更        UpdateDomainRecord（完整属性回填；只改 Value，TTL 除非显式要求）
                    记录 before/after + RequestId + Code
[5] API 结果        api_accepted
[6] 权威确认        dig @<每台权威NS> → authoritative_updated
[7] 解析器观察      多公共解析器 → resolver_observed（明细）
[8] 业务验证        经新节点做完整探测链 → 区分"中转层已切换"与"业务已恢复"
[9] 记账            dns_change_log 逐记录结果 + switch_events 汇总
                    retry_count / manual_intervention / 失败与恢复全程留痕
```

**界面展示（业务组卡片）**

| 显示项 | 数据来源 |
|---|---|
| 首选节点 | 配置 |
| 当前 DNS 指向 | `dns_records.last_seen_value`（+ 解析器观察） |
| 两条路径健康 | §5.1 的 L3 结果（A/B 分开） |
| 调度模式 | `manual`（只告警不自切） / `platform_only`（GTM 托管，平台只读） |
| 最后切换时间与原因 | `switch_events` 最新一条 |
| 传播观察状态 | 三级状态分列显示（API / 权威 / 解析器观察） |
| 固定声明 | **「DNS 高可用不是既有连接迁移，不保证零中断；切换后老 DNS 缓存的老客户端仍会连到旧节点，直到其缓存过期或连接重建」** |

---

## 7. 状态机（供实现与测试对照）

```
        ┌──────────────┐  连续失败≥阈值   ┌───────────────┐  前置校验失败  ┌────────────┐
        │ STEADY(A)    │────────────────▶│ SUSPECT(A)    │──────────────▶│ ALERT_ONLY │
        └──────┬───────┘                 └───────┬───────┘               └─────┬──────┘
               │  A 恢复                          │ 前置校验通过               │ 条件恢复
               │◀────────────── OBSERVING(A) ◀────┘                          │
               │                 (回切用，需观察窗)                            │
               │                                  ▼                          ▼
               │                          ┌───────────────┐          ┌──────────────┐
               │                          │ SWITCHING(A→B)│          │ BOTH_DOWN    │
               │                          └───────┬───────┘          └──────┬───────┘
               │                                  │ 成功                    │ 至少一路 OK
               └──────────── STEADY(B) ◀──────────┘                         │
                                                                           ▼
               ┌──────────────┐  检测到手工改动/漂移                    STEADY(*)
               │ SUSPENDED    │◀── 任意状态
               │(需人工确认)   │
               └──────────────┘
```

| 状态 | 允许的自动动作 | 退出条件 |
|---|---|---|
| `STEADY` | 无 | 失败计数达阈值 → `SUSPECT` |
| `SUSPECT` | 继续观察 | 前置校验通过 → `SWITCHING`；失败 → `ALERT_ONLY` |
| `SWITCHING` | 一次 DNS 变更 | 成功 → `STEADY`；失败 → `ALERT_ONLY` + 重试（有上限） |
| `ALERT_ONLY` | 只告警 | 备用恢复健康 → 重试切换 |
| `BOTH_DOWN` | 只告警，**停止一切自动动作** | 至少一路 OK → `STEADY` |
| `OBSERVING` | 计时 | 满足恢复阈值+观察窗且 `auto_failback=true` → 回切 |
| `SUSPENDED` | **无**（人工确认后解除） | 管理员确认 |

---

## 8. 交付说明中必须明确的话术

1. **DNS 高可用不是既有连接迁移，不保证零中断。**
2. 切换只影响**新发起**的连接；已建立的连接仍走原节点直到自然结束或客户端重连。
3. 客户端实际切换时间取决于其 DNS 缓存与解析器缓存，**平台不宣称"所有客户端已完成切换"**。
4. 本方案保障的是**中转层**高可用；若源站本身不可用，切换中转节点不会让业务恢复。
5. 平台探测结果来自 `⟨location⟩` 的 `⟨kind⟩` 探测点，**不代表**终端用户所在地区的真实体验。
6. 普通云解析 API 模式下，切换是"我们主动改记录"，**不具备**权威侧健康检查能力。
