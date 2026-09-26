#!/usr/bin/env bash
# shengyu 节点安装脚本（在目标节点上以 root 执行）。
#
# 设计原则：**每一步都可重复执行**，且**不覆盖任何既有服务**。
# 脚本不会：动系统自带的 haproxy 配置、pkill 任何进程、修改发行版的默认服务。
# 它只做四件事：建账号与目录、放二进制、装两个独立的 systemd 单元、写内核参数。
#
# 用法：
#   sudo bash deploy/install.sh                       # 安装（幂等，管理端口会交互式询问）
#   sudo bash deploy/install.sh --listen 127.0.0.1:9090   # 直接指定管理端口（非交互）
#   sudo bash deploy/install.sh --dry-run             # 只看会做什么，不动系统
set -euo pipefail

# ---- 参数解析：管理面监听地址 ----
#
# 三个来源，优先级从高到低：命令行 --listen > 环境变量 SHENGYU_EDGELINK_LISTEN > 交互式询问 > 默认值。
# 之所以要有环境变量这一档：批量装机（Ansible/脚本）没有 tty，没法交互式回答，
# 只留交互会让"无人值守部署"直接卡住。
DEFAULT_LISTEN="127.0.0.1:8081"
DRY=0
LISTEN="${SHENGYU_EDGELINK_LISTEN:-}"

# ---- 参数解析：管理面「外网 HTTPS 访问」（可选项） ----
#
# 【默认不开】是刻意的：外网暴露是**显式的运营决定**，不能因为装了包就顺手把门打开。
# 一个默认开着的管理入口，在批量装机时意味着"一堆机器在你不知情时已经对外可达"。
# 所以只有显式给出 --panel-domain（或环境变量 / 交互式确认）才会启用。
PANEL_DOMAIN="${SHENGYU_PANEL_DOMAIN:-}"
PANEL_PORT="${SHENGYU_PANEL_PORT:-9443}"
PANEL_CERT_MODE="${SHENGYU_PANEL_CERT_MODE:-dns}"
PANEL_DNS_PROVIDER="${SHENGYU_PANEL_DNS_PROVIDER:-dnspod}"
PANEL_SERVER_NAME="${SHENGYU_PANEL_SERVER_NAME:-}"
PANEL_WILDCARD="${SHENGYU_PANEL_WILDCARD:-0}"
PANEL_BASIC_USER="${SHENGYU_PANEL_BASIC_USER:-}"
# ACME 账户邮箱：Let's Encrypt 用它发"证书即将过期"的通知。允许为空，
# 但建议填写 —— 否则证书出问题时唯一的提前通知渠道也没有了。
ACME_EMAIL="${ACME_EMAIL:-}"

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY=1 ;;
    --listen)
      shift || true
      LISTEN="${1:-}"
      if [ -z "$LISTEN" ]; then
        echo "错误：--listen 需要一个参数，例如 --listen 127.0.0.1:8081" >&2
        exit 1
      fi
      ;;
    --listen=*) LISTEN="${1#--listen=}" ;;
    --panel-domain)
      shift || true
      PANEL_DOMAIN="${1:-}"
      ;;
    --panel-domain=*) PANEL_DOMAIN="${1#--panel-domain=}" ;;
    --panel-port)
      shift || true
      PANEL_PORT="${1:-}"
      ;;
    --panel-port=*) PANEL_PORT="${1#--panel-port=}" ;;
    --panel-cert)
      shift || true
      PANEL_CERT_MODE="${1:-}"
      ;;
    --panel-cert=*) PANEL_CERT_MODE="${1#--panel-cert=}" ;;
    --panel-dns-provider)
      shift || true
      PANEL_DNS_PROVIDER="${1:-}"
      ;;
    --panel-dns-provider=*) PANEL_DNS_PROVIDER="${1#--panel-dns-provider=}" ;;
    --panel-server-name)
      shift || true
      PANEL_SERVER_NAME="${1:-}"
      ;;
    --panel-server-name=*) PANEL_SERVER_NAME="${1#--panel-server-name=}" ;;
    --panel-wildcard) PANEL_WILDCARD=1 ;;
    --panel-basic-user)
      shift || true
      PANEL_BASIC_USER="${1:-}"
      ;;
    --panel-basic-user=*) PANEL_BASIC_USER="${1#--panel-basic-user=}" ;;
    --panel-acme-email)
      shift || true
      ACME_EMAIL="${1:-}"
      ;;
    --panel-acme-email=*) ACME_EMAIL="${1#--panel-acme-email=}" ;;
    -h | --help)
      cat <<EOF
用法：$0 [选项]

管理面：
  --listen 地址:端口   管理面监听地址（默认 $DEFAULT_LISTEN）。
                       例：--listen 127.0.0.1:9090

管理面外网访问（可选，默认关闭）：
  --panel-domain 域名      证书主域，同时用于判定"是否开通面板"。给出本参数即启用。
                           例：--panel-domain panel.example.com
  --panel-port 端口        外网监听端口（默认 9443）。**不允许 80/443**：
                           80 要留给本机 acme 续期，443 常属既有服务。
  --panel-server-name 域名 浏览器访问的域名（server_name），默认同 --panel-domain。
                           签泛域名时用它指定具体子域。
  --panel-wildcard         同时签发 *.证书主域（通配符证书只能用 DNS 方式签）。
  --panel-cert dns|http    证书签发方式，默认 dns（DNS-01，不占任何本地端口，推荐）。
                           http 走 HTTP-01，需要 80 端口空闲且可被公网访问。
  --panel-dns-provider 名  DNS 厂商：dnspod(默认)/aliyun/cloudflare/huawei/tencent。
  --panel-acme-email 邮箱  ACME 账户邮箱（证书到期提醒发到这里）。
  --panel-basic-user 用户名 启用 HTTP Basic 二次认证，与平台口令相互独立。
                           口令从环境变量 SHENGYU_PANEL_BASIC_PASS 读，或交互式输入。

其他：
  --dry-run            只打印会做什么，不动系统

DNS 凭据一律用**环境变量**传入（放命令行会进 shell history 与 ps）：
  DNSPod     DP_Id / DP_Key
  阿里云     Ali_Key / Ali_Secret
  Cloudflare CF_Token
  华为云     HUAWEICLOUD_Username / HUAWEICLOUD_Password
  腾讯云     Tencent_SecretId / Tencent_SecretKey

