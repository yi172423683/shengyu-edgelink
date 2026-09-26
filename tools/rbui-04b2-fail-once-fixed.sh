#!/usr/bin/env bash
# 回滚 UI 验收 - 第 4B(2) 步：修正注入器 flag 位置后重跑 fail-once
set -u
W=/tmp/rbui
API=http://127.0.0.1:8081
NODE=node_06GCGE2NPCC624J5AMKYQWC4C0
CUS=cus_06GCGEQ3G5HH6SGK9Q11AHH9FR
CSRF=$(cat "$W/csrf")
BIZ=$(cat "$W/biz_id")
S9443=$(cat "$W/sni9443_id")
D=/etc/systemd/system/shengyu-edgelink-haproxy.service.d
SD=/run/shengyu-edgelink

echo "===== 4B2.0 clear the stuck flag from the previous attempt ====="
rm -f /run/rbui-reload-fail-once
rm -f "$SD/rbui-reload-fail-once"
systemctl reload shengyu-edgelink-haproxy; echo "manual_reload_after_clear_exit=$?"
echo "VERSION=$(cat /etc/shengyu-edgelink/haproxy/current/VERSION)"

echo "===== 4B2.1 install mode-aware injector (flag lives in a dir shengyu can write) ====="
cat > /usr/local/bin/rbui-reload-inject.sh <<'EOF'
#!/bin/sh
# [RBUI TEST ONLY] reload 注入器。
#   mode=once   -> 若 flag 存在：消费 flag 并失败一次；否则执行真实 reload
#   mode=always -> 永远失败
#   mode=off    -> 永远执行真实 reload
# flag 放在 /run/shengyu-edgelink（root:shengyu 0770）而不是 /run：
# 本脚本以 shengyu 身份运行，只有在该目录里才有写/删权限（/run 下 root 的文件删不掉）。
SD=/run/shengyu-edgelink
MODE=$(cat "$SD/rbui-reload-mode" 2>/dev/null || echo off)
case "$MODE" in
  always)
    echo "RBUI injected reload failure (mode=always)" >&2
    exit 1
    ;;
  once)
    if [ -e "$SD/rbui-reload-fail-once" ]; then
      rm -f "$SD/rbui-reload-fail-once"
      echo "RBUI injected reload failure (mode=once, flag consumed)" >&2
      exit 1
    fi
    ;;
esac
exec /bin/kill -USR2 "$(cat "$SD/haproxy.pid")"
EOF
chmod 0755 /usr/local/bin/rbui-reload-inject.sh

cat > "$D/99-rbui-test.conf" <<'EOF'
# [RBUI TEST ONLY] 临时覆盖：reload 走注入器。
[Service]
ExecReload=
ExecReload=/usr/local/bin/rbui-reload-inject.sh
EOF
systemctl daemon-reload
echo "--- effective ExecReload ---"
systemctl show -p ExecReload --value shengyu-edgelink-haproxy

echo "===== 4B2.2 self-test the injector: mode=off then mode=once ====="
printf 'off\n' > "$SD/rbui-reload-mode"; chmod 0644 "$SD/rbui-reload-mode"
systemctl reload shengyu-edgelink-haproxy; echo "mode=off reload_exit=$? (expect 0)"
printf 'once\n' > "$SD/rbui-reload-mode"
touch "$SD/rbui-reload-fail-once"; chown shengyu:shengyu "$SD/rbui-reload-fail-once"
systemctl reload shengyu-edgelink-haproxy; echo "mode=once 1st reload_exit=$? (expect 1)"
systemctl reload shengyu-edgelink-haproxy; echo "mode=once 2nd reload_exit=$? (expect 0)"
echo "--- flag should be gone now ---"; ls -la "$SD/rbui-reload-fail-once" 2>&1

