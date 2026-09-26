#!/usr/bin/env bash
# 复核：6C 之后 applied_version 的口径是否自愈
set -u
W=/tmp/rbui
API=http://127.0.0.1:8081
NODE=node_06GCGE2NPCC624J5AMKYQWC4C0

echo "### now: $(date -u +%H:%M:%S)"
echo -n "disk current/VERSION = "; cat /etc/shengyu-edgelink/haproxy/current/VERSION

echo "### meta (node block)"
curl -s -b "$W/ck" "$API/api/meta" -o "$W/m2.json"
python3 - <<'PY'
import json
d=json.load(open('/tmp/rbui/m2.json'))
n=d['node']
print("applied_version  =", n['applied_version'])
print("expected_version =", n['expected_version'])
print("version_skewed   =", n['version_skewed'])
print("health           =", n['health'])
print("initialized      =", d['initialized'], " business_count =", d['business_count'])
PY

echo "### nodes endpoint (raw applied/expected + heartbeat)"
curl -s -b "$W/ck" "$API/api/nodes" -o "$W/n2.json"
python3 - <<'PY'
import json
d=json.load(open('/tmp/rbui/n2.json'))
for it in d['items']:
    print("applied_version =", it['applied_version'], " expected_version =", it['expected_version'],
          " last_heartbeat =", it['last_heartbeat'])
    print("health_detail   =", json.dumps(it.get('health_detail'), ensure_ascii=False))
PY

echo "### wait 30s (one heartbeat cycle) then re-read"
sleep 30
curl -s -b "$W/ck" "$API/api/meta" -o "$W/m3.json"
python3 - <<'PY'
import json
d=json.load(open('/tmp/rbui/m3.json'))
n=d['node']
print("after 30s: applied_version =", n['applied_version'],
      " expected_version =", n['expected_version'], " skewed =", n['version_skewed'])
PY
echo -n "disk current/VERSION = "; cat /etc/shengyu-edgelink/haproxy/current/VERSION
echo "RECHECK_DONE"
