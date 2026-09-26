# shengyu —— 自有品牌中转服务管理平台

面向"我方持有中转服务器、客户只改 DNS"的商业中转场景。第一版只做我方管理员后台。

- 转发内核：**HAProxy 社区版**（唯一的生产实现，版本范围见 `docs/04`）
  - 仓库内另有 `internal/testplane`（纯 Go 的 SNI/TCP 转发实现），**仅供自动化测试使用**。
    它不随主程序提供，`-dataplane` 只接受 `haproxy`，且生产二进制里不含它的任何符号
    （Go 只链接被 import 的包，`cmd/shengyu-edgelink` 从不 import 它）。
- 后端：**Go 1.22+**，核心逻辑只用标准库 + `database/sql`，唯一第三方依赖是纯 Go 的 SQLite 驱动
- 前端：TypeScript + Vue3 + Element Plus（`web/`，与后端同源部署）
- 配置与审计：SQLite（`meta.db`）；连接日志：**按小时分片**的独立 SQLite（`logs/`）

---

## 一、目录

```
docs/                       设计文档（先读这几份，再读代码）
  01-architecture.md        总体架构与模块边界
  02-data-model.md          数据模型（元数据库 + 日志分片）
  03-log-and-metrics-pipeline.md  日志与实时指标采集设计
  04-haproxy-baseline.md    受支持的 HAProxy 版本、逐个日志字段的可用性核对表
  05-phased-plan.md         分阶段计划
  06-ha-and-dns-scheduling.md    双节点高可用与阿里云 DNS 调度设计
  07-haproxy-syntax-baseline.md  HAProxy 2.8 语法逐条核对记录（含官方手册原文与出处）
  08-acceptance-linux.md    Linux 真机验收手册（机器配置建议 + 17 项逐步命令与回填区）

internal/
  id/           稳定唯一 ID（含进 HAProxy 对象名的 HANDLE；**不用域名拼对象名**）
  model/        领域对象，只有数据结构
  validate/     结构化校验（域名/源站/入口/超限/能力），产出可直接展示的中文结论
  haproxy/      配置渲染 + 日志字段登记表 + 对象命名
  logparse/     连接日志解析（HAProxy 原样 JSON → 结构化）
  logstore/     日志分片、查询、保留期 / 容量上限 / 压缩 / 磁盘水位
  store/        管理面元数据库（客户/节点/业务/路由/版本/发布/审计/账号）
  auth/         口令哈希（PBKDF2-HMAC-SHA256）、令牌与常量时间比较
  sni/          握手期 SNI 解析（不终止 TLS）
  dataplane/    转发内核抽象 + HAProxy 实现（唯一的生产实现）
  testplane/    纯 Go 转发实现，**仅供测试**（不被主程序 import）
  publish/      发布流水线（候选→校验→原子替换→平滑生效→等待就绪→验证→失败回滚）
  agent/        节点侧日志接收、攒批摄入、本机三维健康心跳
  api/          管理面 HTTP API 与节点 Agent 接口
cmd/shengyu-edgelink/   主程序（管理面 + 同机节点运行时）
e2e/                  端到端验收测试
scripts/              构建环境、冒烟脚本
deploy/               systemd 单元、polkit 受限授权规则、安装/卸载脚本、内核参数
```

## 二、快速开始

```bash
source scripts/goenv.sh            # 本机 Go 环境（隔离安装，不动系统 PATH）

# 1) 跑全部测试（含端到端验收）
go test ./...
go test -race ./...                # 需要 CGO_ENABLED=1 与一个 C 编译器
go vet ./...

# 2) 出发布产物（会先跑 vet + 测试，再构建 amd64/arm64、打包、算 SHA256）
bash scripts/build-release.sh v0.1.0
```

> **不要用"能编译"当"没问题"**。构建脚本里有一道闸门：先用**不裁剪符号**的副本扫
> `go tool nm`，确认生产二进制里没有 `testplane`（测试用数据面）的符号，通过后才出正式产物。
> 测试用数据面一旦混进交付物，"验收时跑的转发行为"和"客户机器上跑的"就不是一回事。

**关于"起真实服务"**：主程序只支持 HAProxy 数据面，启动时会探测 HAProxy 是否存在、
版本是否在受支持范围内、是否启用了 master-worker，任何一条不满足都**拒绝启动**
（而不是退到别的内核上跑）。所以：

- 纯逻辑与端到端链路的验证由 `go test ./...` 完成，其中用测试替身数据面
  （`internal/testplane`）驱动，**不需要 HAProxy**；
- 要跑真实服务，只能在 Linux 上先装好受支持的 HAProxy 社区版，见下面的部署方式。

生产节点（Linux）—— **一条命令装完**：

```bash
# 1) 把发布包传到服务器并解压，进入解压目录
tar xzf shengyu-edgelink-linux-amd64-v0.4.0.tar.gz
cd shengyu-edgelink-linux-amd64-v0.4.0

# 2) 一条命令：依赖检查 → 建账号目录 → systemd → 基线 → 建管理员 → 启动服务
sudo bash install.sh                       # 管理端口会以交互方式询问，回车用默认 127.0.0.1:8081
sudo bash install.sh --listen 127.0.0.1:8081   # 或直接指定（非交互，适合脚本化）
```

安装结束会打印：管理后台地址、SSH 隧道命令、以及**只显示一次**的管理员凭据。
管理面只绑 `127.0.0.1`，在你自己电脑上打开请先建隧道：

```bash
ssh -L 8081:127.0.0.1:8081 -p <SSH端口> root@<服务器IP>
# 然后浏览器打开 http://127.0.0.1:8081/
```

