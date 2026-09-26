#!/usr/bin/env bash
# shengyu 管理面「公网 HTTPS 访问」模块（由 deploy/install.sh 调用，也可单独执行）
#
# 为什么单独一个文件而不是塞进 install.sh：
#   ① install.sh 已经很长，而这段逻辑有独立的价值 —— **已装好的机器上单独开通面板**时
#      不该被迫重跑整套安装；
#   ② 与 uninstall.sh 需要成对（开通/回退），放一个文件里便于两边引用同一份路径常量。
#   逻辑只有一处 —— 这与仓库根 install.sh「只做转发、真正的逻辑只有一份」是同一条原则。
#
# ─────────────────────────────────────────────────────────────────────────────
# 用法
#   sudo bash deploy/panel-https.sh --domain panel.example.com --acme-email me@example.com
#   sudo bash deploy/panel-https.sh --domain example.com --wildcard --server-name panel.example.com
#   sudo bash deploy/panel-https.sh --disable              # 移除面板配置（保留证书）
#   sudo bash deploy/panel-https.sh --dry-run --domain ... # 只看会做什么
#
# 参数
#   --domain <域名>        证书主域（acme.sh 用它作为证书标识与续期依据）。必填（--disable 除外）
#   --server-name <域名>   浏览器访问用的域名（nginx server_name）。默认同 --domain
#   --wildcard             同时签发 *.基础域（通配符证书；**只能**用 DNS-01 签）
#   --port <端口>          公网监听端口，默认 9443。**不要用 80/443**（见下）
#   --cert <dns|http>      证书签发方式：dns=DNS-01（默认，推荐）；http=HTTP-01（需 80 空闲）
#   --dns-provider <名字>  DNS 厂商：dnspod(默认) | aliyun | cloudflare | huawei | tencent
#   --acme-email <邮箱>    ACME 账户邮箱（Let's Encrypt 用它发"证书即将过期"通知）
#   --basic-user <用户名>  可选：启用 HTTP Basic 二次认证（口令只能从环境变量/交互输入取）
#   --disable              移除面板配置
#   --dry-run              只打印会做什么
#
# 凭据一律走**环境变量**（不进命令行 —— 命令行会进 shell history 与 ps）：
#   DNSPod    : DP_Id / DP_Key
#   阿里云    : Ali_Key / Ali_Secret
#   Cloudflare: CF_Token (可选 CF_Account_ID)
#   华为云    : HUAWEICLOUD_Username / HUAWEICLOUD_Password
#   腾讯云    : Tencent_SecretId / Tencent_SecretKey
#   Basic 口令: SHENGYU_PANEL_BASIC_PASS
#
# ─────────────────────────────────────────────────────────────────────────────
# 三条不能破的约束（都不是"风格问题"，每一条都有真实后果）
#
#  【一】绝不监听 80 与 443，只监听 --port（默认 9443）
#      80 常常是**本机 acme.sh 续期要临时抢占**的端口（HTTP-01 standalone 模式）。
#      nginx 一旦常驻 80，别人（甚至本平台自己）的证书续期就会静默失败 ——
#      而"证书续期失败"通常要等到证书过期、浏览器报错才发现，中间没有任何告警。
#      443 在很多机器上属于既有服务（反向代理、伪装站点等）。改它等于改别人的业务。
#
#  【二】装完 nginx 必须撤掉发行版默认站点
#      Debian 系默认站点会 `listen 80 default_server`。装了不动它，等于违反了【一】。
#
#  【三】证书必须复制出 /root，落到 nginx 能读的目录
#      acme.sh 把证书放在 /root/.acme.sh/ 下，而 /root 权限是 0700，
#      nginx 的 worker 进程（www-data/nginx，非 root）**读不到**。
#      表现为 nginx 启动报 `cannot load certificate ... Permission denied`，
#      而证书文件明明"存在" —— 这类问题很容易被误判成证书没签成功。
#
# ─────────────────────────────────────────────────────────────────────────────
# 【失败必须回滚】本脚本记录自己**新建**了哪些东西，任何一步失败都按序撤销：
#   systemd 单元 → nginx 配置 → 被移除的发行版默认站点 → 本次新签的证书 → 状态目录。
#   为什么不让运维手工收拾：失败现场往往只剩半份配置（nginx 指向一个不存在的证书、
#   timer 指向一个不存在的脚本），这些残留不会报错，只会在下次 reload/重启时炸，
#   而且难以判断"哪些是这次留下的"。
#   注意边界：**只回滚本次新建的**。如果是复用机器上已有的证书（例如别的服务签的），
#   回滚不会去动它 —— 那会把别人的服务一起弄坏。
set -euo pipefail

# ============================== 参数 ==============================
DRY=0
DOMAIN=""
SERVER_NAME=""
WILDCARD=0
PORT="9443"
CERT_MODE="dns"
DNS_PROVIDER="dnspod"
BASIC_USER=""
DISABLE=0
ACME_EMAIL="${ACME_EMAIL:-}"

while [ $# -gt 0 ]; do
  case "$1" in
    --domain) shift || true; DOMAIN="${1:-}" ;;
    --domain=*) DOMAIN="${1#--domain=}" ;;
    --server-name) shift || true; SERVER_NAME="${1:-}" ;;
    --server-name=*) SERVER_NAME="${1#--server-name=}" ;;
    --wildcard) WILDCARD=1 ;;
    --port) shift || true; PORT="${1:-}" ;;
    --port=*) PORT="${1#--port=}" ;;
    --cert) shift || true; CERT_MODE="${1:-}" ;;
    --cert=*) CERT_MODE="${1#--cert=}" ;;
    --dns-provider) shift || true; DNS_PROVIDER="${1:-}" ;;
    --dns-provider=*) DNS_PROVIDER="${1#--dns-provider=}" ;;
    --acme-email) shift || true; ACME_EMAIL="${1:-}" ;;
    --acme-email=*) ACME_EMAIL="${1#--acme-email=}" ;;
    --basic-user) shift || true; BASIC_USER="${1:-}" ;;
    --basic-user=*) BASIC_USER="${1#--basic-user=}" ;;
    --disable) DISABLE=1 ;;
    --dry-run) DRY=1 ;;
    -h | --help) sed -n '2,52p' "$0"; exit 0 ;;
    *) echo "未知参数：$1（用 --help 查看用法）" >&2; exit 1 ;;
  esac
  shift || true
