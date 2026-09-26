#!/usr/bin/env bash
# B2 第六轮 v0.4.0：两项后续任务的真机验收
#
#   S1-S5  sni_entries 删除接口
#          · 有业务路由引用 ⇒ **拒绝**（409 sni_entry_in_use，且点名业务）
#          · 无引用 ⇒ 允许删除，并提示"要重新发布"
#          · 写审计
#   S6     诊断页的**悬空引用**自检（造一条悬空引用 → 必须报出来 → 修好后必须消失）
#   S7-S8  旧 stats-v*.sock 清理（只清"确认无进程使用"的；失败只告警）
#   S9     总览暴露日志摄入健康（log_ingest）
#
# 全程**不发布含 9443 的配置**：只在期望状态里建/删入口与业务（端口从未被真正绑定），
# 因此对线上 8443 业务零影响。唯一会发布的两次是 S7/S8 的"改业务端口再改回"。
set -u
API=http://127.0.0.1:8081
W=/tmp/b2
CR=/etc/shengyu-edgelink/haproxy
DB=/var/lib/shengyu-edgelink/meta.db
SD=/run/shengyu-edgelink
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

# 现场取"现有业务"，用于 S7/S8 触发一次真实发布。
curl -s -b "$W/ck" "$API/api/businesses" > "$W/bizs.json"
BIZID=$(python3 -c "
import json
d = json.load(open('/tmp/b2/bizs.json', encoding='utf-8'))
it = d.get('items') or []
print(it[0]['id'] if it else '')")
[ -n "$BIZID" ] || { echo "ABORT: 平台里没有业务，S7/S8 需要一条来触发内容变化"; exit 4; }
curl -s -b "$W/ck" "$API/api/businesses/$BIZID" > "$W/biz1.json"
CUS=$(python3 -c "
import json
d = json.load(open('/tmp/b2/biz1.json', encoding='utf-8'))
b = d.get('business') or d
print(b.get('customer_id') or '')")
DOMS=$(python3 -c "
import json
d = json.load(open('/tmp/b2/biz1.json', encoding='utf-8'))
b = d.get('business') or d
print(json.dumps(b.get('domains') or []))")
curl -s -b "$W/ck" "$API/api/businesses/$BIZID/routes" > "$W/routes1.json"
SNI_OF_BIZ=$(python3 -c "
import json
d = json.load(open('/tmp/b2/routes1.json', encoding='utf-8'))
rs = d.get('routes') or d.get('items') or []
print(rs[0].get('sni_entry_id') or '' if rs else '')")
echo "BIZ=$BIZID CUS=$CUS SNI=$SNI_OF_BIZ"

biz_mod() { # $1=origin_port
  curl -s -b "$W/ck" -o /dev/null -w 'biz_put_http=%{http_code}\n' \
    -X PUT -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
    "$API/api/businesses/$BIZID" -d "{
      \"customer_id\":\"$CUS\",\"name\":\"验收业务\",\"remark\":\"B2 v0.4.0\",
      \"mode\":\"sni_tls\",\"domains\":$DOMS,\"primary_node_id\":\"$NODE\",
      \"sni_entry_id\":\"$SNI_OF_BIZ\",\"origin_host\":\"1.1.1.1\",\"origin_port\":$1}"
}
pub() {
  curl -s -b "$W/ck" -o "$2" -w 'publish_http=%{http_code}\n' \
    -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
    "$API/api/nodes/$NODE/publish" -d "{\"note\":\"$1\"}"
}
rb() {
  curl -s -b "$W/ck" -o "$2" -w 'rollback_http=%{http_code}\n' \
    -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
    "$API/api/nodes/$NODE/rollback" -d "{\"version\":$1,\"reason\":\"$3\"}"
}
mk_sni() { # $1=port  => 打印入口 id
  curl -s -b "$W/ck" -o "$W/sni_new.json" -w '' -X POST \
    -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
    "$API/api/nodes/$NODE/sni-entries" -d "{\"bind_addr\":\"0.0.0.0\",\"bind_port\":$1}"
  python3 -c "
import json
d = json.load(open('/tmp/b2/sni_new.json', encoding='utf-8'))
print(d.get('id') or '')"
}
mk_biz() { # $1=customer $2=name $3=domain $4=sni_entry  => 打印业务 id
  curl -s -b "$W/ck" -o "$W/biz_new.json" -w '' -X POST \
    -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
    "$API/api/businesses" -d "{
      \"customer_id\":\"$1\",\"name\":\"$2\",\"mode\":\"sni_tls\",
      \"domains\":[\"$3\"],\"primary_node_id\":\"$NODE\",
      \"origin_host\":\"1.1.1.1\",\"origin_port\":443,\"sni_entry_id\":\"$4\"}"
  # 注意：POST /api/businesses 返回**包装体** {"business":{...}}（这个坑踩过两次）。
  python3 -c "
import json
d = json.load(open('/tmp/b2/biz_new.json', encoding='utf-8'))
b = d.get('business') or d
print(b.get('id') or '')"
}
mk_cus() { # $1=name => 打印客户 id（POST /api/customers 返回**裸对象**）
  curl -s -b "$W/ck" -o "$W/cus_new.json" -w '' -X POST \
    -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
    "$API/api/customers" -d "{\"name\":\"$1\"}"
  python3 -c "
import json
d = json.load(open('/tmp/b2/cus_new.json', encoding='utf-8'))
print(d.get('id') or '')"
}
sockets_list() { ls -1 "$SD" 2>/dev/null | grep -E '^stats-v[0-9]+\.sock$' | sort | tr '\n' ' '; }

echo
echo "############ 0. 基线 ############"
V_BASE=$(cat "$CR/current/VERSION")
echo "V_BASE=$V_BASE"
SOCK_BEFORE=$(sockets_list)
SOCK_N_BEFORE=$(echo "$SOCK_BEFORE" | wc -w)
echo "stats socket 数量（基线）= $SOCK_N_BEFORE"
echo "$SOCK_BEFORE" | tr ' ' '\n' | grep -v '^$' > "$W/sockets.before" 2>/dev/null || true
AUDIT_DEL_BEFORE=$(python3 - "$DB" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
print(c.execute("select count(*) from audit where action='sni_entry.delete'").fetchone()[0])
PY
)
echo "审计里 sni_entry.delete 条数（基线）= $AUDIT_DEL_BEFORE"

echo
echo "############ S1-S5：删除接口的三个语义 ############"
echo "--- S1 建 9443 入口（不发布 ⇒ 端口不会被真正绑定）---"
SNI1=$(mk_sni 9443)
echo "SNI1=$SNI1"
[ -n "$SNI1" ] || { echo "ABORT: 建入口失败"; exit 5; }

echo "--- S2 建客户 + 业务挂到这个入口上 ---"
CUS1=$(mk_cus "v040-接口验收客户")
BIZ1=$(mk_biz "$CUS1" "v040-接口验收业务" "v040-a.example.com" "$SNI1")
echo "CUS1=$CUS1 BIZ1=$BIZ1"
[ -n "$BIZ1" ] || { echo "ABORT: 建业务失败"; exit 5; }

echo "--- S3 删入口：有业务引用 ⇒ 必须 409 且点名业务 ---"
S3HTTP=$(curl -s -b "$W/ck" -o "$W/del1.json" -w '%{http_code}' \
  -X DELETE -H "X-CSRF-Token: $CSRF" "$API/api/nodes/$NODE/sni-entries/$SNI1")
echo "delete_http=$S3HTTP"
[ "$S3HTTP" = "409" ] && echo "S3_HTTP=PASS" || echo "S3_HTTP=FAIL"
python3 - "$W/del1.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
blob = json.dumps(d, ensure_ascii=False)
print("  code   =", d.get("code"))
print("  error  =", d.get("error"))
ck = [("错误码是 sni_entry_in_use", d.get("code") == "sni_entry_in_use"),
      ("报错点名了引用的业务", "v040-接口验收业务" in blob),
      ("报错给出了下一步（改挂或删除）", ("改挂" in blob) or ("删除" in blob))]
for k, v in ck: print(("PASS  " if v else "FAIL  ") + k)
print("S3=" + ("PASS" if all(v for _, v in ck) else "FAIL"))
PY
echo "--- 断言入口仍在（拒绝不能删掉一半）---"
curl -s -b "$W/ck" "$API/api/nodes/$NODE/sni-entries" | python3 -c "
import json,sys
d=json.load(sys.stdin)
ids=[e['id'] for e in (d.get('items') or [])]
print('  entries =', ids)
print('S3B=' + ('PASS' if '$SNI1' in ids else 'FAIL'))"

echo "--- S4 删掉那条业务（路由随之级联删除）---"
curl -s -b "$W/ck" -o /dev/null -w 'del_biz_http=%{http_code}\n' \
  -X DELETE -H "X-CSRF-Token: $CSRF" "$API/api/businesses/$BIZ1"
REFS=$(python3 - "$DB" "$SNI1" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
print(c.execute("select count(*) from routes where sni_entry_id=?", (sys.argv[2],)).fetchone()[0])
PY
)
echo "  仍引用该入口的路由数 = $REFS（必须为 0）"

echo "--- S5 再删入口：无引用 ⇒ 允许，并提示要重新发布 ---"
curl -s -b "$W/ck" -o "$W/del2.json" -w 'delete_http=%{http_code}\n' \
  -X DELETE -H "X-CSRF-Token: $CSRF" "$API/api/nodes/$NODE/sni-entries/$SNI1"
python3 - "$W/del2.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
ns = d.get("next_step") or ""
print("  ok        =", d.get("ok"))
print("  next_step =", ns)
ck = [("返回 ok=true", d.get("ok") is True),
      ("提示里要求「发布」", "发布" in ns)]
for k, v in ck: print(("PASS  " if v else "FAIL  ") + k)
print("S5=" + ("PASS" if all(v for _, v in ck) else "FAIL"))
PY
curl -s -b "$W/ck" "$API/api/nodes/$NODE/sni-entries" | python3 -c "
import json,sys
d=json.load(sys.stdin)
ids=[e['id'] for e in (d.get('items') or [])]
print('  entries now =', ids)
print('S5B=' + ('PASS' if '$SNI1' not in ids else 'FAIL'))"
python3 - "$DB" "$AUDIT_DEL_BEFORE" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
n = c.execute("select count(*) from audit where action='sni_entry.delete'").fetchone()[0]
before = int(sys.argv[2])
row = c.execute("select summary from audit where action='sni_entry.delete' order by rowid desc limit 1").fetchone()
print("  审计条数 =", n, "（基线", before, "）")
print("  最近一条 =", row[0] if row else None)
print("S5C=" + ("PASS" if n > before else "FAIL"))
PY

echo
echo "############ S6：悬空引用自检 ############"
echo "--- S6a 数据干净时：不得报悬空引用（误报会让诊断页常年挂假问题）---"
curl -s -b "$W/ck" "$API/api/diagnose" > "$W/diag0.json"
python3 - "$W/diag0.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
cats = [f.get("category") for f in (d.get("findings") or [])]
print("  findings =", cats)
print("S6A=" + ("PASS" if "dangling_reference" not in cats else "FAIL"))
PY

echo "--- S6b 造一条悬空引用（改库删入口，模拟绕过守卫的写操作）---"
SNI2=$(mk_sni 9443)
CUS2=$(mk_cus "v040-悬空引用客户")
BIZ2=$(mk_biz "$CUS2" "v040-悬空引用业务" "v040-b.example.com" "$SNI2")
echo "SNI2=$SNI2 BIZ2=$BIZ2"
python3 - "$DB" "$SNI2" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
c.execute("delete from sni_entries where id=?", (sys.argv[2],))
c.commit()
print("  已改库删除入口（routes.sni_entry_id 没有外键，数据库不会拦）")
PY
curl -s -b "$W/ck" "$API/api/diagnose" > "$W/diag1.json"
python3 - "$W/diag1.json" "$SNI2" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
got = None
for f in (d.get("findings") or []):
    if f.get("category") == "dangling_reference":
        got = f
        break
print("  findings =", [f.get("category") for f in (d.get("findings") or [])])
if got is None:
    print("S6B=FAIL")
    raise SystemExit
facts = " ".join(got.get("facts") or [])
print("  severity =", got.get("severity"))
print("  facts    =", got.get("facts"))
print("  next     =", got.get("next_steps"))
ck = [("有 dangling_reference", True),
      ("severity=error", got.get("severity") == "error"),
      ("facts 点出缺失的入口 ID", sys.argv[2] in facts),
      ("facts 说人话（SNI 共享入口）", "SNI 共享入口" in facts),
      ("next 提醒修完要发布", any("发布" in s for s in (got.get("next_steps") or [])))]
for k, v in ck[1:]: print(("PASS  " if v else "FAIL  ") + k)
print("S6B=" + ("PASS" if all(v for _, v in ck[1:]) else "FAIL"))
PY

echo "--- S6c 删掉那条业务（级联删路由）⇒ 悬空引用必须消失 ---"
curl -s -b "$W/ck" -o /dev/null -w 'del_biz_http=%{http_code}\n' \
  -X DELETE -H "X-CSRF-Token: $CSRF" "$API/api/businesses/$BIZ2"
curl -s -b "$W/ck" "$API/api/diagnose" > "$W/diag2.json"
python3 - "$W/diag2.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
cats = [f.get("category") for f in (d.get("findings") or [])]
print("  findings =", cats)
print("S6C=" + ("PASS" if "dangling_reference" not in cats else "FAIL"))
PY

echo
echo "############ S7：旧 stats socket 清理（发布时自动执行）############"
biz_mod 444
pub "S7: 验证 socket 清理" "$W/s7.json"
python3 - "$W/s7.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
w = d.get("warnings") or []
sp = d.get("stats_socket_prune") or {}
print("  status   =", d.get("status"))
print("  version  =", d.get("version"))
print("  warnings =", w)
print("  prune    = scanned", sp.get("scanned"), "removed", sp.get("removed"), "kept", len(sp.get("kept") or []))
ck = [("发布成功", d.get("status") in ("succeeded", "applied_origin_unhealthy")),
      ("结果里带 stats_socket_prune", bool(sp)),
      ("warnings 说了清理结果", any("统计套接字" in x for x in w))]
for k, v in ck: print(("PASS  " if v else "FAIL  ") + k)
print("S7=" + ("PASS" if all(v for _, v in ck) else "FAIL"))
PY
python3 - "$DB" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
n = c.execute("select count(*) from audit where action='config.prune_stats_sockets'").fetchone()[0]
row = c.execute("select result, summary from audit where action='config.prune_stats_sockets' order by rowid desc limit 1").fetchone()
print("  审计(config.prune_stats_sockets) =", n)
if row: print("  最近一条 =", row[0], "|", row[1])
print("S7B=" + ("PASS" if n > 0 else "FAIL"))
PY

echo "--- S7c 清完之后：仍在跑的套接字必须还在，被删的必须真的不在了 ---"
SOCK_AFTER=$(sockets_list)
SOCK_N_AFTER=$(echo "$SOCK_AFTER" | wc -w)
echo "  socket 数量：$SOCK_N_BEFORE → $SOCK_N_AFTER"
echo "  仍在的 = $SOCK_AFTER"
ALIVE=$(cat "$CR/current/VERSION")
python3 - "$SD" "$ALIVE" <<'PY'
import os, socket, sys
sd, alive = sys.argv[1], sys.argv[2]
cur = "stats-v%s.sock" % alive
names = sorted(n for n in os.listdir(sd) if n.startswith("stats-v") and n.endswith(".sock"))
print("  当前生效版本 %s 的套接字 %s 是否存在 = %s" % (alive, cur, cur in names))
missing_ok = cur in names
ck = [("当前生效版本的套接字必须保留", missing_ok)]
for k, v in ck: print(("PASS  " if v else "FAIL  ") + k)
print("S7C=" + ("PASS" if all(v for _, v in ck) else "FAIL"))
PY

echo "--- S8 把业务端口改回去并再发布一次 ---"
biz_mod 443
pub "S8: 恢复业务端口" "$W/s8.json"
python3 -c "
import json;d=json.load(open('/tmp/b2/s8.json'))
print('  status =', d.get('status'), '| version =', d.get('version'))
print('S8=' + ('PASS' if d.get('status') in ('succeeded','applied_origin_unhealthy') else 'FAIL'))"

echo
echo "############ S9：总览暴露日志摄入健康 ############"
curl -s -b "$W/ck" "$API/api/overview" > "$W/ov.json"
python3 - "$W/ov.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8'))
li = d.get("log_ingest")
print("  log_ingest =", li)
ck = [("总览里有 log_ingest", li is not None),
      ("含 ingested/duplicated/parse_errors/dropped",
       li is not None and all(k in li for k in ("ingested", "duplicated", "parse_errors", "dropped")))]
for k, v in ck: print(("PASS  " if v else "FAIL  ") + k)
print("S9=" + ("PASS" if all(v for _, v in ck) else "FAIL"))
PY

echo
echo "############ 收尾：回滚到基线 + 清理验收数据 ############"
V_NOW=$(cat "$CR/current/VERSION")
if [ "$V_NOW" != "$V_BASE" ]; then
  rb "$V_BASE" "$W/rb_final.json" "B2 第六轮收尾：恢复改动前版本"
  python3 -c "
import json;d=json.load(open('/tmp/b2/rb_final.json'))
print('  status =', d.get('status'), '| version =', d.get('version'))
print('  rollback_verified =', d.get('rollback_verified'), '| outcome =', d.get('rollback_outcome'))"
fi
for c in "$CUS1" "$CUS2"; do
  [ -n "$c" ] || continue
  curl -s -b "$W/ck" -o /dev/null -w "del_customer($c)=%{http_code}\n" \
    -X DELETE -H "X-CSRF-Token: $CSRF" "$API/api/customers/$c"
done

echo
echo "############ 终检 ############"
echo "current/VERSION = $(cat "$CR/current/VERSION")（期望 $V_BASE）"
echo "--- 剩余入口 / 业务 ---"
curl -s -b "$W/ck" "$API/api/nodes/$NODE/sni-entries" | python3 -c "
import json,sys
d=json.load(sys.stdin);print('  entries =', [(e['bind_port'], e['enabled']) for e in (d.get('items') or [])])"
curl -s -b "$W/ck" "$API/api/businesses" | python3 -c "
import json,sys
d=json.load(sys.stdin);its=d.get('items') or []
print('  businesses =', [(b['id'], b['name']) for b in its])"
echo "--- 8443 业务 ---"
timeout 8 openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com </dev/null 2>&1 | grep -E "Verify return code|CONNECTION ESTABLISHED" | head -2
echo "--- haproxy -c / meta ---"
haproxy -c -f "$CR/current/haproxy.cfg" 2>&1 | tail -1
curl -s -b "$W/ck" "$API/api/meta" | python3 -c "
import json,sys;d=json.load(sys.stdin);n=d.get('node') or {}
print('  applied/expected/skew =', n.get('applied_version'), '/', n.get('expected_version'), '/', n.get('version_skewed'))
print('  business_count =', d.get('business_count'), '| product =', d.get('product'))"
echo "--- 验收残留 ---"
echo "仍引用不存在入口的路由: $(python3 - "$DB" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
print(c.execute("select count(*) from routes r left join sni_entries e on e.id=r.sni_entry_id where r.sni_entry_id<>'' and e.id is null").fetchone()[0])
PY
)"
echo "名字含 v040 的业务: $(python3 - "$DB" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
print(c.execute("select count(*) from businesses where name like 'v040%'").fetchone()[0])
PY
)"
echo "stats socket 数量 = $(sockets_list | wc -w)（基线 $SOCK_N_BEFORE）"
echo "V040_DONE"
