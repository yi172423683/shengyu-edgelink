#!/usr/bin/env bash
# B2 第五轮 - 第 0 步：部署 v0.3.7（只换管理面二进制）
#
# v0.3.7 相对 v0.3.6 的四项改动（清单外文件的处理边界，评审确认）：
#   A) 版本目录里"清单之外的普通文件" **不再阻断**发布/回滚，改为 warning + 审计 + 页面提示；
#   B) 但"配置**实际引用**了清单外文件" **必须拒绝**（那种文件运行时会被按绝对路径读到）；
#   C) 清单列出的文件缺失仍**必须拒绝**（且不得进入 verified）；
#   D) current/ 仍然只包含清单允许的文件（多余文件绝不进入）。
set -u
BIN=/tmp/shengyu-edgelink-v0.3.7
EXPECT_SHA=dc878c52889a51ab114d02ac51a765395900ed322f146831b26664962594da4d

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
echo "DEPLOY37_DONE"
