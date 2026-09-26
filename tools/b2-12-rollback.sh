#!/usr/bin/env bash
# B2 第二轮 - 第 2 步：回滚相关的五种结论（真机逐一构造）
#
#   S3  数据面自愈（reload 失败一次）      → 期望 not_needed（不是"已回滚"，更不是"并验证通过"）
#   S4  reload 返回 0 但什么都没做        → 期望 verified：真回滚 + 四项判据全部成立（真机首次拿到）
#   S5  回滚目标统计套接字不可读           → 期望 verify_failed（证明 Verify **真的在执行**）
#   S6  回滚目标缺状态快照（清单同步去掉） → 期望 restored_not_verified
#   S7  回滚目标目录不完整（清单仍列着）   → 期望 failed + 告警「current/ 停留在未获批准版本」
#   S8  向别的/不存在的 nodeID 发布        → 期望被拒绝
set -u
API=http://127.0.0.1:8081
W=/tmp/b2
CR=/etc/shengyu-edgelink/haproxy
SD=/run/shengyu-edgelink
D=/etc/systemd/system/shengyu-edgelink-haproxy.service.d
mkdir -p "$W"

login() {
  PW=$(grep -oE '[A-Za-z0-9]{32}' /root/shengyu-install.log | head -1)
  curl -s -c "$W/ck" -o "$W/login.json" -X POST "$API/api/login" \
    -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$PW\"}"
  CSRF=$(sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p' "$W/login.json")
}
login
NODE=$(cat "$W/node_id")
echo "NODE_ID=$NODE csrf_len=${#CSRF}"
BIZID=$(python3 -c "import json;d=json.load(open('/tmp/b2/biz1.json'));b=d.get('business') or d;print(b['id'])")
CUS=$(python3 -c "import json;d=json.load(open('/tmp/b2/biz1.json'));b=d.get('business') or d;print(b.get('customer_id',''))")
DOMS=$(python3 -c "import json;d=json.load(open('/tmp/b2/biz1.json'));b=d.get('business') or d;print(json.dumps(b.get('domains') or []))")
SNI_OF_BIZ=$(python3 -c "import json;d=json.load(open('/tmp/b2/routes1.json'));rs=d.get('routes') or d.get('items') or [];print(rs[0].get('sni_entry_id',''))")
echo "BIZ=$BIZID CUS=$CUS SNI=$SNI_OF_BIZ DOMS=$DOMS"

biz_mod() { # $1=origin_port
  curl -s -b "$W/ck" -o /dev/null -w 'biz_put_http=%{http_code}\n' \
    -X PUT -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
    "$API/api/businesses/$BIZID" -d "{
      \"customer_id\":\"$CUS\",\"name\":\"验收业务\",\"remark\":\"B2 pass2\",
      \"mode\":\"sni_tls\",\"domains\":$DOMS,\"primary_node_id\":\"$NODE\",
      \"sni_entry_id\":\"$SNI_OF_BIZ\",\"origin_host\":\"1.1.1.1\",\"origin_port\":$1}"
}

pub() { # $1=note  $2=out
  curl -s -b "$W/ck" -o "$2" -w 'publish_http=%{http_code}\n' \
    -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
    "$API/api/nodes/$NODE/publish" -d "{\"note\":\"$1\"}"
}
dump() {
python3 - "$1" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
for k in ("status","phase","rollback_outcome","rollback_code","rolled_back","rollback_verified",
          "rollback_verify_ran","rollback_restored","no_rollback_needed"):
    print("%-22s = %s" % (k, d.get(k)))
print("rollback_note          =", d.get("rollback_note"))
mf = d.get("manual_fix") or []
print("manual_fix (%d lines):" % len(mf))
for l in mf: print("      ", l)
print("rollback_checks:")
for l in (d.get("rollback_checks") or []): print("      -", l)
PY
}
expect() { python3 - "$1" "$2" <<'PY'
import json, sys
d = json.load(open(sys.argv[1])); scen = sys.argv[2]
note = d.get("rollback_note") or ""
mf = "\n".join(d.get("manual_fix") or [])
ck = d.get("rollback_checks") or []
if scen == "S3":
    c = [("outcome == not_needed", d.get("rollback_outcome") == "not_needed"),
         ("no_rollback_needed == True", d.get("no_rollback_needed") is True),
         ("rolled_back == False（没有发生切换）", d.get("rolled_back") is False),
         ("rollback_verified == True（该版确实被验证过）", d.get("rollback_verified") is True),
         ("rollback_verify_ran == True", d.get("rollback_verify_ran") is True),
         ("note 含「仍在正常运行」", "仍在正常运行" in note),
         ("note **不含**「并验证通过」", "并验证通过" not in note),
         ("manual_fix 为空（确认安全）", not (d.get("manual_fix") or []))]
elif scen == "S4":
    c = [("rolled_back == True", d.get("rolled_back") is True),
         ("rollback_verified == True", d.get("rollback_verified") is True),
         ("rollback_verify_ran == True", d.get("rollback_verify_ran") is True),
         ("rollback_restored == True", d.get("rollback_restored") is True),
         ("outcome == verified", d.get("rollback_outcome") == "verified"),
         ("rollback_code 为空", not d.get("rollback_code")),
         ("note 含「并验证通过」", "并验证通过" in note),
         ("判据四条齐全", all(any(k in x for x in ck) for k in ("文件集合","版本标记","统计套接字","监听归属"))),
         ("manual_fix 为空", not (d.get("manual_fix") or []))]
elif scen == "S5":
    c = [("outcome == verify_failed", d.get("rollback_outcome") == "verify_failed"),
         ("rollback_verify_ran == True（Verify 真的执行了）", d.get("rollback_verify_ran") is True),
         ("rollback_verified == False", d.get("rollback_verified") is False),
         ("rolled_back == False", d.get("rolled_back") is False),
         ("rollback_restored == True（文件确实恢复了）", d.get("rollback_restored") is True),
         ("note 含「无法确认」", "无法确认" in note),
         ("note **不含**「并验证通过」", "并验证通过" not in note),
         ("manual_fix 非空", bool(d.get("manual_fix")))]
elif scen == "S6":
    c = [("outcome == restored_not_verified", d.get("rollback_outcome") == "restored_not_verified"),
         ("rollback_verify_ran == False（验证没执行）", d.get("rollback_verify_ran") is False),
         ("rollback_verified == False", d.get("rollback_verified") is False),
         ("rollback_restored == True（文件已恢复）", d.get("rollback_restored") is True),
         ("code == rollback_verification_not_run", d.get("rollback_code") == "rollback_verification_not_run"),
         ("note 含「无法确认」", "无法确认" in note),
         ("note **不含**「并验证通过」", "并验证通过" not in note),
         ("manual_fix 非空", bool(d.get("manual_fix")))]
elif scen == "S7":
    c = [("outcome == failed", d.get("rollback_outcome") == "failed"),
         ("code == rollback_restore_failed", d.get("rollback_code") == "rollback_restore_failed"),
         ("rolled_back == False", d.get("rolled_back") is False),
         ("note 含「警告：current/ 目前停留在第」", "警告：current/ 目前停留在第" in note),
         ("note 含「不要执行 systemctl reload」", "不要执行 systemctl reload" in note),
         ("note **不含**「并验证通过」", "并验证通过" not in note),
         ("manual_fix 第一条就是「先别 reload」", "不要执行 systemctl reload" in mf.split("\n")[0])]
else:
    c = []
for n, o in c: print(("  PASS  " if o else "  FAIL  ") + n)
print(scen + "=" + ("PASS" if c and all(o for _, o in c) else "FAIL"))
PY
}

echo
echo "############ 0. 装 reload 注入器（mode 可切 always/once/silent/off）############"
cat > /usr/local/bin/rbui-reload-inject.sh <<'EOF'
#!/bin/sh
# [B2 PASS2 ONLY] 临时注入器；验收结束由清理脚本删除
SD=/run/shengyu-edgelink
MODE=$(cat "$SD/rbui-reload-mode" 2>/dev/null || echo off)
case "$MODE" in
  always) echo "B2 injected reload failure (always)" >&2; exit 1 ;;
  silent) echo "B2 injected reload no-op (silent)" >&2; exit 0 ;;
  once)
    if [ -e "$SD/rbui-reload-fail-once" ]; then rm -f "$SD/rbui-reload-fail-once"; echo "B2 injected reload failure (once)" >&2; exit 1; fi ;;