> 管理员初始化现在**由安装脚本自动完成**（`-admin-pass` 只在还没有任何管理员时生效，
> 所以重复安装不会覆盖已有口令）。不要再手工执行"创建管理员"那段命令 ——
> 漏跑时症状是"服务起得来但登不进去"，而报错出现在另一个服务里，极难归因。
>
> **升级时重跑 install.sh 不会覆盖正在跑的已发布配置**：第 7 步写基线前会先检查
> `current/haproxy.cfg` 是否已存在，存在就跳过。（这条是真机复现过的故障：
> 无条件写基线会把 VERSION 从 N 打回 0，转发静默中断，而脚本每一行都报成功。）

**三处路径必须一致**，安装脚本按此约定创建目录，改动时三处要一起改：

| 用途 | 路径 | 属主 | 谁读谁写 |
|---|---|---|---|
| 配置 | `/etc/shengyu-edgelink/haproxy` | `shengyu:shengyu` 0750 | 管理面写（`-config-root`），HAProxy 只读 `current/haproxy.cfg` |
| 状态 | `/var/lib/shengyu-edgelink` | `shengyu:shengyu` 0750 | 管理面读写（元数据库、日志分片） |
| 套接字 | `/run/shengyu-edgelink` | `root:shengyu` 0770 | 双方共用，由 `deploy/tmpfiles-shengyu-edgelink.conf` 创建 |

发行版必须自带 **受支持的 HAProxy 社区版**（版本范围见 `docs/04`），
且程序启动时会自检 HAProxy 存在、版本在范围内、`master-worker` 已启用；任何一条不满足**拒绝启动**。

## 二·补 评审整改状态

独立评审（`加速平台-代码评审.md`，F01–F12）与本轮整改的对应关系。
**只列有代码支撑的状态**，未验证的一律标"未验证"：

| 编号 | 问题 | 状态 |
|---|---|---|
| F04 | 远程发布误操作本机 | **已修**。`Pipeline` 绑定本机节点，非本机发布/回滚被拒（409）并留审计；版本目录按节点隔离。回归测试通过 |
| F05 | 验证用磁盘标记冒充运行版本、用任意监听冒充自己 | **已修**。改为按版本独立统计套接字确认"vN 的 worker 在跑"，并按 frontend 名核对监听归属；版本不一致 / 无法确认 → 不通过。回归测试通过 |
| F06 | 中途替换失败留下新正文 + 旧版本号 | **已修**。发布前快照 `current/`，任一写入失败即整体恢复（并删掉新版本残留文件）；恢复本身失败会显式报出。回归测试通过 |
| F07 | 接收器重启后新连接被误判重复丢弃 | **已修**。幂等键加入采集端启动标识 `boot_id`，并对旧分片做列/索引迁移；统计按真实插入数计。回归测试通过 |
| F08 | 长连接日志按开始时间分片、按结束时间查询 | **已修**。分片轴与查询轴统一为 `end_ts`，`accept_ts` 仅作数据保留。回归测试通过 |
| F02 | 配置写入路径与 systemd 读取路径不一致、首次启动无初始配置 | **已修（待 Linux 验证）**。新增 `-config-root`；启动时自动补基线配置；新增 `-write-baseline`；unit / 安装脚本 / README 三处对齐 |
| F03 | 非 root 无法执行 reload、运行目录与套接字权限未闭合 | **已修（待 Linux 验证）**。授权改为 **systemd D-Bus + polkit**（只放行「shengyu 用户 → shengyu-edgelink-haproxy.service → reload」）；启动时做一次 reload 权限预检，不许把这问题推迟到第一次发布；运行目录由 tmpfiles.d 统一创建；统计套接字支持 `mode/user/group`。**尚未在真实 systemd 上验证** |
| F01 | 默认配置含不属于 HAProxy 2.8 的语法 | **已按 2.8 官方手册修正（待真实 HAProxy 验证）**。`accept_date` → 别名 `%T`（GMT 的 accept date）；`json_escape` → `json(utf8s)`；删掉独立的 `expose-fd listeners`；弃用别名改正式名（`req.ssl_sni`）。逐条手册依据见 `docs/07`；渲染器新增 server 行参数白名单，让同类语法错误不可能再通过自检 |
| F09 | TLS 健康检查改变业务传输、部分语法错误 | **已修（待真实 HAProxy 验证）**。业务传输**永不**输出 server 行的 `ssl`/`sni`/`verify`；探测改用 `check-ssl`/`check-sni`；探测超时改为 backend 的 `timeout check`；`maxqueue`/`maxconn` 移到 server 行；HTTP 探测改用 `option httpchk` + `http-check send/expect`。回归测试断言"server 行不得出现会改动业务传输的参数" |
| F10 | 本机节点心跳不续报会被判离线 | **已修**。新增周期性心跳（20 秒）；健康拆成 Agent / 转发内核 / 业务路径三个维度并聚合，且"管理面失联但转发正常"不再被报成离线；心跳读不到版本时**不写 0**（写 0 会被界面显示成"实际版本 0"）。回归测试用假时钟驱动 |
| F11 | 字节方向标反、源站富化未按配置版本 | **已修**。`%B`（源站→客户端）与 `%U`（客户端→源站）拆成 `down`/`up` 两列，旧分片按方向迁移；新增 `%si`/`%sp` 记录**实际连到的**源站；业务/源站归属改为按日志里的 `ver` 查 `config_versions` 的不可变快照，快照缺失时留空而不退回当前配置。回归测试用不对称上下行数据 + 跨版本长连接 |
| F12 | 配额与压缩只实现了一半 | **已修**。策略补齐容量上限（QuotaBytes）与磁盘水位（MinFreeBytes）；压缩**真正实现**（gzip，压缩前先合并 WAL、产物逐字节校验后才删原文件，且压缩分片仍可查询）；删除/压缩都产出动作清单与告警，不静默丢数据 |

