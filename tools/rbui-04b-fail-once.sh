#!/usr/bin/env bash
# 回滚 UI 验收 - 第 4B 步：只让 reload 失败一次（发布失败，但回滚能成功）
set -u
W=/tmp/rbui
API=http://127.0.0.1:8081
NODE=node_06GCGE2NPCC624J5AMKYQWC4C0
CUS=cus_06GCGEQ3G5HH6SGK9Q11AHH9FR
CSRF=$(cat "$W/csrf")
BIZ=$(cat "$W/biz_id")
S9443=$(cat "$W/sni9443_id")
D=/etc/systemd/system/shengyu-edgelink-haproxy.service.d

echo "===== 4B.0 install fail-once wrapper ====="
cat > /usr/local/bin/rbui-reload-inject.sh <<'EOF'
#!/bin/sh
# [RBUI TEST ONLY] reload 注入器：第一次调用失败，之后恢复真实 reload。
# 目的是造出「发布失败 -> 自动回滚 -> 回滚成功」这条路径。
FLAG=/run/rbui-reload-fail-once
if [ -e "$FLAG" ]; then
  rm -f "$FLAG"
  echo "RBUI injected reload failure (fail-once consumed)" >&2
  exit 1
fi
exec /bin/kill -USR2 "$(cat /run/shengyu-edgelink/haproxy.pid)"
EOF
chmod 0755 /usr/local/bin/rbui-reload-inject.sh

cat > "$D/99-rbui-test.conf" <<'EOF'
# [RBUI TEST ONLY] 临时覆盖：reload 改为走注入器（首次失败，后续正常）。
[Service]
ExecReload=
ExecReload=/usr/local/bin/rbui-reload-inject.sh
EOF
systemctl daemon-reload
echo "--- effective ExecReload ---"
systemctl show -p ExecReload --value shengyu-edgelink-haproxy

echo "===== 4B.1 arm the one-shot failure ====="
touch /run/rbui-reload-fail-once
ls -la /run/rbui-reload-fail-once

echo "===== 4B.2 snapshot BEFORE ====="
echo "VERSION=$(cat /etc/shengyu-edgelink/haproxy/current/VERSION)"
sha256sum /etc/shengyu-edgelink/haproxy/current/haproxy.cfg
ps -o pid=,args= -C haproxy
ls /run/shengyu-edgelink/

echo "===== 4B.3 modify test business again (origin_port 444 -> 445) ====="
curl -s -b "$W/ck" -o /dev/null -w 'http=%{http_code}\n' \
  -X PUT -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/businesses/$BIZ" -d "{
    \"customer_id\":\"$CUS\",
    \"name\":\"RBUI-rollback-test-9443\",
    \"remark\":\"rollback UI acceptance temporary business (scenario B)\",
    \"mode\":\"sni_tls\",
    \"domains\":[\"rbui-test.example.com\"],
    \"primary_node_id\":\"$NODE\",
    \"sni_entry_id\":\"$S9443\",
    \"origin_host\":\"1.1.1.1\",
    \"origin_port\":445
  }"

echo "===== 4B.4 publish (expected: FAIL at reload, then rollback SUCCEEDS) ====="
curl -s -b "$W/ck" -o "$W/pubB.json" -w 'publish_http=%{http_code}\n' \
  -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/$NODE/publish" -d '{"note":"RBUI scenario B: reload fails once"}'
python3 - <<'PY'
import json
d=json.load(open('/tmp/rbui/pubB.json'))
print("release_id   =", d.get("release_id"))
print("version      =", d.get("version"), " prev =", d.get("prev_version"))
print("status       =", d.get("status"))
print("phase        =", d.get("phase"))
print("rolled_back  =", d.get("rolled_back"))
print("rollback_note=", d.get("rollback_note"))
print("manual_fix   =", d.get("manual_fix"))
print("message      =", d.get("message"))
PY
echo "-- flag consumed? --"
ls -la /run/rbui-reload-fail-once 2>&1; echo "flag_gone_exit=$?"

echo "===== 4B.5 post-failure invariants ====="
echo "VERSION=$(cat /etc/shengyu-edgelink/haproxy/current/VERSION)"
sha256sum /etc/shengyu-edgelink/haproxy/current/haproxy.cfg
echo "expected sha (v3) = c5937970d8b1ea7f0a19b475dd1e0b432b1b2f8b5a512a41df68e17df83d53ec"
ps -o pid=,args= -C haproxy
echo "--- stats sockets (must NOT contain v5) ---"
ls -la /run/shengyu-edgelink/
echo "--- 8443 handshake ---"
timeout 8 openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com -brief </dev/null 2>&1 | head -3
echo "--- 9443 handshake ---"
timeout 8 openssl s_client -connect 127.0.0.1:9443 -servername rbui-test.example.com -brief </dev/null 2>&1 | head -3

echo "===== 4B.6 manual reload must work again (flag gone) ====="
systemctl reload shengyu-edgelink-haproxy; echo "manual_reload_exit=$?"

echo "===== 4B.7 meta + releases ====="
curl -s -b "$W/ck" "$API/api/meta" > "$W/metaB.json"
python3 -c "import json;d=json.load(open('/tmp/rbui/metaB.json'));print('applied_version=',d['node']['applied_version'],'expected_version=',d['node']['expected_version'],'skew=',d['node']['version_skewed'])"
curl -s -b "$W/ck" "$API/api/nodes/$NODE/releases?limit=3" > "$W/relB.json"
python3 - <<'PY'
import json
d=json.load(open('/tmp/rbui/relB.json'))
for it in d.get("items",[]):
    print(it.get("id"), it.get("action"), "v%s->v%s"%(it.get("from_version"),it.get("to_version")), it.get("status"), it.get("phase"), it.get("result"))
PY
echo "SCENARIO_B_DONE"