esac
exec /bin/kill -USR2 "$(cat "$SD/haproxy.pid")"
EOF
chmod 0755 /usr/local/bin/rbui-reload-inject.sh
mkdir -p "$D"
printf '[Service]\nExecReload=\nExecReload=/usr/local/bin/rbui-reload-inject.sh\n' > "$D/10-b2-reload-inject.conf"
systemctl daemon-reload
echo "ExecReload = $(systemctl show -p ExecReload --value shengyu-edgelink-haproxy | head -c 120)"
printf 'always\n' > "$SD/rbui-reload-mode"
systemctl reload shengyu-edgelink-haproxy; echo "selftest_always_expect_fail=$?"
printf 'once\n' > "$SD/rbui-reload-mode"
systemctl reload shengyu-edgelink-haproxy; echo "selftest_once_noflag_expect_ok=$?"

echo
echo "##################### S3：reload 失败一次 → 数据面自愈 → not_needed #####################"
printf 'once\n' > "$SD/rbui-reload-mode"; rm -f "$SD/rbui-reload-fail-once"
V_BEFORE=$(cat "$CR/current/VERSION"); echo "V_BEFORE=$V_BEFORE"
touch "$SD/rbui-reload-fail-once"
biz_mod 451
pub "S3: reload 只失败一次" "$W/s3r.json"
dump "$W/s3r.json"
echo "current/VERSION=$(cat $CR/current/VERSION)（期望仍为 $V_BEFORE）"
[ "$(cat $CR/current/VERSION)" = "$V_BEFORE" ] && echo "CHECK_VERSION_NOT_ADVANCED=PASS" || echo "CHECK_VERSION_NOT_ADVANCED=FAIL"
expect "$W/s3r.json" S3

