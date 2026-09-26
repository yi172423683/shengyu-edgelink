# 盛愈边缘网关（shengyu-edgelink）任务交接文档

更新时间：2026-09-26

## 1. 项目目标

这是一个基于 Go + HAProxy 的 TCP/TLS SNI 中转管理平台，中文品牌为“盛愈边缘网关”。

主要用途：

- 客户端通过自定义域名和入口端口访问；
- 平台按 TLS SNI 将流量转发到不同源站；
- 管理后台创建入口、业务、源站和健康检查配置；
- 发布时生成 HAProxy 配置，语法检查、平滑 reload、验证、失败自动回滚；
- 记录连接日志、审计日志和发布结果；
- 当前是单节点版本，双节点 HA 和阿里云 DNS 调度尚未完成。

## 2. 本地项目位置

```text
C:\Users\Administrator\Desktop\加速平台
```

主要目录：

```text
cmd/shengyu-edgelink/       Go 主程序
internal/api/               管理 API
internal/auth/              PBKDF2 口令算法
internal/store/             SQLite 数据存储和用户存储
internal/ui/                go:embed 内置管理页面
internal/haproxy/           HAProxy 配置渲染
internal/dataplane/         HAProxy 发布、验证、回滚
internal/publish/           发布流水线
internal/logstore/          日志分片和查询
deploy/install.sh           安装和升级脚本
deploy/shengyu-edgelink-menu.sh 服务器菜单命令
scripts/build-release.sh    Linux 构建脚本
scripts/build-release.ps1  Windows 构建脚本
```

## 3. 当前生产服务器

```text
公网 IP：154.9.235.57
SSH 端口：23722
系统：Debian 12
源码目录：/root/shengyu-source
发布包目录：/server
```

当前主要服务：

```text
shengyu-edgelink-server.service    管理 API 和网页
shengyu-edgelink-haproxy.service   HAProxy 数据面
```

当前监听情况曾验证为：

```text
127.0.0.1:8081    管理面
0.0.0.0:8443      HAProxy 转发入口
:::443            xray，占用 443
0.0.0.0:23722     SSH
```

管理面此前通过 Nginx + HTTPS 域名对外暴露，配置和证书由 `deploy/panel-https.sh` 管理。管理面和数据面端口不要混淆：8081 是后台，8443 是业务转发入口。

## 4. 最近遇到的密码问题

### 根因

安装脚本中的 `-admin-pass` 只在数据库没有用户时生效。只要下面这个文件已经存在：

```text
/var/lib/shengyu-edgelink/meta.db
```

重复安装或升级时输入的新密码不会覆盖旧密码，脚本会沿用数据库中的原密码。

另外，升级脚本为了避免中断转发，检测到服务运行时默认不会自动重启管理服务。因此替换二进制后必须手动执行：

```bash
systemctl restart shengyu-edgelink-server
```

这不会重启 HAProxy，也不会中断现有转发连接。

### 当前恢复方式

如果服务器已经安装菜单：

```bash
edgelink
```

选择 `8. 重置管理员密码`。

如果命令不存在，但服务器上有菜单脚本：

```bash
install -m 0755 /server/shengyu-edgelink-menu.sh /usr/local/bin/edgelink
edgelink
```

不要删除 `meta.db`，否则会丢失业务、入口、版本和审计数据。

## 5. 本轮已经修改的代码

已经在源码中增加当前登录用户修改密码功能：

- `POST /api/me/password`；
- 修改前验证当前密码；
- 新密码至少 12 个字符；
- 修改成功后吊销该用户全部旧会话；
- 网页新增“账号安全 / 修改口令”区域；
- 登录页提示使用 `edgelink` 菜单重置密码。

涉及文件：

```text
internal/api/api.go
internal/ui/index.html
```

这部分修改需要重新构建并部署新版二进制后才会生效。当前代码仍然只有首次初始化的 `admin` 账号，尚未完成网页上的多用户创建、角色、禁用和删除功能。

## 6. 本地打包和上传方式

Windows 没有 Bash 时，不要执行 `scripts/build-release.sh`。可以只打源码压缩包，然后在 Linux VPS 上构建。

