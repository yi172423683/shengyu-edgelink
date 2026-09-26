#!/usr/bin/env bash
set -Eeuo pipefail

DATA_ROOT=/var/lib/shengyu-edgelink
CONF_ROOT=/etc/shengyu-edgelink/haproxy
BIN=/usr/local/bin/shengyu-edgelink
SERVER=shengyu-edgelink-server
HAPROXY=shengyu-edgelink-haproxy

if [ "$(id -u)" -ne 0 ]; then
  echo "请使用 root 执行：sudo edgelink" >&2
  exit 1
fi

pause() {
  printf '\n按回车返回菜单...'
  read -r _
}

status() {
  echo
  echo "服务状态："
  systemctl --no-pager --full status "$SERVER" "$HAPROXY" || true
  echo
  echo "监听端口："
  ss -lntp 2>/dev/null | grep -E ':(8081|8443|9443|443)\b' || true
}

reset_admin() {
  local db backup pass pass2 log
  db="$DATA_ROOT/meta.db"
  if [ ! -f "$db" ]; then echo "找不到元数据库：$db"; return; fi
  if [ ! -x "$BIN" ]; then echo "找不到服务二进制：$BIN"; return; fi
  if ! command -v sqlite3 >/dev/null 2>&1; then
    echo "系统缺少 sqlite3，正在安装..."
    apt-get update
    apt-get install -y sqlite3
  fi
  printf '请输入新的 admin 密码（至少 12 位，不回显）：'
  read -r -s pass
  printf '\n再次输入：'
  read -r -s pass2
  printf '\n'
  if [ "\${#pass}" -lt 12 ]; then echo "密码至少需要 12 位。"; return; fi
  if [ "$pass" != "$pass2" ]; then echo "两次密码不一致。"; return; fi

  backup="$db.before-password-reset.$(date +%Y%m%d%H%M%S)"
  systemctl stop "$SERVER"
  cp -a "$db" "$backup"
  if ! sqlite3 "$db" <<'SQL'
PRAGMA foreign_keys=ON;
DELETE FROM sessions;
DELETE FROM users WHERE username='admin';
SQL
  then
    echo "清理旧管理员失败，正在恢复数据库。"
    cp -a "$backup" "$db"
    systemctl start "$SERVER" || true
    return
  fi

  log="/tmp/shengyu-edgelink-reset-admin.log"
  if ! HOME="$DATA_ROOT" runuser -u shengyu -- "$BIN" \
      -data "$DATA_ROOT" -config-root "$CONF_ROOT" -dataplane haproxy \
      -admin-user admin -admin-pass "$pass" -check >"$log" 2>&1
  then
    echo "新管理员创建失败，正在恢复数据库。"
    cat "$log"
    cp -a "$backup" "$db"
    systemctl start "$SERVER" || true
    return
  fi

  unset pass pass2
  systemctl start "$SERVER"
  echo "管理员密码已重置，旧会话已全部吊销。"
  echo "账号：admin"
  echo "数据库备份：$backup"
}

show_settings() {
  echo
  echo "版本："
  "$BIN" -version 2>/dev/null || true
  echo
  echo "管理服务："
  systemctl show "$SERVER" -p ActiveState -p ExecStart -p FragmentPath --no-pager
  echo
  echo "公网面板配置："
  if [ -f /etc/shengyu-panel/panel.env ]; then
    sed -E 's/(KEY|SECRET|TOKEN|PASS)=.*/\1=***已隐藏***/' /etc/shengyu-panel/panel.env
  else
    echo "未配置公网面板"
  fi
}

while true; do
  clear 2>/dev/null || true
  cat <<'EOF'
╔════════════════════════════════════════════════════╗
║        盛愈边缘网关管理菜单                        ║
║  0. 退出                                           ║
║────────────────────────────────────────────────────║
║  1. 查看服务状态                                   ║
║  2. 启动服务                                       ║
║  3. 停止管理服务                                   ║
║  4. 重启管理服务                                   ║
║  5. 重启 HAProxy                                   ║
║  6. 查看管理服务日志                               ║
║  7. 查看 HAProxy 日志                              ║
║  8. 重置管理员密码                                 ║
║  9. 查看当前设置                                   ║
╚════════════════════════════════════════════════════╝
EOF
  printf '请输入选项 [0-9]: '
  read -r choice
  case "$choice" in
    0) exit 0 ;;
    1) status; pause ;;
    2) systemctl start "$HAPROXY" "$SERVER"; pause ;;
    3) systemctl stop "$SERVER"; pause ;;
    4) systemctl restart "$SERVER"; pause ;;
    5) echo "HAProxy 重启会中断现有连接，日常配置请在网页中发布."; read -r -p "仍然继续？[y/N] " yn; case "$yn" in y|Y) systemctl restart "$HAPROXY" ;; esac; pause ;;
    6) journalctl -u "$SERVER" -n 100 --no-pager; pause ;;
    7) journalctl -u "$HAPROXY" -n 100 --no-pager; pause ;;
    8) reset_admin; pause ;;
    9) show_settings; pause ;;
    *) echo "无效选项"; sleep 1 ;;
  esac
done