echo "===== 4B2.3 snapshot BEFORE ====="
echo "VERSION=$(cat /etc/shengyu-edgelink/haproxy/current/VERSION)"
sha256sum /etc/shengyu-edgelink/haproxy/current/haproxy.cfg
ps -o pid=,args= -C haproxy
ls "$SD"

echo "===== 4B2.4 modify test business (origin_port 445 -> 446) ====="
curl -s -b "$W/ck" -o /dev/null -w 'http=%{http_code}\n' \
  -X PUT -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/businesses/$BIZ" -d "{
    \"customer_id\":\"$CUS\",
    \"name\":\"RBUI-rollback-test-9443\",
    \"remark\":\"rollback UI acceptance temporary business (scenario B2)\",
    \"mode\":\"sni_tls\",
    \"domains\":[\"rbui-test.example.com\"],
    \"primary_node_id\":\"$NODE\",
    \"sni_entry_id\":\"$S9443\",
    \"origin_host\":\"1.1.1.1\",
    \"origin_port\":446
  }"

echo "===== 4B2.5 arm one-shot failure and publish ====="
touch "$SD/rbui-reload-fail-once"; chown shengyu:shengyu "$SD/rbui-reload-fail-once"
echo "armed: $(ls -la $SD/rbui-reload-fail-once)"
curl -s -b "$W/ck" -o "$W/pubB2.json" -w 'publish_http=%{http_code}\n' \
  -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/$NODE/publish" -d '{"note":"RBUI scenario B2: reload fails once"}'
python3 - <<'PY'
import json
d=json.load(open('/tmp/rbui/pubB2.json'))
print("release_id    =", d.get("release_id"))
print("version       =", d.get("version"), " prev =", d.get("prev_version"))
print("status        =", d.get("status"))
print("phase         =", d.get("phase"))
print("rolled_back   =", d.get("rolled_back"))
print("rollback_note =", d.get("rollback_note"))
print("manual_fix    =", d.get("manual_fix"))
print("message       =", d.get("message"))
PY
echo "-- flag consumed? --"
if [ -e "$SD/rbui-reload-fail-once" ]; then echo "FLAG STILL PRESENT (injector did not run)"; else echo "FLAG CONSUMED (injector ran exactly once)"; fi

echo "===== 4B2.6 post-failure invariants ====="
echo "VERSION=$(cat /etc/shengyu-edgelink/haproxy/current/VERSION)"
sha256sum /etc/shengyu-edgelink/haproxy/current/haproxy.cfg
echo "expected good sha (v3) = c5937970d8b1ea7f0a19b475dd1e0b432b1b2f8b5a512a41df68e17df83d53ec"
ps -o pid=,args= -C haproxy
echo "--- stats sockets (must NOT contain v6) ---"; ls "$SD"
echo "--- 8443 ---"; timeout 8 openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com -brief </dev/null 2>&1 | head -3
echo "--- 9443 ---"; timeout 8 openssl s_client -connect 127.0.0.1:9443 -servername rbui-test.example.com -brief </dev/null 2>&1 | head -3
echo "--- haproxy -c current ---"; haproxy -c -f /etc/shengyu-edgelink/haproxy/current/haproxy.cfg 2>&1 | tail -1

echo "===== 4B2.7 meta + releases ====="
curl -s -b "$W/ck" "$API/api/meta" > "$W/metaB2.json"
python3 -c "import json;d=json.load(open('/tmp/rbui/metaB2.json'));print('applied_version=',d['node']['applied_version'],'expected_version=',d['node']['expected_version'],'skew=',d['node']['version_skewed'])"
curl -s -b "$W/ck" "$API/api/nodes/$NODE/releases?limit=4" > "$W/relB2.json"
python3 - <<'PY'
import json
d=json.load(open('/tmp/rbui/relB2.json'))
for it in d.get("items",[]):
    print(it.get("id"), it.get("action"), "v%s->v%s"%(it.get("from_version"),it.get("to_version")), it.get("status"), it.get("phase"), it.get("result"))
PY
echo "SCENARIO_B2_DONE"
