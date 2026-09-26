#!/usr/bin/env bash
# 回滚 UI 验收 - 第 4A 步：注入 reload 永久失败 + 改测试业务并发布
set -u
W=/tmp/rbui
API=http://127.0.0.1:8081
NODE=node_06GCGE2NPCC624J5AMKYQWC4C0
CUS=cus_06GCGEQ3G5HH6SGK9Q11AHH4FR
CUS=cus_06GCGEQ3G5HH6SGK9Q11AHH9FR
CSRF=$(cat "$W/csrf")
BIZ=$(cat "$W/biz_id")
S9443=$(cat "$W/sni9443_id")
D=/etc/systemd/system/shengyu-edgelink-haproxy.service.d

echo "===== 4A.1 inject drop-in: ExecReload forced to /bin/false ====="
mkdir -p "$D"
cat > "$D/99-rbui-test.conf" <<'EOF'
# [RBUI TEST ONLY] 临时覆盖：让 reload 必然失败，用于验收回滚 UI。
# 用两次 ExecReload= 是为了**清空**原列表再设新值（systemd 语义：首个赋值清空，后续追加）。
# 验收结束后本文件会被删除。
[Service]
ExecReload=
ExecReload=/bin/false
EOF
cat "$D/99-rbui-test.conf"
systemctl daemon-reload
echo "--- effective ExecReload ---"
systemctl show -p ExecReload --value shengyu-edgelink-haproxy
echo "--- units still running? ---"
systemctl is-active shengyu-edgelink-server shengyu-edgelink-haproxy

echo "===== 4A.2 sanity: manual reload must now fail (proves injection works) ====="
systemctl reload shengyu-edgelink-haproxy; echo "manual_reload_exit=$?"

echo "===== 4A.3 snapshot BEFORE bad publish ====="
echo "VERSION=$(cat /etc/shengyu-edgelink/haproxy/current/VERSION)"
sha256sum /etc/shengyu-edgelink/haproxy/current/haproxy.cfg
ps -o pid=,args= -C haproxy
ls /run/shengyu-edgelink/
echo "--- 8443 handshake before ---"
timeout 8 openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com -brief </dev/null 2>&1 | head -3

echo "===== 4A.4 modify test business (origin_port 443 -> 444) ====="
curl -s -b "$W/ck" -o "$W/biz_mod.json" -w 'http=%{http_code}\n' \
  -X PUT -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/businesses/$BIZ" -d "{
    \"customer_id\":\"$CUS\",
    \"name\":\"RBUI-rollback-test-9443\",
    \"remark\":\"rollback UI acceptance temporary business (modified for failure test)\",
    \"mode\":\"sni_tls\",
    \"domains\":[\"rbui-test.example.com\"],
    \"primary_node_id\":\"$NODE\",
    \"sni_entry_id\":\"$S9443\",
    \"origin_host\":\"1.1.1.1\",
    \"origin_port\":444
  }"
head -c 300 "$W/biz_mod.json"; echo

echo "===== 4A.5 publish (expected: FAIL at reload phase) ====="
curl -s -b "$W/ck" -o "$W/pubA.json" -w 'publish_http=%{http_code}\n' \
  -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/$NODE/publish" -d '{"note":"RBUI scenario A: reload always fails"}'
echo "--- raw publish response ---"
cat "$W/pubA.json"; echo
echo "--- decoded ---"
python3 - <<'PY'
import json
d=json.load(open('/tmp/rbui/pubA.json'))
print("KEY            | VALUE")
for k in ("release_id","action","version","prev_version","status","phase","no_change","rolled_back","rollback_note","manual_fix","origin_up","origin_down","duration_ms"):
    print(k,"=",d.get(k))
print("message =", d.get("message"))
print("--- manual_fix lines ---")
for i,l in enumerate(d.get("manual_fix") or []): print(i,l)
PY

echo "===== 4A.6 post-failure invariants ====="
echo "VERSION=$(cat /etc/shengyu-edgelink/haproxy/current/VERSION)"
sha256sum /etc/shengyu-edgelink/haproxy/current/haproxy.cfg
ps -o pid=,args= -C haproxy
ls -la /run/shengyu-edgelink/
echo "--- 8443 handshake after ---"
timeout 8 openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com -brief </dev/null 2>&1 | head -3
echo "--- 9443 (must fail: not listening yet) ---"
timeout 5 openssl s_client -connect 127.0.0.1:9443 -servername rbui-test.example.com -brief </dev/null 2>&1 | head -3
echo "--- haproxy -c current ---"
haproxy -c -f /etc/shengyu-edgelink/haproxy/current/haproxy.cfg 2>&1 | tail -2
echo "--- haproxy -c the failed v4 dir ---"
V4=/etc/shengyu-edgelink/haproxy/versions/$NODE/v4
ls -la $V4 2>&1
haproxy -c -f $V4/haproxy.cfg 2>&1 | tail -2

echo "===== 4A.7 meta + releases ====="
curl -s -b "$W/ck" "$API/api/meta" > "$W/metaA.json"
python3 - <<'PY'
import json
d=json.load(open('/tmp/rbui/metaA.json'))
print("initialized =", d.get("initialized"))
print("node =", json.dumps(d.get("node"), ensure_ascii=False))
PY
curl -s -b "$W/ck" "$API/api/nodes/$NODE/releases?limit=3" > "$W/relA.json"
python3 - <<'PY'
import json
d=json.load(open('/tmp/rbui/relA.json'))
for it in d.get("items",[]):
    print(json.dumps({k:it.get(k) for k in ("id","action","from_version","to_version","status","phase","result","detail")}, ensure_ascii=False))
PY
echo "SCENARIO_A_DONE"