已安装的机器上单独开通/回退面板（不必重跑整套安装）：
  sudo bash deploy/panel-https.sh --domain panel.example.com
  sudo bash deploy/panel-https.sh --disable
EOF
      exit 0
      ;;
    *) echo "未知参数：$1（用 --help 查看用法）" >&2; exit 1 ;;
  esac
  shift || true
done

# 没显式指定时：有 tty 就交互式问一次（安装向导），没有 tty 直接用默认值。
# 不加 [ -t 0 ] 判断的话，非交互执行会一直卡在 read 上等一个永远不会来的输入。
#
# 为什么把"地址"和"端口"拆成两问（而不是像 --listen 那样填 地址:端口）：
#   安装向导是给"不熟悉这套系统的人"用的。一个 地址:端口 的输入框，用户很容易
#   只填 8081（然后得到一个叫 8081 的"地址"），或者填 127.0.0.1：8081（全角冒号）——
#   这两种错都会在服务启动时才炸。拆两问后，端口那一问只接受数字，当场就能纠正。
if [ -z "$LISTEN" ]; then
  if [ "$DRY" = 0 ] && [ -t 0 ]; then
    DEF_ADDR="${DEFAULT_LISTEN%%:*}"
    DEF_PORT="${DEFAULT_LISTEN##*:}"
    printf '管理后台内网监听地址（默认 %s，回车即用默认值）：' "$DEF_ADDR"
    if ! read -r ANS_ADDR; then ANS_ADDR=""; fi
    printf '管理后台内网端口（默认 %s，回车即用默认值）：' "$DEF_PORT"
    if ! read -r ANS_LPORT; then ANS_LPORT=""; fi
    LISTEN="${ANS_ADDR:-$DEF_ADDR}:${ANS_LPORT:-$DEF_PORT}"
  else
    LISTEN="$DEFAULT_LISTEN"
  fi
fi

# 校验必须是 地址:端口 且端口合法。
#
# 这一步不能省：端口写错（少了端口、或写成 65536）时，sed 会把一个非法值写进 unit，
# 结果是服务起不来，而报错在 systemctl status 里，看上去像"程序坏了"。
LISTEN_PORT="${LISTEN##*:}"
case "$LISTEN_PORT" in
  '' | *[!0-9]*)
    echo "错误：管理面监听地址必须是 地址:端口 形式且端口为数字（例如 $DEFAULT_LISTEN）。" >&2
    echo "      当前输入：$LISTEN" >&2
    exit 1
    ;;
esac
if [ "$LISTEN_PORT" -lt 1 ] || [ "$LISTEN_PORT" -gt 65535 ]; then
  echo "错误：管理端口必须在 1-65535 之间。当前输入：$LISTEN" >&2
  exit 1
fi

# ---- 公网访问 + 管理员凭据：交互式向导 ----
#
# 为什么在这里问（参数解析之后、动系统之前）：安装向导应当**一次把需要人决策的事问完**，
# 而不是装到一半再停下来等输入 —— 那样一旦输错，系统已经改了一半，还得先收拾。
# 这里只**收集参数**；真正的动作在第 8 步（建管理员）与第 9b 步（配入口）——
# 那时目录已就绪、管理面已在跑，反代目标才存在。
#
# 顺序按"从内到外"排，每一问都能接上上一问的上下文：
#   内网地址 → 内网端口 → 公网域名 → 公网端口 → ACME 邮箱 → 证书验证方式 → 管理员密码
if [ -z "$PANEL_DOMAIN" ] && [ "$DRY" = 0 ] && [ -t 0 ]; then
  printf '是否开通公网 HTTPS 访问？（需要域名；会自动申请并续期证书、自动配好 Nginx 反向代理）[y/N] '
  if ! read -r PANEL_ANS; then PANEL_ANS=""; fi
  case "$PANEL_ANS" in
    y | Y | yes | YES | Yes)
      printf '  公网访问域名（例如 panel.example.com）：'
      if ! read -r PANEL_DOMAIN; then PANEL_DOMAIN=""; fi
      if [ -z "$PANEL_DOMAIN" ]; then
        echo "  未输入域名，跳过公网访问配置。" >&2
      else
        printf '  公网 HTTPS 端口（默认 %s，不能用 80/443）：' "$PANEL_PORT"
        if ! read -r ANS_PORT; then ANS_PORT=""; fi
        if [ -n "$ANS_PORT" ]; then PANEL_PORT="$ANS_PORT"; fi

        printf "  ACME 账号邮箱（Let's Encrypt 用它发证书到期提醒）："
        if ! read -r ANS_MAIL; then ANS_MAIL=""; fi
        if [ -n "$ANS_MAIL" ]; then ACME_EMAIL="$ANS_MAIL"; fi

        printf '  证书验证方式：1) DNS-01（推荐：不占用任何端口，支持泛域名） 2) HTTP-01（需 80 端口空闲） [1]：'
        if ! read -r ANS_CERT; then ANS_CERT=""; fi
        case "$ANS_CERT" in
          2 | http | HTTP) PANEL_CERT_MODE="http" ;;
          *) PANEL_CERT_MODE="dns" ;;
        esac

        if [ "$PANEL_CERT_MODE" = "dns" ]; then
          printf '  DNS 厂商：1) DNSPod 2) 阿里云 3) Cloudflare [1]：'
          if ! read -r ANS_DNS; then ANS_DNS=""; fi
          case "$ANS_DNS" in
            2 | ali | aliyun)
              PANEL_DNS_PROVIDER="aliyun"
              if [ -z "${Ali_Key:-}" ]; then
                printf '    阿里云 AccessKey ID：'
                if ! read -r Ali_Key; then Ali_Key=""; fi
                export Ali_Key
                printf '    阿里云 AccessKey Secret（不回显）：'
                if ! read -r -s Ali_Secret; then Ali_Secret=""; fi
                printf '\n'
                export Ali_Secret
              fi
              ;;
            3 | cf | cloudflare)
              PANEL_DNS_PROVIDER="cloudflare"
              if [ -z "${CF_Token:-}" ]; then
                printf '    Cloudflare API Token（不回显）：'
                if ! read -r -s CF_Token; then CF_Token=""; fi
                printf '\n'
                export CF_Token
              fi
              ;;
            *)
              PANEL_DNS_PROVIDER="dnspod"
              if [ -z "${DP_Id:-}" ]; then
                printf '    DNSPod API ID：'
                if ! read -r DP_Id; then DP_Id=""; fi
                export DP_Id
                printf '    DNSPod API Token（不回显）：'
                if ! read -r -s DP_Key; then DP_Key=""; fi
                printf '\n'
                export DP_Key
              fi
              ;;
          esac
          # 凭据一律经**环境变量**交给子脚本：命令行参数会进 shell history 与 ps，
          # 而这里输入的内容连历史都不该留。
          printf '  是否同时签发通配符证书 *.%s（仅 DNS 方式可用）？[y/N] ' "$PANEL_DOMAIN"
          if ! read -r ANS_WC; then ANS_WC=""; fi
          case "$ANS_WC" in
            y | Y | yes | YES)
              PANEL_WILDCARD=1
              printf '  通配符证书本身不覆盖主域，访问入口请给一个具体子域（例如 panel.%s）：' "$PANEL_DOMAIN"
              if ! read -r PANEL_SERVER_NAME; then PANEL_SERVER_NAME=""; fi
              ;;
          esac
        fi
      fi
      ;;
  esac
