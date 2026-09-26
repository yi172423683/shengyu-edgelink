#!/usr/bin/env bash
# 回滚 UI 验收 - 第 3 步：发布 v3（预期成功），建立 good version
set -u
W=/tmp/rbui
API=http://127.0.0.1:8081
NODE=node_06GCGE2NPCC624J5AMKYQWC4C0
CSRF=$(cat "$W/csrf")

echo "### BEFORE: version / sha / listeners"
cat /etc/shengyu-edgelink/haproxy/current/VERSION
sha256sum /etc/shengyu-edgelink/haproxy/current/haproxy.cfg
ls /run/shengyu-edgelink/

echo "### publish v3"
curl -s -b "$W/ck" -o "$W/pub3.json" -w 'http=%{http_code}\n' \
  -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/$NODE/publish" -d '{"note":"RBUI step3 baseline good version"}'
python3 - <<'PY'
import json
d=json.load(open('/tmp/rbui/pub3.json'))
for k in ("release_id","version","prev_version","status","phase","no_change","rolled_back","rollback_note","manual_fix","origin_up","origin_down","message","verify_waited_ms","verify_attempts"):
    print(k,"=",d.get(k))
v=d.get("verify") or {}
print("verify.ok =",v.get("ok"))
print("verify.dataplane_version =",v.get("dataplane_version"))
print("verify.listeners =",[(l.get("addr"),l.get("port"),l.get("bound"),l.get("owned_by_us")) for l in (v.get("listeners") or [])])
print("verify.problems =",v.get("problems"))
PY

echo "### AFTER: version / sha / sockets"
cat /etc/shengyu-edgelink/haproxy/current/VERSION
sha256sum /etc/shengyu-edgelink/haproxy/current/haproxy.cfg
ls -la /run/shengyu-edgelink/

echo "### listeners after"
ss -lntp | grep -E ':8443|:9443|:443 '

echo "### 9443 TLS handshake (test domain, passthrough -> 1.1.1.1)"
timeout 8 openssl s_client -connect 127.0.0.1:9443 -servername rbui-test.example.com -brief </dev/null 2>&1 | head -8

echo "### 8443 TLS handshake (existing business)"
timeout 8 openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com -brief </dev/null 2>&1 | head -8

echo "### haproxy -c on current"
haproxy -c -f /etc/shengyu-edgelink/haproxy/current/haproxy.cfg 2>&1 | tail -2
echo "STEP3_DONE"
