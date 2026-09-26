# shengyu Linux 真机验收手册

> **这份文档的性质**：它是一份"待执行 + 待回填"的验收记录模板，**不是已经跑过的结论**。
> 本文里所有"判据"都是可以在机器上直接复现的客观现象；每一项的"实测结果"栏是空的，
> 由执行者填。凡没有真机输出支撑的，都不允许被写进 README 的状态表。
>
> 配套的以"官方手册/源码为依据"的语法核对记录在 `docs/07-haproxy-syntax-baseline.md`，
> 那份也不是实测。两份的分工是：07 回答"这样写在 2.8 里对不对"，本文回答"在这台机器上跑起来是不是真的"。

---

## 1. 测试机配置

### 1.1 最低 / 推荐

| 项 | 最低 | 推荐 | 为什么是这个数 |
|---|---|---|---|
| 架构 | x86_64 | x86_64 | 交付物 `shengyu-edgelink-linux-amd64` 只构建 amd64 |
| vCPU | 2 | **4** | 线程数默认取"进程可见 CPU 数"（`-haproxy-nbthread`，0 或省略=自动，上限 64），所以 2 vCPU 也能正常跑、不会打 nbthread 告警。推荐 4 是为了让**并发/长连接**那几项有真实余量，而不是因为线程数写死 |
| 内存 | 2 GiB | 4 GiB | 常驻只有两个进程（管理面 + HAProxy master）。HAProxy 按 `maxconn 20000` 预留会话结构，2 GiB 起得来但没有余量做并发raft；4 GiB 才能把"长连接/并发"验收项跑出可信结果 |
| 磁盘 | 20 GiB | 40 GiB SSD | 元数据是单个 SQLite 库（很小）；日志按小时分片并 gzip 压缩，20 GiB 足够跑完保留策略验证。**不要用 HDD**：SQLite WAL + 高频小文件写入在机械盘上会放大延迟 |
| 网卡 | 1 × 千兆 | 1 × 千兆 | 中转是纯转发，不做加解密，带宽就是转发能力本身 |
| OS | Debian 12 / Ubuntu 22.04 / Rocky 9（**必须有 systemd + polkit**） | 同左，挑你熟悉的 | 安装脚本依赖 `systemd-tmpfiles`、`systemctl`、`sysctl`；非 root reload 依赖 polkit 规则文件。**Alpine / 容器里没有 systemd 的发行版不支持** |
| HAProxy | ≥ 2.4，建议 **2.8.x 社区版** | 2.8.x 社区版 | 基线版本钉死在 2.8（见 `docs/04`）。发行版自带的 2.8 即可，**不要用 HAPEE / hapee-* 包**（许可风险，见 `docs/04 §3`） |

### 1.2 拓扑：一台就够，两台更好

| 方案 | 组成 | 适用 |
|---|---|---|
| **A. 单机** | 一台：中转 + 源站 + 客户端都在本机 | 能跑完 17 项中的绝大部分。源站用 `127.0.0.1`，需要给管理面加 `-allow-loopback-origin=true`（**仅验收用，生产必须关**） |
| **B. 双机（推荐）** | 机器 A 装 shengyu；机器 B 跑两个源站 TLS 服务并作为客户端发起连接 | 更接近真实交付形态：能验证"A 上没有的端口被 B 占用"这类跨机问题，也能避免"自机连自机"掩盖的网络问题 |

两台机器都只需要上面"最低"那一档。**如果只打算长期养一台测试机，用推荐档的单机**。

### 1.3 端口与安全组

- 入口端口按需放行：至少 TCP `443`（共享 SNI 入口），验收还要临时用到一个 TCP 端口转发口（如 `8443`）。
- 管理面**只绑 `127.0.0.1:8081`**，不需要对外开放 —— 验收时用 `curl http://127.0.0.1:8081` 即可。
- **平台不会替你开关防火墙/安全组**，安装脚本也不改 iptables/nftables（见 `docs/05` 与 install.sh 结尾提示）。

---

## 2. 上机前的准备

在开发机上产出 linux 产物，拷到测试机：

```bash
bash scripts/build-release.sh v0.1.0
# 产物在 dist/：两个二进制 + 两个 tar.gz + SHA256SUMS
```

传到测试机后先校验（**别跳过**：这一步能同时发现"下载被截断"和"传错文件"）：

```bash
sha256sum -c SHA256SUMS
tar -xzf shengyu-edgelink-linux-amd64-v0.1.0.tar.gz
cd shengyu-edgelink-linux-amd64-v0.1.0
# 若打包是在 Windows 上做的，tar 里不会有可执行位（Windows 文件系统没这个概念）。
# install.sh 用 `install -m 0755` 落盘，不受影响；
# 但**手工直接跑**二进制前需要先补：chmod +x shengyu-edgelink-linux-amd64
./shengyu-edgelink-linux-amd64 -version    # 期望：shengyu-edgelink v0.1.0 (...)
```

> `-version` 打印 `dev` 就说明这不是发布构建（没注入版本号），不要用它在验收上。

测试机上先装 HAProxy 并确认版本：

```bash
haproxy -vv | head -3
# 期望：HAProxy version 2.8.x ...
```

### 2.1 在测试机装官方 Go 并跑一遍测试（**第 0 项，先做这个**）

> 为什么必须单列：开发机上的 Go 工具链**坏过一次**（`src/` 缺了 `os`/`strings`/`time`
> 等目录，`package unsafe is not in std`，连 hello world 都编不过）。用坏掉的工具链
> 跑出来的"通过/不通过"毫无意义。所以验收链的第一环是：
> **在一台刚装好系统的机器上，用官方包跑出一份可复现的测试结果。**

```bash
# 1) 装官方工具链（不要用发行版仓库里的 Go，版本常常过旧）
GO_VER=1.23.4
curl -fsSLO "https://go.dev/dl/go${GO_VER}.linux-amd64.tar.gz"
# 2) 校验：**必须做**。下载链路上任何一环被替换，都会导致"测试全绿但代码不是这份"。
#    官方值（可自行到 https://go.dev/dl/?mode=json 核对）：
#      go1.23.4.linux-amd64.tar.gz  sha256=6924efde5de86fe277676e929dc9917d466efa02fb934197bc2eba35d5680971
#      go1.23.4.linux-arm64.tar.gz  sha256=16e5017863a7f6071363782b1b8042eb12c6ca4f4cd71528b2123f0a1275b13e
echo "6924efde5de86fe277676e929dc9917d466efa02fb934197bc2eba35d5680971  go${GO_VER}.linux-amd64.tar.gz" | sha256sum -c -
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf "go${GO_VER}.linux-amd64.tar.gz"
export PATH=/usr/local/go/bin:$PATH
go version          # 期望：go version go1.23.4 linux/amd64

# 3) 跑测试（在源码树根目录下）
cd <源码目录>
export GOFLAGS=-mod=mod
export GOPROXY=https://goproxy.cn,direct    # 国内机器；境外机器改成 https://proxy.golang.org,direct
export GOSUMDB=off
go vet ./...
go test ./...
# race 需要 cgo 与一个 C 编译器，缺 gcc 就先装：
sudo apt-get install -y gcc
CGO_ENABLED=1 go test -race ./...
```

**判据**
- [ ] `go version` 输出的不是 `dev` 版本，且与开发机**同版本**（1.23.4）。
- [ ] `go vet ./...`、`go test ./...`、`go test -race ./...` 三条**全部**退出码 0。
- [ ] 把三条命令的退出码与失败包列表贴进下面的结果栏；**任一条非 0 就停下修**，
      不要带着失败的测试去做后面 17 项 —— 那样验的是"已知有问题的代码"。

**实测结果**：
```
go version:
go vet ./...          exit=
go test ./...         exit=
go test -race ./...   exit=
失败包/失败用例（若有）：
```

---

## 3. 十七项验收

> 通用约定：
> - `#（root）` 表示该命令以 root 执行；`#（shengyu）` 表示必须以服务账号执行（这也是验收的一部分：能跑成功才说明权限配对了）。
> - 每台机器上的 `NODE_ID` 会在第 8 项拿到，之后用 `$NODE` 代称。
> - 管理面登录：`curl -c /tmp/ck -X POST http://127.0.0.1:8081/api/login -d '{"username":"admin","password":"<你的口令>"}'`，
>   返回里的 `csrf_token` 存为 `$CSRF`，会话 cookie 在 `/tmp/ck`（cookie 名 `shengyu_edgelink_session`）。
>   **所有写操作（POST/PUT/DELETE）必须带 `X-CSRF-Token: $CSRF`**，否则会被 403 `csrf_missing` 拦下。
> - 结果栏请填 **命令的真实输出**，不要写"pass"。

---

### 项 1 — `install.sh --dry-run`

**目的**：不打草惊蛇地看清脚本要做什么，顺便确认依赖齐全。

```bash
#（root） sudo bash deploy/install.sh --dry-run
```

**判据**
- [ ] 输出包含「依赖检查通过」；若报缺依赖，会把**缺的每个命令**和补齐方式一起列出来（不是一句 `command not found`）。
- [ ] 输出里出现「将以服务身份执行：`runuser -u shengyu -- <命令>`」——**不应**出现未加检查的 `sudo -u shengyu`。
- [ ] 全程以 `[dry-run]` 前缀，最后一行是「（dry-run 结束，未做任何修改）」。
- [ ] 执行完机器上**不存在** `/etc/shengyu`、`/etc/systemd/system/shengyu-*`。

**实测结果**：
```
（粘贴输出）
```

---

### 项 2 — 真实安装

```bash
#（root） sudo bash deploy/install.sh
```

**判据**
- [ ] 出现「目录权限自检通过（已以 shengyu 身份逐个验证可写）」。
- [ ] 出现「基线配置已写入 /etc/shengyu-edgelink/haproxy/current（不监听任何端口）」。
- [ ] `stat -c '%U:%G %a' /var/lib/shengyu-edgelink /var/lib/shengyu-edgelink/logs /etc/shengyu-edgelink/haproxy/current` 三行均为 `shengyu:shengyu`。
- [ ] `stat -c '%U:%G %a' /run/shengyu-edgelink` 为 `root:shengyu 770`。
- [ ] `ls -l /etc/polkit-1/rules.d/50-shengyu-edgelink.rules` 为 `root:root 644`。
- [ ] `systemd-analyze verify /etc/systemd/system/shengyu-{server,haproxy}.service` 无错误（若装有 `systemd-analyze`）。
- [ ] **没有**任何既有服务被改动或停止（脚本不碰系统自带的 haproxy）。

**实测结果**：
```
（关键几行输出 + stat 结果）
```

---

### 项 3 — HAProxy 版本与 master-worker

```bash
haproxy -vv | sed -n '1p'
# 以下三条要求全部为真才算通过（不是"版本号对上就行"）
grep -q 'master-worker' /etc/shengyu-edgelink/haproxy/current/haproxy.cfg && echo OK: 配置里有 master-worker
systemctl cat shengyu-edgelink-haproxy | grep -q -- '-Ws' && echo OK: unit 用 -Ws 启动
ps -o args= -C haproxy | head -2 | grep -q -- '-Ws' && echo OK: 实际进程带 -Ws

# ---- 线程数 ----
echo "进程可见 CPU: $(nproc)"
grep -n 'nbthread' /etc/shengyu-edgelink/haproxy/current/haproxy.cfg
# 上限探针：把线程数改成 65，haproxy -c 必须**拒绝**（证明上限 64 不是拍脑袋定的）
sed 's/^\( *nbthread \)[0-9]*$/\165/' /etc/shengyu-edgelink/haproxy/current/haproxy.cfg > /tmp/nb65.cfg
grep -n 'nbthread' /tmp/nb65.cfg
haproxy -c -f /tmp/nb65.cfg; echo "nbthread=65 的退出码=$?"
```

**判据**
- [ ] 版本落在受支持范围（2.x ≥ 2.4，或 3.x < 3.2），且不是 HAPEE。
- [ ] 上面三条都打印 OK。只认配置关键字而进程没带 `-Ws`，等于 master-worker 没真正启用。
- [ ] `nbthread` 的数值 == `nproc` 的输出（默认取进程可见 CPU 数；若在单元里用
      `-haproxy-nbthread` 显式指定，则等于指定值）。**不等于**就记录下实际值再继续。
- [ ] `nbthread 65` 被 `haproxy -c` 拒绝（退出码非 0）。若它接受了，说明这台机器的
      HAProxy 编译期 `MAX_THREADS` 大于 64 —— 把实际值记下来，代码里的上限要跟着改。

**实测结果**：
```
haproxy 版本：
master-worker 三条 OK 情况：
nproc=           配置里的 nbthread=
nbthread=65 时的报错原文（或"被接受"）：
```

