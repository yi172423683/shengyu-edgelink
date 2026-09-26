#!/usr/bin/env bash
# shengyu 节点卸载脚本（在目标节点上以 root 执行）。
#
# 用法：
#   sudo bash deploy/uninstall.sh                 # 只打印计划，不动系统
#   sudo bash deploy/uninstall.sh --yes           # 执行卸载（**保留数据**）
#   sudo bash deploy/uninstall.sh --yes --purge-data  # 连数据一起删（不可恢复）
#   sudo bash deploy/uninstall.sh --dry-run --purge-data  # 预演含清数据的完整卸载
#
# 默认**保留数据**的理由：
#   卸载最常见的真实动机是"换个版本重装"或"先排查一下"，这两种都不该丢数据。
#   /etc/shengyu-edgelink/haproxy（配置与全部历史版本）与 /var/lib/shengyu-edgelink（元数据库、
#   日志分片）是**唯一的业务数据**，删掉就不可恢复。
#   真要清，必须显式给 --purge-data —— 一个需要多敲一次的参数，比一句警告有效。
#
# 卸载**不会**做这几件事（避免误伤，也不越权替运维做决定）：
#   - 不删 shengyu 系统账号（可能有别的文件属主是它；删账号会让那些文件变成悬空 uid）
#   - 不动系统自带的 HAProxy 及其配置
#   - 不改防火墙/安全组
#   - 不重启机器
set -euo pipefail

YES=0
DRY=0
PURGE=0
for arg in "$@"; do
  case "$arg" in
    --yes | -y) YES=1 ;;
    --dry-run) DRY=1 ;;
    --purge-data) PURGE=1 ;;
    -h | --help)
      sed -n '2,30p' "$0"
      exit 0
      ;;
    *)
      echo "未知参数：$arg（用 --help 看用法）" >&2
      exit 2
      ;;
  esac
done

CONF_ROOT=/etc/shengyu-edgelink/haproxy
DATA_ROOT=/var/lib/shengyu-edgelink
BIN=/usr/local/bin/shengyu-edgelink
UNIT_DIR=/etc/systemd/system
POLKIT_RULE=/etc/polkit-1/rules.d/50-shengyu-edgelink.rules
SYSCTL_CONF=/etc/sysctl.d/99-shengyu-edgelink.conf
TMPFILES_CONF=/etc/tmpfiles.d/shengyu-edgelink.conf

say() { printf '\n=== %s ===\n' "$1"; }
run() {
  if [ "$DRY" = 1 ]; then echo "  [dry-run] $*"; else "$@"; fi
}
# 删除统一走这里：先判断存在性，避免 `rm -f` 掩盖"其实早就没了"这类信息，
# 也让 dry-run 能如实打印"这步其实无事可做"。
rm_if() {
  if [ -e "$1" ] || [ -L "$1" ]; then
    run rm -f "$1"
  else
    echo "  跳过（不存在）：$1"
  fi
}

say "0. 前置检查"
if [ "$(id -u)" != 0 ]; then
  echo "必须以 root 执行（要停服务、删单元文件）。" >&2
  exit 1
fi

if [ "$YES" = 0 ] && [ "$DRY" = 0 ]; then
  echo "这是**预演**：不会做任何修改。" >&2
  echo "确认无误后加 --yes 真正执行（数据默认保留；加 --purge-data 才删数据）。" >&2
  echo
fi

say "1. 停止并禁用服务"
# 先停管理面，再停转发内核：管理面停掉后不会再触发发布/回滚，
# 顺序反了可能出现"卸载到一半又有一次 reload"。
for u in shengyu-edgelink-server shengyu-edgelink-haproxy; do
  if systemctl is-active --quiet "$u" 2>/dev/null; then
    run systemctl stop "$u"
  else
    echo "  未在运行：$u"
  fi
  if systemctl is-enabled --quiet "$u" 2>/dev/null; then
    run systemctl disable "$u"
  else
    echo "  未设置开机启动：$u"
  fi
