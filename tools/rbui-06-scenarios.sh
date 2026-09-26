#!/usr/bin/env bash
# 回滚 UI 验收 - 第 6 步（修复后 v0.3.2）：三个场景
#   6A reload 永久失败 + 运行版本可确认  -> 预期「改动未生效，无需回滚」（已验证）
#   6B reload 永久失败 + 运行版本无法确认 -> 预期「未成功回滚」+ 人工命令
#   6C reload 返回 0 但什么都没做        -> 预期「已自动回滚并验证通过」
set -u
W=/tmp/rbui
API=http://127.0.0.1:8081
NODE=node_06GCGE2NPCC624J5AMKYQWC4C0
CUS=cus_06GCGEQ3G5HH6SGK9Q11AHH9FR
SD=/run/shengyu-edgelink
D=/etc/systemd/system/shengyu-edgelink-haproxy.service.d
GOOD_SHA=c5937970d8b1ea7f0a19b475dd1e0b432b1b2f8b5a512a41df68e17df83d53ec

echo "##### re-login (management service was restarted) #####"
PW=$(grep -oE '[A-Za-z0-9]{32}' /root/shengyu-install.log | head -1)
curl -s -c "$W/ck" -o "$W/login.json" -X POST "$API/api/login" \
  -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$PW\"}"
CSRF=$(sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p' "$W/login.json")
echo "$CSRF" > "$W/csrf"; echo "csrf_len=${#CSRF}"

# ---- 升级注入器：新增 mode=silent（reload 返回 0 但什么都不做）----
cat > /usr/local/bin/rbui-reload-inject.sh <<'EOF'
#!/bin/sh
# [RBUI TEST ONLY]
SD=/run/shengyu-edgelink
MODE=$(cat "$SD/rbui-reload-mode" 2>/dev/null || echo off)
case "$MODE" in
  always) echo "RBUI injected reload failure (mode=always)" >&2; exit 1 ;;
  silent) echo "RBUI injected reload no-op (mode=silent): exit 0 but worker untouched" >&2; exit 0 ;;
  once)
    if [ -e "$SD/rbui-reload-fail-once" ]; then
      rm -f "$SD/rbui-reload-fail-once"
      echo "RBUI injected reload failure (mode=once, flag consumed)" >&2
      exit 1
    fi ;;
esac
exec /bin/kill -USR2 "$(cat "$SD/haproxy.pid")"
EOF
chmod 0755 /usr/local/bin/rbui-reload-inject.sh

biz_mod() { # $1=origin_port  $2=remark
  curl -s -b "$W/ck" -o /dev/null -w "biz_put_http=%{http_code}\n" \
    -X PUT -H "X-CSRF-Token: $(cat $W/csrf)" -H 'Content-Type: application/json' \
    "$API/api/businesses/$(cat $W/biz_id)" -d "{
      \"customer_id\":\"$CUS\",
      \"name\":\"RBUI-rollback-test-9443\",
      \"remark\":\"$2\",
      \"mode\":\"sni_tls\",
      \"domains\":[\"rbui-test.example.com\"],
      \"primary_node_id\":\"$NODE\",
      \"sni_entry_id\":\"$(cat $W/sni9443_id)\",
      \"origin_host\":\"1.1.1.1\",
      \"origin_port\":$1
    }"
}

dump() { # $1=json file  $2=label
  python3 - "$1" "$2" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
print("---- %s ----"%sys.argv[2])
print("status           =", d.get("status"))
print("phase            =", d.get("phase"))
print("version/prev     =", d.get("version"), "/", d.get("prev_version"))
print("rolled_back      =", d.get("rolled_back"))
print("no_rollback_needed=", d.get("no_rollback_needed"))
print("rollback_note    =", d.get("rollback_note"))
mf=d.get("manual_fix") or []
print("manual_fix(%d lines)="%len(mf))
for l in mf: print("   ", l)
print("message          =", d.get("message"))
PY
}

