#!/usr/bin/env bash
# 回滚 UI 验收 - 第 2b 步：建 9443 入口并把测试业务改挂到 9443
set -u
W=/tmp/rbui
API=http://127.0.0.1:8081
NODE=node_06GCGE2NPCC624J5AMKYQWC4C0
CUS=cus_06GCGEQ3G5HH6SGK9Q11AHH9FR
CSRF=$(cat "$W/csrf")
BIZ=$(cat "$W/biz_id")

echo "### create sni entry 0.0.0.0:9443"
curl -s -b "$W/ck" -o "$W/sni9443.json" -w 'http=%{http_code}\n' \
  -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/$NODE/sni-entries" -d '{"bind_addr":"0.0.0.0","bind_port":9443}'
cat "$W/sni9443.json"; echo
S9443=$(sed -n 's/.*"id":"\(sni_[^"]*\)".*/\1/p' "$W/sni9443.json" | head -1)
echo "S9443=$S9443"; echo "$S9443" > "$W/sni9443_id"

echo "### re-point test business to 9443 entry"
curl -s -b "$W/ck" -o "$W/biz_upd.json" -w 'http=%{http_code}\n' \
  -X PUT -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/businesses/$BIZ" -d "{
    \"customer_id\":\"$CUS\",
    \"name\":\"RBUI-rollback-test-9443\",
    \"remark\":\"rollback UI acceptance temporary business\",
    \"mode\":\"sni_tls\",
    \"domains\":[\"rbui-test.example.com\"],
    \"primary_node_id\":\"$NODE\",
    \"sni_entry_id\":\"$S9443\",
    \"origin_host\":\"1.1.1.1\",
    \"origin_port\":443
  }"
cat "$W/biz_upd.json"; echo

echo "### sni entries now"
curl -s -b "$W/ck" "$API/api/nodes/$NODE/sni-entries"; echo

echo "### preview expected listeners"
curl -s -b "$W/ck" "$API/api/nodes/$NODE/preview" > "$W/preview.json"
python3 - <<'PY'
import json
d=json.load(open('/tmp/rbui/preview.json'))
print("validation_ok=",d.get("validation_ok"))
print("version=",d.get("version"))
print("expected_listeners=",d.get("expected_listeners"))
print("content_hash=",d.get("content_hash"))
cfg=d.get("config","")
for ln in cfg.splitlines():
    if 'frontend fe_' in ln or 'bind ' in ln or 'use_backend' in ln:
        print("CFG|",ln.strip())
PY
echo "STEP2B_DONE"