**实测结果**：
```
```

---

### 项 4 — 所有生成的配置都能过 `haproxy -c`

```bash
# current（基线）必须过
haproxy -c -f /etc/shengyu-edgelink/haproxy/current/haproxy.cfg
# 之后每发布一版都会产生 versions/<node>/vN/haproxy.cfg，逐一过
find /etc/shengyu-edgelink/haproxy/versions -name haproxy.cfg -print -exec haproxy -c -f {} \;
```

**判据**
- [ ] 每条都 `[ALERT] ... Configuration file is valid`（退出码 0）。
- [ ] 没有任何 `unknown keyword` / `unknown converter` 告警 —— 这两类正是 `docs/07` 记录的历史错误，出现了就把完整输出贴回来。

> 注意：`-c` 只验证**语法**。它验证不了"语法合法但语义错了"（典型如 TLS 健康检查改写业务传输），
> 那部分由 §4 的/TCP 实际连接来验证，别用 `-c` 通过冒充功能正确。

**实测结果**（本节在每次发布后都要重跑一次，最后统一粘贴）：
```
```

---

### 项 5 — shengyu 用户对配置目录可写

```bash
#（shengyu）runuser -u shengyu -- test -w /etc/shengyu-edgelink/haproxy/current && echo OK
#（shengyu）runuser -u shengyu -- \
    /usr/local/bin/shengyu-edgelink -config-root /etc/shengyu-edgelink/haproxy -write-baseline \
  && echo OK: 以服务身份重写出基线成功
# 反例核对：确认不是靠 root 才写进去的
sudo ls -l /etc/shengyu-edgelink/haproxy/current/haproxy.cfg
```

**判据**
- [ ] 两个 OK 都出现。
- [ ] `haproxy.cfg` 属主为 `shengyu`。
- [ ] 反过来用 root 的同一次执行**不被当作通过依据**（重点就是"最终身份能写"）。

**实测结果**：
```
```

---

### 项 6 — 非 root 管理面经 polkit 触发 reload

```bash
# 先让 HAProxy 跑起来
#（root）systemctl enable --now shengyu-edgelink-haproxy
systemctl is-active shengyu-edgelink-haproxy

# 关键：以 shengyu 身份请求 reload（这正是发布流程里程序会做的事）
#（shengyu）runuser -u shengyu -- systemctl reload shengyu-edgelink-haproxy
echo "exit=$?"
journalctl -u shengyu-edgelink-haproxy --since '1 min ago' | tail -5
journalctl -u polkit  --since '1 min ago' | tail -5
```

**判据**
- [ ] `runuser -u shengyu -- systemctl reload shengyu-edgelink-haproxy` 退出码 0、无 "Access denied"。
- [ ] `journalctl -u shengyu-edgelink-haproxy` 出现重新加载相关日志，且**进程没有重启**（对比 reload 前后的 master PID 不变）。
- [ ] 全程没有用到 sudo；`sudo -u shengyu systemctl reload shengyu-edgelink-haproxy` 若被执行，应该**失败**（NoNewPrivileges 冲突）——这一条是反例验证，确认我们确实不依赖 setuid 提权。

**实测结果**：
```
```

---

### 项 7 — HAProxy worker 明确降权，不长期以 root 运行

```bash
ps -o user=,pid=,ppid=,args= -C haproxy
grep -nE '^[[:space:]]+(user|group)[[:space:]]+' /etc/shengyu-edgelink/haproxy/current/haproxy.cfg
grep -E '^(User|Group|CapabilityBoundingSet|AmbientCapabilities|NoNewPrivileges)=' /etc/systemd/system/shengyu-edgelink-haproxy.service
```

**判据**
- [ ] `ps` 输出**每一行**的第一列（含 master）都是 `shengyu`。出现任何一行 `root` 即不通过。
- [ ] 配置里有 `user shengyu` / `group shengyu` 两行（这是"即使有人手工以 root 起 haproxy，worker 仍会降权"的那一层保证，见 `docs/04 §8`）。
- [ ] unit 里 `User=shengyu`、`Group=shengyu`、`CapabilityBoundingSet=CAP_NET_BIND_SERVICE`、`AmbientCapabilities=CAP_NET_BIND_SERVICE`。
- [ ] 补充确认：仍能绑定 443 —— 见项 9 中 `ss -lntp | grep ':443'` 由 haproxy 持有。

**实测结果**：
```
（ps 输出必须原样贴，一行都不能少）
```

---

### 项 8 — 首次发布（含管理员初始化）

```bash
# 1) 建管理员（**必须**以服务身份跑，否则文件属主是 root，服务自己就写不进去了）
#（shengyu）runuser -u shengyu -- /usr/local/bin/shengyu-edgelink \
     -config-root /etc/shengyu-edgelink/haproxy -data /var/lib/shengyu-edgelink \
     -dataplane haproxy -admin-pass '<强口令>' -check

# 2) 起来
#（root）systemctl enable --now shengyu-edgelink-haproxy shengyu-edgelink-server
systemctl is-active shengyu-edgelink-haproxy shengyu-edgelink-server

# 3) 拿到 node id
curl -c /tmp/ck -s -X POST http://127.0.0.1:8081/api/login \
     -d '{"username":"admin","password":"<强口令>"}' | tee /tmp/login.json
CSRF=$(sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p' /tmp/login.json)
curl -sb /tmp/ck http://127.0.0.1:8081/api/nodes | tee /tmp/nodes.json
NODE=$(sed -n 's/.*"id":"\(node_[^"]*\)".*/\1/p' /tmp/nodes.json | head -1)

# 4) 首次发布（此时还没有任何业务 —— 验证"空白配置也能发"）
curl -sb /tmp/ck -w '\nHTTP=%{http_code}\n' -X POST \
     -H "X-CSRF-Token: $CSRF" \
     http://127.0.0.1:8081/api/nodes/$NODE/publish -d '{"note":"首次发布"}'
```

> 单机方案下，若源站用 `127.0.0.1`，需要在 `shengyu-edgelink-server.service` 的 ExecStart 里临时加
> `-allow-loopback-origin=true` 并 `systemctl daemon-reload && systemctl restart shengyu-edgelink-server`。
> **验收结束后必须撤掉这个参数**（生产禁止）。

**判据**
- [ ] `-check` 自检通过（无 "自检未通过"）：其中应包含「HAProxy 转发进程将降权到 shengyu:shengyu」。
- [ ] 出现「已确认可对 shengyu-edgelink-haproxy.service 执行平滑 reload（当前身份 ✓）」—— 这一条同时证明项 6 是真的。
- [ ] 两个服务都是 `active`，`journalctl -u shengyu-edgelink-server` 没有 permission denied / EROFS。
- [ ] 发布返回 HTTP 200 且 `status` 为 `succeeded`；`version` ≥ 1。
- [ ] 发布后重跑项 4：`haproxy -c` 对新生成的版本目录通过。

**实测结果**：
```
```

---

### 项 9 — 多域名共享 443，互不串流

准备两个本地 TLS 源站（示例用 openssl，或用任意 HTTPS server）：

```bash
# 两个不同证书不同响应的 TLS 源站（单机方案）
openssl req -x509 -newkey rsa:2048 -nodes -keyout /tmp/a.key -out /tmp/a.crt -days 30 -subj '/CN=a.example.com'
openssl req -x509 -newkey rsa:2048 -nodes -keyout /tmp/b.key -out /tmp/b.crt -days 30 -subj '/CN=b.example.com'
# 起两个回显服务略（用 openssl s_server -accept 9443 -cert a.crt -key a.key -www 等）
```

建 SNI 入口与两条业务，然后发布：

```bash
# SNI 入口
curl -sb /tmp/ck -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  http://127.0.0.1:8081/api/nodes/$NODE/sni-entries \
  -d '{"bind_addr":"0.0.0.0","bind_port":443}'          # 记下返回里的 id 作为 SNI_ENTRY

# 两条业务，各自不同源站
curl -sb /tmp/ck -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  http://127.0.0.1:8081/api/businesses -d '{
    "customer_id":"'"$CUS"'","name":"业务A","mode":"sni_tls",
    "domains":["a.example.com"],"primary_node_id":"'"$NODE"'",
    "sni_entry_id":"'"$SNI_ENTRY"'","origin_host":"<源站IP>","origin_port":9443}'
# 业务B 同理：域名 b.example.com，origin_port 9444

# 发布
curl -sb /tmp/ck -X POST -H "X-CSRF-Token: $CSRF" http://127.0.0.1:8081/api/nodes/$NODE/publish -d '{"note":"多域名共享443"}'

# 验证：两条 TLS 连接必须各自到达自己的源站
openssl s_client -connect 127.0.0.1:443 -servername a.example.com -brief </dev/null 2>&1 | head
openssl s_client -connect 127.0.0.1:443 -servername b.example.com -brief </dev/null 2>&1 | head
ss -lntp | grep ':443'
```

> 用户这里要注意一个**验收口径**：`-brief` 只证明握手成功。要证明"互不串流"，必须看
> 源站侧的实际连接计数或响应体差异 —— 证书 CN 不同（`Verify return code` + subject）就是最直接的证据。

**判据**
- [ ] 两次握手呈现出**不同 subject/CN**，各自对应自己的域名。
- [ ] `ss -lntp` 显示 `:443` 由 haproxy 持有（且进程是非 root，呼应项 7）。
- [ ] 未配置域名（如 `nope.example.com`）握手**必然失败**。

**实测结果**：
```
```

---

### 项 10 — 长连接在 reload 之后仍能通信

```bash
# 建一条长连接并保持空闲/间隔收发
openssl s_client -connect 127.0.0.1:443 -servername a.example.com -quiet &
LPID=$!
sleep 2

# 触发一次真实发布（先做一点无关改动，或改另一条业务的源站端口）
curl -sb /tmp/ck -X POST -H "X-CSRF-Token: $CSRF" http://127.0.0.1:8081/api/nodes/$NODE/publish -d '{"note":"长连接验收"}'

# 复用上面那条已建立的连接继续发数据
#（在 s_client 的 stdin 上敲一行业务数据，观察源站是否仍在同一条连接上收到）
journalctl -u shengyu-edgelink-haproxy --since '1 min ago' | grep -i -E 'reload|worker|USR2'
```

**判据**
- [ ] reload 后**原连接没有被断开**：s_client 没有报 `read:errno=0`，在其 stdin 上发送数据后源站仍能收到。
- [ ] journal 出现老 worker 排空/新 worker 接管相关记录，且**没有**出现 `restart` 语义的整体重启。
- [ ] 明确记录：`hard-stop-after` 的取值决定了这条连接"最长能挺多久"（默认 30 分钟），
     它是**有上限**的，不是"永不中断"。验收报告里要把这个值写清楚。

**实测结果**：
```
```

---

### 项 11 — 端口冲突时原业务保持不变

```bash
# 外部先占住一个端口（模拟运维不小心起了一个监听器）
python3 -m http.server 18443 --bind 0.0.0.0 & 
sleep 1; ss -lntp | grep 18443

# 尝试把一条 TCP 端口业务发布到这个端口
curl -sb /tmp/ck -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  http://127.0.0.1:8081/api/businesses -d '{
    "customer_id":"'"$CUS"'","name":"冲突业务","mode":"tcp_port",
    "primary_node_id":"'"$NODE"'","entry_addr":"0.0.0.0","entry_port":18443,
    "origin_host":"<源站IP>","origin_port":9443}'
curl -sb /tmp/ck -w '\nHTTP=%{http_code}\n' -X POST -H "X-CSRF-Token: $CSRF" \
  http://127.0.0.1:8081/api/nodes/$NODE/publish -d '{"note":"端口冲突验收"}'

# 关键：老业务必须还在正常转发
openssl s_client -connect 127.0.0.1:443 -servername a.example.com -brief </dev/null 2>&1 | head
cat /etc/shengyu-edgelink/haproxy/current/VERSION
```

**判据**
- [ ] 发布**失败**（非 2xx，或返回的 release `status` 为 `failed`），且失败原因可读地指向端口冲突/验证未通过。
- [ ] **既有 443 业务完全不受影响**：握手照常成功。
- [ ] `current/VERSION` **没有推进**到冲突的那一版（这是"冲突不该把生效版本改坏"的直接证据）。
- [ ] `GET /api/nodes/$NODE/releases` 里该次失败有记录，可被追溯。

**实测结果**：
```
```

---

### 项 12 — 配置替换中途失败要完整回滚

故障注入方式（选一种，记录你选了哪种）：