invariants() { # $1=label
  echo "--- invariants [$1] ---"
  echo -n "current/VERSION = "; cat /etc/shengyu-edgelink/haproxy/current/VERSION
  echo -n "current/cfg sha = "; sha256sum /etc/shengyu-edgelink/haproxy/current/haproxy.cfg | cut -d' ' -f1
  echo    "expected good   = $GOOD_SHA"
  echo    "stats sockets   = $(ls $SD | grep -c '^stats-v') 个: $(ls $SD | grep '^stats-v' | tr '\n' ' ')"
  echo    "haproxy procs   = $(ps -o pid= -C haproxy | tr '\n' ' ')"
  echo -n "8443 handshake  = "; timeout 8 openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com -brief </dev/null 2>&1 | head -1
  echo -n "9443 handshake  = "; timeout 8 openssl s_client -connect 127.0.0.1:9443 -servername rbui-test.example.com -brief </dev/null 2>&1 | head -1
  echo -n "haproxy -c      = "; haproxy -c -f /etc/shengyu-edgelink/haproxy/current/haproxy.cfg 2>&1 | tail -1
  curl -s -b "$W/ck" "$API/api/meta" > "$W/m.json"
  python3 -c "import json;d=json.load(open('/tmp/rbui/m.json'));n=d['node'];print('meta applied/expected/skew =',n['applied_version'],'/',n['expected_version'],'/',n['version_skewed']);print('meta initialized =',d['initialized'])"
}

echo
echo "##################### 6A: reload 永久失败，运行版本可确认 #####################"
printf 'always\n' > "$SD/rbui-reload-mode"; chmod 0644 "$SD/rbui-reload-mode"
biz_mod 447 "RBUI scenario 6A"
curl -s -b "$W/ck" -o "$W/p6a.json" -w 'publish_http=%{http_code}\n' \
  -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/$NODE/publish" -d '{"note":"RBUI 6A reload always fails"}'
dump "$W/p6a.json" "6A publish response"
invariants "6A"

echo
echo "##################### 6B: reload 永久失败 + 运行版本无法确认 #####################"
echo "break stats socket readability of the running version v3"
chmod 000 "$SD/stats-v3.sock"; ls -l "$SD/stats-v3.sock"
biz_mod 448 "RBUI scenario 6B"
curl -s -b "$W/ck" -o "$W/p6b.json" -w 'publish_http=%{http_code}\n' \
  -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/$NODE/publish" -d '{"note":"RBUI 6B reload always fails + cannot confirm running version"}'
dump "$W/p6b.json" "6B publish response"
echo "restore stats socket perms"
chmod 660 "$SD/stats-v3.sock"; ls -l "$SD/stats-v3.sock"
invariants "6B"

echo
echo "##################### 6C: reload 返回 0 但什么都没做（绝不自杀式 reload） #####################"
printf 'silent\n' > "$SD/rbui-reload-mode"
biz_mod 449 "RBUI scenario 6C"
echo "(waitVerify 会轮询等待满 30s 才判失败，请稍候)"
curl -s -b "$W/ck" -o "$W/p6c.json" -w 'publish_http=%{http_code}\n' \
  -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/$NODE/publish" -d '{"note":"RBUI 6C reload is a silent no-op"}'
dump "$W/p6c.json" "6C publish response"
invariants "6C"

echo
echo "##################### 恢复注入器为 off #####################"
printf 'off\n' > "$SD/rbui-reload-mode"
systemctl reload shengyu-edgelink-haproxy; echo "reload_exit=$?"
echo -n "VERSION="; cat /etc/shengyu-edgelink/haproxy/current/VERSION

echo
echo "##################### releases #####################"
curl -s -b "$W/ck" "$API/api/nodes/$NODE/releases?limit=8" > "$W/rel6.json"
python3 -c "
import json
d=json.load(open('/tmp/rbui/rel6.json'))
for it in d.get('items',[]):
    print(it['id'], it['action'], 'v%s->v%s'%(it['from_version'],it['to_version']), it['status'], it['phase'], it.get('result'))
"
echo "STEP6_DONE"