echo
echo "##################### S4：reload 返回 0 但什么都没做 → 真回滚 #####################"
echo "（waitVerify 会轮询等满 30 秒才判失败，请稍候）"
printf 'silent\n' > "$SD/rbui-reload-mode"
V_BEFORE=$(cat "$CR/current/VERSION"); echo "V_BEFORE=$V_BEFORE"
biz_mod 452
pub "S4: reload 是静默空操作" "$W/s4.json"
dump "$W/s4.json"
echo "current/VERSION=$(cat $CR/current/VERSION)（期望回到 $V_BEFORE）"
[ "$(cat $CR/current/VERSION)" = "$V_BEFORE" ] && echo "CHECK_VERSION_RESTORED=PASS" || echo "CHECK_VERSION_RESTORED=FAIL"
expect "$W/s4.json" S4
echo "--- 独立取证：running worker 与 current/ 是否一致 ---"
echo "stats sockets:"; ls -1 "$SD" | grep '^stats-v' | sed 's/^/    /'
echo -n "8443 handshake = "; timeout 8 openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com -brief </dev/null 2>&1 | head -1
echo "haproxy -c = $(haproxy -c -f $CR/current/haproxy.cfg 2>&1 | tail -1)"
printf 'off\n' > "$SD/rbui-reload-mode"; systemctl reload shengyu-edgelink-haproxy; echo "real_reload_exit=$?"

echo
echo "##################### S5：回滚目标 stats socket 不可读 → verify_failed #####################"
V_T=$(cat "$CR/current/VERSION"); SOCK="$SD/stats-v$V_T.sock"
echo "target=$V_T socket=$SOCK mode_before=$(stat -c '%a' "$SOCK")"
chmod 000 "$SOCK"
printf 'once\n' > "$SD/rbui-reload-mode"; rm -f "$SD/rbui-reload-fail-once"; touch "$SD/rbui-reload-fail-once"
biz_mod 453
pub "S5: 回滚目标 socket 不可读" "$W/s5.json"
dump "$W/s5.json"
chmod 660 "$SOCK"; echo "socket mode restored = $(stat -c '%a' "$SOCK")"
echo "current/VERSION=$(cat $CR/current/VERSION)（期望仍为 $V_T）"
expect "$W/s5.json" S5

