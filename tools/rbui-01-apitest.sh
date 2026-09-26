#!/usr/bin/env bash
# 回滚 UI 验收 - 第 1 步：登录并盘点现状（只读）
set -u
W=/tmp/rbui
mkdir -p "$W"
API=http://127.0.0.1:8081

echo "### tools"
command -v jq || echo "jq: MISSING"
command -v python3 || echo "python3: MISSING"

echo "### extract admin password"
PW=$(grep -oE '[A-Za-z0-9]{32}' /root/shengyu-install.log | head -1)
if [ -z "$PW" ]; then echo "PASSWORD_NOT_FOUND"; exit 1; fi
echo "password length: ${#PW}"

echo "### login"
curl -s -c "$W/ck" -o "$W/login.json" -w 'login_http=%{http_code}\n' \
  -X POST "$API/api/login" -H 'Content-Type: application/json' \
  -d "{\"username\":\"admin\",\"password\":\"$PW\"}"
cat "$W/login.json"; echo
CSRF=$(sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p' "$W/login.json")
echo "csrf_len=${#CSRF}"
echo "$CSRF" > "$W/csrf"

echo "### customers"
curl -s -b "$W/ck" -o "$W/customers.json" -w 'http=%{http_code}\n' "$API/api/customers"
cat "$W/customers.json"; echo

echo "### nodes"
curl -s -b "$W/ck" -o "$W/nodes.json" -w 'http=%{http_code}\n' "$API/api/nodes"
cat "$W/nodes.json"; echo

echo "### sni-entries"
curl -s -b "$W/ck" -o "$W/sni.json" -w 'http=%{http_code}\n' "$API/api/sni-entries"
cat "$W/sni.json"; echo

echo "### businesses"
curl -s -b "$W/ck" -o "$W/biz.json" -w 'http=%{http_code}\n' "$API/api/businesses"
cat "$W/biz.json"; echo

echo "### meta"
curl -s -b "$W/ck" -o "$W/meta.json" -w 'meta_http=%{http_code}\n' "$API/api/meta"
cat "$W/meta.json"; echo

echo "### releases"
NODE=$(sed -n 's/.*"id":"\(node_[^"]*\)".*/\1/p' "$W/nodes.json" | head -1)
echo "NODE=$NODE"
curl -s -b "$W/ck" -o "$W/releases.json" -w 'http=%{http_code}\n' "$API/api/nodes/$NODE/releases"
cat "$W/releases.json"; echo

echo "### origin reachability probe (1.1.1.1:443)"
python3 - <<'PY' 2>&1 || echo "PY_PROBE_FAILED"
import socket
for hp in [("1.1.1.1",443),("1.1.1.1",8443)]:
    try:
        s=socket.create_connection(hp,3); s.close(); print("OK", hp)
    except Exception as e:
        print("FAIL", hp, e)
PY
echo "STEP1_DONE"