### 本轮按复核意见另修的四项

| 项 | 状态 |
|---|---|
| `NoNewPrivileges=true` 与 sudo 提权方案冲突 | **已修**。sudo 依赖 setuid，与 `NoNewPrivileges` 语义直接冲突（必然失败）。改为 polkit 受限授权（进程不提权，由 systemd 完成特权动作），并删掉 `deploy/sudoers-shengyu` |
| 活跃分片不能依赖 `immutable`、不能逐条关闭连接 | **已修**。读侧去掉 `immutable`（正常读 WAL），因此**写连接可以复用**；写侧按分片保活（生产开启）+ 攒批写入（一批一个事务）。**关键证明**：写连接保持打开时，查询仍能看到刚提交的数据 |
| 远程上报协议与入库补齐 `boot_id` | **已修**。`/api/agent/logs` 强制要求 `boot_id`（缺失即 400 并说明原因）；补了"重传判重 / 重启不丢 / 旧分片自动迁移"三组测试 |
| 补配置替换中断、reload 异步就绪、回滚到历史版本的测试 | **已补**。其中「reload 异步就绪」还暴露并修掉了一个**产品缺陷**：原来只验证一次，一次稍慢但成功的 reload 会被判失败并触发无谓回滚；现在改为在 30 秒窗口内轮询等待就绪，并把等待时长/次数记进发布结果 |
| HAProxy worker 必须明确降权到 shengyu，不得长期以 root 运行 | **已修（待 Linux 验证）**。两层都做：unit 侧 `User=shengyu` + 仅保留 `CAP_NET_BIND_SERVICE`；配置侧输出 global 的 `user`/`group`（由 `-haproxy-user`/`-haproxy-group` 控制，默认 `shengyu`）。二者缺一层都不成立，判据表见 `docs/04 §8`。启动自检新增 `checkRunIdentity()`：降权目标账号不存在则拒绝启动；以 root 运行且未配置降权目标则拒绝启动 |
| `install.sh` 不要依赖未检查的 `sudo -u` | **已修（待 Linux 验证）**。改用 `runuser`（属 util-linux，凡有 systemd 的机器必有；不读 sudoers、无"授权没配好"这种失败模式），sudo 仅作兜底且必须打印警告。同时新增依赖检查：缺什么一次性列清并给出补齐命令，不再表现成一句 `command not found` |
| 清理过期文档说法（`json_escape` / `accept_date` / `req_ssl_sni`） | **已修**。`docs/01`、`03`、`04` 全部改为正确写法并说明为什么（`json(utf8s)` / 别名 `%T` / `req.ssl_sni`）；顺带修正 `docs/02` 的同三处。`docs/03` 那段 `log-format` 示例原本整串用的是不存在的 sample fetch（`%[src]`、`%[be_name]`、`%[bytes_out]`…），已换成真实渲染产物并注明以代码为准。`docs/07` 里保留这些错误写法是对的 —— 那份文档记录的就是"哪些写法被判定为错误" |

### 本轮顺手修掉的一个安装缺陷（不在清单里）

`/var/lib/shengyu-edgelink` 这个目录此前从未被显式创建赋权：安装脚本只建了它下面的 `logs/`，
于是父目录由 `install -d` 以 **root:root 0755** 建成。而元数据库 `meta.db` 恰恰写在
`/var/lib/shengyu-edgelink` 这一层 —— 以 shengyu 身份启动会直接 Permission denied，
且报错出现在 `systemctl status shengyu-edgelink-server` 里，与"SQLite 库坏了"无法区分。

修法：显式建立三层并对每一层做**以 shengyu 身份**的可写自检（root 能写 ≠ 服务账号能写），
不通过就当场中止安装，不再把失败推迟到第一次发布。


---

## 二·补·2 本轮构建验证环境（先修工具链，再说结论）

**上一轮的判断不作数**：开发机（Windows）上的 Go 工具链是坏的 ——
`src/` 只剩 35 个顶层目录（正常约 74 个），缺 `os`/`strings`/`time`/`io`/`net` 等，
连 hello world 都报 `package unsafe is not in std`。用它跑出来的"通过"没有意义。

本轮先把工具链换成官方包，再重跑：

| 项 | 值 |
|---|---|
| 工具链 | 官方 `go1.23.4`（与项目 `go 1.22` 兼容，且与原先使用的版本一致） |
| 安装位置 | `C:\Users\Administrator\.workbuddy\binaries\go\official\go` |
| 完整性校验 | sha256 `16c59ac9…3c39`，与 `https://go.dev/dl/?mode=json` 返回的官方值**逐字符一致**（大小 81942945 也一致） |

在这套工具链上的**实测结果**（2026-09-22）：

| 命令 | 结果 |
|---|---|
| `go version` | `go version go1.23.4 windows/amd64` |
| `go build ./...` | exit 0 |
| `go vet ./...` | exit 0 |
| `go test ./...` | exit 0，14 个包全 `ok` |
| `go test -race ./...` | exit 0，14 个包全 `ok`（`CGO_ENABLED=1` + mingw gcc 16.1.0；Windows 上 race 必须有 cgo） |
| `gofmt -l internal cmd e2e` | 无输出（全部已格式化） |

