#!/usr/bin/env bash
# B2 第二轮 - 第 0 步：部署 v0.3.6（只换管理面二进制）
#
# v0.3.6 相对 v0.3.4 的两处修复：
#   A) 验证口径与渲染器对齐：空 SNI 入口（没有任何启用路由）不再被要求监听
#      —— 真机 v11 那次"0.0.0.0:9443 未监听（等了 30.1 秒、重试 61 次）"就是它。
#   B) 回滚失败时把"current/ 停在未获批准版本上"写进说明与人工命令第一条。
set -u
BIN=/tmp/shengyu-edgelink-v0.3.6
EXPECT_SHA=0ad467ea248571513c7fd012bac7238526b2ab9e7f4d1add69770f5fb88a9e23

echo "############ 0. 校验上传的二进制 ############"
[ -f "$BIN" ] || { echo "MISSING_BINARY"; exit 2; }
GOT=$(sha256sum "$BIN" | cut -d' ' -f1)
echo "incoming_sha256 = $GOT"
echo "expected_sha256 = $EXPECT_SHA"
[ "$GOT" = "$EXPECT_SHA" ] || { echo "SHA_MISMATCH —— 拒绝部署"; exit 3; }
echo "OK: 与本地构建产物一致"

echo
echo "############ 1. 替换前 ############"
echo "installed_before=$(sha256sum /usr/local/bin/shengyu-edgelink | cut -d' ' -f1)"
/usr/local/bin/shengyu-edgelink -version 2>&1 | head -1
PID_BEFORE=$(systemctl show -p MainPID --value shengyu-edgelink-haproxy)
echo "haproxy_master_pid_before=$PID_BEFORE"
CR=/etc/shengyu-edgelink/haproxy
echo "current/VERSION=$(cat $CR/current/VERSION)"
echo "current_files=$(ls $CR/current | tr '\n' ',')"

echo
echo "############ 2. 原子替换 + 只重启管理面 ############"
install -m 0755 "$BIN" /usr/local/bin/.shengyu-edgelink.new
mv -f /usr/local/bin/.shengyu-edgelink.new /usr/local/bin/shengyu-edgelink
ls -l /usr/local/bin/shengyu-edgelink
/usr/local/bin/shengyu-edgelink -version 2>&1 | head -1
systemctl restart shengyu-edgelink-server
sleep 2
systemctl is-active shengyu-edgelink-server shengyu-edgelink-haproxy
PID_AFTER=$(systemctl show -p MainPID --value shengyu-edgelink-haproxy)
[ "$PID_BEFORE" = "$PID_AFTER" ] && echo "OK: haproxy master PID 未变" || echo "WARN: haproxy master PID 变了（$PID_BEFORE -> $PID_AFTER）"

echo
echo "############ 3. 健康检查 ############"
curl -s -m 5 http://127.0.0.1:8081/api/health; echo
curl -s -m 5 -o /dev/null -w 'page_http=%{http_code} size=%{size_download}\n' http://127.0.0.1:8081/
echo -n "8443 handshake = "
timeout 8 openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com -brief </dev/null 2>&1 | head -1
echo "haproxy -c = $(haproxy -c -f $CR/current/haproxy.cfg 2>&1 | tail -1)"
echo "testplane 符号命中（strings）= $(strings -a /usr/local/bin/shengyu-edgelink | grep -ci 'gorelay\|testplane' || echo 0)"
echo "DEPLOY36_DONE"
