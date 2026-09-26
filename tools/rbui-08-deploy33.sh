#!/usr/bin/env bash
# 回滚 UI 验收 - 第 8 步：部署 v0.3.3 并复验「当前运行版本」口径
set -u
W=/tmp/rbui
API=http://127.0.0.1:8081
NODE=node_06GCGE2NPCC624J5AMKYQWC4C0
CUS=cus_06GCGEQ3G5HH6SGK9Q11AHH9FR
SD=/run/shengyu-edgelink

echo "##### 1. deploy v0.3.3 #####"
sha256sum /tmp/shengyu-edgelink-v0.3.3
install -m 0755 /tmp/shengyu-edgelink-v0.3.3 /usr/local/bin/.shengyu-edgelink.new
mv -f /usr/local/bin/.shengyu-edgelink.new /usr/local/bin/shengyu-edgelink
/usr/local/bin/shengyu-edgelink -version
OLDPID=$(systemctl show -p MainPID --value shengyu-edgelink-haproxy)
systemctl restart shengyu-edgelink-server
sleep 2
systemctl is-active shengyu-edgelink-server shengyu-edgelink-haproxy
NEWPID=$(systemctl show -p MainPID --value shengyu-edgelink-haproxy)
echo "haproxy MainPID before/after = $OLDPID / $NEWPID"

PW=$(grep -oE '[A-Za-z0-9]{32}' /root/shengyu-install.log | head -1)
curl -s -c "$W/ck" -o "$W/login.json" -X POST "$API/api/login" \
  -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$PW\"}"
CSRF=$(sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p' "$W/login.json")
echo "$CSRF" > "$W/csrf"; echo "csrf_len=${#CSRF}"

echo "##### 2. re-run scenario 6C (silent reload) on v0.3.3 #####"
printf 'silent\n' > "$SD/rbui-reload-mode"
curl -s -b "$W/ck" -o /dev/null -w 'biz_put_http=%{http_code}\n' \
  -X PUT -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/businesses/$(cat $W/biz_id)" -d "{
    \"customer_id\":\"$CUS\",\"name\":\"RBUI-rollback-test-9443\",
    \"remark\":\"RBUI scenario 6C rerun on v0.3.3\",\"mode\":\"sni_tls\",
    \"domains\":[\"rbui-test.example.com\"],\"primary_node_id\":\"$NODE\",
    \"sni_entry_id\":\"$(cat $W/sni9443_id)\",
    \"origin_host\":\"1.1.1.1\",\"origin_port\":450}"

echo "--- NOTE: 发布期间 current/VERSION 会短暂变成新版号，心跳(20s)会把那个号写进节点表 ---"
date -u +"publish start %H:%M:%S"
curl -s -b "$W/ck" -o "$W/p6c2.json" -w 'publish_http=%{http_code}\n' \
  -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/$NODE/publish" -d '{"note":"RBUI 6C rerun: silent reload, on v0.3.3"}'
date -u +"publish end   %H:%M:%S"

python3 - <<'PY'
import json
d=json.load(open('/tmp/rbui/p6c2.json'))
print("phase =",d.get("phase")," rolled_back =",d.get("rolled_back"),
      " no_rollback_needed =",d.get("no_rollback_needed"))
print("note  =",d.get("rollback_note"))
print("manual_fix lines =",len(d.get("manual_fix") or []))
PY

echo "##### 3. IMMEDIATELY read meta (still inside the 20s heartbeat window) #####"
date -u +"meta query    %H:%M:%S"
curl -s -b "$W/ck" "$API/api/meta" -o "$W/m4.json"
python3 - <<'PY'
import json
n=json.load(open('/tmp/rbui/m4.json'))['node']
print("applied_version  =", n['applied_version'])
print("expected_version =", n['expected_version'])
print("version_skewed   =", n['version_skewed'])
PY
echo -n "disk current/VERSION = "; cat /etc/shengyu-edgelink/haproxy/current/VERSION
echo "(v0.3.3 起 applied_version 以数据面为准；旧版这里会显示回滚前那个更大的号)"

echo "##### 4. 恢复注入器 #####"
printf 'off\n' > "$SD/rbui-reload-mode"
systemctl reload shengyu-edgelink-haproxy; echo "reload_exit=$?"
echo "RECHECK33_DONE"