**仍未验证的**（不因为上面的绿就变成"已验证"）：

- Linux 真机 17 项 + 附加 2 项：`docs/08-acceptance-linux.md` 的实测栏**全空**。
- F01/F02/F03/F09 里涉及真实 HAProxy 语法与 systemd/polkit 的部分。
- 上面这些测试是在 **Windows** 上跑的；同一份代码在 Linux 上要按 `docs/08 §2.1`
  重新跑一遍（装官方 Go → `go vet` → `go test` → `go test -race`），结果以 Linux 那次为准。

**未验证的都不算完成。** F01/F03 以及安装、首次发布、长连接 reload、端口冲突、
失败恢复、停管理进程、整机重启这些项，需要在干净 Linux 环境用真实 HAProxy、
真实 systemd、最终的非 root 服务身份逐条跑过。**具体怎么跑、跑什么命令、每项的判据
都写在 `docs/08-acceptance-linux.md`**（含测试机的最低/推荐配置）。

`docs/07` 第 3 节列出的第 1–18 项语法仍全部标着「待真实 HAProxy 验证」—— 本机没有 HAProxy，
**指令白名单与单测不能替代 `haproxy -c`**。

## 二·补·3 HAProxy 线程数（`nbthread`）

| | 值 |
|---|---|
| 参数 | `-haproxy-nbthread` |
| 默认值 | **进程可见的 CPU 数**（`runtime.NumCPU()`；Linux 上读 `sched_getaffinity`） |
| 特殊值 | `0` = 同"默认"（不想在 systemd 单元里写死数字时用） |
| 上限 | **64**（HAProxy 编译期 `MAX_THREADS`，超限会被 `haproxy -c` 拒绝） |
| 生效位置 | 渲染产物 `global: nbthread N` |
| 可观测性 | 启动日志固定打印一行：`HAProxy 线程数 nbthread=N（进程可见 CPU=M，上限 64，0=自动…）` |
| 回归测试 | `internal/haproxy/nbthread_test.go`（7 项，本轮全通过） |

**为什么不再写死 4**（之前就是写死的）：

1. HAProxy 自己的默认值就是"进程被绑到的 CPU 数"。2.8 手册 §3.1 `nbthread <number>` 原文：
   > On some platforms supporting CPU affinity, the default "nbthread" value is
   > automatically set to the number of CPUs the process is bound to upon startup.
   > … Otherwise, this value defaults to 1.

   我们与它对齐，而不是自创一个数字。
2. 写死 4 的真实代价：2 vCPU 的机器上 HAProxy 会打一条 nbthread 相关告警，
   吞吐数据不可信 —— 用它出的性能结论是假的。
3. 线程数本质是**部署机的属性**，不是产品常量，所以它必须能在部署时指定。

**越界处理**：超过 64 **直接失败**（渲染期报错 + 启动自检 `log.Fatalf`），
不静默夹断成 64 —— 夹断会让运维以为自己配了 128 线程、实际只跑 64，
而这种偏差没有任何告警，是最难查的一类。

**验收时怎么记**：`docs/08` 项 3 要求 `nproc` 与配置里的 `nbthread` 数值比对，
并用 `nbthread 65` 探一次 `haproxy -c`（确认上限 64 在这台机器上是真的）；
项 17 的汇总报告里记录实际线程数。

## 二·补·4 发布产物

```bash
bash scripts/build-release.sh v0.1.0
```

产物都在 `dist/`（`SHA256SUMS` 是全部产物的 sha256 清单）：

| 文件 | 说明 |
|---|---|
| `shengyu-edgelink-linux-amd64` | linux/amd64 二进制（`CGO_ENABLED=0`，静态，无外部依赖） |
| `shengyu-edgelink-linux-arm64` | linux/arm64 二进制 |
| `shengyu-edgelink-linux-amd64-<版本>.tar.gz` | amd64 交付包：二进制 + `deploy/` + `docs/` + `README.md` + `BUILD.txt` |
| `shengyu-edgelink-linux-arm64-<版本>.tar.gz` | arm64 交付包 |

脚本里有两道闸门，任一不过就不出产物：

1. **先跑 `go vet` + `go test`**，再构建 —— 产物必须产自"测试通过的那份代码"。
2. **扫符号**：先用不裁剪符号的副本跑 `go tool nm`，确认生产二进制里
   **没有** `testplane`（测试用数据面）的任何符号，通过后才编正式产物。
   这条守的是"验收时验的转发行为"与"客户机器上跑的"必须是同一套东西。

版本号由 `-ldflags -X main.Version=` 注入，`shengyu-edgelink -version` 可查；
未注入时显示 `dev`（看到 `dev` 就说明这不是发布构建，不该上生产）。

## 三、端到端验收：怎么证明它是真的

先说清**哪些是真的、哪一处是替身**，避免把结论说过头：

| 环节 | 状态 |
|---|---|
| TLS 客户端 / TLS 源站、TCP 收发、真实 socket、真实 SNI 解析 | **真实**，无替身 |
| HTTP 管理 API（登录、CSRF、业务接入、发布、回滚、日志查询、诊断、审计） | **真实**，走 `httptest` 的真实 HTTP |
| SQLite 元数据库与按小时分片的日志库 | **真实**，落真实文件 |
| 发布流水线（候选生成 → HAProxy 语法检查 → 原子替换 → 平滑生效 → 验证 → 回滚） | **真实** |
| 转发内核 | **替身**（`internal/testplane`）。开发机是 Windows，HAProxy 没有 Windows 版，只能用替身驱动整条链路 |