done

# ============================== 常量 ==============================
PANEL_DIR=/etc/shengyu-panel
TLS_DIR="$PANEL_DIR/tls"
STATE_FILE="$PANEL_DIR/panel.env"
BACKUP_DIR="$PANEL_DIR/backup"
NGINX_SITE=shengyu-panel
RATE_CONF=/etc/nginx/conf.d/shengyu-panel-ratelimit.conf
SNIPPET=/etc/nginx/snippets/shengyu-panel-proxy.conf
HTPASS_FILE=/etc/nginx/.htpasswd-shengyu-panel
UPSTREAM="${SHENGYU_PANEL_UPSTREAM:-127.0.0.1:8081}"
ACME_HOME=/root/.acme.sh
# 续期用的 systemd 单元。名字刻意不用 shengyu-edgelink- 前缀：
# 那两个单元属于转发内核与管理面，而这是"证书维护"这一独立职责，混在一起会让人
# 以为停掉转发就能停掉续期。
RENEW_SERVICE=shengyu-panel-acme-renew.service
RENEW_TIMER=shengyu-panel-acme-renew.timer
SITE_TARGET=/etc/nginx/sites-available/$NGINX_SITE

say() { printf '\n=== %s ===\n' "$1"; }
log() { printf '  %s\n' "$*"; }
ok() { printf '  [OK] %s\n' "$*"; }
warn() { printf '  [!!] %s\n' "$*" >&2; }
die() {
  printf '\n[中止] %s\n' "$*" >&2
  exit 1
}
run() {
  if [ "$DRY" = 1 ]; then echo "  [dry-run] $*"; else "$@"; fi
}
rm_if() {
  if [ -e "$1" ] || [ -L "$1" ]; then
    run rm -f "$1"
  else
    log "跳过（不存在）：$1"
  fi
}

# ============================== 事务回滚 ==============================
# 只记录"本次新建/本次改动"的东西，失败时按序撤销。
# 为什么用 ERR trap 而不是把每步都包进 if：shell 里最可靠的"无论从哪一步失败都能收尾"
# 的机制就是 trap；靠手写 if 覆盖每个命令，迟早会漏掉一处，而漏掉的那处正是故障点。
NEW_FILES=()          # 本次新写的文件（删除即还原）
REMOVED_DEFAULT=0     # 本次移除了发行版默认站点
CERT_CREATED_HERE=""  # 本次**新签**的证书主域（空=复用已有，不是本次签的）
NEW_UNITS=()          # 本次新建的 systemd 单元
ROLLED_BACK=0

track_file() { NEW_FILES+=("$1"); }
track_unit() { NEW_UNITS+=("$1"); }

rollback_all() {
  local rc="${1:-1}"
  # 防递归：回滚过程中任何命令失败都不该再次触发回滚。
  # （trap 里已经 set +e，这个守卫是防止将来有人改回去时静默地无限递归。）
  [ "$ROLLED_BACK" = 1 ] && return 0
  ROLLED_BACK=1

  printf '\n【回滚】配置过程中失败，正在撤销本次的全部修改…\n' >&2

  # ① systemd 单元：先停掉，再删文件。顺序反了会留下"被引用但文件已删"的失败单元。
  local u
  for u in "${NEW_UNITS[@]}"; do
    systemctl disable --now "$u" >/dev/null 2>&1 || true
    rm -f "/etc/systemd/system/$u"
    printf '  已撤销 systemd 单元：%s\n' "$u" >&2
  done
  if [ "${#NEW_UNITS[@]}" -gt 0 ]; then
    systemctl daemon-reload >/dev/null 2>&1 || true
  fi

  # ② nginx 配置
  local f
  for f in "${NEW_FILES[@]}"; do
    if [ -e "$f" ] || [ -L "$f" ]; then
      rm -f "$f"
      printf '  已撤销文件：%s\n' "$f" >&2
    fi
  done

  # ③ 恢复被我们移除的发行版默认站点（否则 nginx 会少了发行版原本的行为）
  if [ "$REMOVED_DEFAULT" = 1 ] && [ -f "$BACKUP_DIR/default.site" ]; then
    cp -a "$BACKUP_DIR/default.site" /etc/nginx/sites-enabled/default 2>/dev/null || true
    printf '  已恢复发行版默认站点（/etc/nginx/sites-enabled/default）\n' >&2
  fi

  # ④ 本次新签的证书：撤掉，免得留下一个没人管理、却在后台自动续期的证书。
  #    只动**本次签的**：复用机器上已有证书时绝不动它（那是别人的服务在用）。
  if [ -n "$CERT_CREATED_HERE" ] && [ -x "$ACME_HOME/acme.sh" ]; then
    "$ACME_HOME/acme.sh" --remove -d "$CERT_CREATED_HERE" --ecc >/dev/null 2>&1 || true
    rm -rf "$ACME_HOME/${CERT_CREATED_HERE}_ecc" 2>/dev/null || true
    printf '  已撤销本次签发的证书：%s\n' "$CERT_CREATED_HERE" >&2
  fi

  # ⑤ nginx 配置现在应当是"我们没来过"的样子：合法则 reload，不合法就不动
  #    （不合法说明回滚本身出了岔子，这时**不能** reload，否则把问题扩大成停机）。
  if command -v nginx >/dev/null 2>&1; then
    if nginx -t >/dev/null 2>&1; then
      systemctl reload nginx >/dev/null 2>&1 || true
      printf '  nginx 已 reload（回到配置前的状态）\n' >&2
    else
      printf '  【注意】nginx 配置当前不合法，未 reload。请手工检查：nginx -t\n' >&2
    fi
  fi

  # ⑥ 状态目录最后删（它同时存着 ③ 要用的备份）
  rm -rf "$TLS_DIR" "$STATE_FILE" 2>/dev/null || true
  rmdir "$BACKUP_DIR" 2>/dev/null || true
  rmdir "$PANEL_DIR" 2>/dev/null || true

  printf '【回滚完成】平台本体未受影响；管理面仍只监听 %s。\n' "$UPSTREAM" >&2
}

# 只在"真会改系统"时装 trap：dry-run 什么都没做，回滚没有意义。
install_rollback_trap() {
  [ "$DRY" = 1 ] && return 0
  trap 'rc=$?; set +e; rollback_all "$rc"; exit "$rc"' ERR
}

