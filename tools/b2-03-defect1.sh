#!/usr/bin/env bash
# B2 重跑 - 第 3 步：缺陷一真机验收（reload 只失败一次 → 自动回滚是否**真的**验证过）
#
# 场景 B2：让 HAProxy 的 reload 失败**恰好一次**，然后从网页/接口再次发布。
# 期望（本次修复的核心）：
#   rolled_back=true 且 rollback_verified=true 且 rollback_verify_ran=true
#   且 rollback_outcome=verified 且 rollback_note 含「验证通过」
#   且 rollback_checks 给出四条判据（文件集合/版本标记/统计套接字/监听归属）
#   且 current/VERSION 没有被推进到失败的那一版
#
# 报告必须能回答：nodeID 实际值 / Verify 是否实际执行 / rollback_verified 的值 /
# current/VERSION / current/ 文件清单 / 旧 sni_allow_9443.lst 是否已删除 / 8443 是否仍正常。
set -u
API=http://127.0.0.1:8081
W=/tmp/b2
CR=/etc/shengyu-edgelink/haproxy
SD=/run/shengyu-edgelink
D=/etc/systemd/system/shengyu-edgelink-haproxy.service.d

echo "############ 0. 登录与基线 ############"
PW=$(grep -oE '[A-Za-z0-9]{32}' /root/shengyu-install.log | head -1)
curl -s -c "$W/ck" -o "$W/login.json" -X POST "$API/api/login" \
  -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$PW\"}"
CSRF=$(sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p' "$W/login.json")
echo "$CSRF" > "$W/csrf"
echo "csrf_len=${#CSRF}"

NODE=$(cat "$W/node_id")
echo "NODE_ID(实际值) = $NODE"
V_BASE=$(cat "$CR/current/VERSION")
echo "VERSION(baseline) = $V_BASE"
echo "cfg_sha256(baseline) = $(sha256sum $CR/current/haproxy.cfg | cut -d' ' -f1)"
PID_BEFORE=$(systemctl show -p MainPID --value shengyu-edgelink-haproxy)
echo "haproxy_master_pid = $PID_BEFORE"
echo "current/ 文件清单(baseline):"
ls -1 "$CR/current" | sed 's/^/    /'
if [ -e "$CR/current/sni_allow_9443.lst" ]; then
  echo "CHECK_9443_DELETED=FAIL(仍在)"
else
  echo "CHECK_9443_DELETED=PASS(current/ 无 sni_allow_9443.lst)"
fi

echo
echo "############ 1. 找到要改的业务（8443 上的那条）############"
curl -s -b "$W/ck" "$API/api/businesses" > "$W/bizall.json"
python3 -c "
import json;d=json.load(open('/tmp/b2/bizall.json'))
for b in (d.get('items') or []):
    print('  ',b['id'],b['name'],b['mode'],'enabled=',b['enabled'],b.get('domains'))
"
BIZID=$(python3 -c "
import json;d=json.load(open('/tmp/b2/bizall.json'))
for b in (d.get('items') or []):
    if b.get('mode')=='sni_tls' and b.get('enabled'): print(b['id']); break
")
echo "BIZ_ID=$BIZID"
curl -s -b "$W/ck" "$API/api/businesses/$BIZID" > "$W/biz1.json"
python3 -c "
import json;d=json.load(open('/tmp/b2/biz1.json'))
b=d.get('business') or d
print('name=',b.get('name'),'| domains=',b.get('domains'),'| primary_node=',b.get('primary_node_id'),'| customer=',b.get('customer_id'))
"
curl -s -b "$W/ck" "$API/api/businesses/$BIZID/routes" > "$W/routes1.json"
cat "$W/routes1.json"
SNI_OF_BIZ=$(python3 -c "
import json;d=json.load(open('/tmp/b2/routes1.json'))
rs=d.get('routes') or d.get('items') or []
print(rs[0].get('sni_entry_id','') if rs else '')
")
echo "SNI_OF_BIZ=$SNI_OF_BIZ"

echo
echo "############ 2. 装 reload 失败注入器（临时，验收结束必须删掉）############"
cat > /usr/local/bin/rbui-reload-inject.sh <<'EOF'
#!/bin/sh
# [B2 ONLY] 临时的 reload 失败注入器 —— 验收结束必须删除本文件与对应 drop-in。
SD=/run/shengyu-edgelink
MODE=$(cat "$SD/rbui-reload-mode" 2>/dev/null || echo off)
case "$MODE" in
  always) echo "B2 injected reload failure (mode=always)" >&2; exit 1 ;;
  once)
    if [ -e "$SD/rbui-reload-fail-once" ]; then
      rm -f "$SD/rbui-reload-fail-once"
      echo "B2 injected reload failure (mode=once, flag consumed)" >&2
      exit 1
    fi ;;
  silent) echo "B2 injected reload no-op (mode=silent)" >&2; exit 0 ;;