替身带来的差异由 `internal/dataplane/haproxy_test.go` 单独覆盖：版本范围判定、
`systemctl reload` 而非 pkill、reload 失败后**配置正文**回滚、`/proc/net/tcp` 监听解析、
统计 CSV **按列名**取值（防新版插列后静默取错字段）。
**真实 HAProxy + 真实 systemd + 非 root 身份下的整链路验收，必须在 Linux 节点上做。**

`go test ./e2e/` 覆盖的行为：

| 验收项 | 断言方式 |
|---|---|
| 添加业务 → 真实转发 | 真实 TLS 客户端经中转握手到真实 TLS 源站并回显 |
| 多域名共享 443，互不串流 | 两个域名共用入口端口，各自只连到自己的源站（按**完成 TLS 握手**的连接计数） |
| 无 SNI / 非 TLS 走 TCP 专用端口 | 模式 B 独立入口，明文 TCP 收发 |
| 未知 SNI 被拒绝且可定位原因 | 握手必须失败，源站**零**业务连接，日志写明「未匹配 SNI」 |
| 产生日志 → 网页查询 | 经 HTTP API 按业务 ID 查询，校验业务归属/SNI/字节数/解析状态 |
| 修改规则 → 平滑生效 | 改源站后重发配置，新连接走新源站 |
| 平滑更新不中断现有长连接 | **发布前建立的长连接在发布后继续收发** |
| 端口冲突不破坏现有服务 | 外部占用端口 → 发布失败但原业务照常工作，生效版本不推进 |
| 发布失败留下可查痕迹 | 失败版本状态 `failed` 且带失败原因；同节点同一时刻只有一个 `active` |
| 回滚 | 一键回滚到历史版本，转发真实回到旧源站，审计留痕 |
| 平台离线不影响转发 | 关掉管理面后转发继续工作 |
| 区分「发布失败」与「源站不可用」 | 源站不可达时发布状态为 `applied_origin_unhealthy`，`config_healthy=true` |
| 实时指标能看到未结束的长连接 | 连接仍在时 `active_conns>=1` 且字节数已累加 |
| 日志查询边界 | 不指定时间范围时明确回报默认窗口；超宽范围被拒 |
| 诊断不臆断 | 每条结论含事实/依据/可能原因/下一步；**不得**由 timeout 直接定性「被封」 |
| 生产只允许 HAProxy | `cmd/shengyu-edgelink`：`-dataplane` 拒绝任何非 haproxy 取值；二进制里无替身符号 |

`internal/dataplane/haproxy_test.go` 另有一组针对生产路径的测试：版本范围判定、
reload 失败后**配置正文**回滚（不只是改版本号）、不出现 pkill 类命令、
`/proc/net/tcp` 解析、统计 CSV **按列名**取值（防新版插列后静默取错字段）。

`scripts/smoke.sh` 是对**真实运行的 server 进程**的黑盒冒烟，覆盖健康检查、登录、
CSRF 防护、客户/节点/业务/发布/转发/日志/诊断/版本/审计共 11 项。

## 四、关键设计决策（为什么这么做）

**1. 不用域名拼接内部对象名。** backend 名是 `bk_<handle(businessID)>`，
businessID 随机且永不复用。因此 `backend 名 → 业务` 的映射**与配置版本无关**：
域名易主、业务停用都不会让历史日志的归属发生漂移。日志解析器因此可以按
"全部业务"建表，而不是按某一版配置快照。

**2. 版本目录不可变 + 文件级原子替换。** `versions/vN/` 一旦写下就不再修改，
`current/` 是 HAProxy 实际读取的目录，其内容由「逐个文件原子替换 + 最后写 VERSION」
更新。VERSION 是"这一套文件完整且属于该版本"的提交标记：中途失败时它仍是旧的，
界面看到的就是真实的旧版本，而不是"一半新一半旧却自称新版"。

**3. 回滚必须用当时那一版的状态快照。** `versions/vN/state.json` 保存完整的
结构化期望状态。若用当前业务配置去套旧版本号，对"以文件为准"的数据面恰好能工作，
对"以状态为准"的数据面就会**回滚了却什么都没变** —— 这是一个只在特定实现下暴露的 bug。

**4. 未匹配的 SNI 一律拒绝，且绝不拿 SNI 去解析域名。** SNI 只用于查本节点的路由表，
查不到就拒绝。需求明确禁止"依据任意外部 SNI 自动解析并访问任意目标"。

**5. 日志系统故障不得阻塞转发。** `LogSink` 类型的签名里没有 error —— 这条纪律
固化在类型里，而不是靠调用点自觉。解析失败的行**仍然入库**（`parse_ok=0` + 原文），
因为丢掉的日志就是事后查不到的日志。

**6. TCP 日志只有会话结束才完整，所以实时指标不能靠它。** 在线连接数与吞吐来自
数据面的统计接口（HAProxy 统计 socket），而不是从结束日志里算。测试替身数据面也遵循同一条纪律
（它的字节数是边转发边累加的），因此这条性质在自动化测试里同样被验到：
一条挂了一天的长连接不会在吞吐曲线上"消失"。

**7. 校验分两层，且只把它当"更早一层"而不是替代 HAProxy。** 域名重复、端口冲突、
源站回环、能力缺失这类**语义**问题在平台侧拦下；配置语法交给 `haproxy -c` ——
HAProxy 的语法复杂且有版本差异，只有它自己能给权威结论。

