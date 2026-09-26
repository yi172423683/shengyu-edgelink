#!/usr/bin/env bash
# B2 重跑 - 第 1 步：把 v0.3.4 部署到测试机（只换管理面二进制，不动 HAProxy）
#
# 前置：b2-run.ps1 已把二进制上传到 /tmp/shengyu-edgelink-v0.3.4
set -u
BIN=/tmp/shengyu-edgelink-v0.3.4
EXPECT_SHA=82338df51320088630c1f3ec3ba82a9bb5890f7cfaab3271fd404ab4b2df0aa2

echo "############ 0. 校验上传的二进制 ############"
if [ ! -f "$BIN" ]; then echo "MISSING_BINARY"; exit 2; fi
GOT=$(sha256sum "$BIN" | cut -d' ' -f1)
echo "incoming_sha256 = $GOT"
echo "expected_sha256 = $EXPECT_SHA"
if [ "$GOT" != "$EXPECT_SHA" ]; then echo "SHA_MISMATCH —— 拒绝部署"; exit 3; fi
echo "本地构建产物与上传件一致（sha256 相同）"

echo
echo "############ 1. 替换前的现场 ############"
echo "installed_bin_before=$(sha256sum /usr/local/bin/shengyu-edgelink | cut -d' ' -f1)"
/usr/local/bin/shengyu-edgelink -version 2>&1 | head -2
PID_BEFORE=$(systemctl show -p MainPID --value shengyu-edgelink-haproxy)
echo "haproxy_master_pid_before=$PID_BEFORE"

BK=$(cat /tmp/b2/backup_dir 2>/dev/null || echo "")
if [ -n "$BK" ] && [ ! -f "$BK/shengyu-edgelink.installed.bin.v0.3.4pre" ]; then
  cp -a /usr/local/bin/shengyu-edgelink "$BK/shengyu-edgelink.installed.bin.v0.3.4pre"
  echo "backed up pre-deploy binary to $BK"
fi

echo
echo "############ 2. 原子替换 ############"
install -m 0755 "$BIN" /usr/local/bin/.shengyu-edgelink.new
mv -f /usr/local/bin/.shengyu-edgelink.new /usr/local/bin/shengyu-edgelink
ls -l /usr/local/bin/shengyu-edgelink
/usr/local/bin/shengyu-edgelink -version 2>&1 | head -2

echo
echo "############ 3. 只重启管理面 ############"
systemctl restart shengyu-edgelink-server
sleep 2
systemctl is-active shengyu-edgelink-server shengyu-edgelink-haproxy
PID_AFTER=$(systemctl show -p MainPID --value shengyu-edgelink-haproxy)
echo "haproxy_master_pid_after=$PID_AFTER"
if [ "$PID_BEFORE" = "$PID_AFTER" ]; then
  echo "OK: haproxy master PID 未变（管理面重启不影响转发进程）"
else
  echo "WARN: haproxy master PID 变了（$PID_BEFORE -> $PID_AFTER）"
fi

echo
echo "############ 4. 健康检查 ############"
curl -s -m 5 http://127.0.0.1:8081/api/health; echo
curl -s -m 5 -o /dev/null -w 'meta_http=%{http_code} (401=需登录，正常)\n' http://127.0.0.1:8081/api/meta
curl -s -m 5 -o /dev/null -w 'page_http=%{http_code} size=%{size_download}\n' http://127.0.0.1:8081/

echo
echo "############ 5. 8443 正常业务仍可握手 ############"
timeout 8 openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com -brief </dev/null 2>&1 | head -2

echo
echo "############ 6. 平滑 reload 仍可用（此时还没有注入器）############"
systemctl reload shengyu-edgelink-haproxy; echo "reload_exit=$?"
CR=/etc/shengyu-edgelink/haproxy
echo "VERSION=$(cat $CR/current/VERSION)"
echo "current_files=$(ls $CR/current | tr '\n' ',')"
echo "haproxy -c: $(haproxy -c -f $CR/current/haproxy.cfg 2>&1 | tail -1)"

echo
echo "############ 7. 新二进制里不得有测试用数据面符号 ############"
nm -C /usr/local/bin/shengyu-edgelink 2>/dev/null | grep -ci testplane || echo "0"
echo "（上行为 testplane 符号命中数，必须是 0；用 -trimpath -s -w 构建，符号可能已被裁剪）"
strings -a /usr/local/bin/shengyu-edgelink 2>/dev/null | grep -ci 'gorelay\|testplane' || echo "0"
echo "DEPLOY_DONE"