```bash
# 方式 A（推荐，确定性最好）：让版本目录对 shengyu 不可写，制造"写到一半失败"
#（root）chmod 550 /etc/shengyu-edgelink/haproxy/current
curl -sb /tmp/ck -w '\nHTTP=%{http_code}\n' -X POST -H "X-CSRF-Token: $CSRF" \
  http://127.0.0.1:8081/api/nodes/$NODE/publish -d '{"note":"故障注入"}'
#（root）chmod 750 /etc/shengyu-edgelink/haproxy/current

# 检查：current/ 的内容与失败前完全一致
sha256sum /etc/shengyu-edgelink/haproxy/current/haproxy.cfg
cat /etc/shengyu-edgelink/haproxy/current/VERSION
openssl s_client -connect 127.0.0.1:443 -servername a.example.com -brief </dev/null 2>&1 | head
```

**判据**
- [ ] 注入后发布失败，错误原因明确（不是被静默吞掉）。
- [ ] `current/` **没有被写成半截**：`haproxy.cfg` 与失败前同一份（`haproxy -c -f` 仍通过）、`VERSION` 未变。
- [ ] 故障期间与清理权限后，**转发一直正常**（新连接能握手）。
- [ ] 没有残留"新正文 + 旧版本号"的混合状态（评审 F06 就是这个场景）。

**实测结果**：
```
```

---

### 项 13 — 管理面停止，转发继续

```bash
# 先确认当前有业务在跑并有长连接
#（root）systemctl stop shengyu-edgelink-server

sleep 30
openssl s_client -connect 127.0.0.1:443 -servername a.example.com -brief </dev/null 2>&1 | head
ps -o user=,pid=,args= -C haproxy

# 恢复
#（root）systemctl start shengyu-edgelink-server
```

**判据**
- [ ] `shengyu-edgelink` 停止期间，**新连接仍能建立**、已有长连接未断。
- [ ] `shengyu-edgelink-haproxy` 仍 `active`，进程身份仍是 shengyu。
- [ ] 管理面恢复后不需要任何补救操作即可继续发布（右下 shard 未损坏）。
- [ ] 需要**实测并如实记录**一件事：管理面停止的这段时间里，HAProxy 仍往
      `/run/shengyu-edgelink/log.sock` 投递日志，但没人接收 —— 这部分日志会不会丢、丢多少，
      只能以实测为准（看恢复后缺口时间段能否被查到），不许拿"理论上应该……"当结论。

**实测结果**：
```
```

---

### 项 14 — 整机重启后自动恢复

```bash
#（root）reboot
```

重启后：

```bash
systemctl is-active shengyu-edgelink-haproxy shengyu-edgelink-server
ps -o user=,pid=,args= -C haproxy
cat /etc/shengyu-edgelink/haproxy/current/VERSION
openssl s_client -connect 127.0.0.1:443 -servername a.example.com -brief </dev/null 2>&1 | head
curl -s http://127.0.0.1:8081/api/health
```

**判据**
- [ ] 两个服务都自动起来了（`WantedBy=multi-user.target`）。
- [ ] `VERSION` 与重启前一致，**不需要重新发布**。
- [ ] 443 业务立刻可用；`/api/health` 正常。
- [ ] `/run/shengyu-edgelink` 及其内的套接字被 tmpfiles.d 正确重建（若缺失会出现"日志收不到"）。
- [ ] worker 身份仍是 shengyu（重启不该把权限跑偏）。

**实测结果**：
```
```

---

### 项 15 — 日志：落盘 → 查询 → 压缩 → 恢复

```bash
# 1) 造流量，看日志有没有落到分片
openssl s_client -connect 127.0.0.1:443 -servername a.example.com -brief </dev/null
find /var/lib/shengyu-edgelink/logs -name '*.db' | head

# 2) 通过管理面查询（用真实 API，而不是直接 sqlite3）
curl -sb /tmp/ck "http://127.0.0.1:8081/api/logs/conn?business_id=<业务ID>&limit=20"

# 3) 压缩与保留：查看当前策略，然后把保留期调到极小观察压缩/删除
curl -sb /tmp/ck http://127.0.0.1:8081/api/retention
find /var/lib/shengyu-edgelink/logs -name '*.db.gz' | head
# 压缩过的分片仍要能查（这是 F12 的核心承诺：压缩不丢查询能力）
journalctl -u shengyu-edgelink-server --since '20 min ago' | grep -i -E '维护|压缩|删除'
```

**判据**
- [ ] 连接产生后有新的分片文件出现，且**能被 `/api/logs/conn` 查到**（写出来却查不到是评审 F12 的老问题）。
- [ ] 维护日志出现「日志维护：删除 N 个分片…压缩 N 个分片（X MiB → Y MiB）」。
- [ ] 出现 `.db.gz` 后，对相应时间窗的查询**仍然返回结果**，且数量与压缩前一致。
- [ ] 若磁盘低于水位，日志出现「【磁盘告警】…」，且确实删了最旧分片。

**实测结果**：
```
```

---

### 项 16 — 源站不可达时状态为 `applied_origin_unhealthy`

```bash
# 把一条业务的源站指到一个不可达地址（私有网段未占用端口即可）
curl -sb /tmp/ck -X PUT -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  http://127.0.0.1:8081/api/businesses/<业务ID> -d '{"origin_host":"10.255.255.1","origin_port":9}'
curl -sb /tmp/ck -w '\nHTTP=%{http_code}\n' -X POST -H "X-CSRF-Token: $CSRF" \
  http://127.0.0.1:8081/api/nodes/$NODE/publish -d '{"note":"源站不可达验收"}'
```

**判据**
- [ ] 发布**返回成功语义但带警告**：release `status` 恰为 `applied_origin_unhealthy`。
- [ ] 同时必须区分清楚两件事，验收时分别记录：
      - **配置本身已生效**（`config_healthy=true`，`haproxy -c` 仍过，其它业务照常转发）；
      - **该业务的源站不可达**（不是"发布失败"）。
- [ ] 健康详情里这条业务的路径维度为 unhealthy，而 Agent/Proxy 维度仍 healthy（三维健康不是"一坏全坏"）。
- [ ] 把源站改回可达地址再发布一次，状态应回到 `succeeded`。

**实测结果**：
```
```

---

### 项 17 — 完整验收报告

把上面 16 项的输出汇总成一份报告，必须包含：

1. **环境信息**：`cat /etc/os-release`、`uname -a`、`haproxy -vv | head -3`、`nproc`、`free -h`、`df -h /var/lib/shengyu-edgelink`。
2. **身份证据**：项 7 的 `ps` 原始输出；
3. **每一项的命令 + 原始输出 + 通过/不通过**（不通过的那几项要单独列出原因）。
4. **回填 `docs/07`**：把第 3 节里标着「待真实 HAProxy 验证」的 18 条逐条对照实测结果改成 `verified`
   （并同步代码：`internal/haproxy/spec.go` 的 `Registry` 里各字段的 `Status`，或由
   `scripts/verify-log-fields.sh` 产出的 `field-availability.json` 经 `ApplyAvailability` 覆盖）。
5. **同步 README**：`README.md §二·补` 里凡标「已修（待 Linux 验证）」的 F01/F02/F03 等项，按实测结果改为已修或改回未修。
6. **实测与文档不一致的地方单独列出** —— 这类差异比"某条挂了"更有价值，因为它说明我们某处理解错了。

> **诚实要求**：任何一项没跑、或跑出与预期不同的结果，就如实写"未完成 / 结果不符"，
> 并附上原始输出。禁止把"没执行"写成"通过"，也禁止把"不确定"写成"应该没问题"。

---

## 4. 已知在完成机器上需要重点复核的点

这些不是推测的结论，而是"文档/代码里写着的承诺"，需要真机证据：

| # | 承诺 | 靠什么复核 |
|---|---|---|
| 1 | worker 降权到 shengyu（含 master） | 项 7 的 `ps` 输出 |
| 2 | 非 root 能触发 reload 且不依赖 setuid | 项 6 + 反过来验证 sudo 必失败 |
| 3 | 2.8 语法全部合法 | 项 4 的 `haproxy -c` 全量跑过每一版 |
| 4 | 长连接在 reload 后不断 | 项 10（留意 `hard-stop-after` 上限） |
| 5 | 端口冲突不改坏现有业务与生效版本 | 项 11 的 `VERSION` 比对 |
| 6 | 替换失败完整回滚，无半截状态 | 项 12 的哈希比对 |
| 7 | 管理面停不影响转发 | 项 13（含日志取舍的如实记录） |
| 8 | 重启后自动恢复且版本不变 | 项 14 |
| 9 | 日志"写了就能查、压缩后仍能查" | 项 15 |
| 10 | 源站不可达 ≠ 发布失败 | 项 16 的 `applied_origin_unhealthy` |
| 11 | 线程数 = 进程可见 CPU 数，上限 64 是真的 | 项 3 的 `nproc` 比对与 `nbthread 65` 探针 |

---

## 5. 附加验收：升级与卸载（项 18–19）

17 项之外另加这两项，因为**安装脚本的"第二次运行"和"反向运行"从来没被真机跑过**，
而生产里真正高频的恰恰是这两条路径（第一次安装只发生一次）。

### 项 18 — 原地升级（服务**不提前停**）

```bash
#（root）先记下"升级前"的状态
/usr/local/bin/shengyu-edgelink -version
systemctl is-active shengyu-edgelink-server shengyu-edgelink-haproxy

# 服务保持运行，直接装新包 —— 就是要验"运行中覆盖二进制"这条路
sudo bash deploy/install.sh --dry-run
sudo bash deploy/install.sh

# 装完后
/usr/local/bin/shengyu-edgelink -version
systemctl restart shengyu-edgelink-server      # 只重启管理面；转发由 HAProxy 独立承担，不受影响
```

**判据**
- [ ] 安装过程**没有**出现 `Text file busy`。这正是旧写法的坑：直接写正在被执行的
      inode 会返回 ETXTBSY，于是"服务开着时升级"必然失败，而报错在现场极易被误判成
      "磁盘只读 / 包损坏"。现在的写法是先写同目录临时文件再 `mv`（原子替换）。
- [ ] 安装结束打印「升级提示」，指出正在运行的服务用的仍是旧二进制并给出该执行的命令；
      脚本**不自动重启**（重启会断开在途连接，属于运维决策）。
- [ ] 重启管理面期间，项 10 那条长连接**不断**（趁这次一并复测）。
- [ ] `-version` 变成新版本号；业务与日志数据完全不变。

**实测结果**：
```
升级前版本：
安装输出中的关键行（含"原子替换"/"升级提示"）：
是否出现 Text file busy：
重启管理面期间长连接状态：
升级后版本：
```

---

### 项 19 — 卸载与重装

```bash
#（root）第一步永远是预演
sudo bash deploy/uninstall.sh
# 真正执行（**保留数据**）
sudo bash deploy/uninstall.sh --yes

ls /etc/shengyu-edgelink/haproxy /var/lib/shengyu-edgelink   # 数据应仍在
systemctl is-active shengyu-edgelink-server         # 期望：inactive
ls /etc/systemd/system/shengyu-*.service   # 期望：No such file

# 重装，确认数据被沿用
sudo bash deploy/install.sh
systemctl enable --now shengyu-edgelink-haproxy shengyu-edgelink-server
```

**判据**
- [ ] 不加 `--yes` 时**不做任何修改**（输出里明确写着"以上是预演结果"）。
- [ ] 默认卸载**保留** `/etc/shengyu-edgelink/haproxy` 与 `/var/lib/shengyu-edgelink`（唯一业务数据）。
- [ ] 已删除：两个 unit、polkit 规则、sysctl、tmpfiles、`/usr/local/bin/shengyu-edgelink`，
      并执行了 `daemon-reload`。
- [ ] **没有**删除 shengyu 系统账号（脚本故意保留：删账号会让仍属主于它的文件变成悬空 uid）。
- [ ] 重装后业务与日志数据被沿用（不需要重新建业务）。
- [ ] `--purge-data` 只在**数据可以丢的机器**上验一次，验完重装；它必须真的把两个目录删掉。

**实测结果**：
```
预演输出（关键行）：
执行后保留/删除对照：
重装后数据是否沿用：
```

---

## 6. 回滚 UI 真机验收（评审缺陷一 / 缺陷二修复后）

> 评审要求：**这两个问题修复前，不要把 B2 标记为通过，也不要写"回滚成功并验证通过"。**
> 本节按此纪律组织：先说修了什么、本地证据是什么，再写真机执行状态。
> **真机验收尚未执行**，因此本节所有"实测结果"栏保持空白/待填，B2 状态 = **未通过（待真机验证）**。

### 6.1 缺陷一：回滚验证被空 nodeID 绕过