# ============================== 发行版适配 ==============================
# nginx 的 worker 身份在不同发行版叫法不同：Debian 系是 www-data，RHEL 系是 nginx。
# 写错会导致私钥/口令文件的属组不对 ⇒ nginx 读不到 ⇒ 502 且看不出原因。
NGINX_GROUP=""
nginx_group() {
  if [ -n "$NGINX_GROUP" ]; then
    printf '%s' "$NGINX_GROUP"
    return
  fi
  local g
  for g in www-data nginx; do
    if getent group "$g" >/dev/null 2>&1; then
      NGINX_GROUP="$g"
      printf '%s' "$g"
      return
    fi
  done
  # 都找不到时不猜，返回 root：至少能让 nginx 读到（root 组），并让调用方给出警告。
  NGINX_GROUP="root"
  printf 'root'
}

detect_pkg_mgr() {
  if command -v apt-get >/dev/null 2>&1; then printf 'apt'
  elif command -v dnf >/dev/null 2>&1; then printf 'dnf'
  elif command -v yum >/dev/null 2>&1; then printf 'yum'
  else printf ''
  fi
}

# dns_plugin 把厂商别名映射到 acme.sh 的插件名。
dns_plugin() {
  case "$1" in
    dnspod) printf 'dns_dp' ;;
    aliyun | ali) printf 'dns_ali' ;;
    cloudflare | cf) printf 'dns_cf' ;;
    huawei) printf 'dns_huaweicloud' ;;
    tencent) printf 'dns_tencent' ;;
    *) printf '' ;;
  esac
}

# ============================== 回退（用户主动） ==============================
disable_panel() {
  say "移除管理面公网入口（保留证书与 nginx 本体）"
  log "为什么保留证书：泛域名证书很可能被本机其它服务共用（例如某个面板/站点）。"
  log "自动删掉它会让那些服务在下次续期前一直用旧证，甚至直接过期 —— 删不删该由人决定。"
  local u
  for u in "$RENEW_TIMER" "$RENEW_SERVICE"; do
    if [ -e "/etc/systemd/system/$u" ]; then
      if [ "$DRY" = 0 ]; then
        systemctl disable --now "$u" >/dev/null 2>&1 || true
      fi
      rm_if "/etc/systemd/system/$u"
    fi
  done
  rm_if "/etc/nginx/sites-enabled/$NGINX_SITE"
  rm_if "/etc/nginx/sites-available/$NGINX_SITE"
  rm_if "/etc/nginx/conf.d/$NGINX_SITE.conf"
  rm_if "$RATE_CONF"
  rm_if "$SNIPPET"
  rm_if "$HTPASS_FILE"
  if [ "$DRY" = 0 ]; then
    if command -v systemctl >/dev/null 2>&1; then
      systemctl daemon-reload >/dev/null 2>&1 || true
    fi
    if command -v nginx >/dev/null 2>&1; then
      if nginx -t >/dev/null 2>&1; then
        systemctl reload nginx
        ok "nginx 已 reload（面板入口已撤下）"
      else
        warn "nginx 配置现在不合法，未 reload。请手工检查：nginx -t"
      fi
    fi
  fi
  rm_if "$STATE_FILE"
  log "证书仍在：$TLS_DIR （如需删除请自行处理）"
  log "acme.sh 的续期任务未改动；如确认该证书再无用途，可用：$ACME_HOME/acme.sh --remove -d <域名>"
  log "管理面本身**没有**任何改动：它仍然只监听 $UPSTREAM。"
}

# ============================== 主流程 ==============================
if [ "$(id -u)" != 0 ]; then
  die "必须以 root 执行（要装 nginx、写 /etc、复制证书）。"
fi

if [ "$DISABLE" = 1 ]; then
  disable_panel
  [ "$DRY" = 1 ] && printf '\n（dry-run 结束，未做任何修改）\n'
  exit 0
fi

[ -n "$DOMAIN" ] || die "缺少 --domain（证书主域）。例如：--domain panel.example.com"
[ -n "$SERVER_NAME" ] || SERVER_NAME="$DOMAIN"

# 端口校验：这一步不能省。端口写错（少了、或 65536）会写进 nginx 配置，
# 结果是 nginx -t 失败或服务起不来，而报错位置与"参数写错"相隔很远。
case "$PORT" in
  '' | *[!0-9]*) die "端口必须是数字，当前：$PORT" ;;
esac
if [ "$PORT" -lt 1 ] || [ "$PORT" -gt 65535 ]; then
  die "端口必须在 1-65535，当前：$PORT"
fi
# 这两条是硬约束（见文件头【一】）。用显式拒绝而不是"默默让你改"：
# 让人改一个参数，比让人事后从"别人的证书续期为什么失败了"倒查回来便宜得多。
case "$PORT" in
  80 | 443) die "端口 $PORT 是本模块明确禁止使用的：80 要留给 acme 续期、443 常属既有服务。请改用 9443 之类的非标端口。" ;;
esac
if [ "$PORT" = "8443" ]; then
  warn "端口 8443 与平台默认的转发入口端口相同，极可能冲突。确认这台机器上没有业务用它再继续。"
fi
case "$CERT_MODE" in
  dns | http) ;;
  *) die "--cert 只能是 dns 或 http，当前：$CERT_MODE" ;;
esac
if [ "$WILDCARD" = 1 ] && [ "$CERT_MODE" != "dns" ]; then
  die "通配符证书只能用 DNS-01 签发（ACME 协议规定），请用 --cert dns。"
fi
if [ "$CERT_MODE" = "dns" ]; then
  PLUGIN="$(dns_plugin "$DNS_PROVIDER")"
  [ -n "$PLUGIN" ] || die "不支持的 --dns-provider：$DNS_PROVIDER（支持 dnspod/aliyun/cloudflare/huawei/tencent）"
fi

say "0. 前置检查"
log "证书主域   : $DOMAIN$([ "$WILDCARD" = 1 ] && printf ' + *.%s' "$DOMAIN")"
log "浏览器访问 : https://$SERVER_NAME:$PORT/"
log "签发方式   : $CERT_MODE $([ "$CERT_MODE" = dns ] && printf '（插件 %s）' "$PLUGIN")"
[ -n "$ACME_EMAIL" ] && log "ACME 邮箱  : $ACME_EMAIL"
log "反代目标   : http://$UPSTREAM"