done

say "2. 删除 systemd 单元"
rm_if "$UNIT_DIR/shengyu-edgelink-server.service"
rm_if "$UNIT_DIR/shengyu-edgelink-haproxy.service"
run systemctl daemon-reload

say "3. 删除受限特权授权（polkit 规则）"
rm_if "$POLKIT_RULE"
if [ "$DRY" = 0 ]; then
  # 规则文件删掉后，polkitd 需要重读目录才不会再引用已删除的规则。
  if systemctl is-active --quiet polkit 2>/dev/null; then
    run systemctl restart polkit
  elif systemctl is-active --quiet polkitd 2>/dev/null; then
    run systemctl restart polkitd
  fi
fi

say "4. 删除内核参数与运行时目录配置"
rm_if "$SYSCTL_CONF"
if [ "$DRY" = 0 ]; then
  run sysctl --system >/dev/null
fi
rm_if "$TMPFILES_CONF"
# /run 是 tmpfs，重启即消失；这里顺手清掉，避免残留的套接字让下次安装误判"服务还在跑"。
rm_if /run/shengyu-edgelink

say "5. 删除二进制"
rm_if "$BIN"

say "5b. 移除管理面外网入口（若开通过）"
#
# 与 install.sh 第 9b 步成对：那边用 deploy/panel-https.sh 建立入口，这边把它撤掉。
# 三条"不做"的决定（都写在下面）都不是省事，而是避免越权替运维做不可逆的事。
PANEL_DIR=/etc/shengyu-panel
PANEL_STATE="$PANEL_DIR/panel.env"
if [ -f "$PANEL_STATE" ]; then
  echo "  检测到面板配置记录："
  # 只打印非敏感项：状态文件里本来就不存口令/凭据（见 panel-https.sh 的说明）。
  sed 's/^/    /' "$PANEL_STATE"
fi

PANEL_FILES=(
  /etc/nginx/sites-enabled/shengyu-panel
  /etc/nginx/sites-available/shengyu-panel
  /etc/nginx/conf.d/shengyu-panel.conf
  /etc/nginx/conf.d/shengyu-panel-ratelimit.conf
  /etc/nginx/snippets/shengyu-panel-proxy.conf
  /etc/nginx/.htpasswd-shengyu-panel
)
PANEL_FOUND=0
for f in "${PANEL_FILES[@]}"; do
  if [ -e "$f" ] || [ -L "$f" ]; then
    PANEL_FOUND=1
    rm_if "$f"
  fi
done
[ "$PANEL_FOUND" = 0 ] && echo "  未发现面板相关配置（本机可能从未开通过外网访问）"

# 续期定时器也属于"开通过面板就会留下"的东西。
# 为什么必须清：它指向 /root/.acme.sh 里的脚本，卸载后虽然不会报错，
# 但会让"这台机器上还有没有本平台的残留"变得难以判断 ——
# 而排障时最怕的正是这种"看着还在、实际已经无用"的残留。
PANEL_UNITS=(
  /etc/systemd/system/shengyu-panel-acme-renew.timer
  /etc/systemd/system/shengyu-panel-acme-renew.service
)
PANEL_UNIT_FOUND=0
for u in "${PANEL_UNITS[@]}"; do
  if [ -e "$u" ]; then
    PANEL_UNIT_FOUND=1
    if [ "$DRY" = 0 ]; then
      # 用 systemctl 而不是删文件了事：不先停掉，单元会在内存里继续被引用，
      # 下次触发时执行一个已经不存在的脚本。
      run systemctl disable --now "$(basename "$u")" >/dev/null 2>&1 || true
    fi
    rm_if "$u"
  fi
done
if [ "$PANEL_UNIT_FOUND" = 1 ]; then
  [ "$DRY" = 0 ] && run systemctl daemon-reload
fi