fi

# ---- 管理员初始密码 ----
#
# 放在向导里（而不是等第 8 步才问）的理由：一次问完，用户中途不用被再拦一次。
# 第 8 步仍然会用它；这里没收集到（非交互执行、或已有数据时回车跳过）时，
# 第 8 步会自己再问一次，或在完全非交互的场景自动生成强口令 ——
# 三条路径都通向"一定有管理员可用"。
#
# 这里用**字面路径**判断"是否已有数据"（不引用 DATA_ROOT）：DATA_ROOT 在下面才定义，
# 而向导要在这之前问完。多写一个路径常量，比打乱变量定义顺序更不容易出错。
if [ -z "${ADMIN_PASS:-}" ] && [ "$DRY" = 0 ] && [ -t 0 ]; then
  # 标记"已经问过了"：第 8 步只在没问过时才追问，否则用户会被同一件事拦两次。
  # 用 ${VAR:-0} 的默认值判断而不是裸引用，是为了在 set -u 下也安全。
  ADMIN_PASS_ASKED=1
  if [ -f "/var/lib/shengyu-edgelink/meta.db" ]; then
    printf '管理员初始密码（检测到已有数据，本项不生效，直接回车跳过）：'
    if ! read -r -s ANS_PASS; then ANS_PASS=""; fi
    printf '\n'
  else
    printf '管理员初始密码（直接回车则自动生成 32 位强口令；输入时不回显）：'
    if ! read -r -s ANS_PASS; then ANS_PASS=""; fi
    printf '\n'
    if [ -n "$ANS_PASS" ]; then
      printf '再输入一次：'
      if ! read -r -s ANS_PASS2; then ANS_PASS2=""; fi
      printf '\n'
      if [ "$ANS_PASS" != "$ANS_PASS2" ]; then
        echo "  两次输入不一致，改为自动生成强口令。" >&2
        ANS_PASS=""
      fi
      unset ANS_PASS2
    fi
  fi
  if [ -n "$ANS_PASS" ]; then ADMIN_PASS="$ANS_PASS"; fi
  unset ANS_PASS
fi

# 面板参数校验。**刻意与 panel-https.sh 重复一遍**：安装到第 9b 步才发现参数非法，
# 意味着前面 8 个步骤已经改过系统了；在这里拦住，代价是零。
# 重复带来的"两处不一致"风险由 internal/deployscan 的测试兜住（两边都断言同样的禁令）。
if [ -n "$PANEL_DOMAIN" ]; then
  case "$PANEL_PORT" in
    '' | *[!0-9]*)
      echo "错误：--panel-port 必须是数字，当前：$PANEL_PORT" >&2
      exit 1
      ;;
  esac
  if [ "$PANEL_PORT" -lt 1 ] || [ "$PANEL_PORT" -gt 65535 ]; then
    echo "错误：--panel-port 必须在 1-65535，当前：$PANEL_PORT" >&2
    exit 1
  fi
  case "$PANEL_PORT" in
    80 | 443)
      echo "错误：--panel-port 不能用 $PANEL_PORT。" >&2
      echo "      80 要留给本机 acme 续期（HTTP-01 需要临时占用它），" >&2
      echo "      443 在很多机器上属于既有服务（反向代理/伪装站点）。" >&2
      echo "      请改用非标端口，例如 9443。" >&2
      exit 1
      ;;
  esac
  case "$PANEL_CERT_MODE" in
    dns | http) ;;
    *)
      echo "错误：--panel-cert 只能是 dns 或 http，当前：$PANEL_CERT_MODE" >&2
      exit 1
      ;;
  esac
  if [ "$PANEL_WILDCARD" = 1 ] && [ "$PANEL_CERT_MODE" != "dns" ]; then
    echo "错误：通配符证书只能用 DNS-01 签发（ACME 协议规定）。请用 --panel-cert dns。" >&2
    exit 1
  fi
  PANEL_ENABLED=1
else
  PANEL_ENABLED=0
fi

# 架构：交付物只有 amd64 / arm64 两种，装错架构的二进制会以 "cannot execute binary
# file" 的形式失败，而现场常常先怀疑包坏了。这里先判一次，直接说清楚。
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *)
    echo "不支持的机器架构：$ARCH（交付物只提供 amd64 / arm64）。" >&2
    exit 1
    ;;
esac

# 二进制在哪儿，**取决于这份脚本是从仓库跑的还是从发布包里跑的**：
#   从发布包跑：deploy/ 与二进制同级 —— <包>/deploy/install.sh、<包>/shengyu-edgelink-linux-amd64
#   从仓库跑：  二进制在 <仓库>/dist/ 下
# 只认 `../dist` 一种布局的话，"从发布包里安装"会静默跳过放二进制这一步，
# 之后服务起不来，而报错在 systemctl status 里，与"包坏了"无法区分。
SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
resolve_dist() {
  for cand in "$SELF_DIR/../dist" "$SELF_DIR" "$SELF_DIR/.."; do
    if [ -f "$cand/shengyu-edgelink-linux-$ARCH" ]; then
      printf '%s\n' "$cand"
      return 0
    fi
  done
  printf '%s\n' "$SELF_DIR/../dist"
}
DIST="${DIST:-$(resolve_dist)}"
CONF_ROOT=/etc/shengyu-edgelink/haproxy
# DATA_ROOT 是**状态目录本身**（= 服务运行时的 -data），LOG_ROOT 是它下面的日志分片根。
#
# 为什么必须显式写出 DATA_ROOT 而不能只建 LOG_ROOT（这是本轮排查发现的缺陷）：
#   `install -d /var/lib/shengyu-edgelink/logs` 会顺带创建父目录 /var/lib/shengyu-edgelink，
#   但父目录的属主是 **root:root 0755**，不是 shengyu。而元数据库 meta.db 恰恰
#   写在 /var/lib/shengyu-edgelink 这一层 —— 于是管理与首发会在启动时报
#   "permission denied"，且报错发生在另一个服务里，看上去像是数据库坏了。
#   只要有一层父目录忘了赋权，这类问题就必然出现，所以两层都显式建、显式赋权。
DATA_ROOT=/var/lib/shengyu-edgelink
LOG_ROOT=$DATA_ROOT/logs
BIN_DIR=/usr/local/bin
HAPROXY_BIN="${HAPROXY_BIN:-/usr/sbin/haproxy}"