**8. 口令、令牌、凭据的存储。** 口令用 PBKDF2-HMAC-SHA256（默认 60 万次迭代，启动时
断言不得低于 10 万），每用户独立盐；会话令牌与节点凭据只存 SHA-256 摘要，
明文仅在创建响应里出现一次；注册令牌一次性、可撤销、可轮换，与长期凭据分表存放。

**9. 已修掉的真实缺陷（都由测试发现，值得单独记住）。**
- `Verify()` 对空哈希返回成功 → **任意口令都能登录**。现强制最小长度下限。
- SQLite 唯一冲突报的是**列组合**而非索引名，原先按索引名匹配导致「端口冲突」
  被误报成「该业务已存在路由」。
- 查询撞上"刚创建、还没建表"的日志分片会报 `no such table`。现按"尚未初始化"跳过并说明。
- 日志解析器缓存 30 秒，导致同机部署时**新接入业务**在 30 秒内日志归属为空。
  现改为"未命中即刷新"（带 2 秒最小间隔防滥用）。
- SNI 模式的路由被写入了独立入口地址/端口，破坏"多域名共享 443"的前提。已由校验层拦下并在写入侧修正。
- **只读 DSN 被拼成 `?mode=ro?_pragma=...`（两个问号）**：第二个问号之后的内容被当成了
  `mode` 的值，驱动报出一句与"参数拼错"毫无关系的 `SQL logic error: out of memory (1)`；
  而查询层又把它当成"分片打不开"静默跳过 —— 表现是「日志明明写进去了却查不到，且没有任何错误」。
  这是本轮把只读从 `immutable` 改成正常读 WAL 时踩出来的。
- **只读连接上设了 `journal_mode(WAL)`**：它是写操作，在 `mode=ro` 的 DSN 上会被 SQLite 拒绝，
  症状与上一条相同（打开成功、第一次查询即失败）。现在读写两份 pragma 分开定义。
- **压缩时源文件句柄没关就删原文件**：Windows 上必然失败（Linux 上侥幸能过），
  而"压缩产物已生成、原文件删不掉"会让人以为压缩没问题。
- **压缩前没有合并 WAL**：WAL 模式下最近提交的行可能还只在 `-wal` 文件里，
  只压 `.db` 等于把这部分日志丢掉 —— 文件"压缩成功"、原文件"正常删除"，
  直到几天后查历史才发现少了一段。现在压缩前强制 checkpoint 并**确认 `-wal` 已空**。
- **压缩一个不存在的分片会"成功"**：压缩流程里有一处以写模式打开分片（为了 checkpoint），
  而写模式会**创建**一个空库 —— 于是"压缩不存在的东西"会产出一个空分片。
- **发布后只验证一次**：`systemctl reload` 是异步的，新 worker 还要几百毫秒到几秒才就绪。
  只验一次会把一次正常但稍慢的 reload 判成失败，然后触发一次**完全没有必要的回滚**。
  现在改为在 30 秒窗口内轮询等待就绪，并把等待时长/次数记进发布结果。

## 五、安全边界（当前状态）

- 管理面默认只绑 `127.0.0.1:8081`（端口可用 `install.sh --listen` 或 `-listen` 配置，见 §6.1），
  HTTPS 由外层反代提供；`-secure-cookie` 生产必须为 `true`
- 会话 Cookie `HttpOnly + SameSite=Strict`；写操作强制 `X-CSRF-Token`
- 登录限流（按来源 IP 滑动窗口）、账号来源白名单、登录失败审计（**不记录口令**）
- 节点接口用独立的 `X-Agent-Token`，与管理员会话完全分离
- 平台**不接受**用户输入的原始 HAProxy 配置或任何 Shell 命令
- `-allow-loopback-origin` 仅用于本机验收，生产必须关闭（开启时会打印警告）

## 六、端口与域名配置（自定义端口 / 自定义域名）

### 6.1 管理后台端口（可配置，默认 8081）

管理面监听地址**不是写死的**，默认 `127.0.0.1:8081`。三种指定方式（优先级从高到低）：

| 方式 | 用法 | 适用场景 |
|---|---|---|
| 安装脚本参数 | `./install.sh --listen 127.0.0.1:9090` | 手动装机（推荐） |
| 环境变量 | `SHENGYU_EDGELINK_LISTEN=127.0.0.1:9090 ./install.sh` | 批量装机 / Ansible（无 tty） |
| 交互式向导 | 直接跑 `./install.sh`，会提示输入 | 首次手动安装 |

未显式指定且 stdin 是终端时会交互式询问；非终端（CI / 管道）自动用默认值，**不会卡在 read 上**。
`install.sh` 会把实际值注入 `/etc/systemd/system/shengyu-edgelink-server.service` 的 `-listen` 参数，
且用正则匹配整段 token 而非只匹配默认字面量，**重复安装换端口同样生效**
（避免"我明明传了 `--listen 9090`，服务还是 8081"这类改了不生效的事故）。

也可以直接给二进制传参：`shengyu-edgelink -listen 127.0.0.1:9090`。

**健康检查**用 `GET /api/health`（返回 `{"ok":true}`），**与端口无关**。
不要用根路径判断存活 —— 根路径现在是管理台页面，未登录也可能返回 200，会误判成"活着"。

### 6.1b 管理面的公网 HTTPS 访问（可选；安装时自动配置）

管理面默认只绑 `127.0.0.1`，从外面访问要先建 SSH 隧道。如果希望**直接用域名打开**，
安装时就会自动装好 Nginx、申请证书、配好反向代理与自动续期 —— 不需要手工写 nginx 配置。

**两种开通方式**（等价，第二种用于已装好的机器）：