# 记录 443/8443 的现状，结束时对比 —— 本模块不该改变它们。
# 先记后比，是为了让"我没动它"这句话有据可查，而不是一句承诺。
BEFORE_PORTS="$(ss -lnt 2>/dev/null | awk '{print $4}' | grep -oE ':(443|8443)$' | sort -u | tr '\n' ' ' || true)"
log "现有 443/8443 监听：${BEFORE_PORTS:-（无）}"

# 上游必须在跑：否则配好反代只会得到 502，而排查会从 nginx 开始，方向就偏了。
if [ "$DRY" = 0 ]; then
  if curl -fsS -o /dev/null --max-time 5 "http://$UPSTREAM/api/health" 2>/dev/null; then
    ok "管理面可达（$UPSTREAM/api/health）"
  else
    warn "管理面 $UPSTREAM 无响应。反代配好后会返回 502。请先确认 shengyu-edgelink-server 已启动。"
  fi
fi

# 前置检查通过，从这里开始每个动作都会改系统 —— 装回滚 trap。
install_rollback_trap

say "1. 安装 nginx 并设为开机启动"
if command -v nginx >/dev/null 2>&1; then
  ok "已安装：$(nginx -v 2>&1)"
else
  MGR="$(detect_pkg_mgr)"
  [ -n "$MGR" ] || die "没有可用的包管理器（apt/dnf/yum），无法自动安装 nginx。请手工安装后重跑本脚本。"
  log "本机没有 nginx，用 $MGR 安装"
  case "$MGR" in
    apt)
      run apt-get update -qq
      run env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nginx
      ;;
    dnf) run dnf install -y -q nginx ;;
    yum) run yum install -y -q nginx ;;
  esac
  if [ "$DRY" = 0 ] && ! command -v nginx >/dev/null 2>&1; then
    die "nginx 安装失败（包管理器返回成功但命令不存在）。请手工安装后重跑。"
  fi
  ok "nginx 已安装"
fi
# enable --now：既开机自启，也立刻在跑。分两步写容易漏掉其中一步，
# 而漏掉的后果是"重启后面板没了"，通常要等到下次重启才被发现。
run systemctl enable --now nginx || true

say "2. 撤掉发行版默认站点（硬约束【二】：nginx 不许占 80）"
# Debian/Ubuntu：默认站点是 sites-enabled/default，内容含 `listen 80 default_server`。
# 只删**软链**，保留 sites-available 里的原件 —— 这是发行版的原意（可再启用）。
if [ -e /etc/nginx/sites-enabled/default ]; then
  if [ "$DRY" = 1 ]; then
    echo "  [dry-run] rm -f /etc/nginx/sites-enabled/default（并备份内容）"
  else
    install -d -m 0755 "$BACKUP_DIR"
    cp -a /etc/nginx/sites-enabled/default "$BACKUP_DIR/default.site" 2>/dev/null || true
    rm -f /etc/nginx/sites-enabled/default
    REMOVED_DEFAULT=1
    ok "已移除 sites-enabled/default（内容备份在 $BACKUP_DIR/default.site，失败回滚时会自动放回）"
  fi
else
  if [ "$DRY" = 1 ] && ! command -v nginx >/dev/null 2>&1; then
    # dry-run 时 nginx 还没装，所以看不到 default 站点 —— 但真实执行时它一定在
    # （第 1 步刚装上）。这里明确说出来，否则看 dry-run 的人会以为"不会处理它"。
    log "无 sites-enabled/default（dry-run 下 nginx 尚未安装；真实执行时它会出现并被移除）"
  else
    log "无 sites-enabled/default（非 Debian 系，或已被移除）"
  fi
fi

if [ "$DRY" = 0 ]; then
  if nginx -t >/dev/null 2>&1; then
    systemctl reload nginx 2>/dev/null || systemctl restart nginx
  fi
  sleep 1
  if ss -lnt | grep -q ':80 '; then
    OWNER="$(ss -lntp 2>/dev/null | grep ':80 ' | head -1 || true)"
    warn "80 端口仍被占用：$OWNER"
    warn "若占用者是 nginx，请检查 /etc/nginx/ 下还有没有别的 listen 80："
    warn "  grep -rn 'listen.*80' /etc/nginx/ | grep -v '#'"
    warn "若占用者是别的服务，与本模块无关，但要注意它会挡住 HTTP-01 方式（--cert http）。"
  else
    ok "80 端口未被占用（本机的 acme 续期路径保持可用）"
  fi
fi

say "3. 准备 acme.sh（证书的申请与自动续期）"
# 为什么用 acme.sh 而不是 certbot：
#   ① certbot 的 nginx 插件会**改写 nginx 配置**，侵入性强且回退不干净；
#      acme.sh 只碰自己的目录，与本模块"只加自己的文件"的原则一致；
#   ② acme.sh 是纯 shell 单文件，自带续期，不引入 python 依赖树；
#   ③ 本机可能已经在用它（例如别的服务签过证书）—— 复用同一个工具，避免两套 ACME
#      客户端各自续期、各自 reload，那是最容易互相踩的组合。
if [ -x "$ACME_HOME/acme.sh" ]; then
  ok "已存在 acme.sh（$("$ACME_HOME/acme.sh" --version 2>/dev/null | head -1 || true)）"
else
  if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
    die "需要 curl 或 wget 才能安装 acme.sh。"
  fi
  log "安装 acme.sh 到 $ACME_HOME"
  if [ "$DRY" = 1 ]; then
    echo "  [dry-run] curl https://get.acme.sh | sh -s email=$ACME_EMAIL"
  else
    if command -v curl >/dev/null 2>&1; then
      curl -fsS https://get.acme.sh | sh -s "email=$ACME_EMAIL" >/dev/null 2>&1 || die "acme.sh 安装失败"
    else
      wget -qO- https://get.acme.sh | sh -s "email=$ACME_EMAIL" >/dev/null 2>&1 || die "acme.sh 安装失败"
    fi
    [ -x "$ACME_HOME/acme.sh" ] || die "acme.sh 安装后仍不存在（$ACME_HOME/acme.sh），请手工安装。"
    ok "acme.sh 已安装"
  fi
fi

# ACME 账户邮箱：Let's Encrypt 用它发"证书即将过期"通知。
# 已有账户时再注册会返回"已存在"，这不是错误 —— 所以只提示不中止。
if [ -n "$ACME_EMAIL" ] && [ "$DRY" = 0 ]; then
  if "$ACME_HOME/acme.sh" --register-account -m "$ACME_EMAIL" --server letsencrypt >/dev/null 2>&1; then
    ok "ACME 账户已注册：$ACME_EMAIL"
  else
    log "ACME 账户邮箱未更新（通常表示账户已存在，或邮箱没变化）"
  fi