say() { printf '\n=== %s ===\n' "$1"; }
run() {
  if [ "$DRY" = 1 ]; then echo "  [dry-run] $*"; else "$@"; fi
}

say "0. 前置检查"
echo "  管理面监听地址：$LISTEN（可用 --listen 修改，默认 $DEFAULT_LISTEN）"
if [ "$(id -u)" != 0 ]; then
  echo "必须以 root 执行（需要建账号、装 systemd 单元、绑特权端口）。" >&2
  exit 1
fi
if [ ! -x "$HAPROXY_BIN" ]; then
  echo "未找到 haproxy（$HAPROXY_BIN）。请先用发行版包管理器安装 HAProxy 社区版。" >&2
  echo "受支持版本范围见 docs/04-haproxy-baseline.md。" >&2
  exit 1
fi
# 关键：把版本打出来，并**卡住受支持范围**。
# 版本不在范围内的节点不允许发布任何业务（平台侧还会再卡一次）。
VER="$("$HAPROXY_BIN" -vv 2>&1 | awk '/^HAProxy version/{print $3}' | head -1)"
echo "检测到 HAProxy: ${VER:-未知}"
case "${VER%%.*}" in
  2) echo "  → 2.x 分支，受支持（需 ≥ 2.4）" ;;
  3) echo "  → 3.x 分支，受支持（需 < 3.2）" ;;
  *) echo "  警告：该主版本未经验证，平台会拒绝发布。请对照 docs/04。" ;;
esac

# ---- 依赖检查 ----
#
# 为什么必须有这一段（复核意见：「不要依赖未检查的 sudo -u，改用 runuser 或增加依赖检查」）：
# 一个 shell 脚本最容易失败的方式不是逻辑错，而是**缺一个命令**：
# 命令不存在时 bash 返回 127，而 `set -e` 会让它就地退出，
# 现场看到的只是一句 `xxx: command not found`，没有任何上下文，
# 于是"最小化安装的系统没装 shadow-utils"会被误判成"这个脚本有 bug"。
# 在这里把缺的东西一次性列清楚，安装者不用读完脚本才知道该装什么。
say "0b. 依赖检查"
MISSING=()
need() {
  command -v "$1" >/dev/null 2>&1 || MISSING+=("$1")
}
# 必需：整个安装过程都会用到。
for c in awk install id getent systemctl; do need "$c"; done
# 建账号用：只有账号/组不存在时才真的需要，存在则不必依赖。
getent group shengyu >/dev/null 2>&1 || need groupadd
id -u shengyu >/dev/null 2>&1 || need useradd
# 必需：tmpfiles.d 与 sysctl 的处理工具随 systemd 一起提供，缺了说明这台机不是 systemd 环境。
for c in systemd-tmpfiles sysctl; do need "$c"; done
# 必需：以服务身份执行命令（下一步会决定用 runuser 还是 sudo）。
if ! command -v runuser >/dev/null 2>&1 && ! command -v sudo >/dev/null 2>&1; then
  MISSING+=("runuser(util-linux) 或 sudo")
fi
# 可选缺失只提示，不阻断。
OPTIONAL_MISSING=()
for c in systemd-analyze; do
  command -v "$c" >/dev/null 2>&1 || OPTIONAL_MISSING+=("$c")
done

if [ "${#MISSING[@]}" -gt 0 ]; then
  echo "缺少必需的依赖，安装无法继续：" >&2
  printf '  - %s\n' "${MISSING[@]}" >&2
  echo >&2
  echo "常见补齐方式：" >&2
  echo "  Debian/Ubuntu: apt-get install -y util-linux coreutils passwd systemd" >&2
  echo "  RHEL/Rocky:    dnf install -y util-linux coreutils shadow-utils systemd" >&2
  echo "  Alpine(openrc)：本脚本依赖 systemd，不支持。" >&2
  exit 1
fi
[ "${#OPTIONAL_MISSING[@]}" -gt 0 ] && printf '  可选依赖缺失（不影响安装，少一项校验）：%s\n' "${OPTIONAL_MISSING[*]}"
echo "  依赖检查通过"

# ---- 决定"以 shengyu 身份执行命令"的方式 ----
#
# 为什么用 runuser 而不是 sudo（复核意见明确要求）：
#   1. sudo 是**可选包**。最小化安装的服务器常常没装，此时 `sudo -u shengyu ...`
#      只会得到一句 `sudo: command not found`，与前一步"用户没建出来"看起来完全不同，
#      现场根本无法判断到底少了什么。
#   2. runuser 属于 util-linux，而 systemd 本身就依赖 util-linux ——
#      凡是能跑 systemd 的机器一定有它。它不读 /etc/sudoers、不需要额外配置，
#      行为只取决于内核的 uid/gid 切换，也就没有"授权没配好"这种额外失败模式。
#   3. 这里刻意**不用** `runuser -l`（不加载 PAM 登录会话）：服务账号的 shell 是
#      /usr/sbin/nologin，目标只是"用它跑一次命令"，不需要登录语义。
#
# sudo 保留为**兜底**（仅当 runuser 不存在时），并且必须打印警告 ——
# 兜底路径不能默默生效，否则现场会以为我们本来就是靠 sudo 工作的。
SHENGYU_RUN=""
setup_run_as_shengyu() {
  if [ "$DRY" = 1 ]; then
    # dry-run 不能真的去检查 runuser：此刻还没做任何修改，检查结论也要打印给人看，
    # 所以这里只声明"将会怎么做"，不动系统也不做可能失败的分支。
    SHENGYU_RUN="runuser -u shengyu --"
    return 0
  fi
  if command -v runuser >/dev/null 2>&1; then
    SHENGYU_RUN="runuser -u shengyu --"
    echo "  将以服务身份执行：$SHENGYU_RUN <命令>"
    return 0
  fi
  if command -v sudo >/dev/null 2>&1; then
    SHENGYU_RUN="sudo -u shengyu"
    echo "  警告：本系统没有 runuser，退回用 sudo -u shengyu 兜底。" >&2
    echo "        这在功能上可行，但请尽快安装 util-linux 以获得确定的行为。" >&2
    return 0
  fi
  echo "错误：既没有 runuser 也没有 sudo，无法以 shengyu 身份执行安装步骤。" >&2
  exit 1
}