```bash
# ① 安装时向导会问：内网监听地址 → 内网端口 → 公网域名 → 公网 HTTPS 端口
#    → ACME 邮箱 → 证书验证方式 → 管理员初始密码
sudo bash install.sh

# ② 已装好的机器上单独开通（不必重跑整套安装）
sudo bash deploy/panel-https.sh --domain panel.example.com --acme-email you@example.com

# 回退（撤掉 Nginx 入口与续期定时器；保留证书，因为它可能被同机其它服务共用）
sudo bash deploy/panel-https.sh --disable
```

**它会自动做完这些事**：

| 步骤 | 说明 |
|---|---|
| 装 Nginx | 按发行版用 `apt` / `dnf` / `yum`；`enable --now` |
| 撤掉发行版默认站点 | Debian 系默认站点会 `listen 80 default_server`，留着会占 80 |
| 申请证书 | 默认 **DNS-01**（不占任何本地端口）；无 DNS API 时可用 HTTP-01 |
| 部署证书 | 复制到 `/etc/shengyu-panel/tls/`（`/root` 是 0700，nginx 读不到） |
| 写反向代理 | `公网HTTPS端口 → 127.0.0.1:8081`，含限速与安全响应头 |
| 装续期任务 | systemd timer（每天两次，`Persistent=true`）；若本机已有 acme cron 则保留它 |
| 续期后自动生效 | `--reloadcmd "systemctl reload nginx"`，续期成功即加载新证书 |
| 失败回滚 | 任何一步失败都撤销**本次新建**的 Nginx 配置、证书、systemd 单元 |

**参数**（完整列表见 `deploy/panel-https.sh --help`）：

| 参数 | 默认 | 说明 |
|---|---|---|
| `--panel-domain` | — | 证书主域。给出即启用；不给则不开通（**默认关闭**是安全默认值） |
| `--panel-port` | `9443` | 公网监听端口。**禁止 80/443**：80 要留给本机 acme 续期，443 常属既有服务 |
| `--panel-server-name` | 同主域 | 浏览器访问用的域名（签泛域名时指定具体子域） |
| `--panel-cert` | `dns` | `dns`=DNS-01（推荐，不占端口、支持泛域名）；`http`=HTTP-01（需 80 空闲） |
| `--panel-dns-provider` | `dnspod` | `dnspod` / `aliyun` / `cloudflare` / `huawei` / `tencent` |
| `--panel-wildcard` | 关 | 同时签发 `*.主域`（只能用 DNS 方式） |
| `--panel-acme-email` | — | 证书到期提醒发到这里 |
| `--panel-basic-user` | — | 启用 HTTP Basic 二次认证（与平台口令相互独立） |

DNS 凭据与 Basic 口令一律走**环境变量**（放命令行会进 shell history 与 `ps`）：

```bash
# DNSPod
DP_Id=xxx DP_Key=yyy sudo -E bash deploy/panel-https.sh --domain panel.example.com
# 阿里云 / Cloudflare
Ali_Key=xxx Ali_Secret=yyy sudo -E bash deploy/panel-https.sh --domain ...
CF_Token=xxx sudo -E bash deploy/panel-https.sh --domain ...
# Basic 二次认证（可选）
SHENGYU_PANEL_BASIC_PASS='...' sudo -E bash deploy/panel-https.sh --domain ... --basic-user admin
```

**两个反代之后"看起来配好了、其实失效"的点**（脚本已在 Nginx 层补上，但你应当知道）：

1. **平台内置的 `-admin-ip-allow` 在反代下不起作用。** 平台的来源 IP 解析只信任直连地址
   （源码注释写明：`X-Forwarded-For` 可伪造，用它做白名单等于给攻击者留后门），
   所以反代之后所有客户端在平台眼里都是 `127.0.0.1`：
   白名单填 `127.0.0.1` 等于**放行所有人**，填你的真实 IP 等于**把所有人（含你）锁在门外**。
   ⇒ 要做白名单，请写在 Nginx 层（`allow` / `deny`）。
2. **平台内置的登录限流会变成"全局共用一个桶"。** 限流按来源 IP 计数，
   反代后同样都算 `127.0.0.1` ⇒ 多人同时登录会互相挤掉配额，攻击者也能打满配额把合法用户挡在外面。
   ⇒ 脚本已在 Nginx 层对 `/api/login` 单独限速（6 次/分钟）作为补偿。

**开通前需要你在外部做两件事**：DNS 加一条 A 记录指向本机；云控制台安全组放行该端口。
平台不会替你改 DNS，也不会替你开关安全组。

### 6.2 数据面入口端口（任意端口；443 只是默认推荐值）

- 入口即"共享 SNI 入口"，端口范围 **1–65535**，取自**数据库** `node_sni_entries.bind_port`，
  代码里不存在"端口必须是 443"的常量。**443 只是推荐的默认值，不是系统唯一入口。**
- 常用替代：`8443`、`9443`。同机已有程序占用 443（例如 xray）时，直接建一个 8443 入口即可，
  **无需迁移或停止那个程序**。
- **创建前检查端口占用**：`POST /api/nodes/{id}/sni-entries` 会读 `/proc/net/tcp{,6}`，
  已被占用则返回 `409 port_in_use` 并给出占用者（此刻 HAProxy 尚未绑定该入口，不会误判自己）。
- **发布前再次检查**：先跑平台校验，再检查"本次新增的监听"是否被**外部进程**占用；
  冲突则 `422 port_in_use` **在写文件之前中止**，现有转发零影响。
  判定时会排除"当前生效版本自己的监听"，因此改个域名重新发布不会被误拦。