PowerShell 创建源码包时不要包含 `dist`：

```powershell
$root = 'C:\Users\Administrator\Desktop\加速平台'
$zip = 'C:\Users\Administrator\Desktop\shengyu-edgelink-source-new.zip'
Remove-Item $zip -Force -ErrorAction SilentlyContinue
Compress-Archive -Path @(
  "$root\cmd", "$root\deploy", "$root\docs", "$root\e2e",
  "$root\internal", "$root\scripts", "$root\tools", "$root\web",
  "$root\go.mod", "$root\go.sum", "$root\install.sh", "$root\README.md"
) -DestinationPath $zip -CompressionLevel Optimal
```

VPS 上接收：

```bash
mkdir -p /server
cd /server
rz
```

在 ZMODEM 窗口选择刚才的 zip。解压：

```bash
rm -rf /root/shengyu-source-new
mkdir -p /root/shengyu-source-new
unzip -q /server/shengyu-edgelink-source-new.zip -d /root/shengyu-source-new
cd /root/shengyu-source-new
```

安装 Go 并构建：

```bash
export PATH=/usr/local/go/bin:$PATH
export GOPROXY=https://goproxy.cn,direct
export GOMAXPROCS=1
bash scripts/build-release.sh v0.4.4
```

注意：完整 `go test ./...` 可能因 e2e 使用随机端口而撞上系统临时端口。若只是构建发布包，可先备份脚本，临时把测试范围改成：

```bash
cp scripts/build-release.sh /tmp/build-release-full.sh
sed -i 's/go test \.\/\.\.\./go test .\/cmd\/... .\/internal\/.../' scripts/build-release.sh
bash scripts/build-release.sh v0.4.4
mv /tmp/build-release-full.sh scripts/build-release.sh
```

构建后检查：

```bash
ls -lh dist/
sha256sum dist/*
```

## 7. 部署新版

解压 amd64 发布包：

```bash
rm -rf /root/shengyu-release-v0.4.4
mkdir -p /root/shengyu-release-v0.4.4
tar -xzf /root/shengyu-source-new/dist/shengyu-edgelink-linux-amd64-v0.4.4.tar.gz \
  -C /root/shengyu-release-v0.4.4 --strip-components=1
cd /root/shengyu-release-v0.4.4
bash -n install.sh
bash install.sh --dry-run --listen 127.0.0.1:8081
bash install.sh --listen 127.0.0.1:8081
systemctl restart shengyu-edgelink-server
```

确认运行的是新版：

```bash
/usr/local/bin/shengyu-edgelink -version
systemctl status shengyu-edgelink-server --no-pager
curl -s http://127.0.0.1:8081/api/health
```

不要执行 `systemctl restart shengyu-edgelink-haproxy`，配置发布应该从网页点“发布”，由平台执行平滑 reload。

## 8. 下一模型必须完成的事项

按优先级：

1. 在 Linux 上重新构建，确认 `internal/api` 和 `internal/ui` 的改密改动编译通过；
2. 部署后验证网页显示“账号安全 / 修改口令”；
3. 使用旧密码修改为新密码，确认旧密码失败、新密码成功；
4. 验证修改密码后旧会话被吊销；
5. 增加账号管理页面：创建用户、角色、启用/禁用、删除，并限制只有管理员可操作；
6. 增加管理员重置其他用户密码接口；
7. 修复或隔离 e2e 随机临时端口冲突，恢复完整 `go test ./...`；
8. 完成双节点 HA、主备线路切换和阿里云 DNS 调度；
9. 补充发布包版本、SHA256SUMS 和升级回滚验收记录。

## 9. 重要安全注意事项

- 不要把管理员密码、DNS API Token、Basic Auth 密码发到聊天记录；
- 不要删除 `/var/lib/shengyu-edgelink/meta.db` 来“解决密码问题”；
- 不要把管理面和 HAProxy 数据面端口混用；
- 不要直接重启 HAProxy 代替网页发布，否则可能中断现有连接；
- 外网管理面应使用 HTTPS，且云安全组只开放必要端口；
- 443 已被 xray 占用，数据面可使用 8443 或其他未占用端口。