say "1. 创建账号与目录"
run getent group shengyu >/dev/null || run groupadd --system shengyu
run id -u shengyu >/dev/null 2>&1 || run useradd --system --gid shengyu --home-dir /var/lib/shengyu-edgelink --shell /usr/sbin/nologin shengyu
# 目录所有权分工（最小权限，三处路径必须与两个 unit 和程序参数完全一致 —— 评审 F02）：
#   /etc/shengyu-edgelink/haproxy  —— 配置。shengyu 写，HAProxy 只读
#   /var/lib/shengyu-edgelink      —— 状态。元数据库与日志分片，只有管理面读写
#   /run/shengyu-edgelink          —— 套接字。两个服务共用，由 tmpfiles.d 创建（见下）
run install -d -m 0750 -o shengyu -g shengyu "$CONF_ROOT"
run install -d -m 0750 -o shengyu -g shengyu "$CONF_ROOT/versions"
run install -d -m 0750 -o shengyu -g shengyu "$CONF_ROOT/current"
# DATA_ROOT **必须单独建**：它不只是 LOG_ROOT 的父目录，它本身就是 -data 指向的位置，
# meta.db 直接写在它下面（详见文件头的说明）。只建 LOG_ROOT 会把这一层留给 root，
# 结果是"日志文件能写、元数据库写不了"这种最难归因的半通状态。
run install -d -m 0750 -o shengyu -g shengyu "$DATA_ROOT"
run install -d -m 0750 -o shengyu -g shengyu "$LOG_ROOT"

# 账号已存在后再决定如何以它身份执行命令（见 setup_run_as_shengyu 的说明）。
setup_run_as_shengyu

# 写权限自检：这一步不做，上面的失败会推迟到"启动 shengyu-edgelink"时才暴露，
# 而那时错误出现在 systemctl status 里，与"数据库损坏"完全无法区分。
if [ "$DRY" = 0 ]; then
  for d in "$CONF_ROOT" "$CONF_ROOT/versions" "$CONF_ROOT/current" "$DATA_ROOT" "$LOG_ROOT"; do
    if [ ! -w "$d" ]; then
      echo "错误：$d 对 root 不可写，安装已中止。" >&2; exit 1
    fi
    # 关键：用最终身份验，不是用 root 验。root 能写 ≠ shengyu 能写。
    if ! $SHENGYU_RUN test -w "$d"; then
      echo "错误：$d 对 shengyu 用户不可写（请检查属主与 SELinux/AppArmor 策略）。" >&2
      echo "      当前属主：$(stat -c '%U:%G %a' "$d" 2>/dev/null)" >&2
      exit 1
    fi
  done
  echo "  目录权限自检通过（已以 shengyu 身份逐个验证可写）"
fi

say "2. 运行时目录（tmpfiles.d）"
# 不用 systemd 的 RuntimeDirectory：两个服务都要往这个目录放套接字，
# 谁声明它，谁重启时就会把另一个的套接字一起删掉。交给 tmpfiles.d 更稳。
run install -m 0644 deploy/tmpfiles-shengyu-edgelink.conf /etc/tmpfiles.d/shengyu-edgelink.conf
run systemd-tmpfiles --create /etc/tmpfiles.d/shengyu-edgelink.conf

say "3. 放置二进制（$ARCH）"
SRC="$DIST/shengyu-edgelink-linux-$ARCH"
if [ -f "$SRC" ]; then
  if [ "$DRY" = 1 ]; then
    echo "  [dry-run] install -m 0755 $SRC $BIN_DIR/shengyu-edgelink"
  else
    # 先写同目录下的临时文件，再 rename 覆盖。**不能直接写正在运行的可执行文件**：
    # Linux 对"正在被执行的 inode"写入返回 ETXTBSY（Text file busy），
    # 于是"服务还开着时升级"必然失败，而它的报错（Text file busy）在现场
    # 很容易被当成"磁盘只读/包损坏"去查。
    # rename 是原子的：已经运行的进程继续持有旧 inode 直到退出，不会中途崩。
    TMP="$BIN_DIR/.shengyu-edgelink.new.$$"
    install -m 0755 "$SRC" "$TMP"
    mv -f "$TMP" "$BIN_DIR/shengyu-edgelink"
    echo "  已放置 $BIN_DIR/shengyu-edgelink（原子替换，旧进程继续用旧 inode）"
  fi
else
  if [ "$DRY" = 1 ]; then
    echo "  [dry-run] 未找到 $SRC（正式执行到此会中止）"
  else
    echo "错误：未找到 $SRC。" >&2
    echo "      发布包里二进制应与 deploy/ 同级；源码树里应在 dist/ 下。" >&2
    echo "      也可用 DIST=<目录> 显式指定所在目录。" >&2
    exit 1
  fi
fi

MENU_SRC="$SELF_DIR/shengyu-edgelink-menu.sh"
if [ -f "$MENU_SRC" ]; then
  if [ "$DRY" = 1 ]; then
    echo "  [dry-run] install -m 0755 $MENU_SRC /usr/local/bin/edgelink"
  else
    install -m 0755 "$MENU_SRC" /usr/local/bin/edgelink
    echo "  已安装管理菜单：edgelink"
  fi
fi

say "4. 安装 systemd 单元（独立实例，不影响系统自带服务）"
run install -m 0644 deploy/shengyu-edgelink-haproxy.service /etc/systemd/system/shengyu-edgelink-haproxy.service
run install -m 0644 deploy/shengyu-edgelink-server.service /etc/systemd/system/shengyu-edgelink-server.service
# 把管理面监听地址写进**已安装**的 unit（需求：安装时可配置管理后台端口）。
#
# 为什么在这里注入而不是改源文件：deploy/ 里的模板要保留默认值给"不传参数"的场景，
# 真正生效的是 /etc/systemd/system 下这份。
#
# 正则刻意匹配 `-listen ` 之后的整段_token_（[^ ]+），而不是只匹配默认字面量
# 127.0.0.1:8081：否则第二次安装想换端口时，sed 找不到默认值就**不替换**，
# 表现为"我明明传了 --listen 9090，服务还是 8081" —— 这正是最难查的那类"改了不生效"。
if [ "$DRY" = 1 ]; then
  echo "  [dry-run] 将把 shengyu-edgelink-server.service 的 -listen 设为 $LISTEN"