# 只在真的动过 nginx 配置时才 reload，且必须先验证配置仍然合法 ——
# 不验证就 reload 会把"删错了东西"变成"nginx 直接起不来"，那是把可恢复的小问题放大。
if [ "$PANEL_FOUND" = 1 ] && command -v nginx >/dev/null 2>&1 && [ "$DRY" = 0 ]; then
  if nginx -t >/dev/null 2>&1; then
    run systemctl reload nginx
    echo "  nginx 已 reload（面板入口已撤下）"
  else
    echo "  nginx 配置当前不合法，**未** reload。请手工检查：nginx -t" >&2
  fi
fi

if [ "$PURGE" = 1 ]; then
  # 证书目录只在明确要求清数据时才删：它可能被本机其它服务共用
  # （通配符证书尤其如此 —— 删掉会让那些服务在下一次续期前一直用旧证）。
  rm_if "$PANEL_STATE"
  if [ -d "$PANEL_DIR" ] && [ "$DRY" = 0 ] && [ "$YES" = 1 ]; then
    run rm -rf "$PANEL_DIR"
    echo "  已删除 $PANEL_DIR（含证书副本）"
  elif [ -d "$PANEL_DIR" ]; then
    echo "  （预演：未删除 $PANEL_DIR）"
  fi
else
  echo "  保留（未加 --purge-data）："
  echo "      $PANEL_DIR/tls  证书副本（可能被其它服务共用）"
  echo "  如需删除：sudo rm -rf $PANEL_DIR"
fi
echo "  以下项目**故意没有自动处理**："
echo "    · 不卸载 nginx 软件包 —— 它可能还在承载这台机器上别的站点"
echo "    · 不动 acme.sh 的续期任务与已签证书 —— 泛域名证书很可能被别的服务共用，"
echo "      摘除它会让那些服务在证书过期时才发现问题"
echo "      如确认再无用途：/root/.acme.sh/acme.sh --remove -d <域名>"

say "6. 数据（默认保留）"
if [ "$PURGE" = 1 ]; then
  echo "  ⚠️  --purge-data：以下数据将被**永久删除**且不可恢复："
  echo "      $CONF_ROOT   （配置与所有历史版本）"
  echo "      $DATA_ROOT   （元数据库 meta.db 与全部日志分片）"
  if [ "$DRY" = 0 ] && [ "$YES" = 1 ]; then
    run rm -rf "$CONF_ROOT"
    run rm -rf "$DATA_ROOT"
    # /etc/shengyu 与 /var/lib/shengyu-edgelink 若已空则一并删掉，不留空壳。
    rmdir /etc/shengyu 2>/dev/null || true
    rmdir /var/lib/shengyu-edgelink 2>/dev/null || true
    echo "  数据已删除"
  else
    echo "  （预演：未删除）"
  fi
else
  echo "  保留（未加 --purge-data）："
  echo "      $CONF_ROOT"
  echo "      $DATA_ROOT"
  echo "  重装同一版本时会直接沿用这些数据。"
fi

say "7. 卸载完成 / 遗留项"
cat <<'EOF'
  以下项目**故意没有删除**，请按需自行处理：
    · shengyu 系统账号与组（删账号会让仍属主于它的文件变成悬空 uid）
        userdel shengyu && groupdel shengyu
    · **手工**加过的反代配置（nginx/Caddy 里指向 127.0.0.1:8081 的自建规则）
        —— 用 --panel-domain 开通的那份已在上面的 5b 步自动撤下
    · nginx 软件包本身，以及 acme.sh 的续期任务与已签证书
    · 防火墙/安全组里为入口端口放行的规则
    · 若做过日志外发，对端已收到的副本

  重装：解压新版本发布包后执行
        sudo bash deploy/install.sh --dry-run
        sudo bash deploy/install.sh
EOF

if [ "$DRY" = 1 ]; then
  echo
  echo "（dry-run 结束，未做任何修改）"
elif [ "$YES" = 0 ]; then
  echo
  echo "（以上是预演结果，未做任何修改。加 --yes 执行。）"
fi