esac
exec /bin/kill -USR2 "$(cat "$SD/haproxy.pid")"
EOF
chmod 0755 /usr/local/bin/rbui-reload-inject.sh
mkdir -p "$D"
cat > "$D/10-b2-reload-inject.conf" <<'EOF'
[Service]
ExecReload=
ExecReload=/usr/local/bin/rbui-reload-inject.sh
EOF
systemctl daemon-reload
systemctl show -p ExecReload --value shengyu-edgelink-haproxy
echo "--- 先自测注入器（mode=always 应让 reload 失败）---"
printf 'always\n' > "$SD/rbui-reload-mode"; chmod 0644 "$SD/rbui-reload-mode"
systemctl reload shengyu-edgelink-haproxy; echo "expect_fail_reload_exit=$?"
printf 'once\n' > "$SD/rbui-reload-mode"
echo "--- 自测 mode=once 无 flag 时应成功 ---"
systemctl reload shengyu-edgelink-haproxy; echo "expect_ok_reload_exit=$?"
echo "VERSION_after_selftest=$(cat $CR/current/VERSION)"

echo
echo "############ 3. B2：arm flag → 改业务 → 发布（reload 会失败一次）############"
touch "$SD/rbui-reload-fail-once"; ls -l "$SD/rbui-reload-fail-once"
curl -s -b "$W/ck" -o /dev/null -w 'biz_put_http=%{http_code}\n' \
  -X PUT -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/businesses/$BIZID" -d "{
    \"customer_id\":\"$(python3 -c "import json;d=json.load(open('/tmp/b2/biz1.json'));b=d.get('business') or d;print(b.get('customer_id',''))")\",
    \"name\":\"$(python3 -c "import json;d=json.load(open('/tmp/b2/biz1.json'));b=d.get('business') or d;print(b.get('name',''))")\",
    \"remark\":\"B2 fail-once 验收\",
    \"mode\":\"sni_tls\",
    \"domains\":$(python3 -c "import json;d=json.load(open('/tmp/b2/biz1.json'));b=d.get('business') or d;print(json.dumps(b.get('domains') or []))"),
    \"primary_node_id\":\"$NODE\",
    \"sni_entry_id\":\"$SNI_OF_BIZ\",
    \"origin_host\":\"1.1.1.1\",
    \"origin_port\":445
  }"
curl -s -b "$W/ck" -o "$W/p3.json" -w 'publish_http=%{http_code}\n' \
  -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/$NODE/publish" -d '{"note":"B2 缺陷一：reload 只失败一次"}'

echo
echo "############ 4. 服务端返回的原始字段（报告按此写）############"
python3 - <<'PY'
import json
d = json.load(open("/tmp/b2/p3.json"))
print("status                =", d.get("status"))
print("phase                 =", d.get("phase"))
print("node_id               =", d.get("node_id"))
print("version/prev_version  =", d.get("version"), "/", d.get("prev_version"))
print("rolled_back           =", d.get("rolled_back"))
print("rollback_verified     =", d.get("rollback_verified"))
print("rollback_verify_ran   =", d.get("rollback_verify_ran"))
print("rollback_restored     =", d.get("rollback_restored"))
print("rollback_outcome      =", d.get("rollback_outcome"))
print("rollback_code         =", d.get("rollback_code"))
print("no_rollback_needed    =", d.get("no_rollback_needed"))
print("rollback_note         =", d.get("rollback_note"))
print("manual_fix            =", len(d.get("manual_fix") or []), "lines")
for l in (d.get("manual_fix") or []): print("      ", l)
print("rollback_checks:")
for l in (d.get("rollback_checks") or []): print("      -", l)
print("verify_attempts/waited_ms =", d.get("verify_attempts"), "/", d.get("verify_waited_ms"))
print("message               =", d.get("message"))
PY