fi

say "4. 申请证书（$CERT_MODE）"
# 为什么默认 DNS-01 而不是 HTTP-01：
#   · HTTP-01 需要 80 端口在本机**可被公网访问**，且**续期时同样需要** ——
#     一旦以后有别的服务占用 80，续期就会静默失败；
#   · DNS-01 完全不占用本机任何端口，也不要求本机对外暴露 80；
#   · 通配符证书只能用 DNS-01。
# 代价是 DNS 厂商的 API 凭据，这属于"一次配置、长期省事"的交换。
if [ -f "$ACME_HOME/${DOMAIN}_ecc/${DOMAIN}.conf" ]; then
  ok "acme.sh 里已有 $DOMAIN 的证书记录，跳过签发（交由续期任务续期）"
  log "如需强制重签：$ACME_HOME/acme.sh --issue --force ..."
else
  ISSUE_ARGS=(-d "$DOMAIN")
  [ "$WILDCARD" = 1 ] && ISSUE_ARGS+=(-d "*.$DOMAIN")
  if [ "$CERT_MODE" = "dns" ]; then
    # 凭据校验：缺凭据时的报错来自 acme.sh 内部，往往只有一行
    # "Please set the env variable"，现场不容易判断是哪个厂商缺哪一项。这里先说清楚。
    case "$DNS_PROVIDER" in
      dnspod)
        [ -n "${DP_Id:-}" ] && [ -n "${DP_Key:-}" ] || die "缺少 DNSPod 凭据：请设置环境变量 DP_Id 与 DP_Key（不要放命令行，会进 history 与 ps）"
        ;;
      aliyun | ali)
        [ -n "${Ali_Key:-}" ] && [ -n "${Ali_Secret:-}" ] || die "缺少阿里云凭据：请设置环境变量 Ali_Key 与 Ali_Secret"
        ;;
      cloudflare | cf)
        [ -n "${CF_Token:-}" ] || die "缺少 Cloudflare 凭据：请设置环境变量 CF_Token"
        ;;
      huawei)
        [ -n "${HUAWEICLOUD_Username:-}" ] && [ -n "${HUAWEICLOUD_Password:-}" ] || die "缺少华为云凭据：请设置 HUAWEICLOUD_Username / HUAWEICLOUD_Password"
        ;;
      tencent)
        [ -n "${Tencent_SecretId:-}" ] && [ -n "${Tencent_SecretKey:-}" ] || die "缺少腾讯云凭据：请设置 Tencent_SecretId / Tencent_SecretKey"
        ;;
    esac
    run "$ACME_HOME/acme.sh" --issue --dns "$PLUGIN" "${ISSUE_ARGS[@]}" \
      --server letsencrypt --keylength ec-256
  else
    if [ "$DRY" = 0 ] && ss -lnt | grep -q ':80 '; then
      die "HTTP-01 需要 80 端口空闲，但当前 80 已被占用。请改用 --cert dns，或先释放 80。"
    fi
    run "$ACME_HOME/acme.sh" --issue --standalone "${ISSUE_ARGS[@]}" \
      --server letsencrypt --keylength ec-256
  fi
  # 记下来：这是本次新签的证书，失败时必须撤销（否则会留下一个后台自动续期、
  # 却没人知道其存在的证书）。
  CERT_CREATED_HERE="$DOMAIN"
  ok "证书申请完成"
fi

say "5. 部署证书到 nginx 可读目录（硬约束【三】）"
if [ "$DRY" = 1 ]; then
  echo "  [dry-run] mkdir -p $TLS_DIR && chmod 0755 $PANEL_DIR $TLS_DIR"
  echo "  [dry-run] $ACME_HOME/acme.sh --install-cert -d $DOMAIN --ecc \\"
  echo "              --key-file $TLS_DIR/privkey.pem --fullchain-file $TLS_DIR/fullchain.pem \\"
  echo "              --reloadcmd 'systemctl reload nginx'"
else
  install -d -m 0755 "$PANEL_DIR"
  install -d -m 0755 "$TLS_DIR"
  # --install-cert 是"自动续期能生效"的闭环：它把当前证书复制到指定路径，
  # 并把 --reloadcmd 写进该证书的续期配置 —— 续期成功后 acme.sh 会执行它，
  # nginx 于是加载到新证书。**少了 --reloadcmd，续期成功但 nginx 仍用旧证**，
  # 这是最典型的"续了但没生效"，且没有任何报错。
  # 这里只影响本域名自己的 conf（$ACME_HOME/<域名>_ecc/<域名>.conf），
  # 不会改动本机其它证书（例如别的服务签的）的 reloadcmd。
  "$ACME_HOME/acme.sh" --install-cert -d "$DOMAIN" --ecc \
    --key-file "$TLS_DIR/privkey.pem" \
    --fullchain-file "$TLS_DIR/fullchain.pem" \
    --reloadcmd "systemctl reload nginx" || die "证书部署失败（--install-cert）"
  GRP="$(nginx_group)"
  chmod 0644 "$TLS_DIR/fullchain.pem"
  chmod 0640 "$TLS_DIR/privkey.pem"
  chown root:"$GRP" "$TLS_DIR/privkey.pem" 2>/dev/null || true
  ok "证书已就位：$TLS_DIR（私钥 root:$GRP 0640）"
  # 打印证书信息只为"当场能看见签的是谁、什么时候过期"，读失败不该让开通流程中断，
  # 所以两处都带 `|| true`。
  openssl x509 -in "$TLS_DIR/fullchain.pem" -noout -subject -enddate 2>/dev/null | sed 's/^/  /' || true
  openssl x509 -in "$TLS_DIR/fullchain.pem" -noout -ext subjectAltName 2>/dev/null | tail -n +2 | tr ',' '\n' | sed 's/^ */     SAN: /' || true
fi