else
  sed -i -E "s|(-listen )[^ ]+|\1${LISTEN}|" /etc/systemd/system/shengyu-edgelink-server.service
  echo "  管理面监听地址已写入 unit：$LISTEN"
fi
run systemctl daemon-reload
# 用 systemd-analyze verify 抓出单元里的语法/引用问题，
# 而不是等 systemctl start 才看到一句含糊的 "Job failed"。
if command -v systemd-analyze >/dev/null 2>&1; then
  run systemd-analyze verify /etc/systemd/system/shengyu-edgelink-haproxy.service
  run systemd-analyze verify /etc/systemd/system/shengyu-edgelink-server.service
fi

say "5. 受限特权授权（非 root 管理面需要令 HAProxy 平滑生效）"
# 授权走 **systemd D-Bus + polkit**，不是 sudoers。
#
# 为什么必须换掉 sudoers（复核意见明确要求）：
#   shengyu-edgelink-server.service 带 NoNewPrivileges=true，其语义是"不允许通过 setuid 提权"，
#   而 sudo 依赖 setuid —— 两者冲突，sudo 永远不可能成功，失败信息还只有一句
#   "sudo: effective uid is not 0"，无法与"授权没配好"区分。
#   polkit 不要求客户端有任何特权位，因此与 NoNewPrivileges 完全兼容。
#
# 规则文件把授权收窄到「shengyu 用户 + shengyu-edgelink-haproxy.service + reload 动作」，
# 收窄理由见 deploy/polkit-50-shengyu-edgelink.rules 的注释。
if [ -d /etc/polkit-1/rules.d ] || [ "$DRY" = 1 ]; then
  if [ "$DRY" = 1 ]; then
    echo "  [dry-run] install -m 0644 deploy/polkit-50-shengyu-edgelink.rules /etc/polkit-1/rules.d/50-shengyu-edgelink.rules"
  else
    run install -d -m 0755 /etc/polkit-1/rules.d
    # 权限必须 root:root 0644：polkitd 以自身身份读取该目录，属主或权限不对会被忽略。
    run install -m 0644 deploy/polkit-50-shengyu-edgelink.rules /etc/polkit-1/rules.d/50-shengyu-edgelink.rules
    # polkit 读取规则后需要重启守护进程才生效（不同发行版服务名不同）。
    if systemctl is-active --quiet polkit 2>/dev/null; then
      run systemctl restart polkit
    elif systemctl is-active --quiet polkitd 2>/dev/null; then
      run systemctl restart polkitd
    else
      echo "  未发现运行中的 polkit 守护进程：请确认系统装有 polkit（多数发行版默认有）。" >&2
    fi
    echo "  已安装 /etc/polkit-1/rules.d/50-shengyu-edgelink.rules（仅放行 shengyu → reload shengyu-edgelink-haproxy.service）"
  fi
else
  echo "  未找到 /etc/polkit-1/rules.d：本系统可能没有 polkit。" >&2
  echo "  没有它，非 root 管理面无法触发平滑生效。请手工配置等价授权，" >&2
  echo "  或改用 root 运行管理面（不推荐：面向网络的服务不应有 root）。" >&2
fi

say "6. 写入内核参数（转发平台必须调的两项）"
run install -m 0644 deploy/99-shengyu-edgelink.conf /etc/sysctl.d/99-shengyu-edgelink.conf
run sysctl --system >/dev/null

say "7. 生成基线配置（否则 HAProxy 首次启动会因配置不存在而失败）"
#
# 【真机踩过的坑】这一步原本**无条件**执行，于是重复安装（升级）时会拿基线
# 覆盖掉 current/ 里正在跑的已发布配置 —— 现象是"升级完转发全没了、端口不监听了"，
# 而脚本每一行都报成功。基线只在**还没有任何配置**时才该写。
#
# 为什么不能"总是写、反正待会儿会重新发布"：升级场景下没人保证管理员马上会点发布，
# 中间那段时间是真实的业务中断。宁可跳过这一步，也不要动已经在服务的配置。
if [ -f "$CONF_ROOT/current/haproxy.cfg" ]; then
  echo "  检测到已有配置 $CONF_ROOT/current/haproxy.cfg —— 跳过写基线（升级场景不覆盖在跑的配置）"
elif [ -x "$BIN_DIR/shengyu-edgelink" ]; then
  if [ "$DRY" = 1 ]; then
    echo "  [dry-run] $SHENGYU_RUN $BIN_DIR/shengyu-edgelink -config-root $CONF_ROOT -write-baseline"
  else
    # 以最终服务身份运行，顺带验证 shengyu 对配置目录确实有写权限 ——
    # 这一步用 root 跑就失去意义了（评审 F03：验收必须用最终身份）。
    #
    # HOME 显式指到服务 HOME：部分发行版在 uid 切换后保留调用者的 HOME，
    # 若那个目录对 shengyu 不可写，会得到与"配置目录没权限"难以区分的报错。
    if HOME=/var/lib/shengyu-edgelink $SHENGYU_RUN \
         "$BIN_DIR/shengyu-edgelink" -config-root "$CONF_ROOT" -write-baseline; then
      echo "  基线配置已写入 $CONF_ROOT/current（不监听任何端口）"
    else
      echo "  以 shengyu 身份写基线失败 —— 这正说明目录权限不对，请先修正再继续。" >&2
      echo "  快速核对三件事：" >&2
      echo "    1) $CONF_ROOT 及其子目录属主是否为 shengyu:shengyu（见本脚本第 1 步）" >&2
      echo "    2) $BIN_DIR/shengyu-edgelink 是否对其他用户可执行" >&2
      echo "    3) 是否启用了 SELinux/AppArmor 限制了该目录写入（ausearch -m AVC 查看）" >&2
      exit 1
    fi
  fi
else
  echo "  未找到 $BIN_DIR/shengyu-edgelink，跳过。首次启动时程序会自动补基线。"