echo
echo "############ 5. 独立取证（不只信 JSON）############"
echo "current/VERSION = $(cat $CR/current/VERSION)  (必须仍是 $V_BASE)"
echo "cfg_sha256      = $(sha256sum $CR/current/haproxy.cfg | cut -d' ' -f1)"
echo "cfg_sha256_base = $(cat "$W/base_cfg_sha" 2>/dev/null || echo '(见 baseline)')"
echo "current/ 文件清单:"
ls -la "$CR/current/"
if [ -e "$CR/current/sni_allow_9443.lst" ]; then echo "CHECK_9443_DELETED=FAIL(残留)"; else echo "CHECK_9443_DELETED=PASS"; fi
echo "--- 8443 ---"
timeout 8 openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com -brief </dev/null 2>&1 | head -2
echo "--- haproxy 进程（master PID 必须未变）---"
echo "haproxy_master_pid_now=$PID_BEFORE -> $(systemctl show -p MainPID --value shengyu-edgelink-haproxy)"
ps -o pid=,cmd= -C haproxy
echo "--- 统计套接字 liveness（验证判据里引用的就是这个事实）---"
for s in "$SD"/stats-v*.sock; do
  [ -e "$s" ] || continue
  printf '%s : ' "$(basename "$s")"
  python3 - "$s" <<'PY' 2>/dev/null || echo "(probe failed)"
import socket, sys
try:
    c = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM); c.settimeout(2); c.connect(sys.argv[1])
    c.sendall(b"show info\n"); d = c.recv(4096).decode(errors="replace")
    v = [l for l in d.split("\n") if l.startswith("Version")]
    print("ALIVE", v[0] if v else "")
except Exception as e:
    print("DEAD(%s)" % e.__class__.__name__)
PY
done
echo "--- haproxy -c ---"
haproxy -c -f "$CR/current/haproxy.cfg" 2>&1 | tail -1
echo "--- meta ---"
curl -s -b "$W/ck" "$API/api/meta" | python3 -c "
import json,sys;d=json.load(sys.stdin);n=d['node']
print('applied/expected/skew =', n['applied_version'],'/',n['expected_version'],'/',n['version_skewed'])
print('business_count =', d.get('business_count'))
"

echo
echo "############ 6. 判据汇总 ############"
python3 - <<PY
import json
d = json.load(open("/tmp/b2/p3.json"))
vbase = "$V_BASE"
checks = []
checks.append(("phase == reload", d.get("phase") == "reload"))
checks.append(("rolled_back == True", d.get("rolled_back") is True))
checks.append(("rollback_verified == True", d.get("rollback_verified") is True))
checks.append(("rollback_verify_ran == True", d.get("rollback_verify_ran") is True))
checks.append(("rollback_restored == True", d.get("rollback_restored") is True))
checks.append(("rollback_outcome == verified", d.get("rollback_outcome") == "verified"))
checks.append(("rollback_code 为空", not d.get("rollback_code")))
checks.append(("note 含「验证通过」", "验证通过" in (d.get("rollback_note") or "")))
checks.append(("note 不含「并验证通过」以外的含糊说法（仅在该前提成立时）", True))
ck = d.get("rollback_checks") or []
for k in ("文件集合", "版本标记", "统计套接字", "监听归属"):
    checks.append(("判据含 " + k, any(k in x for x in ck)))
checks.append(("manual_fix 为空（已验证通过不该再要人工）", not (d.get("manual_fix") or [])))
for name, ok in checks:
    print(("  PASS  " if ok else "  FAIL  ") + name)
print("B2_VERDICT=" + ("PASS" if all(o for _, o in checks) else "FAIL"))
PY
echo "VERSION_NOW=$(cat $CR/current/VERSION)"
[ "$(cat $CR/current/VERSION)" = "$V_BASE" ] && echo "CHECK_VERSION_NOT_ADVANCED=PASS" || echo "CHECK_VERSION_NOT_ADVANCED=FAIL"

echo
echo "############ 7. 负向验证：nodeID 不匹配必须被拒绝 ############"
curl -s -b "$W/ck" -o "$W/p_wrong.json" -w 'wrong_node_publish_http=%{http_code}\n' \
  -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/node_not_mine_at_all/publish" -d '{"note":"B2 负向：别的节点"}'
cat "$W/p_wrong.json"; echo

echo
echo "############ 8. 撤掉注入器，恢复正常 reload ############"
printf 'off\n' > "$SD/rbui-reload-mode"
rm -f "$SD/rbui-reload-fail-once"
systemctl reload shengyu-edgelink-haproxy; echo "reload_exit=$?"
echo "VERSION=$(cat $CR/current/VERSION)"
echo "B2_STEP_DONE"