**现象**（v0.3.1 真机可稳定复现）：`rollbackIfNeeded` 调 `p.rollback(ctx, prevVer, "")`
传入空 nodeID，`p.rollback` 里 `if nodeID != "" && state.Node.ID != ""` 那段验证被整体跳过，
函数却仍然返回 `(true, "已自动回滚到第 N 版**并验证通过**")`。运维据此收工，
而线上到底在跑什么没人确认过。

**要求逐条 → 落点**

| 评审要求 | 实现位置 |
|---|---|
| 1. `rollbackIfNeeded` 禁止接收空 nodeID | `internal/publish/publish.go`：签名改为 `rollbackIfNeeded(ctx, nodeID, prevVer)`，入口即守卫 |
| 2. nodeID 为空必须返回明确错误（`node_id_missing`） | `RollbackCodeNodeIDMissing = "node_id_missing"`，写进 `Result.rollback_code` 与说明；**不退回"当作本机"** |
| 3. 回滚文件恢复成功 ≠ 回滚验证成功 | `Result.rollback_restored`（文件集合逐字节一致）与 `Result.rollback_verified`（四项判据全成立）分开 |
| 4. 只有全部条件满足才允许 (rolled_back=true, rollback_verified=true, note 含"验证通过") | `inspectRollbackTarget`：① 文件集合 ② VERSION ③ 目标版本 stats socket 可连 ④ 目标版本 frontend/监听归属；另有前置"nodeID 非空且必须是本实例绑定的节点" |
| 5. Verify 未执行或失败时不得显示"并验证通过" | 失败措辞分两支：`已把第 N 版配置放回 current/，但**回滚验证未通过**：…` / `（文件已恢复），但**回滚验证没有执行**：…` |
| 6. 页面必须显示"已恢复但未验证"或"回滚验证失败"，并提供人工命令 | `internal/ui/index.html` 的 `renderFailure`：按服务端 `rollback_outcome` 分五种措辞，含"已恢复但未验证（需人工确认）"与"回滚验证失败（需人工介入）"，并显示"回滚验证（未执行/已执行通过/已执行未通过）""配置正文是否已恢复""结论代码""回滚判据（逐条）" |

**新增回归测试**（`internal/publish/rollback_verify_test.go`，本地已通过）

- `TestRollbackIfNeededRejectsEmptyNodeID` —— 评审点名的三条断言：
  Verify 不许"被静默跳过之后还当成成功"（若 `verifyCalls==0` 则必须有
  `node_id_missing`/`rollback_verification_not_run` 之一，且 `VerifyRan=false`、`Verified=false`）；
  结果不得出现"验证通过"；必须返回 `node_id_missing`；且**全过程 `Apply` 调用次数必须为 0**。
- `TestRollbackRefusesForeignNodeID` —— 传别的节点 ID 同样拒绝，且 `apply=0 / verify=0`。
- `TestAutoRollbackClaimsVerifiedOnlyAfterRealVerification` —— 通过时四条判据逐条留在 `rollback_checks`。
- `TestAutoRollbackMustNotClaimVerifiedWhenVerificationFails` —— `outcome=verify_failed`、
  `rollback_verified=false`、`rolled_back=false`、说明不得含"并验证通过"、必须有 `manual_fix`。
- `TestAutoRollbackRefusedWhenFileSetIncomplete` / `TestAutoRollbackRefusedWhenFileSetUnreadable` ——
  `rollback_restore_failed` / `rollback_verification_not_run`，且后者**不再继续跑 frontend 归属验证**（fail-closed）。
- `TestRollbackRestoredButNotVerifiedWhenSnapshotMissing` —— `outcome=restored_not_verified`、
  `rollback_verify_ran=false`、`rollback_restored=true`、说明含"回滚验证没有执行"；
  并断言人工命令里必须有"先看清现场"的三条（`systemctl status` / `VERSION` / `stats-v`）。
- `TestNoRollbackNeededMustBeVerifiedNotAssumed` / `TestNoRollbackNeededRefusedWhenCannotConfirmRunning`。
- `TestFileSetCheckRecordedAsNotApplicableWhenUnsupported` —— 不以文件为准的数据面记"不适用"并留痕。
- `TestExplicitRollbackRecordsVerificationEvidence` —— 运维点按钮的回滚同样留下验证证据。
- `internal/ui/embed_test.go` 新增 `TestHandlerDistinguishesRestoredFromVerified`（界面五态字面量 + 必须读服务端字段）。

### 6.2 缺陷二：成功发布没有清理陈旧辅助文件

**现象**：旧版本存在 `current/sni_allow_9443.lst`（9443 入口删掉之后），成功发布后该文件仍残留。
它不会被 HAProxy 读到（渲染出的配置引用的是**版本目录**的绝对路径），
但 `ls current/` 会显示一个"看起来当前生效"的允许清单，足以让排查者得出与事实相反的结论。

**要求逐条 → 落点**

| 评审要求 | 实现位置 |
|---|---|
| 1. 发布候选版本时生成完整文件清单 | `internal/haproxy/manifest.go`（`manifest.json` + `VersionManifest`）；`writeVersionDir` 一边写一边记，最后落盘清单 —— 清单是"刚刚真的写下了哪些文件"的副产物 |
| 2. 成功切换前清理 current/ 中候选版本不存在的旧文件 | `dataplane.HAProxy.pruneStaleFiles` 改为按清单 keep 集合删除，且**放在写文件之前**（先清后写，失败时 current/ 还没被动过） |
| 3. 删除旧文件失败时发布整体失败并恢复快照 | `publishFiles` 快照 → 清理 → 写 → VERSION；任一失败都 `restoreCurrent(snap)`，恢复失败则显式报出"current 目录可能不一致，请人工核对" |
| 4. 回滚必须恢复完整文件集合，不只是 VERSION 和 haproxy.cfg | 回滚走同一个 `Apply→publishFiles`，按目标版本清单写全集 + 清残留；并由 `CompareCurrentToVersion` 逐字节核对 |
| 5. 回归测试（旧版有 9443 清单、新版没有 ⇒ 发布后必须不存在；current/ 集合必须与目标版本完全一致） | `internal/dataplane/manifest_fileset_test.go`、`internal/publish/version_manifest_test.go` |
| 6. 检查旧的 stats socket、allowlist、meta.json 和其他辅助文件是否都会被清理 | 清理判据是"在不在清单里"，不看文件名长什么样 ⇒ allowlist / meta.json / state.json / manifest.json 以及将来任何新辅助文件自动覆盖；stats socket 属进程生命周期（`/run` 下由 HAProxy 绑定），**不属文件发布范畴**，真机验收时单独取证（见 6.4） |

**新增回归测试**（本地已通过）

- `manifest_fileset_test.go`：
  `TestPublishFilesKeepsCurrentEqualToTargetManifest`（旧版有 `sni_allow_9443.lst`、新版没有 ⇒
  发布后文件必须消失，且 `current/` 除 `VERSION` 外与目标版本目录**逐项相同**）、
  `TestCompareCurrentToVersionDetectsHalfRestore`（VERSION 回去了、正文没回去 ⇒ 必须报差异，
  否则"回滚成功"就是假话）、`TestPublishRefusesWhenManifestListsMissingFile`、
  `TestPublishRefusesWhenDirHasFileNotInManifest`、`TestPublishRefusesWhenManifestCorrupt`、
  `TestPublishFilesRestoresSnapshotWhenPruneFails`。
- `version_manifest_test.go`：`TestVersionDirManifestIsComplete`（清单与实际落盘文件集合完全一致）、
  `TestVersionManifestShrinksWithRemovedEntry`（停用 9443 后新版本清单里不再有它）。

### 6.3 本地验证汇总（可直接复现）

```
go build ./...            exit 0
go vet ./...              exit 0
go test ./...             20 个包全部 ok
go test -race ./...       20 个包全部 ok（无数据竞争）
gofmt                     无改动
./scripts/build-release.sh v0.3.4
  dist/shengyu-edgelink-linux-amd64
  sha256 82338df51320088630c1f3ec3ba82a9bb5890f7cfaab3271fd404ab4b2df0aa2
  大小   10793112 字节
  符号检查：go tool nm | grep -i testplane -> 0 命中（构建脚本的闸门）
```

**顺手修的真缺陷（不在评审清单里，单独列出）**

`scripts/build-release.sh` 的 `build_one` 只把二进制写进 stage 目录（脚本末尾会删），
从不写 `dist/shengyu-edgelink-linux-<arch>`；而 `SHA256SUMS` 又给这两个路径算摘要 ⇒
**校验值覆盖的是上一次构建残留的旧二进制**（上一轮 dist 里那个 `0274a64b…` 就是 v0.3.3 的旧文件，
而当时刚构建出来的 v0.3.4 摘要其实在 tar.gz 那条上）。
修法：`build_one` 里显式 `cp` 到 dist 根目录。修后 `SHA256SUMS-v0.3.4` 与实测一致。

### 6.4 真机验收（154.9.235.57:23722）

**执行状态：未执行。** 脚本已上传到测试机 `/tmp/b2-scripts/`（7 个），
修好的二进制在 `/tmp/shengyu-edgelink-v0.3.4`（sha256 与上表一致）。
一条命令即可跑完全部六个阶段：

```bash
bash /tmp/b2-scripts/b2-99-all.sh 2>&1 | tee /root/b2-accept.log
```

（详细步骤、回滚办法与需要回帖的内容见 `tools/b2-OPERATIONS.md`。）

**报告必须写出的七项 —— 第一轮（v0.3.4）实测值**

| 项 | 实测值 |
|---|---|
| nodeID 实际值 | `node_06GCGE2NPCC624J5AMKYQWC4C0` |
| Verify 是否实际执行（`rollback_verify_ran`） | `true`。另有**行为层反证**：见下方 4A —— 把回滚目标的 `stats-vN.sock` chmod 000 后，结论从"验证通过"翻成 `verify_failed`，只有 Verify 真在执行才可能出现这种变化 |
| `rollback_verified` | `true` |
| `current/VERSION` | `12` —— 失败的那次发布是 v13，**没有被留在 current/ 上**（`CHECK_VERSION_NOT_ADVANCED=PASS`）；`cfg_sha256` 与失败前一致（`9a1eb6ea…`） |
| `current/` 文件清单 | `haproxy.cfg  manifest.json  meta.json  sni_allow_8443.lst  state.json  VERSION`（6 个） |
| 旧 `sni_allow_9443.lst` 是否已删除 | **已删除**。两处独立证据：① 07:03 的改动前快照里 current/ **还有**这个文件（旧代码留下的残留，`sha256=ed5f6e28…`，时间戳 09-23 11:35），而 v2 自己的版本目录里从来没有它；② v0.3.4 发布/回滚之后 `CHECK_9443_REMOVED=PASS` + `CHECK_9443_DELETED=PASS`（两次），且 `current/` 里"不在清单内"的残留数为 0 |
| 8443 业务是否仍正常 | `CONNECTION ESTABLISHED / Protocol version: TLSv1.3`；HAProxy master PID `3426717` **从第 0 步到第 5 步全程未变**（管理面重启、6 次发布/回滚都没动过转发进程） |

**场景实测（第一轮）**

| 场景 | 期望 | 实测 |
|---|---|---|
| B2（reload 只失败一次，v0.3.4） | 失败阶段为 reload、不谎报"已回滚"、`current/VERSION` 不推进 | `phase=reload`；`rolled_back=false`、`rollback_verified=true`、`rollback_verify_ran=true`、`rollback_restored=true`、`rollback_outcome=not_needed`、`no_rollback_needed=true`；note =「已确认数据面仍在正常运行第 12 版：本次改动没有生效，无需回滚」；`manual_fix` 0 行；`CHECK_VERSION_NOT_ADVANCED=PASS`。<br>**结论：正确，但不是我原先预期的 `rolled_back=true`** —— 原因见 6.6-A |
| 4A（回滚目标 `stats-vN.sock` chmod 000） | `verify_failed` + 人工命令 | `outcome=verify_failed`、`rollback_verify_ran=true`、`rollback_verified=false`、`rolled_back=false`、`rollback_restored=true`、note 含"无法确认"、`manual_fix` 13 行、判据里"统计套接字 **不成立**""监听归属 0/1 **不成立**" —— **SCENARIO_4A=PASS** |
| 4B（回滚目标缺 `state.json`） | `restored_not_verified` | 得到的是 `outcome=failed` / `code=rollback_restore_failed` —— **SCENARIO_4B=FAIL**，原因与修正见 6.6-B |
| 负向：向别的 nodeID 发布 | 409 `remote_node_unsupported` | 得到 **404 `not_found`「节点不存在」**（请求仍被拒绝，但**不是**我预期的那个分支）；向不存在的节点发布会先在节点查询处 404，走不到 `checkLocalNode` 的 409 |
| 清理后 `current/` 与第一轮备份逐字节比对 | 一致 | `FILESET_SAME=FAIL` / `BYTE_IDENTICAL=FAIL`，**唯一差异是 `sni_allow_9443.lst` 被删掉了**（其余 5 个文件 sha256 全部 SAME、`VERSION` SAME）。也就是说：恢复本身是成功的，差异恰恰是缺陷二被修好的结果 —— 改动前那份 current/ 里本来就不该有那个文件 |
| 清理后 `ExecReload` | 回到 `/bin/kill -USR2 $MAINPID` | 是（`{ path=/bin/kill ; argv[]=/bin/kill -USR2 $MAINPID }`），`10-b2-reload-inject.conf` 与注入器均已删除，`haproxy -c` valid |
| 旧 stats socket 是否残留/可连 | 逐个探测 | 探测日：`stats-v0/v1/v2/v3/v11/v12/v15` 共 7 个文件，**只有当时在跑的那个 ALIVE**（`Version: 2.6.12-1+deb12u3`），其余全部 `ConnectionRefusedError`。清尾时在跑的 v2 为 ALIVE。**结论：文件会按版本累积，但是惰性的**（无 worker 应答），不会造成"假验证通过" —— `Verify` 还要同时满足 VERSION 标记与监听归属 |