echo
echo "##################### S6：回滚目标缺状态快照（清单同步去掉）→ restored_not_verified #####################"
V_T=$(cat "$CR/current/VERSION"); VD="$CR/versions/$NODE/v$V_T"
echo "target=$V_T dir=$VD"
# 关键：移出**版本目录**，不要在里面改名。
# 在目录里改名会留下一个"不在清单内"的文件，于是 versionFileSet 先报
# "目录与清单不一致，拒绝发布"，结论落在 failed —— 测不到 restored_not_verified。
mkdir -p /tmp/b2/moved
mv "$VD/state.json" "/tmp/b2/moved/v$V_T-state.json" && echo "state.json -> /tmp/b2/moved/v$V_T-state.json"
python3 - "$VD/manifest.json" <<'PY'
import json, sys
p = sys.argv[1]
m = json.load(open(p))
m["files"] = [f for f in m["files"] if f != "state.json"]
json.dump(m, open(p, "w"), ensure_ascii=False, indent=2)
print("manifest.files ->", m["files"])
PY
printf 'once\n' > "$SD/rbui-reload-mode"; rm -f "$SD/rbui-reload-fail-once"; touch "$SD/rbui-reload-fail-once"
biz_mod 454
pub "S6: 回滚目标缺状态快照" "$W/s6.json"
dump "$W/s6.json"
mv -f "/tmp/b2/moved/v$V_T-state.json" "$VD/state.json"
python3 - "$VD/manifest.json" <<'PY'
import json, sys
p = sys.argv[1]
m = json.load(open(p))
if "state.json" not in m["files"]:
    m["files"].append("state.json"); m["files"].sort()
json.dump(m, open(p, "w"), ensure_ascii=False, indent=2)
print("manifest.files restored ->", m["files"])
PY
expect "$W/s6.json" S6

echo
echo "##################### S7：回滚目标目录不完整（清单仍列着）→ failed + 告警 #####################"
V_T=$(cat "$CR/current/VERSION"); VD="$CR/versions/$NODE/v$V_T"
echo "target=$V_T dir=$VD"
mkdir -p /tmp/b2/moved
mv "$VD/state.json" "/tmp/b2/moved/v$V_T-state.json.s7" && echo "state.json -> /tmp/b2/moved/v$V_T-state.json.s7（清单仍列着它 ⇒ 目录不完整）"
printf 'once\n' > "$SD/rbui-reload-mode"; rm -f "$SD/rbui-reload-fail-once"; touch "$SD/rbui-reload-fail-once"
biz_mod 455
pub "S7: 回滚目标目录不完整" "$W/s7.json"
dump "$W/s7.json"
echo "current/VERSION=$(cat $CR/current/VERSION)（**预期会停在未获批准的新版本上**，这正是告警要说明的事）"
expect "$W/s7.json" S7
echo "--- 复原被改坏的版本目录 ---"
mv -f "/tmp/b2/moved/v$V_T-state.json.s7" "$VD/state.json"; ls -l "$VD/state.json"

echo
echo "##################### S8：向别的/不存在的 nodeID 发布 #####################"
curl -s -b "$W/ck" -o "$W/s8.json" -w 'publish_other_node_http=%{http_code}\n' \
  -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/node_not_mine_at_all/publish" -d '{"note":"S8 负向"}'
cat "$W/s8.json"; echo
python3 - <<'PY'
import json
d = json.load(open("/tmp/b2/s8.json"))
code = d.get("code"); err = d.get("error")
ok = code in ("not_found", "remote_node_unsupported")
print("拒绝原因 code =", code, "|", err)
print("S8=" + ("PASS" if ok else "FAIL"))
PY

echo
echo "############ 收尾：注入器归 off，业务内容改回 443 ############"
printf 'off\n' > "$SD/rbui-reload-mode"; rm -f "$SD/rbui-reload-fail-once"
biz_mod 443
systemctl reload shengyu-edgelink-haproxy; echo "reload_exit=$?"
echo "current/VERSION=$(cat $CR/current/VERSION)"
echo "STEP_ROLLBACK_DONE"
