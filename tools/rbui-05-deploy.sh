#!/usr/bin/env bash
# 回滚 UI 验收 - 第 5 步：把修复后的 v0.3.2 部署到测试机（只换管理面二进制）
set -u
BIN=/tmp/shengyu-edgelink-v0.3.2

echo "### 0. incoming binary hash"
sha256sum "$BIN"

echo "### 1. old binary hash (backup copy) for comparison"
BK=$(ls -d /root/rollback-ui-accept-* | head -1)
echo "backup dir: $BK"
sha256sum "$BK/shengyu-edgelink.v0.3.1.bin" /usr/local/bin/shengyu-edgelink

echo "### 2. atomic replace (write temp in same dir, then mv)"
install -m 0755 "$BIN" /usr/local/bin/.shengyu-edgelink.new
mv -f /usr/local/bin/.shengyu-edgelink.new /usr/local/bin/shengyu-edgelink
ls -l /usr/local/bin/shengyu-edgelink
/usr/local/bin/shengyu-edgelink -version

echo "### 3. restart management plane only (HAProxy untouched)"
OLDPID=$(systemctl show -p MainPID --value shengyu-edgelink-haproxy)
echo "haproxy MainPID before = $OLDPID"
systemctl restart shengyu-edgelink-server
sleep 2
systemctl is-active shengyu-edgelink-server shengyu-edgelink-haproxy
NEWPID=$(systemctl show -p MainPID --value shengyu-edgelink-haproxy)
echo "haproxy MainPID after  = $NEWPID   (must equal before: 转发进程不该被动过)"
[ "$OLDPID" = "$NEWPID" ] && echo "OK: haproxy master PID 未变（管理面重启不影响转发）" || echo "WARN: haproxy master PID 变了"

echo "### 4. health + version"
curl -s -m 5 http://127.0.0.1:8081/api/health; echo
curl -s -m 5 -o /dev/null -w 'meta_http=%{http_code} (401=需登录，正常)\n' http://127.0.0.1:8081/api/meta

echo "### 5. 8443 still forwarding"
timeout 8 openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com -brief </dev/null 2>&1 | head -3

echo "### 6. reload still works (injector mode file should be 'once', no flag armed)"
cat /run/shengyu-edgelink/rbui-reload-mode 2>/dev/null || echo "(no mode file)"
ls /run/shengyu-edgelink/rbui-reload-fail-once 2>&1
systemctl reload shengyu-edgelink-haproxy; echo "reload_exit=$?"
echo "VERSION=$(cat /etc/shengyu-edgelink/haproxy/current/VERSION)"
echo "DEPLOY_DONE"