say "6. 写 nginx 配置（公网 HTTPS 端口 → $UPSTREAM）"
# 限速区必须定义在 http 上下文里，所以单独放 conf.d（server 块在 sites-enabled）。
#
# 为什么必须在 nginx 层限速（而不是只靠平台自己）：
#   平台的登录限流是按 clientIP 计数的，而它的 clientIP **只信任直连地址**
#   （源码注释写明：X-Forwarded-For 可伪造，用它做判定等于给攻击者留后门）。
#   于是反代之后，所有外部客户端在平台眼里都是 127.0.0.1 —— 共用一个令牌桶。
#   后果有两个方向：多人同时登录会互相挤掉配额；攻击者打满配额可以把合法用户挡在外面。
#   在 nginx 层按真实来源限速，是补上这个缺口的正解。
if [ "$DRY" = 1 ]; then
  echo "  [dry-run] 写 $RATE_CONF / $SNIPPET / $SITE_TARGET"
else
  install -d -m 0755 /etc/nginx/conf.d /etc/nginx/snippets
  cat >"$RATE_CONF" <<EOF
# 由 deploy/panel-https.sh 生成。只定义限速区与连接数区，不含 server 块。
# 数值取舍：登录接口 6 次/分钟 —— 正常人手输密码不会超过这个量级，
# 而暴力破解需要的是每秒量级，于是这条限制几乎只挡住攻击者。
limit_req_zone  \$binary_remote_addr zone=shengyu_panel_all:10m   rate=30r/s;
limit_req_zone  \$binary_remote_addr zone=shengyu_panel_login:10m rate=6r/m;
limit_conn_zone \$binary_remote_addr zone=shengyu_panel_conn:10m;
EOF
  track_file "$RATE_CONF"

  cat >"$SNIPPET" <<'EOF'
# 管理面反代的公共参数。抽成 snippet 供多个 location 引用，避免两处各写一份后漂移。
proxy_http_version 1.1;
proxy_set_header   Host              $host;
# 传真实来源仅供 nginx 日志与排错参考：
# **平台不采信它**（clientIP 只信直连地址），所以不要指望用它做平台侧白名单，
# 白名单要写在 nginx 层（allow/deny）。
proxy_set_header   X-Real-IP         $remote_addr;
proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
proxy_set_header   X-Forwarded-Proto $scheme;
proxy_read_timeout 120s;
proxy_send_timeout 120s;
proxy_buffering    off;
EOF
  track_file "$SNIPPET"

  # nginx < 1.25 用 `listen ... ssl http2;`，>= 1.25 用 `listen ... ssl;` + `http2 on;`
  # 写错版本分支的后果不是报错，而是 nginx -t 直接失败（1.25+ 用旧语法会告警，
  # 1.24- 用新语法会报 unknown directive）—— 所以这里必须真读版本，不能猜。
  # `| head -1` 后必须跟 `|| true`：head 读够一行就关管道，上游 grep 收到 SIGPIPE(141)，
  # 在 set -o pipefail 下整条命令的退出码变成 141，set -e 会当场终止脚本且没有任何报错。
  NGVER="$(nginx -v 2>&1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 || true)"
  HTTP2_LINE="listen $PORT ssl http2;"
  EXTRA_HTTP2=""
  case "${NGVER%%.*}" in
    1)
      MINOR="$(printf '%s' "$NGVER" | cut -d. -f2)"
      if [ "${MINOR:-0}" -ge 25 ] 2>/dev/null; then
        HTTP2_LINE="listen $PORT ssl;"
        EXTRA_HTTP2="    http2 on;"
      fi
      ;;
  esac

  BASIC_BLOCK=""
  if [ -n "$BASIC_USER" ]; then
    BASIC_BLOCK="    # Basic 二次认证（见第 7 步说明）
    auth_basic           \"shengyu-panel\";
    auth_basic_user_file $HTPASS_FILE;"
  fi

  cat >"$SITE_TARGET" <<EOF
# 管理面公网入口。由 deploy/panel-https.sh 生成，请勿手工编辑（重跑会覆盖）。
#
# 只监听 $PORT：80 要留给本机 acme 续期、443 常属既有服务（见模块头部的硬约束【一】）。
server {
    $HTTP2_LINE
$EXTRA_HTTP2
    server_name $SERVER_NAME;

    ssl_certificate     $TLS_DIR/fullchain.pem;
    ssl_certificate_key $TLS_DIR/privkey.pem;
    ssl_protocols       TLSv1.2 TLSv1.3;
    ssl_prefer_server_ciphers off;
    ssl_session_cache   shared:shengyu_panel_ssl:10m;
    ssl_session_timeout 1h;

    server_tokens off;
    client_max_body_size 4m;
    client_body_timeout  30s;

    # 安全响应头。**刻意不加 CSP**：内置管理台是内嵌 HTML + 内联脚本，
    # 一上来就上 CSP 很容易把界面弄成白屏；而"开通公网入口"与"改造前端"不该捆在一次操作里。
    add_header Strict-Transport-Security "max-age=31536000" always;
    add_header X-Content-Type-Options    "nosniff" always;
    add_header X-Frame-Options           "DENY" always;
    add_header Referrer-Policy           "no-referrer" always;

    limit_req  zone=shengyu_panel_all   burst=60 nodelay;
    limit_conn shengyu_panel_conn 24;

$BASIC_BLOCK

    # 登录接口单独用更严的限速：这条是替代"平台侧限流在反代后失效"的主要手段。
    location = /api/login {
        limit_req zone=shengyu_panel_login burst=5 nodelay;
        proxy_pass http://$UPSTREAM;
        include $SNIPPET;
    }

    location / {
        proxy_pass http://$UPSTREAM;
        include $SNIPPET;
    }
}
EOF
  track_file "$SITE_TARGET"

  # Debian 系靠 sites-enabled 生效；RHEL 系没有这套机制，则直接把文件放进 conf.d。
  if [ -d /etc/nginx/sites-enabled ]; then
    ln -sfn "$SITE_TARGET" "/etc/nginx/sites-enabled/$NGINX_SITE"
    track_file "/etc/nginx/sites-enabled/$NGINX_SITE"
    ok "已启用 /etc/nginx/sites-enabled/$NGINX_SITE"
  else
    install -m 0644 "$SITE_TARGET" "/etc/nginx/conf.d/$NGINX_SITE.conf"
    track_file "/etc/nginx/conf.d/$NGINX_SITE.conf"
    rm -f "$SITE_TARGET"
    ok "已启用 /etc/nginx/conf.d/$NGINX_SITE.conf"
  fi
fi