- 发布后还有一层 `Verify`：按 frontend 名核对监听归属，绑定失败自动回滚到上一个好版本。

> 非 Linux 或读不到 `/proc` 时，端口预检会「判不出来即放行」，把最终裁决交给发布后的
> `Verify` + 回滚 —— 宁可多放一次，也不把常规发布拦死。

### 6.3 自定义域名与源站

- 一个入口下可填**多个域名**，按 TLS SNI **精确匹配**转发到各自源站（多域名共享同一端口）。
- 源站地址三种写法都收：**IPv4 / IPv6 / 域名**。
  填 IPv6 时渲染会自动加方括号（`server s1 [2001:db8::1]:443`）——
  少了方括号端口会被吃掉，这是修过的一个真实渲染缺陷。
  填**域名**时 HAProxy 只在启动/重载时解析一次，DNS 记录变更后必须重新发布才生效；
  校验会给出 `ORIGIN_HOSTNAME_DNS` 警告把这个语义讲清楚。
- **禁止通配符域名**：`*.example.com` 会被后端明确拒绝（`DOMAIN_WILDCARD_UNSUPPORTED`），
  管理台前端也会先拦一次并给出同样提示。需要多个子域请逐条填写。
- **证书平台不管理**：转发层是 TLS SNI 透传、**不终止业务 TLS**，
  证书留在客户自己的源站上（平台全程不接触私钥）。

### 6.4 内置管理台

`shengyu-edgelink` 二进制内置了管理台页面（`internal/ui`，`go:embed` 打进产物，**零额外依赖**）：

- 启动后访问 `http://<管理地址>/` 或 `/ui` 即可（与 API 同源，无需 CORS、无需配静态目录）。
- 提供输入项：**管理端口（只读展示）**、监听地址、监听端口、域名、源站地址、源站端口、健康检查方式。
- 健康检查方式：`tcp`（TCP 连接）/ `tls`（TLS 握手）/ `http`（明文 HTTP）/ `https`（走 TLS 的 HTTP）。
  注意它**只影响探测**，不影响业务传输。

### 6.5 首次初始化向导

系统**还没有任何已发布业务**时，登录后直接进入 5 步向导，而不是先给一个空白后台；
已有业务时直接进入总览页，可继续新增入口 / 域名 / 源站 / 健康检查。

判定依据是**数据面真正的生效版本**（`Applier.ActiveVersion`）而不是节点表里的字段 ——
后者只在进程启动时刷新，用它判断会出现"发布明明成功了，刷新页面又被踢回向导"。

| 步骤 | 内容 |
|---|---|
| 1 欢迎 | 产品名、当前版本、管理端口（只读）、节点信息、当前生效版本 |
| 2 数据面入口 | 监听地址、监听端口；可先点「检查端口」——同时查 IPv4 与 IPv6，冲突时显示占用地址/进程与排查命令 |
| 3 业务 | 业务名称、域名、源站地址、源站端口、健康检查方式、TLS 模式 |
| 4 检查 | 完整配置摘要、DNS 应指向的 IP、实际访问地址（**非 443 端口显式带 `:端口`**） |
| 5 发布 | 逐阶段展示：平台校验 → 端口检查 → 生成 → 写入 → `haproxy -c` → reload → 等待就绪 → 版本校验 → 源站探测 |

**失败展示**必须包含五件事：失败阶段、具体原因、当前运行版本、是否已回滚、
回滚失败时可逐行复制的人工处理命令（`Result.ManualFix`）。

### 6.6 证书：当前只有 TLS 透传

> 注意：本节说的是**数据面**（业务流量）的证书。**管理面**自己的公网 HTTPS 证书由
> 安装脚本自动申请与续期，与本节无关 —— 见 6.1b。

- **TLS 透传（唯一已实现）**：平台不终止 TLS、不接触也不申请证书，
  客户端与源站直接握手，**证书由源站自己持有和续期**。
- **TLS 终止（尚未实现）**：`/api/meta` 的 `tls.terminate.supported` 恒为 `false`，
  界面据此把它渲染成**不可选**并标注"尚未实现"。
  在这个模式下**证书仍然必须由源站自己持有** —— 界面不会显示"证书已申请/即将到期"，
  因为平台确实没有申请过任何证书。

## 七、尚未完成 / 下一阶段

已设计但**尚未实现**（对应 `docs/06-ha-and-dns-scheduling.md`）：

- 双节点自动故障切换、自动回切、健康阈值与防抖、调度控制权租约
- 阿里云 DNS 适配层（普通云解析读写、GTM 评估、漂移检测、变更留痕）
- 独立的多位置外部探测（当前只有"中转节点 → 源站"的 TCP 探测）
- 脱敏诊断包导出（API 已返回应包含的内容清单，导出动作未实现）
- 远程节点独立 agent 进程（`internal/agent` 已具备日志接收与摄入能力，
  但 agent 自身的注册/心跳/配置拉取客户端尚未接上 `/api/agent/*`）
- 独立 Vue3 前端（`web/`）：已改为**内置管理台**方案（`internal/ui`，`go:embed` 随二进制分发），
  见「六、端口与域名配置 §6.4」。不再需要独立构建流水线与反代静态目录
- **TLS 终止模式（平台代申请/续期证书，ACME）**：未实现，界面明确标注为不可用。
  当前只有「TLS 透传」，证书留在源站（见 §6.6）

不承诺的事（与需求一致）：不承诺永久不被封、不承诺固定加速比例、
DNS 高可用**不迁移既有连接**、不保证零中断。