### 6.5 已知偏差与未验证项（如实记录，不得省略）

1. **`sni_entries` 没有删除接口** ⇒ 验收用的 9443 入口是**改库**删的（先备份 `meta.db`）。
   这既是本轮的操作偏差，也是产品侧的缺口，应作为待办提出。
2. **无法证明"8443 入口在本次验收前就存在"**：`sni_entries` 无 `updated_at` 列，
   上一轮留下的时间线只能说明"8443 入口的创建时间早于既有业务的创建时间"，
   不能反推它一定先于验收。报告只能给时间线证据 + "当前状态与验收前一致"的结论。
3. **旧 stats socket 的归属**：`/run/shengyu-edgelink/stats-vN.sock` 由 HAProxy 进程在绑定
   unix socket 时创建，属进程生命周期，不由本平台的文件发布负责。上一轮实测观察到
   多个版本的 socket 并存；它们是**惰性**的（无 worker 时 connect 被拒），不会造成"假验证通过"
   （`Verify` 还要同时满足 VERSION 标记与监听归属）。本轮仍逐个探测并如实记录，
   **不**做自动删除 —— 删掉一个排空中的旧 worker 的 socket 会破坏 `Stats()` 的兜底取值。
4. **未验证：真人浏览器走查**。本轮界面结论来自 `GET /` 的字节内容断言
   （`internal/ui/embed_test.go` 钉住五态字面量必须存在）与接口返回的 JSON 字段，
   没有在真实浏览器里点过一遍失败面板。
5. **未验证：`rollbackOutcomeNotApplied`（渲染/写目录/语法检查阶段失败）在真机上的表现**。
   这条分支只会给出"改动尚未下发到数据面（无需回滚）"，真机上未构造对应场景。

---

## 7. 第一轮真机验收（v0.3.4）暴露的问题与修复（v0.3.5）

这一节记的是**只有真机才会暴露**的四件事。本地 20 个包的测试全绿，一条都没提前发现。

### 7.1 产品真缺陷：验证口径与渲染器不一致 → 空 SNI 入口造成假失败

**真机原始证据**（`/root/b2-accept.log`，第 2 步，v0.3.4）：

```
status = failed | version = 11 | phase = verify
message = 应用后验证未通过：版本 11 配置已生效，但存在以下问题：0.0.0.0:9443 未监听
          （已等待 30.1 秒、重试 61 次，仍未就绪）；已自动回滚到第 2 版并验证通过
          （文件集合、版本标记、统计套接字、监听归属四项判据全部成立）
```

**成因**：验收脚本先建了一个 9443 的 SNI 入口，但业务没建成功（客户响应解析写错，
`customer_id` 为空 → 建业务 400），于是该入口是**空的**（没有任何启用路由）。
渲染器 `haproxy.Render` 对空入口是**故意跳过**的（注释原文："空入口不下发：避免占用端口
却没有规则"），因此配置里没有 9443 的 frontend —— 这是设计行为；
而 `dataplane.ExpectedListeners` 却把"每条启用的 SNIEntry"都算作应当监听，
于是 Verify 一直等一个永远不会出现的监听。

**代价不是"更严格"**：一次假失败 + 一次假回滚 + 运维去查一个不存在的故障 + 30 秒白等。
而且 `meta.json` 里 `rendered.ExpectedListeners`（渲染器口径）与
`dataplane.ExpectedListeners`（验证口径）是两个数 —— **同一个概念有两份实现，
迟早会分叉，这次就是分叉了**。

**修法**：新增 `dataplane.sniEntriesInUse`，只把"至少有一条启用 SNI 路由引用"的入口算作在用；
`ExpectedListeners` 与 `expectedFrontends` 共用它。并加**跨包对拍**测试：
`TestExpectedListenersAgreeWithRenderer` 把 `dataplane.ExpectedListeners(st)` 与
`haproxy.Render(st).ExpectedListeners` 的键集合逐个比对 —— 这类"两个包各算一遍"的分歧，
只有对拍才发现得了。

### 7.2 产品风险：回滚失败时 `current/` 停在未获批准的新版本上

**真机原始证据**（第一轮 4B，v0.3.4）：故意把 v12 的 `state.json` 移走，然后发布。

```
rollback_outcome = failed          rollback_code = rollback_restore_failed
current/VERSION  = 15              （而正在服务的是 v12 的 worker）
rollback_note    = 回滚到第 12 版失败：haproxy: 清理 current/ 里不属于新版本 12 的文件失败，
                   已完整恢复旧配置（现有转发不受影响）: 版本清单 manifest.json 列出了 "state.json"，
                   但版本目录 .../v12 里没有这个文件 —— 版本目录不完整，拒绝发布
```

