#!/usr/bin/env bash
# B2 第二轮-补：诊断 + 修复被验收脚本写坏的数据 + 确认发布链路恢复
#
# 背景（第二轮真机日志）：验收脚本把 POST /api/businesses 的响应当成裸对象解析，
# 拿到空 id ⇒ "删业务"删了个空（404）⇒ 业务还在、它的路由还挂在 9443 入口上
# ⇒ 脚本再 SQL 删入口 ⇒ **留下悬空引用**（routes.sni_entry_id 没有外键）
# ⇒ 之后每次发布都被平台前置校验拦成 422 validation_failed（SNI_ENTRY_MISSING）。
#
# 这一步先把现场取下来（只读），再修（只删验收脚本自己造的数据），最后用
# 「内容未变 → 返回 no_change」这种无损探测确认发布链路真的通了。
set -u
API=http://127.0.0.1:8081
W=/tmp/b2
CR=/etc/shengyu-edgelink/haproxy
DB=/var/lib/shengyu-edgelink/meta.db
mkdir -p "$W"

login() {
  PW=$(grep -oE '[A-Za-z0-9]{32}' /root/shengyu-install.log | head -1)
  curl -s -c "$W/ck" -o "$W/login.json" -X POST "$API/api/login" \
    -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$PW\"}"
  CSRF=$(sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p' "$W/login.json")
}

echo "##################### PART A：诊断（只读） #####################"
echo "--- A1. 第二轮日志里的 S1/S2（看当时每个发布返回什么）---"
sed -n '/第 11 步/,/第 12 步/p' /root/b2-accept2.log 2>/dev/null | head -80

echo
echo "--- A2. 数据库现状 ---"
python3 - "$DB" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
print("sni_entries:")
for r in c.execute("select id,node_id,bind_addr,bind_port,enabled from sni_entries"):
    print("   ", r)
print("businesses:")
for r in c.execute("select id,name,mode,enabled from businesses"):
    print("   ", r)
print("routes (含 sni_entry_id):")
for r in c.execute("select id,business_id,node_id,mode,sni_entry_id,entry_addr,entry_port,origin_port,enabled from routes"):
    print("   ", r)
print("config_versions (最近 8 条):")
for r in c.execute("select version,status,substr(coalesce(error,''),1,60) from config_versions order by version desc limit 8"):
    print("   ", r)
print("--- 悬空引用检查（routes.sni_entry_id 指向不存在的入口）---")
bad = list(c.execute("""select r.id, r.business_id, r.sni_entry_id from routes r
                        where r.sni_entry_id<>'' and r.sni_entry_id not in (select id from sni_entries)"""))
for r in bad: print("   ORPHAN", r)
print("   orphan_count =", len(bad))
print("--- 路由引用的业务是否都存在 ---")
miss = list(c.execute("""select r.id, r.business_id from routes r
                         where r.business_id not in (select id from businesses)"""))
for r in miss: print("   DANGLING-BIZ", r)
print("   dangling_business_count =", len(miss))
c.close()
PY

echo
echo "--- A3. /preview（发布前的同一套校验；422 的原因就在这里）---"
login
NODE=$(cat "$W/node_id" 2>/dev/null || echo "")
if [ -z "$NODE" ]; then
  NODE=$(curl -s -b "$W/ck" "$API/api/nodes" | python3 -c "
import json,sys;d=json.load(sys.stdin);its=d.get('items') or d.get('nodes') or []
print(its[0]['id'] if its else '')")
  echo "$NODE" > "$W/node_id"
fi
echo "NODE=$NODE"
curl -s -b "$W/ck" "$API/api/nodes/$NODE/preview" | python3 -c "
import json,sys;d=json.load(sys.stdin)
print('validation_ok =', d.get('validation_ok'))
print('version       =', d.get('version'))
print('issues        =', json.dumps(d.get('issues'), ensure_ascii=False, indent=1))
print('expected_listeners =', d.get('expected_listeners'))"

echo
echo "--- A4. current/ 与 VERSION ---"
echo "VERSION=$(cat $CR/current/VERSION 2>/dev/null)"
ls -la "$CR/current/"
if [ -f "$W/pass2-baseline.txt" ]; then
  echo "--- (上一轮抓的 pass2 baseline) ---"; cat "$W/pass2-baseline.txt"
fi

echo
echo "##################### PART B：修复（只动验收脚本自己造的数据） #####################"
cp -a "$DB" "$DB.before-repair-2"
echo "--- B1. 列出待删的验收业务（名字含 B2）---"
curl -s -b "$W/ck" "$API/api/businesses" | python3 -c "
import json,sys;d=json.load(sys.stdin)
for b in (d.get('items') or []):
    print(('DELETE ' if 'B2' in (b.get('name') or '') else 'KEEP   ') + b['id'] + '  ' + b['name'])" 
curl -s -b "$W/ck" "$API/api/businesses" | python3 -c "
import json,sys;d=json.load(sys.stdin)
for b in (d.get('items') or []):
    if 'B2' in (b.get('name') or ''): print(b['id'])" > "$W/biz_del2.txt"
while read -r id; do
  [ -n "$id" ] || continue
  curl -s -b "$W/ck" -o /dev/null -w "del_biz($id)=%{http_code}\n" -X DELETE \
    -H "X-CSRF-Token: $CSRF" "$API/api/businesses/$id"
done < "$W/biz_del2.txt"

echo "--- B2. 删掉留下来的非 8443 入口（先确认已无路由引用）---"
python3 - "$DB" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
refs = list(c.execute("""select r.id, r.sni_entry_id from routes r
                         where r.sni_entry_id in (select id from sni_entries where bind_port<>8443)"""))
if refs:
    print("REFUSED: 仍有路由引用待删入口，先处理这些路由:", refs)
    sys.exit(0)
print("entries before:", list(c.execute("select id,bind_port from sni_entries")))
c.execute("delete from sni_entries where bind_port<>8443")
c.commit()
print("entries after :", list(c.execute("select id,bind_port from sni_entries")))
c.close()
PY
systemctl restart shengyu-edgelink-server; sleep 2; systemctl is-active shengyu-edgelink-server

echo "--- B3. 再查悬空引用（必须为 0）---"
python3 - "$DB" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
bad = list(c.execute("""select r.id, r.sni_entry_id from routes r
                        where r.sni_entry_id<>'' and r.sni_entry_id not in (select id from sni_entries)"""))
print("orphan routes =", bad)
print("orphan_count  =", len(bad))
c.close()
PY

echo "--- B4. /preview 必须通过 ---"
login
curl -s -b "$W/ck" "$API/api/nodes/$NODE/preview" | tee "$W/prev2.json" | python3 -c "
import json,sys;d=json.load(sys.stdin)
print('validation_ok =', d.get('validation_ok'))
print('issues        =', json.dumps(d.get('issues'), ensure_ascii=False))
if d.get('validation_ok') is not True: raise SystemExit(1)" \
  && echo "PREVIEW_OK=PASS" || echo "PREVIEW_OK=FAIL"

echo "--- B5. 无损探测：内容未变时应返回 no_change（不产生新版本、不 reload）---"
V_BEFORE=$(cat "$CR/current/VERSION")
curl -s -b "$W/ck" -o "$W/probe.json" -w 'probe_publish_http=%{http_code}\n' \
  -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/$NODE/publish" -d '{"note":"修复后探测：内容未变应为 no_change"}'
python3 -c "
import json;d=json.load(open('/tmp/b2/probe.json'))
print('no_change =', d.get('no_change'), '| status =', d.get('status'), '| version =', d.get('version'))
print('message   =', d.get('message'))
print('error/code=', d.get('code'), d.get('error'))
print('PUBLISH_UNBLOCKED=' + ('PASS' if d.get('no_change') or d.get('status')=='succeeded' else 'FAIL'))"
echo "VERSION=$(cat $CR/current/VERSION) （期望仍为 $V_BEFORE —— no_change 不该产生新版本）"
echo "REPAIR_DONE"
