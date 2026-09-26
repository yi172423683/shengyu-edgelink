#!/usr/bin/env bash
# B2 重跑 - 第 4 步：「已恢复但未验证」与「回滚验证失败」两个中间态的真机验证
#
# 这两条是本次修复新增的界面分支，必须真的出现过，否则"界面能区分它们"只是纸面结论。
#
#   4A：让回滚目标的统计套接字不可读 ⇒ Verify 真的跑了但没通过 ⇒ verify_failed
#   4B：把回滚目标的 state.json 移走  ⇒ 验证根本没法执行 ⇒ restored_not_verified
#
# 4A 的价值尤其大：**只有 Verify 真的在执行，结果才会随统计套接字的可读性改变**。
# 这是"Verify 没有被跳过"最硬的现场证据（比 JSON 里那个布尔值更硬）。
set -u
API=http://127.0.0.1:8081
W=/tmp/b2
CR=/etc/shengyu-edgelink/haproxy
SD=/run/shengyu-edgelink

PW=$(grep -oE '[A-Za-z0-9]{32}' /root/shengyu-install.log | head -1)
curl -s -c "$W/ck" -o "$W/login.json" -X POST "$API/api/login" \
  -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$PW\"}"
CSRF=$(sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p' "$W/login.json")
NODE=$(cat "$W/node_id")
BIZID=$(python3 -c "
import json;d=json.load(open('/tmp/b2/biz1.json'));b=d.get('business') or d;print(b['id'])")
BIZJSON=/tmp/b2/biz1.json
ORIG=$(python3 -c "
import json;d=json.load(open('$BIZJSON'));b=d.get('business') or d
print(json.dumps({'customer_id':b.get('customer_id'),'name':b.get('name'),'remark':b.get('remark',''),
 'mode':b.get('mode'),'domains':b.get('domains') or [],'primary_node_id':b.get('primary_node_id')}))")
SNI_OF_BIZ=$(python3 -c "
import json;d=json.load(open('/tmp/b2/routes1.json'));rs=d.get('routes') or d.get('items') or [];print(rs[0].get('sni_entry_id',''))")
V_TARGET=$(cat "$CR/current/VERSION")
echo "NODE=$NODE BIZ=$BIZID V_TARGET=$V_TARGET"

biz_mod() { # $1=origin_port
  curl -s -b "$W/ck" -o /dev/null -w 'biz_put_http=%{http_code}\n' \
    -X PUT -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
    "$API/api/businesses/$BIZID" -d "$(python3 -c "
import json,sys
b=json.loads('''$ORIG''')
b['origin_host']='1.1.1.1'; b['origin_port']=$1; b['sni_entry_id']='$SNI_OF_BIZ'
print(json.dumps(b))")"
}

dump() {
python3 - "$1" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
for k in ("status","phase","version","prev_version","rolled_back","rollback_verified",
          "rollback_verify_ran","rollback_restored","rollback_outcome","rollback_code",
          "no_rollback_needed"):
    print("%-22s = %s" % (k, d.get(k)))
print("rollback_note          =", d.get("rollback_note"))
mf = d.get("manual_fix") or []
print("manual_fix (%d lines):" % len(mf))
for l in mf: print("      ", l)
print("rollback_checks:")
for l in (d.get("rollback_checks") or []): print("      -", l)
PY
}

echo
echo "##################### 4A：回滚目标统计套接字不可读 ⇒ 回滚验证失败 #####################"
SOCK="$SD/stats-v$V_TARGET.sock"
echo "target_socket=$SOCK  (mode before: $(stat -c '%a %U:%G' "$SOCK" 2>/dev/null))"
chmod 000 "$SOCK"
echo "mode after chmod 000: $(stat -c '%a' "$SOCK")"
printf 'once\n' > "$SD/rbui-reload-mode"
touch "$SD/rbui-reload-fail-once"
biz_mod 446
curl -s -b "$W/ck" -o "$W/p4a.json" -w 'publish_http=%{http_code}\n' \
  -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/$NODE/publish" -d '{"note":"B2 4A：回滚目标 socket 不可读"}'
dump "$W/p4a.json"
chmod 660 "$SOCK"
echo "mode restored: $(stat -c '%a' "$SOCK")"
echo "current/VERSION = $(cat $CR/current/VERSION)  (应仍为 $V_TARGET)"
python3 - <<'PY'
import json
d = json.load(open("/tmp/b2/p4a.json"))
ck = []
ck.append(("outcome == verify_failed", d.get("rollback_outcome") == "verify_failed"))
ck.append(("rollback_verify_ran == True（验证确实执行了）", d.get("rollback_verify_ran") is True))
ck.append(("rollback_verified == False", d.get("rollback_verified") is False))
ck.append(("rolled_back == False（没通过就不许说已回滚）", d.get("rolled_back") is False))
ck.append(("rollback_restored == True（文件确实恢复了）", d.get("rollback_restored") is True))
ck.append(("note 不含「并验证通过」", "并验证通过" not in (d.get("rollback_note") or "")))
ck.append(("manual_fix 非空", bool(d.get("manual_fix"))))
for n, o in ck: print(("  PASS  " if o else "  FAIL  ") + n)
print("SCENARIO_4A=" + ("PASS" if all(o for _, o in ck) else "FAIL"))
PY

echo
echo "##################### 4B：回滚目标缺状态快照 ⇒ 已恢复但未验证 #####################"
SNAP="$CR/versions/$NODE/v$V_TARGET/state.json"
echo "snapshot=$SNAP"
mv "$SNAP" "$SNAP.b2moved"; ls -l "$SNAP.b2moved"
printf 'once\n' > "$SD/rbui-reload-mode"
touch "$SD/rbui-reload-fail-once"
biz_mod 447
curl -s -b "$W/ck" -o "$W/p4b.json" -w 'publish_http=%{http_code}\n' \
  -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/$NODE/publish" -d '{"note":"B2 4B：回滚目标无状态快照"}'
dump "$W/p4b.json"
mv "$SNAP.b2moved" "$SNAP"; ls -l "$SNAP"
echo "current/VERSION = $(cat $CR/current/VERSION)  (应仍为 $V_TARGET)"
python3 - <<'PY'
import json
d = json.load(open("/tmp/b2/p4b.json"))
ck = []
ck.append(("outcome == restored_not_verified", d.get("rollback_outcome") == "restored_not_verified"))
ck.append(("rollback_verify_ran == False（验证没执行）", d.get("rollback_verify_ran") is False))
ck.append(("rollback_verified == False", d.get("rollback_verified") is False))
ck.append(("rolled_back == False", d.get("rolled_back") is False))
ck.append(("rollback_restored == True（文件已恢复）", d.get("rollback_restored") is True))
ck.append(("rollback_code == rollback_verification_not_run", d.get("rollback_code") == "rollback_verification_not_run"))
ck.append(("note 含「回滚验证没有执行」", "回滚验证没有执行" in (d.get("rollback_note") or "")))
ck.append(("note 不含「并验证通过」", "并验证通过" not in (d.get("rollback_note") or "")))
ck.append(("manual_fix 非空", bool(d.get("manual_fix"))))
for n, o in ck: print(("  PASS  " if o else "  FAIL  ") + n)
print("SCENARIO_4B=" + ("PASS" if all(o for _, o in ck) else "FAIL"))
PY

echo
echo "##################### 收尾：恢复正常 reload，并把业务内容改回去 #####################"
printf 'off\n' > "$SD/rbui-reload-mode"
rm -f "$SD/rbui-reload-fail-once"
systemctl reload shengyu-edgelink-haproxy; echo "reload_exit=$?"
biz_mod 443
echo "current/VERSION = $(cat $CR/current/VERSION)"
timeout 8 openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com -brief </dev/null 2>&1 | head -2
echo "STEP4_DONE"