平台 **fail-closed 拒绝回滚是对的**（拿一份不完整的版本去回滚只会更糟），
但结果是 `current/VERSION=15`：**未获批准的新版本留在了 current/**。
此时任何一次 `systemctl reload`（甚至一次计划外重启）都会加载它。
本次因为 v15 恰好是合法配置所以没出事，但这就是"一次失败被放大成重启即故障"的路径。

**修法**（v0.3.5）：新增 `Pipeline.warnCurrentNotTarget`，在所有"回滚没能让目标版本生效"
的返回路径上都去读一次 `ActiveVersion`，只要它不是目标版本，就在说明里追加：

> **警告：current/ 目前停留在第 15 版，不是第 12 版 —— 在恢复之前请不要执行
> systemctl reload / restart，否则会加载第 15 版这份未通过验证的配置**
> （正在服务的仍是老 worker，所以此刻转发通常还是正常的）

同时 `manualFixCommands` 的第 0 条固定为「⚠️ 在确认 current/ 里的内容之前，不要执行
systemctl reload / restart」。回归测试：`TestRollbackFailureWarnsThatCurrentIsNotTheTarget`
（用 `rollbackStub.failRollbackApply` 构造"回滚下发失败且数据面停在新版本上"）。

### 7.3 验收脚本自身的两处修正（不是产品问题）

1. **B2 场景的期望写错了**。`reload 只失败一次` 时数据面会**自愈**：`HAProxy.Apply` 在 reload
   失败后会自己把 current/ 换回上一版（v0.3.2 起），于是 `ActiveVersion == prevVersion`，
   `rollbackIfNeeded` 走"无需回滚"分支并去验证旧版确实在跑 → `not_needed`。
   这是**语义正确**的：没有发生过切换，就不该说"已自动回滚"。
   要拿到真正的 `rolled_back=true`，得构造"reload 返回 0 但什么都没做"（注入器 `mode=silent`）——
   那时 current/ 已换成新版、老 worker 还在跑，`waitVerify` 判失败 → 走真正回滚 → 四项判据全成立。
2. **4B 的构造方式不对**。要让"文件集合判据成立但缺状态快照"，必须**同时**把 `state.json`
   移走并从清单 `files` 里去掉；只移走文件会让清单校验先失败，结论落在 `failed` 而不是
   `restored_not_verified`（这正是真机 4B 得到 `rollback_restore_failed` 的原因）。

### 7.4 本地既有偶发（与本次修复无关，如实记录）

`e2e.TestEndToEndMainFlow` 在**整仓运行**（`go test ./...`，含 `-p 1`）时丢日志分片
（`分片说明 []`），单独跑 `go test ./e2e/` 三次全过。
**对照实验**：把 7.1 的口径改动回退成旧行为后，整仓运行**同样复现**同一处失败 ⇒
与本次修复无关，是既有的偶发。

### 7.5 第二轮（v0.3.5）验收状态与预期

**状态：未执行。** 二进制与脚本已上传（sha256 `fec0c54f…c45dc3`），一条命令：

```bash
bash /tmp/b2-scripts/b2-99c-second.sh 2>&1 | tee /root/b2-accept2.log
```

| 场景 | 构造方式 | 期望结论 |
|---|---|---|
| S1 空入口不再假失败 | 建空 9443 入口后直接发布 | `status=succeeded`；`verify.listeners` 里没有 9443；正文不引用 9443 清单；端口 9443 REFUSED |
| S2 缺陷二复验 | 给该入口挂业务→发布；再删业务+删入口→发布 | 先出现 `sni_allow_9443.lst`，再 `CHECK_9443_REMOVED=PASS` + 文件集合与版本目录完全一致 + 逐文件 sha256 SAME + 无多余残留 |
| S3 自愈 | 注入器 `mode=once` | `outcome=not_needed`、`rolled_back=false`、`rollback_verified=true`、note 不含"并验证通过"、VERSION 不推进 |
| **S4 真回滚** | 注入器 `mode=silent` | **`rolled_back=true` + `rollback_verified=true` + `rollback_verify_ran=true` + `outcome=verified` + note 含"并验证通过" + 四条判据齐全 + `manual_fix` 为空**（v0.3.4 那次是靠空入口缺陷的副产物才走到这里，这次是主路径） |
| S5 验证真的在跑 | 回滚目标 `stats-vN.sock` chmod 000 | `outcome=verify_failed`、`verify_ran=true`、`manual_fix` 非空 |
| S6 已恢复但未验证 | 回滚目标移走 `state.json` **并**从清单去掉 | `outcome=restored_not_verified`、`verify_ran=false`、`code=rollback_verification_not_run` |
| S7 当前版本不是目标版本 | 只移走 `state.json`（清单仍列着） | `outcome=failed` + note 出现「**警告：current/ 目前停留在第 N 版**」+ 人工命令第一条是"先别 reload" |
| S8 负向 | 向不存在的 nodeID 发布 | 4xx 且 `code=not_found`（接受现实分支，不再硬套 409） |
| S9 清理恢复 | — | `FILESET_SAME=PASS` / `BYTE_IDENTICAL=PASS`（以第二轮改动前的快照为基准）/ `VERSION_SAME=PASS` / `ExecReload` 复原 / 无临时文件残留 |

### 7.6 v0.3.5 本地验证

```
go build ./...        exit 0
go vet ./...          exit 0
go test ./...         19/20 包 ok；e2e 为 7.4 记录的既有偶发（对照实验已排除与本次改动相关）
gofmt                 无改动
./scripts/build-release.sh v0.3.5
  dist/shengyu-edgelink-linux-amd64  sha256 fec0c54f19d3f08606cc26679592a8e0c962989d7d9334b190a7eadc28c45dc3
  大小 10801304 字节
```

新增/改写测试：`TestExpectedListenersSkipsEmptySNIEntry`、
`TestExpectedListenersAgreeWithRenderer`（跨包对拍）、`TestExpectedListenersFollowsEntryEnabledFlag`、
`TestRollbackFailureWarnsThatCurrentIsNotTheTarget`；
并修正三处"夹具不真实"的既有测试（`dataplane.modelStateWith`、`testplane` 的两处 SNI 用例与
`TestExpectedListeners`）—— 那些夹具只设了 `Mode: sni_tls` 却没有 `SNIEntryID`，
而渲染器本来就要求 `SNIEntryID` 才会下发 frontend，属于夹具与真实数据形状不符。

---

## 8. 第二轮（v0.3.5）执行中断的根因：验收脚本把测试机数据写坏了

### 8.1 现象

第二轮日志里 `S4/S5/S6/S7` 全 FAIL，且发布的返回体里**所有字段都是 `None`**：
`publish_http=422`、没有 `status`/`phase`/`rollback_*`。
`422` 说明请求**根本没进发布流水线**，是在 API 前置校验就被拒了
（`handlePublish` 里只有两个 422：`validation_failed` 与 `port_in_use`）。
连带 `CHECK_9443_REMOVED=FAIL`、`CHECK_FILESET_EQUAL=FAIL`（`V3=None` → 比了个空目录）。

### 8.2 根因链（每一步都有代码依据）

1. `POST /api/businesses` 返回的是**包装体** `{"business":{...},"next_step":...}`
   （`handlers.go` 的 `writeJSON(w, http.StatusCreated, map[string]any{"business": b, ...})`），
   而我在验收脚本里按**裸对象**解析 `.get('id')` ⇒ `BIZ_ID` 为空。
   （对比：`POST /api/customers` 返回的是**裸对象** `writeJSON(w, 201, c)` ——
   两个接口形状不一样，我按同一个模子解析，错了一边。）
2. `BIZ_ID` 为空 ⇒ `DELETE /api/businesses/` 删了个空 ⇒ **404**，业务没删掉。
3. 业务还在 ⇒ 它在 9443 入口上的**路由还在**（`routes.business_id` 有
   `ON DELETE CASCADE`，但没删业务就轮不到级联）。
4. 脚本接着用 SQL 删掉 9443 入口。`routes.sni_entry_id` **没有外键约束**
   （`schema.go` 里那一列没有 `REFERENCES`），所以数据库**没有拦**，
   于是留下一条悬空引用。
5. 此后每次发布：`handlePublish` → `validate.State` →
   `SNI_ENTRY_MISSING`「引用的 SNI 入口 %s 不存在」→ **422 validation_failed**。
   所有发布都被挡在"写文件 + reload"之前。

### 8.3 这次值得记的两件事

**① 平台做对了一件事：坏数据没有变成坏配置。**
悬空引用一旦被渲染，9443 那条路由会被静默丢掉（渲染器的 `sniByEntry` 按入口 ID 索引），
发布"成功"而业务不转发 —— 那才是最难查的故障。`validate.State` 这一层把它挡在了前面，
`current/` 一个字节都没动（实测 `VERSION` 全程没推进、旧配置继续服务）。
**校验器在这里救了这次事故。**

**② 平台缺一件事：`sni_entries` 没有删除接口。**
正因为没有 `DELETE /api/nodes/{id}/sni-entries/{eid}`，验收只能改库；
而改库绕过了所有一致性检查 —— 这次事故的直接推手就是这个缺口。
建议补上，并且**在有路由引用时拒绝删除**（或要求 `?force=1` 并级联删路由），
另可在 `/api/diagnose` 里加一条"悬空引用"检查：现在这个问题只有到"发布被 422"时才暴露。

### 8.4 验收脚本的修正（已随第三轮上传）

- 解析修正：业务按 `(d.get('business') or d).get('id')` 取，客户按裸对象取。
- **三重守卫**（缺一条就会重演这次事故）：
  1. `BIZ_ID` 为空 ⇒ 立刻中止，**绝不**继续删入口；
  2. `DELETE /api/businesses/{id}` 必须返回 **200**，否则中止；
  3. 删入口前先查 `select count(*) from routes where sni_entry_id=?`，**必须为 0**；
     删入口后先跑一次 `GET /api/nodes/{id}/preview`，**`validation_ok` 必须为 true** 才继续发布。
- `b2-99-all.sh`（第一轮的串联脚本）已加保护：直接拒绝执行并指向新脚本，
  防止有人再跑一遍旧期望。

### 8.5 第三轮执行状态

**状态：未执行。** 已上传 `b2-19-repair.sh`（诊断 + 修复 + 无损探测）与
`b2-99d-third.sh`（19 → 11 → 12 → 13），一条命令：

```bash
bash /tmp/b2-scripts/b2-99d-third.sh 2>&1 | tee /root/b2-accept3.log
```

其中"无损探测"这一步很关键：修复后发起一次**内容未变**的发布，
期望返回 `no_change`（不产生新版本、不 reload）——
它用一个零副作用的动作证明"发布链路真的通了"，而不是靠猜。

### 8.6 口径纪律（写给自己）

这一轮我犯的错，和这个仓库一直在防的错是同一类：**把"我期望的"当成"实际发生的"**。
- 第一轮：我以为 `reload 只失败一次` 必须走回滚，实际数据面会自愈 ⇒ 结论应是 `not_needed`。
- 第二轮：我以为业务创建返回裸对象，实际是包装体 ⇒ 脚本把数据写坏。
两次都不是产品的问题，两次都是"没先看真实返回/真实行为就写死了期望"。
所以后面每一步都先做**只读取证**（`/preview`、DB 查询、`ls`、`sha256`），
再动任何会改状态的东西。

---

## 9. 第四轮（v0.3.6）真机验收：全项通过

**B2 结论：通过。** 主路径上的真回滚（S4）拿到了 `rolled_back=true` + `rollback_verified=true`
+ note 含"并验证通过" + 四条判据齐全，`current/VERSION` 未被失败版本推进。

### 9.1 部署

| 项 | 值 |
|---|---|
| 二进制 | `v0.3.6`，sha256 `0ad467ea248571513c7fd012bac7238526b2ab9e7f4d1add69770f5fb88a9e23`（服务器实测与本地一致） |
| 替换前 | `fec0c54f…c45dc3`（v0.3.5） |
| 方式 | 只替换 `/usr/local/bin/shengyu-edgelink` 并重启**管理面**；HAProxy 未动 |
| HAProxy master PID | `3426717` —— 部署前后、以及之后 6 次发布/回滚，**全程未变** |

### 9.2 本轮修掉的一个真缺陷：`ListenerStatus` 的 JSON 键名

**真机症状**（第三轮 dump）：`verify.listeners = [(None, None, None, None)]`。
界面里同一处写的是 `l.addr`，于是**发布成功的气泡显示成"监听：undefined:undefined"**。

**根因**：`ListenerStatus` 当初只有 `Frontend` 带了 json 标签，`Expected`/`Bound`/`OwnedByUs`/`Note`
按 Go 默认规则序列化成驼峰（`Expected`/`Bound`/`OwnedByUs`/`Note`）。
两处"读到空值都不报错"的代码叠在一起，就成了一条**看起来验证过、其实什么都没验**的路径 ——
我那一条"9443 不在监听列表里"的断言也因此变成了空断言（永远成立）。

**修法**：给四个字段补 json 标签（`expected`/`bound`/`owned_by_us`/`note`），界面改读 `l.expected.addr`；
新增两个回归测试 `TestVerifyResultJSONShapeIsStable`（钉住键名，且禁止出现驼峰键）
与 `TestHandlerReadsNestedListenerAddress`（钉住界面读嵌套地址）。

**真机复验**（第四轮）：dump 直接打印出
`verify.listeners = [('0.0.0.0', 8443, True, True)]`（S1）与
`[('0.0.0.0', 8443, True, True), ('0.0.0.0', 9443, True, True)]`（S2）—— 键名与嵌套结构都对了。

### 9.3 本轮修掉的三处验收脚本问题

| 问题 | 症状 | 修法 |
|---|---|---|
| `tr '\\n' ','` 多了一个反斜杠 | `tr` 实际执行的是 `tr 'n' ','`，把**所有字母 n 换成了逗号**，基线文件清单被写成 `ma,ifest.jso,`，于是 `FILESET_SAME` 永远 FAIL | 改成单反斜杠；并且基线清单改从 `sha256 <name> <hash>` 行推导（逐文件记的那几行本来就是好的） |
| S6/S7 在**版本目录里**给 `state.json` 改名 | 留下一个"不在清单内"的文件 ⇒ `versionFileSet` 先报"目录与清单不一致，拒绝发布" ⇒ 结论落在 `failed`，测不到 `restored_not_verified` | 改成移出到 `/tmp/b2/moved/`，收尾再复位 |
| 监听取值用了扁平键 | 与 9.2 同源，脚本也读到 None | 改读嵌套键（并同时兼容旧驼峰，便于回看历史日志） |

### 9.4 逐项结果（v0.3.6，真机）

| 场景 | 结论 | 关键字段 |
|---|---|---|
| S1 空 SNI 入口不再假失败 | **PASS** | v27 发布 `succeeded`；`verify.listeners` **只有 8443**；`1 个监听全部就绪`；第 27 版清单不含 `sni_allow_9443.lst`；正文不引用它；**9443 REFUSED** |
| S2 缺陷二（陈旧辅助文件） | **PASS** | v28（挂业务）→ current/ 出现 `sni_allow_9443.lst`；`del_biz_http=200`、`routes_referencing_9443=0`、`preview validation_ok=True`；v29 发布后 `CHECK_9443_REMOVED=PASS`、`CHECK_FILESET_EQUAL=PASS`、`BYTE_IDENTICAL=PASS`（逐文件 sha256 全 SAME） |
| S3 数据面自愈 | **PASS** | `phase=reload`、`outcome=not_needed`、`rolled_back=False`、`rollback_verified=True`、`rollback_verify_ran=True`、`no_rollback_needed=True`、note **不含**"并验证通过"、`manual_fix` 0 行、`CHECK_VERSION_NOT_ADVANCED=PASS` |
| **S4 真回滚（B2 主路径）** | **PASS** | `phase=verify`、`outcome=verified`、**`rolled_back=True`**、**`rollback_verified=True`**、`rollback_verify_ran=True`、`rollback_restored=True`、`rollback_code` 空、`manual_fix` 0 行、`CHECK_VERSION_RESTORED=PASS` |
| S5 回滚目标统计套接字不可读 | **PASS** | `outcome=verify_failed`、`code=rollback_verification_failed`、`verify_ran=True`、`verified=False`、`rolled_back=False`、`restored=True`、判据里"统计套接字 **不成立**""监听归属 0/1 **不成立**"、`manual_fix` 14 行 |
| S6 回滚目标缺状态快照 | **PASS** | `outcome=restored_not_verified`、`code=rollback_verification_not_run`、**`verify_ran=False`**、`verified=False`、`rolled_back=False`、**`restored=True`**、`manual_fix` 14 行 |
| S7 回滚目标目录不完整 | **PASS** | `outcome=failed`、`code=rollback_restore_failed`、note 含「**警告：current/ 目前停留在第 34 版，不是第 29 版 —— 在恢复之前请不要执行 systemctl reload / restart，否则会加载第 34 版这份未通过验证的配置**」、`current/VERSION=34`（真机上确实出现了"停在未获批准版本"这个状态，告警如实说出来了） |
| S8 向不存在的 nodeID 发布 | **PASS** | HTTP 404 `not_found`「节点不存在」——请求被拒（不是 `checkLocalNode` 那条 409，而是更前面的节点查询 404；两者都算拒绝，已按现实分支校正期望） |
| 清理与恢复 | **PASS** | `FILESET_SAME=PASS` / `BYTE_IDENTICAL=PASS` / `VERSION_SAME=PASS`（回到 v18）；`ExecReload` 复原为 `/bin/kill -USR2 $MAINPID`；drop-in / 注入器 / mode 文件全部删除；`haproxy -c` valid；8443 `CONNECTION ESTABLISHED`；9443 `REFUSED`；页面 200（39187 字节，产品名 4 次）；`applied/expected/skew = 18/18/False`；`business_count=1`；SNI 入口只剩 8443 |

### 9.5 报告必须写出的七项 —— 第四轮（v0.3.6，S4）实测值

| 项 | 实测值 |
|---|---|
| nodeID 实际值 | `node_06GCGE2NPCC624J5AMKYQWC4C0` |
| Verify 是否实际执行 | **`rollback_verify_ran = true`**。另有两条行为层反证：S5 把 `stats-v29.sock` chmod 000 后结论从"验证通过"翻成 `verify_failed`；S6 把状态快照移走后 `verify_ran` 变成 `false` 且结论变成 `restored_not_verified` —— 只有 Verify 真在执行、且真的依赖这两个输入，才可能出现这种随输入变化的结果 |
| `rollback_verified` | **`true`** |
| `current/VERSION` | **`29`**（失败的那次是 v30，**没有被留在 current/ 上**；`CHECK_VERSION_RESTORED=PASS`） |
| `current/` 文件清单 | 6 个：`haproxy.cfg`、`manifest.json`、`meta.json`、`sni_allow_8443.lst`、`state.json`、`VERSION`；其中"版本清单里的 5 个文件"与第 29 版目录**逐字节一致**（判据原文） |
| 旧 `sni_allow_9443.lst` 是否已删除 | **已删除**（S2：`CHECK_9443_REMOVED=PASS`，且 `current/` 的"不在清单内"残留数为 0） |
| 8443 业务是否仍正常 | `CONNECTION ESTABLISHED`；HAProxy master PID `3426717` **全程未变** |

`rollback_note` 原文（第三轮 v0.3.5 与第四轮 v0.3.6 走的是同一段代码，文本一致、只有版本号不同）：

> 已自动回滚到第 29 版并验证通过（文件集合、版本标记、统计套接字、监听归属四项判据全部成立）

### 9.6 留给你决定的一件事：版本目录里"多出来的文件"要不要阻断发布

真机证据（第三轮 S6）：我在版本目录里把 `state.json` 改名成 `state.json.b2moved`，
平台报「版本目录 … 里有 1 个文件不在清单 manifest.json 内（state.json.b2moved）—— 目录与清单不一致，拒绝发布」。

- **必须拒绝的情况**：清单列出的文件**有一个不存在** ⇒ 版本不完整 ⇒ 拿它去回滚只会更糟。
  这一条不能松。（第三轮 S7 就是这个：`state.json` 被移走而清单仍列着它。）
- **可以商量的情况**：目录里**多出**一个文件（例如运维手工 `cp haproxy.cfg haproxy.cfg.orig`）。
  它不会被写进 `current/`（`writeVersionFiles` 只写清单里的文件），所以它不影响正确性；
  但它现在会**把回滚挡下来** —— 而回滚恰好发生在最不能拖延的时候。

我的建议：**多出来的文件不阻断，但在回滚判据里明确记一条**
（"版本目录里有 N 个文件不在清单内，已忽略，不会进入 current/"），
这样既留住可见性，又不在事故中制造新的拦路石。
**这一步我没有擅自改** —— 它改变的是 fail-closed 的边界，属于你的判断。

---

## 10. 清单外文件的处理边界（v0.3.7）：评审确认后的实现与真机验收

第 9.6 节留给评审决定的那条边界，现在定了。四条要求逐条落地，并在真机上逐条取证。

### 10.1 边界（评审结论）

| 情形 | 处理 | 为什么 |
|---|---|---|
| 版本目录里有**清单之外**的普通文件 | **不阻断**发布/回滚；warning + 审计 + 页面提示 | 它不会被写进 `current/`（写入以清单为准），对正确性无影响；而当硬错误会**在事故中挡住回滚** —— 回滚是最不能拖的时候。常见来源是运维手工备份（`cp haproxy.cfg haproxy.cfg.orig`） |
| **清单列出的**文件缺失 | **拒绝**，且不得进入 verified | 版本目录不完整 ⇒ 拿它发布/回滚只会更糟 |
| 配置**实际引用**了清单外文件 | **拒绝** | 渲染产物引用的是**版本目录的绝对路径**（`{CONFIGDIR}` 在写盘时被替换），所以那个文件在运行时真的会被 HAProxy 读到 —— 未纳入清单的配置会影响运行，清单也就不再是"这一版由哪些文件组成"的权威说明 |
| `current/` 的文件集合 | 只允许清单里的文件（+ `manifest.json`、`VERSION`） | 多余文件绝不进入 `current/` |

### 10.2 实现落点

| 要求 | 位置 |
|---|---|
| 生成完整文件清单 | `internal/haproxy/manifest.go`（`VersionManifest`，v0.3.4 起）；`publish.writeVersionDir` 一边写一边记 |
| 多余文件不阻断、并上报 | `dataplane.versionFileSet` 第三个返回值 `extra`；`publish.Pipeline.warnFilesOutsideManifest` 采集 → `Result.Warnings` + 审计（`config.version_dir_extra_files`，result=`warn`） |
| 清单缺文件仍拒绝 | `dataplane.versionFileSet` 在 `!onDisk[n]` 时直接返回错误（**这一条没松**） |
| 引用清单外文件必须拒绝 | `internal/haproxy/manifest_scan.go`：`ScanCitedFiles` 只认"以本版本目录绝对路径开头"的引用（误报压到最低），`dataplane.publishFiles` 在**改动任何文件之前**先查它 |
| 回滚路径同样处理 | 自动回滚：`inspectRollbackTarget` 用 `CompareCurrentToVersion` 带回的 `VersionDirExtra` 写进 `rollback_checks` 与 `Result.Warnings`；显式回滚：`warnFilesOutsideManifest(target)` |
| 页面显示"存在未纳入清单的额外文件" | `internal/ui/index.html` 的 `warningsBlock()`，成功页与失败页**共用同一个渲染器**（同一件事不许在两处说不同的话） |

### 10.3 新增/改写的测试（本地全绿）

- `internal/haproxy/manifest_scan_test.go`：识别渲染器形状的引用；同文件多处引用只报一次；**不该认的别认**（注释里的裸文件名、别的目录、运行时套接字）；二进制跳过；`ScanVersionDir` 把"多余"与"被引用的多余"分开报。
- `internal/dataplane/manifest_fileset_test.go`：
  `TestPublishReportsExtraFilesWithoutBlocking`（多余文件 → **发布成功** + `extra` 上报 + `current/` 不含它）、
  `TestPublishRefusesWhenConfigCitesFileOutsideManifest`（引用清单外文件 → 拒绝，且**改动前**就拒绝）、
  `TestPublishRefusesWhenManifestListsMissingFile`（清单缺文件 → 拒绝，`current/` 一个字节不动）、
  `TestPublishFilesKeepsCurrentEqualToTargetManifest`（`current/` 只含清单允许的文件）。
  `assertCurrentMatches` 的期望集合改为**按清单**推导（原先按目录列举，多余文件会被误判成"缺失"）。
- `internal/publish/extra_files_test.go`（新增）：
  `TestPublishWarnsAboutFilesOutsideManifest`（发布成功 + warning + 审计）、
  `TestExplicitRollbackWarnsAboutFilesOutsideManifest`（回滚成功 + warning + 审计）、
  `TestAutoRollbackReportsVersionDirExtraFiles`（自动回滚路径同样上报，且**不改变回滚结论**）、
  `TestRollbackRefusesWhenTargetConfigCitesFileOutsideManifest`（用**真实** `dataplane.HAProxy` 测：引用清单外文件 → 回滚被拒）。
- `internal/ui/embed_test.go`：`TestHandlerShowsWarningsOutsideManifest` 钉住 `warningsBlock`、文案、以及成功/失败两条路径都读服务端字段。

### 10.4 真机验收（154.9.235.57:23722，v0.3.7）

`sha256 dc878c52889a51ab114d02ac51a765395900ed322f146831b26664962594da4d`，10817688 字节；
`go tool nm | grep -c testplane` = 0；部署只换管理面二进制，**HAProxy master PID `3426717` 全程未变**。

| 场景 | 结论 | 关键证据 |
|---|---|---|
| **S1** 版本目录有清单外多余文件 | **PASS** | 预置 `v36/haproxy.cfg.orig`（属主 shengyu）→ 发布 `status=applied_origin_unhealthy`、`version=36`、**成功**；`warnings` = 「第 36 版目录里有 1 个未纳入清单的额外文件（haproxy.cfg.orig）—— 它们不会被发布到 current/，也不影响转发；但说明该目录在清单生成后被改动过，建议核对后清理」；`manual_fix` 为空。<br>（`applied_origin_unhealthy` 的语义是"**配置本身已生效**，只是源站探测没通"—— 本用例把 `origin_port` 改成 444，`1.1.1.1:444` 本来就不通，与清单外文件无关） |
| **S1b** warning 写进审计 | **PASS** | `select count(*) from audit where action='config.version_dir_extra_files'` = **2**，`result=warn`，summary 里点名 `haproxy.cfg.orig` |
| **S1c** 页面带提醒文案 | **PASS** | `GET /` 的返回内容里含「存在未纳入清单的额外文件」（真机取页面，不只信单元测试的字面量） |
| **S4** `current/` 只含清单允许的文件 | **PASS** | `manifest.files = [haproxy.cfg, meta.json, sni_allow_8443.lst, state.json]`；`current/ = [VERSION] + 上面 4 个 + manifest.json`；多余残留 0、缺文件 0；`haproxy.cfg.orig` **不在** `current/` 里 |
| **S2a** 被配置引用的允许清单缺失 | **PASS**（被拒） | 移走 `v36/sni_allow_8443.lst` → 回滚 **409**，被 **HAProxy 自己的语法检查**拦下：`failed to open pattern file </…/v36/sni_allow_8443.lst>`。这是"两道防线"里的第一道 |
| **S2b** 不被配置引用的清单内文件缺失 | **PASS**（被拒） | 移走 `v36/state.json`（HAProxy 看不见它，只有平台的清单判据能拦）→ 回滚 **409**：「版本清单 manifest.json 列出了 "state.json"，但版本目录 …/v36 里没有这个文件 —— 版本目录不完整，拒绝发布」。**这条才是真正在验清单完整性** |
| **S3** 配置引用了清单外文件 | **PASS**（被拒） | 给 `v18/haproxy.cfg` 注入一条指向 `v18/extra_allow.lst` 的 `tcp-request … -f`（该文件没进清单）；**先单独确认注入后 `haproxy -c` = VALID**（避免假阳性：若本身语法不合法，就分不清拒绝来自哪一层）→ 回滚 **409**：「版本目录 …/v18 的配置引用了 1 个不在清单 manifest.json 内的文件（extra_allow.lst（被 haproxy.cfg 引用））—— 这类文件不会被发布到 current/，但运行时配置按绝对路径读的是版本目录，因此会真实生效，拒绝发布」 |
| 清理与恢复 | **PASS** | `current/VERSION=18`；`current/`（除 VERSION）与第 18 版清单**逐项相同**、5 个文件 sha256 全 `SAME`（`FINAL_FILESET=PASS`）；被改坏的 `v18/haproxy.cfg` 复位后 sha256 与备份一致（`CFG_RESTORED=PASS`，`7ddbe0f5…`）；验收造的文件与临时移动文件残留均为 **0**；收尾回滚 `rollback_verified=True / outcome=verified` |
| 业务不受影响 | **PASS** | 8443 `Verify return code: 0 (ok)`；`haproxy -c` = valid；`applied/expected/skew = 18/18/False`；`business_count=1` |

三个 FAIL 之后重跑的原因，全部是**验收脚本自己的问题**（记下来免得再犯）：

| 现象 | 根因 | 修法 |
|---|---|---|
| S1 `phase=write`、`写入版本目录失败: … permission denied` | 脚本用 root 执行 `mkdir -p` 预置版本目录 ⇒ 目录属主 root，而平台以 `shengyu` 运行 ⇒ 写不进去 | 改用 `install -d -o shengyu -g shengyu -m 0750` |
| S2「拒绝」了但原因不是清单判据 | 移走的是**被 cfg 引用**的允许清单 ⇒ HAProxy 语法检查先拦（走不到平台的判据）。这个"拒绝"是真的，但**没验到我要验的那条** | 拆成 S2a（两道防线任一即可）+ **S2b（移走不被引用的 `state.json`，只能由平台判据拦）** |
| 终检 `No such file .../v18/VERSION` | 脚本假设版本目录里有 `VERSION` —— 它只存在于 `current/` | 比对集合改为 `manifest.files ∪ {manifest.json}`，`VERSION` 单独读 `current/` |
| S1 断言 `status == succeeded` 失败 | 改了 `origin_port=444`，源站探测不通 ⇒ `applied_origin_unhealthy`（= 配置已生效，**不是**发布失败） | 断言接受两种状态并注明语义 |

### 10.5 排进后续（评审确认）

**① 增加 `sni_entries` DELETE 接口**
- 有业务路由引用时**拒绝**删除；无引用时允许删除；
- 删除后提示/触发重新发布（否则配置与期望状态不一致）；
- 写审计。
- 背景：第 8 节那次事故的直接推手就是这个缺口 —— 没有删除接口，验收只能改库，而改库绕过了所有一致性检查。附带建议：在 `/api/diagnose` 加一条"悬空引用"自检（现在这个问题只有等"发布被 422"才暴露）。

**② 清理旧的 `stats-v*.sock`**
- 只清理**已确认无进程使用**的 socket；
- 当前 ALIVE 的、以及正在优雅退出的旧 worker 的 socket **不得删除**；
- 清理失败只告警，**不影响已成功发布的配置**。
- 背景：真机上已累积 20+ 个（`stats-v0/v1/v2/v3/v11/v12/v15/…/v26`），只有当前在跑的那个 ALIVE，其余惰性。它们不会造成假验证通过（`Verify` 还要同时满足版本标记与监听归属），但会一直堆积。

### 10.6 未验证项（如实记录）

1. **真人浏览器走查**：界面结论来自 `GET /` 的字节内容（真机 S1c + 单元测试契约），没有在真实浏览器里点过一遍。
2. **`publishFiles` 的"引用检查"只在"以文件为准"的数据面上生效**：纯 Go 数据面（`internal/testplane`）没有版本目录，这条判据标为"不适用"而不是"通过"。
3. **只识别绝对路径引用**：`ScanCitedFiles` 只认"以本版本目录绝对路径开头"的引用（这是渲染产物的实际形状）。若将来渲染器改用相对路径，这条检查会静默失效 —— 需要同步改，已在函数注释里写明。

---

## 11. 两项排期任务 + 顺带修的真缺陷（v0.4.0 正式版）

第 10.5 节排的两项，加上过程中挖出的一个数据完整性缺陷，一起做完并在真机上取证。
**v0.4.0 是正式发布包**：不再按补丁号往上堆，服务器上也不再累积临时测试版本。

### 11.1 `sni_entries` 删除接口

| 要求 | 实现 |
|---|---|
| 有业务路由引用时拒绝删除 | `store.DeleteSNIEntry` 在**一个事务里**先数引用再删（`routes.sni_entry_id` 没有外键，所以守卫只能由代码提供）；返回 `ErrSNIEntryInUse` ⇒ 接口 **409 `sni_entry_in_use`**，报错里点名引用的业务 |
| 无引用时允许删除 | 同上事务的另一条分支；删除后返回 `next_step`：「已从期望状态中移除，但节点上仍在按上一版配置运行 —— 请执行发布」 |
| 删除后提示重新发布 | 见上；删入口只改期望状态，不发布就会让"界面上的入口列表"与"节点上跑的东西"不一致 |
| 写审计 | `sni_entry.delete`，摘要含 `地址:端口` |
| 诊断页增加悬空引用检查 | `store.FindDanglingReferences`（覆盖 routes 的三个外键 + `sni_entries.node_id` + `businesses.primary_node_id`）⇒ `/api/diagnose` 输出 `dangling_reference`（severity=error） |

**为什么守卫放在 store 而不是 handler**：handler 是可以被绕过的（批量脚本、未来的其它调用方），
而"不允许留下悬空引用"是数据完整性约束。

**真机证据**（154.9.235.57:23722，v0.4.0）：

```
--- S1 建 9443 入口 + S2 建业务挂上去（全程不发布 ⇒ 端口从未被真正绑定）---
--- S3 删入口（有引用）---
delete_http=409                                   S3_HTTP=PASS
  code  = sni_entry_in_use
  error = store: 共享入口仍被业务路由引用: 仍被 1 条业务路由引用（v040-接口验收业务）
          —— 请先删除这些业务，或把它们改挂到别的入口，再删除本入口
  entries = ['sni_…ANH0', 'sni_…QY4']             S3B=PASS（拒绝时入口原样保留）
--- S4 删业务（路由级联删除）→ 仍引用该入口的路由数 = 0 ---
--- S5 再删入口（无引用）---
delete_http=200
  next_step = 入口 0.0.0.0:9443 已从期望状态中移除，但节点上仍在按上一版配置运行 —— 请执行「发布」使其生效
  entries now = ['sni_…ANH0']                     S5B=PASS
  审计条数 = 1（基线 0），最近一条 = 删除共享 SNI 入口 0.0.0.0:9443    S5C=PASS
--- S6a 数据干净时：findings 只有 log_parse_failed，**没有** dangling_reference ---  S6A=PASS
--- S6b 改库删掉那个入口（模拟绕过守卫的写操作）---
  findings = ['log_parse_failed', 'dangling_reference']                    S6B=PASS
  severity = error
  facts    = ['rt_…MMB0 引用了不存在的SNI 共享入口（sni_…KV0）']
  next     = ['…该路由在渲染时会被**静默丢掉**，表现为「发布成功但业务不转发」…',
              '修完数据后**务必发布一次**…',
              '优先用平台接口删除或改挂，不要再直接改库…（SNI 入口已支持删除接口）']
--- S6c 删掉业务（级联删路由）⇒ findings 回到只有 log_parse_failed ---  S6C=PASS
```

> 已知措辞瑕疵：上面 `facts` 里 `不存在的SNI 共享入口` 少了一个空格
>（`"...不存在的%s"` 与 `"SNI 共享入口"` 直接相接）。语义无歧义，**本次不重打包**，
> 下次随其它改动一起修 —— 避免为一句排版再堆一个版本号。

### 11.2 旧 `stats-v*.sock` 清理

| 要求 | 实现（`dataplane.PruneStaleStatsSockets`） |
|---|---|
| 只清理确认没有进程使用的 socket | 逐个人工判据，按保守程度排序、前一条否决后一条：① 只处理 `stats-v<N>.sock` 形状；② **当前生效版本**的套接字一律保留（reload 窗口里它可能暂时连不上，删它没有收益只有风险）；③ 能连上的一律保留；④ **判不出来**的保留；⑤ 以上都不成立才删 |
| ALIVE 与优雅退出中的 worker 不删除 | 判据③就是这条：优雅退出中的旧 worker 仍会在它的套接字上应答 ⇒ 被保留。<br>只把"连接被拒（ECONNREFUSED）或文件已不存在"当作"确认无人使用" |
| 清理失败只告警，不影响发布结果 | 返回结构里分 `Removed / Kept / Problems` 三类；`Problems` 只进 `Result.Warnings` 与审计，**绝不改变 `Status`**；数据面不实现该能力时静默跳过（"不适用"≠"失败"） |

**真机证据**（S7，发布成功后自动执行）：

```
  prune = scanned 21  removed 20  kept 1
  removed = [stats-v0, v1, v11, v12, v15…v21, v26…v29, v3, v34, v35, v36]
  kept    = 1 个（当前生效版本 v18）
  socket 数量：20 → 1                                  S7=PASS / S7C=PASS
  审计(config.prune_stats_sockets) = 1                  S7B=PASS
```

> 一个细节：`scanned` 比基线多 1，是因为**本次发布刚创建了当前版本**的套接字
>（判据②把它保留了）。这正是"当前版本不删"这条规则在起作用的直接证据。

### 11.3 顺带修的真缺陷：日志分片并发首次写入丢数据

**怎么发现的**：给 `-race` 全量测试时，`e2e.TestEndToEndMainFlow` 变得不稳定
（最初 1/5 通过），失败形态是"等待日志超时"，且无筛选查询只有**一条**会话日志
（两个并发会话丢了一半）。

**排除自身改动**：把本次全部运行时影响短路（`pruneStatsSockets` 调用、diagnose 的悬空引用查询）后，
同一命令仍 4/5 失败、形态完全相同 ⇒ 与本次改动无关，是既有缺陷。

**根因（两处叠加）**：

1. **并发 DDL**：首次写入一个新分片时，多个 goroutine 同时进 `openWriteShard`
   （`acquireWriter` 的双重检查拦不住"都还没建表"这个窗口），每人各执行一遍
   `CREATE TABLE / CREATE INDEX` ⇒ SQLite 抢写锁，抢不到的报错，调用方把**整批**日志判失败丢掉。
2. **`journal_mode(WAL)` 放在 DSN 里**：DSN 里的 pragma 会被**每个新物理连接**执行一次，
   而切换 journal_mode 是**写操作、要独占锁** ⇒ 连接池并发（写入侧 `SetMaxOpenConns(8)`）
   时互相抢锁，抢不到的那次报 `attempt to write a readonly database`
   —— 这条报错的字面意思（"只读库"）与真实原因（抢锁失败）完全无关，极易把排查带偏。

**修法**：

- `internal/db`：把 `journal_mode(WAL)` 从 DSN 移出，改为 `ensureWAL` 在打开后**每库只设一次**
  （包级锁 + `sync.Map` 记住已设过的路径）—— journal_mode 本就是持久化在文件头、设一次永久有效的属性。
- `internal/logstore`：`openWriteShard` 全程持 `openMu` 串行化"打开 + 建表"，并二次检查 `schemaReady`。

**效果**：新增 `internal/logstore/ingest_integrity_test.go`（顺序写不丢 / 并发写不丢 / 同键去重），
修复前并发写 24 行只落 16 行、修复后全绿；e2e 在 `-race` 下从 **1/5 → 9/10** 通过。

### 11.4 顺带修的真缺陷：日志被丢弃这件事在平台上完全不可见

`agent.IngestStats()` 与 `agent.Batcher.Stats()` **定义之后没有任何调用方** ——
也就是说日志在"解析入库"或"攒批"阶段被丢弃时，只累加一个计数器，
界面上"查不到日志"会被解释成"这段时间确实没有连接"。

**修法**：`Server` 挂上 `Ingest/Batch`，`/api/overview` 输出 `log_ingest`
（ingested / duplicated / parse_errors / dropped / buffer_drops / pending / flushes / last_error），
并在有任何丢弃时附一句 warning，明确写出"「查不到日志」很可能正是它们造成的，而不是「这段时间没有连接」"。

**真机证据**（S9）：

```
log_ingest = {'boot_id': 'b45d1c75b4aad28e', 'ingested': 10, 'duplicated': 0,
              'parse_errors': 9, 'dropped': 0, 'buffer_drops': 0,
              'pending': 5, 'flushes': 3,
              'last_error': '该版本/该配置下产出的不是 JSON 日志行'}      S9=PASS
```

### 11.5 正式发布包 v0.4.0

```
./scripts/build-release.sh v0.4.0
  dist/shengyu-edgelink-linux-amd64        sha256 ef5c64517c7219b55db8025712cf55b793e2b0e624381f0498e1096089d8b406  10858648 字节
  dist/shengyu-edgelink-linux-arm64        sha256 1773cd54523d11903494360c230433e34bedcf1cf3491989baaab54ff4b1c094
  dist/shengyu-edgelink-linux-amd64-v0.4.0.tar.gz  sha256 c35152dcd5acb65d612173faea908a7f94e6852c4d14303b2d6601cdf3f4f04d
  dist/shengyu-edgelink-linux-arm64-v0.4.0.tar.gz  sha256 e68a392c558620b18e721307b0142737edeb9873aa13a03e88e3ee567bf45676
  dist/SHA256SUMS-v0.4.0（与实测一致）
```

**版本号为什么是 0.4.0 而不是 1.0.0**：`docs/05-phased-plan.md` 里 P5（双节点高可用与阿里云 DNS 调度）
与 P6（验收、加固与交付）尚未完成，`0.x` 更贴合当前成熟度；
它的意义在于"这是一个**里程碑版本**"——之后的改动不再按补丁号往上堆。

真机部署与验收：`sha256` 与本地构建产物一致；只替换管理面二进制并重启管理面，
**HAProxy master PID `3426717` 全程未变**（只重启了管理面进程，转发进程一次都没动过）。

### 11.6 本轮真机验收汇总（全项 PASS）

| 项 | 结果 |
|---|---|
| S3 有引用删入口 | **PASS** 409 `sni_entry_in_use`，报错点名业务，入口原样保留 |
| S5 无引用删入口 | **PASS** 200 + `next_step` 要求发布 + 审计 1 条 |
| S6a/b/c 悬空引用自检 | **PASS** 干净时不误报 → 造出后报 `dangling_reference`（error）→ 修好后消失 |
| S7/7b/7c socket 清理 | **PASS** 扫描 21、清 20、留 1（当前版本）；审计 1 条；当前版本套接字仍在 |
| S8 恢复性发布 | **PASS** |
| S9 摄入健康可见 | **PASS** `log_ingest` 齐备，`last_error` 具体 |
| 收尾 | `current/VERSION=18`（回基线）；入口只剩 8443；业务只剩"验收业务"；8443 `Verify return code: 0`；`haproxy -c` valid；`applied/expected/skew = 18/18/False`；**悬空引用残留 0、验收造的业务残留 0** |

### 11.7 本轮未完成 / 待办（如实记录）

1. **e2e 在 `-race` 下仍有约 1/10 的偶发失败**（"等待日志超时"）。已从 1/5 改善到 9/10，
   残余未定位到具体丢弃路径。**已通过 11.4 让这类情况在真机上可见**（`log_ingest.dropped`），
   下次再出现时可直接看计数判断是"丢了"还是"没产生"。
2. **`Ingestor` 写入失败没有重试**：`LogSink` 是 `func(string)`（无返回值），
   数据面无法感知写入失败；`agent.Ingestor` 记 dropped 但不重试。
   考虑到日志是"过期即失效"的数据，加一次短退避重试是合理的下一步（幂等键已具备，重试不会产生重复行）。
3. **界面还没显示 `log_ingest`**：当前界面只有向导与发布详情两块，没有总览页。
   数据已通过 `/api/overview` 暴露，界面展示留待总览页实现时一并做。
4. **`facts` 文案缺空格**（见 11.1 末尾）。