say "7. HTTP Basic 二次认证${BASIC_USER:+（用户 $BASIC_USER）}"
if [ -z "$BASIC_USER" ]; then
  log "未启用（未传 --basic-user）。"
  log "如果来源 IP 不固定（无法用白名单），建议启用：它会挡掉扫描器与爬虫，"
  log "使平台的登录页不对外可见，且与平台口令相互独立。"
else
  # 口令只从环境变量读 —— **不接受命令行参数**，因为参数会进 shell history 与 ps 输出。
  # 交互式输入走 read -s（不回显、不进 history）。
  PASS="${SHENGYU_PANEL_BASIC_PASS:-}"
  if [ -z "$PASS" ] && [ "$DRY" = 0 ] && [ -t 0 ]; then
    printf '  为 Basic 认证设置口令（>=12 位，不回显）：'
    if ! read -r -s PASS; then PASS=""; fi
    printf '\n  再输入一次：'
    P2=""
    if ! read -r -s P2; then P2=""; fi
    printf '\n'
    [ "$PASS" = "$P2" ] || die "两次输入不一致"
  fi
  [ -n "$PASS" ] || die "启用 --basic-user 必须提供口令：设置环境变量 SHENGYU_PANEL_BASIC_PASS，或在交互式终端下重跑本脚本。"
  [ ${#PASS} -ge 12 ] || die "Basic 口令太短（建议 >= 12 位）。"
  if [ "$DRY" = 1 ]; then
    echo "  [dry-run] 生成 $HTPASS_FILE（用户 $BASIC_USER）"
  else
    GRP="$(nginx_group)"
    if command -v htpasswd >/dev/null 2>&1; then
      htpasswd -b -c "$HTPASS_FILE" "$BASIC_USER" "$PASS" >/dev/null
    elif openssl passwd -apr1 . >/dev/null 2>&1; then
      printf '%s:%s\n' "$BASIC_USER" "$(openssl passwd -apr1 "$PASS")" >"$HTPASS_FILE"
    else
      MGR="$(detect_pkg_mgr)"
      case "$MGR" in
        apt) run env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq apache2-utils ;;
        dnf) run dnf install -y -q httpd-tools ;;
        yum) run yum install -y -q httpd-tools ;;
        *) die "既没有 htpasswd 也没有可用的包管理器，无法生成 Basic 口令文件。" ;;
      esac
      command -v htpasswd >/dev/null 2>&1 || die "安装 htpasswd 后仍不可用。"
      htpasswd -b -c "$HTPASS_FILE" "$BASIC_USER" "$PASS" >/dev/null
    fi
    chmod 0640 "$HTPASS_FILE"
    chown root:"$GRP" "$HTPASS_FILE" 2>/dev/null || true
    track_file "$HTPASS_FILE"
    ok "已写入 $HTPASS_FILE（root:$GRP 0640）"
    if [ "$GRP" = "root" ]; then
      warn "没找到 www-data/nginx 组，口令文件属组退回 root —— 若 nginx 读不到，请手工改属组。"
    fi
  fi
  unset PASS P2
fi

say "8. 安装证书续期任务（systemd timer）"
# 为什么不用 acme.sh 官方的 cron：
#   ① systemd timer 可被 systemctl 管理：状态、上次/下次执行时间、最近日志都能直接看
#      （systemctl list-timers / journalctl -u）。cron 出问题时通常只剩"它没跑"。
#   ② 本项目其余部分已经强依赖 systemd（install.sh 明确不支持 openrc），
#      再引入一套 cron 等于多一个需要单独排查的机制。
# 但**不覆盖已有 cron**：如果本机已经在用 acme 的 cron 续期（例如别的服务配的），
# 再加一个 timer 会让同一批证书被续期两次，两个进程可能互相打断。
# 那种情况下保留现状，并如实说明"续期任务已存在"。
if crontab -l 2>/dev/null | grep -q 'acme.sh.*--cron'; then
  log "检测到本机已有 acme.sh 的 cron 续期任务 —— 保留它，不新建 timer（避免同一批证书被续期两次）"
  log "查看：crontab -l | grep acme"
elif [ ! -d /etc/systemd/system ]; then
  warn "本机不是 systemd 环境，无法安装续期 timer。请手工添加续期任务。"
else
  if [ "$DRY" = 1 ]; then
    echo "  [dry-run] 写 /etc/systemd/system/$RENEW_SERVICE 与 $RENEW_TIMER 并 enable --now"
  else
    cat >"/etc/systemd/system/$RENEW_SERVICE" <<EOF
[Unit]
Description=盛愈边缘网关：ACME 证书续期（acme.sh --cron）
Documentation=https://example.invalid/shengyu/docs/08-acceptance-linux.md
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
# --cron 会遍历 acme.sh 里的所有证书，只续快到期的；续期成功后执行各自的
# --reloadcmd（本平台在部署证书时写入了 'systemctl reload nginx'），
# 所以"续期成功 → nginx 自动加载新证书"是闭环的。
ExecStart=$ACME_HOME/acme.sh --cron --home $ACME_HOME
EOF
    track_unit "$RENEW_SERVICE"

    cat >"/etc/systemd/system/$RENEW_TIMER" <<EOF
[Unit]
Description=盛愈边缘网关：ACME 证书续期定时器

[Timer]
# 每天两次：Let's Encrypt 证书 90 天有效，acme.sh 默认到期前 30 天才真正续，
# 因此每天两次足够从容地重试失败的情况。
OnCalendar=*-*-* 00,12:30:00
# 随机延迟：避免大量机器在同一秒打 ACME 目录（也避免整点与别的定时任务叠在一起）。
RandomizedDelaySec=30m
# Persistent：计划时间机器关机时，开机后补跑一次。
Persistent=true
Unit=$RENEW_SERVICE

[Install]
WantedBy=timers.target
EOF
    track_unit "$RENEW_TIMER"

    systemctl daemon-reload
    systemctl enable --now "$RENEW_TIMER" || die "启用续期定时器失败（$RENEW_TIMER）"
    ok "续期定时器已启用并启动：$RENEW_TIMER"
    systemctl list-timers "$RENEW_TIMER" --no-pager 2>/dev/null | sed 's/^/  /' || true
  fi
fi

say "9. 应用并自检"
if [ "$DRY" = 1 ]; then
  echo "  [dry-run] nginx -t && systemctl reload nginx"
