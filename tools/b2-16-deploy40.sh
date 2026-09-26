#!/usr/bin/env bash
# B2 第六轮 - 第 0 步：部署 v0.4.0（只换管理面二进制）—— 这是**正式版本**，不再是补丁号
#
# v0.4.0 相对 v0.3.7 的内容（两项排期任务 + 一个真缺陷修复）：
#   A) 新增 DELETE /api/nodes/{id}/sni-entries/{eid}：有业务路由引用则拒绝，
#      无引用则允许，删后提示重新发布并写审计（这是"验收只能改库"的根因修复）；
#   B) /api/diagnose 增加**悬空引用**自检；
#   C) 发布成功后清理"已确认无进程使用"的旧 stats-v*.sock（失败只告警）；
#   D) 修复真缺陷：日志分片并发首次写入时并发执行 DDL ⇒ 整批日志被丢弃
#      （实测并发写 24 行只落 16 行；e2e 在 -race 下丢一半会话日志）；
#      同时把"日志被丢弃"计数暴露到 /api/overview，让静默失败可见。
set -u
BIN=/tmp/shengyu-edgelink-v0.4.0
EXPECT_SHA=ef5c64517c7219b55db8025712cf55b793e2b0e624381f0498e1096089d8b406

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
echo "DEPLOY40_DONE"