fi

say "8. 初始化管理员"
#
# 需求：安装脚本必须**自动**完成管理员初始化 —— 以前这一步是"打印一条命令让用户自己跑"，
# 漏跑就会出现"服务起得来但登不进去"，而报错还发生在另一个服务里，极难归因。
#
# 幂等性：-admin-pass 只在**还没有任何管理员**时才生效，所以重复安装不会覆盖已有口令。
FRESH_DB=0
if [ ! -f "$DATA_ROOT/meta.db" ]; then FRESH_DB=1; fi

ADMIN_PASS="${ADMIN_PASS:-}"
# 向导里已经问过一次（ADMIN_PASS_ASKED=1）时不再追问 —— 同一件事问两遍，
# 用户第二次回车会以为"密码被改了"，而实际是自动生成。
if [ -z "$ADMIN_PASS" ] && [ "${ADMIN_PASS_ASKED:-0}" != 1 ] && [ "$DRY" = 0 ] && [ -t 0 ]; then
  printf '管理员口令（直接回车则自动生成强口令）：'
  if ! read -r ADMIN_PASS; then ADMIN_PASS=""; fi
fi
PASS_GENERATED=0
if [ -z "$ADMIN_PASS" ]; then
  # 只取字母数字：避免口令里出现引号/反斜杠，在命令行里被 shell 二次解释。
  #
  # 【真机踩过的坑】原先这里是「从 /dev/urandom 取字节 → 过滤出字母数字 →
  # 截断到 32 位」这样一条三级管道。在本脚本 `set -euo pipefail` 下它会
  # **直接把安装打断**：末端的截断命令读够就退出并关闭管道，上游随即收到
  # SIGPIPE(141)；pipefail 让整条管道的退出码变成 141，set -e 于是终止脚本 ——
  # 现象是"安装停在第 8 步、服务没起来、也没有任何报错"，极难归因。
  # 修法是在子 shell 里关掉 pipefail（只影响这一条命令，不放松全局严格性）。
  # internal/deployscan 里有测试专门盯这个组合，别再改回去。
  #
  # 另外用循环补足长度：tr 过滤会丢掉非字母数字字节，单次可能不足 32 位。
  ADMIN_PASS=""
  while [ ${#ADMIN_PASS} -lt 32 ]; do
    ADMIN_PASS="${ADMIN_PASS}$(set +o pipefail; LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom 2>/dev/null | head -c 32)"
  done
  ADMIN_PASS="${ADMIN_PASS:0:32}"
  PASS_GENERATED=1
fi
if [ -z "$ADMIN_PASS" ]; then
  echo "  无法生成管理员口令：/dev/urandom 不可用。请用 ADMIN_PASS=xxx ./install.sh 指定。" >&2
  exit 1
fi

if [ -x "$BIN_DIR/shengyu-edgelink" ]; then
  if [ "$DRY" = 1 ]; then
    echo "  [dry-run] $SHENGYU_RUN $BIN_DIR/shengyu-edgelink ... -admin-pass '***' -check"
  else
    CHK_LOG="/tmp/shengyu-edgelink-install-check.log"
    if HOME=/var/lib/shengyu-edgelink $SHENGYU_RUN "$BIN_DIR/shengyu-edgelink" \
        -config-root "$CONF_ROOT" -data "$DATA_ROOT" -dataplane haproxy \
        -admin-pass "$ADMIN_PASS" -check >"$CHK_LOG" 2>&1; then
      echo "  管理员已就绪"
    else
      echo "  管理员初始化失败，输出如下：" >&2
      cat "$CHK_LOG" >&2
      exit 1
    fi
  fi
fi

say "9. 启动服务"
if [ "$DRY" = 0 ]; then
  SRV_UP=0
  HP_UP=0
  if systemctl is-active --quiet shengyu-edgelink-server 2>/dev/null; then SRV_UP=1; fi
  if systemctl is-active --quiet shengyu-edgelink-haproxy 2>/dev/null; then HP_UP=1; fi
  if [ "$SRV_UP" = 1 ] || [ "$HP_UP" = 1 ]; then
    echo "  检测到服务已在运行（升级场景）：**不自动重启** —— 重启时机属于运维决策。"
    echo "    · shengyu-edgelink-server  → systemctl restart shengyu-edgelink-server（不影响转发）"
    echo "    · shengyu-edgelink-haproxy → 日常改配置走「发布」(USR2 平滑生效)，不要 restart"
  else
    echo "  首次安装：启用并启动服务"
    systemctl enable --now shengyu-edgelink-haproxy shengyu-edgelink-server
    sleep 2
    if systemctl is-active --quiet shengyu-edgelink-server 2>/dev/null; then
      echo "    · shengyu-edgelink-server   active"
    else
      echo "    · shengyu-edgelink-server   未启动，请用 systemctl status shengyu-edgelink-server 查看" >&2
    fi
    if systemctl is-active --quiet shengyu-edgelink-haproxy 2>/dev/null; then
      echo "    · shengyu-edgelink-haproxy  active"
    else
      echo "    · shengyu-edgelink-haproxy  未启动，请用 systemctl status shengyu-edgelink-haproxy 查看" >&2
    fi
  fi
fi

say "9b. 管理面外网访问（可选）"
#
# 这一步放在"启动服务"之后，因为它需要管理面**已经在监听** ——
# 反代的目标不存在时配 nginx，只会得到一个到第 9 步为止都正常的安装，
# 以及一个怎么点都是 502 的面板，而报错方向会指向 nginx 而不是"平台没起来"。
if [ "$PANEL_ENABLED" = 0 ]; then
  echo "  未开通（默认关闭，这是一个安全默认值）。"
  echo "  开通方式（已装好的机器上也能单独执行，不必重跑整套安装）："
  echo "    sudo bash deploy/panel-https.sh --domain panel.example.com"
  echo "  或重跑本脚本并加 --panel-domain panel.example.com（域名、端口等见 --help）。"
else
  PANEL_SCRIPT="$SELF_DIR/panel-https.sh"
  if [ ! -f "$PANEL_SCRIPT" ]; then
    echo "  错误：找不到 $PANEL_SCRIPT —— 发布包里应包含它（deploy/ 与 install.sh 同级）。" >&2
    echo "  面板未开通，平台本体不受影响。" >&2
  else
    PANEL_ARGS=(--domain "$PANEL_DOMAIN" --port "$PANEL_PORT" --cert "$PANEL_CERT_MODE")
    [ "$PANEL_CERT_MODE" = "dns" ] && PANEL_ARGS+=(--dns-provider "$PANEL_DNS_PROVIDER")
    [ -n "$PANEL_SERVER_NAME" ] && PANEL_ARGS+=(--server-name "$PANEL_SERVER_NAME")
    [ "$PANEL_WILDCARD" = 1 ] && PANEL_ARGS+=(--wildcard)
    [ -n "$ACME_EMAIL" ] && PANEL_ARGS+=(--acme-email "$ACME_EMAIL")
    [ -n "$PANEL_BASIC_USER" ] && PANEL_ARGS+=(--basic-user "$PANEL_BASIC_USER")
    [ "$DRY" = 1 ] && PANEL_ARGS+=(--dry-run)

    # 面板开通**失败不终止安装**：平台本体已经装好并且能跑，把整个安装判为失败
    # 会让人以为"这台机器没装上"，而实际上只是可选的外部访问没配成。
    # 但绝不能静默 —— 必须显式说明"什么没做成、怎么补"，否则用户会以为已经开通了。
    #
    # 凭据通过环境变量传递（第 8 步之前的交互或调用者 export），
    # 不放命令行：命令行会进 shell history 与 ps 输出。
    if bash "$PANEL_SCRIPT" "${PANEL_ARGS[@]}"; then
      echo "  面板已开通：https://${PANEL_SERVER_NAME:-$PANEL_DOMAIN}:$PANEL_PORT/"
    else
      # 双保险：panel-https.sh 内部已有 ERR trap 回滚，但它对 SIGKILL 无效
      # （脚本被强杀时 trap 不会执行）。这里再调一次 --disable（幂等）清一遍，
      # 确保不留下"半份 nginx 配置 + 一个指向不存在脚本的 timer"这类残留 ——
      # 这类残留不会立刻报错，只会在下次 reload 或重启时才炸。
      if [ "$DRY" = 0 ]; then
        bash "$PANEL_SCRIPT" --disable >/dev/null 2>&1 || true
        echo "  已执行回滚：本次新增的 Nginx 配置、证书配置与续期 systemd 单元都已撤销。" >&2
      fi
      echo >&2
      echo "  【面板开通未完成】上面的报错来自面板配置步骤，**平台本体已正常运行**，不影响转发与管理面。" >&2
      echo "  常见原因与处理：" >&2
      echo "    · DNS 凭据缺失/不正确 → 用环境变量提供后重跑：$PANEL_SCRIPT --domain $PANEL_DOMAIN --port $PANEL_PORT" >&2
      echo "    · 域名尚未解析到本机   → 先在 DNS 加 A 记录，再重跑上面这条命令" >&2
      echo "    · 端口已被占用         → 换一个端口（--port 9444）" >&2
      echo "    · 想先回退             → sudo bash deploy/panel-https.sh --disable" >&2
      echo "  注意：云厂商的**安全组**还要放行 TCP $PANEL_PORT，否则外网仍然连不上。" >&2
    fi
  fi
fi

say "10. 访问方式"
LISTEN_PORT="${LISTEN##*:}"
SERVER_IP="$(ip -4 route get 1.1.1.1 2>/dev/null | grep -oP 'src \K[0-9.]+' | head -1)"
if [ -z "$SERVER_IP" ]; then SERVER_IP="$(hostname -I 2>/dev/null | awk '{print $1}')"; fi
if [ -z "$SERVER_IP" ]; then SERVER_IP="<服务器公网IP>"; fi
SSH_PORT=""
if [ -n "${SSH_CONNECTION:-}" ]; then
  set -- $SSH_CONNECTION
  SSH_PORT="$4"
fi
if [ -z "$SSH_PORT" ]; then SSH_PORT="22"; fi

if [ "$PANEL_ENABLED" = 1 ]; then
  echo "  ===== 公网访问（已开通）====="
  echo "  地址：         https://${PANEL_SERVER_NAME:-$PANEL_DOMAIN}:${PANEL_PORT}/"
  # 用户名与建号时用的值一致：本脚本调用程序时没有传 -admin-user，
  # 因此就是程序的默认值 admin。
  echo "  管理员用户名： admin"
  if [ -n "$PANEL_BASIC_USER" ]; then
    echo "  登录要过**两道**（有意为之，两次口令相互独立）："
    echo "    ① 浏览器弹窗的 Basic 认证 → 用户 $PANEL_BASIC_USER"
    echo "    ② 平台登录页 → 平台账号 admin"
  else
    echo "  登录：先输平台账号 admin，再输平台口令。"
    echo "  提示：本面板直接暴露在公网。若来源 IP 不固定（无法用白名单），"
    echo "        建议加 --panel-basic-user 启用一层与平台口令独立的 Basic 认证。"
  fi
  echo "  证书续期：系统定时任务自动完成，续期成功后会 reload nginx 加载新证书。"
  echo "  记得在**云控制台安全组**放行 TCP ${PANEL_PORT}。"
  echo "  =============================="
  echo
fi
echo "  管理后台地址（服务本机）： http://$LISTEN"
echo "  健康检查：               curl http://$LISTEN/api/health"
echo "    应返回：{\"ok\":true}"
echo
echo "  管理面只绑本机地址。在你自己电脑上打开，请先建 SSH 隧道："
echo "    ssh -L ${LISTEN_PORT}:127.0.0.1:${LISTEN_PORT} -p ${SSH_PORT} root@${SERVER_IP}"
echo "  然后浏览器访问： http://127.0.0.1:${LISTEN_PORT}/"
echo
if [ "$PASS_GENERATED" = 1 ] && [ "$FRESH_DB" = 1 ]; then
  echo "  ===== 管理员凭据（只显示这一次，请立即保存）====="
  echo "  账号： admin"
  echo "  口令： $ADMIN_PASS"
  echo "  ================================================"
elif [ "$FRESH_DB" = 0 ]; then
  echo "  检测到已有数据（$DATA_ROOT/meta.db）：管理员口令沿用原值。"
  echo "  本次输入/生成的口令未生效（-admin-pass 只在首次建号时有效）。"
fi
echo
echo "  改管理端口：重新执行本脚本并加 --listen 地址:端口（会同步改写 systemd unit）。"
echo "  入口端口（转发用）请按你的安全组/防火墙自行放行；平台不会替你开关端口。"

if [ "$DRY" = 1 ]; then
  echo
  echo "（dry-run 结束，未做任何修改）"
fi