else
  nginx -t || die "nginx 配置检查未通过（上面的输出指出了行号）。未 reload，现有服务不受影响。"
  systemctl enable --now nginx >/dev/null 2>&1 || true
  systemctl reload nginx || systemctl restart nginx
  ok "nginx 已 reload"

  say "9b. 自检"
  # ① 端口：只应新增 $PORT；80 必须仍空闲；443/8443 必须与开始时一致。
  if ss -lnt | grep -q ":$PORT "; then ok "监听中：:$PORT"; else warn "没有进程监听 :$PORT"; fi
  if ss -lnt | grep -q ':80 '; then
    warn "80 被占用 —— 若占用者是 nginx，说明还有别的 listen 80 配置："
    ss -lntp 2>/dev/null | grep ':80 ' | sed 's/^/     /' || true
  else
    ok "80 仍空闲"
  fi
  AFTER_PORTS="$(ss -lnt 2>/dev/null | awk '{print $4}' | grep -oE ':(443|8443)$' | sort -u | tr '\n' ' ' || true)"
  if [ "$BEFORE_PORTS" = "$AFTER_PORTS" ]; then
    ok "443/8443 监听未发生变化（${AFTER_PORTS:-无}）"
  else
    warn "443/8443 监听发生变化：开始『${BEFORE_PORTS:-无}』→ 现在『${AFTER_PORTS:-无}』。本模块不碰这两个端口，请核查。"
  fi

  # ② 经 TLS 打一次后端接口：200/401 都说明"nginx→上游"这条路通了。
  #    401 = Basic 认证要求（预期）；200 = 未启用 Basic 且上游正常。
  CODE="$(curl -sS -o /dev/null -w '%{http_code}' -k \
    --resolve "$SERVER_NAME:$PORT:127.0.0.1" "https://$SERVER_NAME:$PORT/api/health" 2>/dev/null || true)"
  case "$CODE" in
    200) ok "经面板访问 /api/health → 200（反代链路通）" ;;
    401) ok "经面板访问 /api/health → 401（Basic 认证生效，属预期）" ;;
    000 | '') warn "TLS 连接失败（无法握手）。检查：systemctl status nginx；nginx -t" ;;
    *) warn "经面板访问返回 $CODE（预期 200 或 401）" ;;
  esac

  # ③ 证书：必须与申请的域名一致，否则浏览器报"证书不匹配"而服务其实正常。
  CN="$(echo | openssl s_client -connect "127.0.0.1:$PORT" -servername "$SERVER_NAME" 2>/dev/null \
    | openssl x509 -noout -subject 2>/dev/null | sed 's/.*CN *= *//' || true)"
  if [ -n "$CN" ]; then
    case " $DOMAIN $SERVER_NAME " in
      *" $CN "*) ok "证书 CN=$CN（与访问域名匹配）" ;;
      *) warn "证书 CN=$CN 与访问域名 $SERVER_NAME 不匹配 —— 浏览器会报证书错误。" ;;
    esac
  else
    warn "无法读取已部署证书的 CN。"
  fi

  # ④ 管理面本身**必须**仍然只绑本机。这是"反代只是加了一层入口，不是把管理面搬上公网"的证据。
  if ss -lnt 2>/dev/null | grep -q '127.0.0.1:8081'; then
    ok "管理面仍只监听 127.0.0.1:8081（没有裸奔到公网）"
  else
    warn "没有看到 127.0.0.1:8081 在监听，请确认管理面地址是否被改过。"
  fi

  # ⑤ 续期任务与 nginx 的开机启动状态
  if systemctl is-enabled --quiet "$RENEW_TIMER" 2>/dev/null; then
    ok "续期定时器已设为开机启动"
  elif crontab -l 2>/dev/null | grep -q 'acme.sh.*--cron'; then
    ok "续期任务由既有 cron 承担"
  else
    warn "没有找到续期任务（timer 或 cron）—— 证书将不会自动续期。"
  fi
  if systemctl is-enabled --quiet nginx 2>/dev/null; then ok "nginx 已设为开机启动"; else warn "nginx 未设为开机启动"; fi
fi

say "10. 记录状态"
if [ "$DRY" = 1 ]; then
  echo "  [dry-run] 写 $STATE_FILE"
else
  install -d -m 0755 "$PANEL_DIR"
  cat >"$STATE_FILE" <<EOF
# 由 deploy/panel-https.sh 生成。记录本次面板开通参数，供重跑与卸载读取。
# 这里**不写任何口令/凭据** —— 凭据只存在于 acme.sh 自己的配置与服务器的内存里。
PANEL_DOMAIN=$DOMAIN
PANEL_SERVER_NAME=$SERVER_NAME
PANEL_PORT=$PORT
PANEL_CERT_MODE=$CERT_MODE
PANEL_DNS_PROVIDER=$DNS_PROVIDER
PANEL_WILDCARD=$WILDCARD
PANEL_BASIC_USER=$BASIC_USER
PANEL_ACME_EMAIL=$ACME_EMAIL
PANEL_TLS_DIR=$TLS_DIR
PANEL_INSTALLED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
EOF
  chmod 0644 "$STATE_FILE"
  ok "已记录 $STATE_FILE"
fi

say "11. 访问方式"
if [ -n "$BASIC_USER" ]; then
  cat <<EOF
  浏览器打开： https://$SERVER_NAME:$PORT/
  登录分两步（这是有意为之）：
    ① 浏览器先弹 Basic 认证框 → 输入用户名 $BASIC_USER 与你在第 7 步设置的口令
    ② 进入平台登录页 → 输入平台账号与平台口令
  两次口令相互独立，任何一次泄漏都不足以进入面板。
EOF
else
  cat <<EOF
  浏览器打开： https://$SERVER_NAME:$PORT/
  登录：平台账号（默认 admin）与平台口令。
EOF
fi
echo
if systemctl is-enabled --quiet "$RENEW_TIMER" 2>/dev/null; then
  echo "  证书续期：由 systemd timer 自动完成（续期成功后会 reload nginx 加载新证书）"
  echo "  查看下次续期： systemctl list-timers $RENEW_TIMER"
else
  echo "  证书续期：由本机既有的 acme cron 承担（查看：crontab -l | grep acme）"
fi
echo "  手动试续期：   $ACME_HOME/acme.sh --cron"
echo "  回退面板：     sudo bash deploy/panel-https.sh --disable"
echo

if [ "$DRY" = 1 ]; then
  printf '（dry-run 结束，未做任何修改）\n'
fi
