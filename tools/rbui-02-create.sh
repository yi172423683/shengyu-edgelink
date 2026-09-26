#!/usr/bin/env bash
# 回滚 UI 验收 - 第 2 步：创建 9443 测试入口 + 测试业务（不碰 8443 / xray）
set -u
W=/tmp/rbui
API=http://127.0.0.1:8081
NODE=node_06GCGE2NPCC624J5AMKYQWC4C0
CUS=cus_06GCGEQ3G5HH6SGK9Q11AHH9FR
CSRF=$(cat "$W/csrf")
TEST_DOMAIN=rbui-test.example.com
TEST_PORT=9443
ORIGIN_HOST=1.1.1.1
ORIGIN_PORT=443

echo "### list sni-entries (node-scoped)"
curl -s -b "$W/ck" -o "$W/sni2.json" -w 'http=%{http_code}\n' "$API/api/nodes/$NODE/sni-entries"
cat "$W/sni2.json"; echo

echo "### port-check 9443 (before create)"
curl -s -b "$W/ck" -w '\nhttp=%{http_code}\n' \
  -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/$NODE/port-check" -d "{\"bind_addr\":\"0.0.0.0\",\"bind_port\":$TEST_PORT}"

SNI_ID=$(sed -n 's/.*"id":"\(sni_[^"]*\)".*/\1/p' "$W/sni2.json" | head -1)
echo "existing SNI_ID=$SNI_ID"

if [ -z "$SNI_ID" ]; then
  echo "### create sni entry 0.0.0.0:$TEST_PORT"
  curl -s -b "$W/ck" -o "$W/sni_new.json" -w 'http=%{http_code}\n' \
    -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
    "$API/api/nodes/$NODE/sni-entries" \
    -d "{\"bind_addr\":\"0.0.0.0\",\"bind_port\":$TEST_PORT}"
  cat "$W/sni_new.json"; echo
  SNI_ID=$(sed -n 's/.*"id":"\(sni_[^"]*\)".*/\1/p' "$W/sni_new.json" | head -1)
fi
echo "SNI_ID=$SNI_ID"
echo "$SNI_ID" > "$W/sni_id"

echo "### create test business"
curl -s -b "$W/ck" -o "$W/biz_new.json" -w 'http=%{http_code}\n' \
  -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/businesses" -d "{
    \"customer_id\":\"$CUS\",
    \"name\":\"RBUI-rollback-test-9443\",
    \"remark\":\"rollback UI acceptance temporary business\",
    \"mode\":\"sni_tls\",
    \"domains\":[\"$TEST_DOMAIN\"],
    \"primary_node_id\":\"$NODE\",
    \"sni_entry_id\":\"$SNI_ID\",
    \"origin_host\":\"$ORIGIN_HOST\",
    \"origin_port\":$ORIGIN_PORT
  }"
cat "$W/biz_new.json"; echo
BIZ_ID=$(sed -n 's/.*"id":"\(biz_[^"]*\)".*/\1/p' "$W/biz_new.json" | head -1)
echo "BIZ_ID=$BIZ_ID"
echo "$BIZ_ID" > "$W/biz_id"

echo "### preview"
curl -s -b "$W/ck" -w '\nhttp=%{http_code}\n' "$API/api/nodes/$NODE/preview"
echo "STEP2_DONE"
