#!/usr/bin/env bash
# B2 第二轮 - 第 3 步：清理临时措施 + 恢复到改动前的状态（逐字节比对）
set -u
API=http://127.0.0.1:8081
W=/tmp/b2
CR=/etc/shengyu-edgelink/haproxy
SD=/run/shengyu-edgelink
D=/etc/systemd/system/shengyu-edgelink-haproxy.service.d

login() {
  PW=$(grep -oE '[A-Za-z0-9]{32}' /root/shengyu-install.log | head -1)
  curl -s -c "$W/ck" -o "$W/login.json" -X POST "$API/api/login" \
    -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$PW\"}"
  CSRF=$(sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p' "$W/login.json")
}

echo "############ 1. 撤掉 reload 注入器与 drop-in ############"
printf 'off\n' > "$SD/rbui-reload-mode" 2>/dev/null || true
rm -f "$SD/rbui-reload-fail-once" "$SD/rbui-reload-mode"
rm -f "$D/10-b2-reload-inject.conf"; rmdir "$D" 2>/dev/null || true
rm -f /usr/local/bin/rbui-reload-inject.sh
systemctl daemon-reload
echo "ExecReload = $(systemctl show -p ExecReload --value shengyu-edgelink-haproxy)"
echo "drop-in: $(ls $D 2>&1 | head -1)"
echo "injector: $(ls /usr/local/bin/rbui-reload-inject.sh 2>&1)"
grep -n 'ExecReload' /etc/systemd/system/shengyu-edgelink-haproxy.service

echo
echo "############ 2. 把可能被移动/改写的版本目录文件复位 ############"
NODE=$(cat "$W/node_id")
for f in $(find "$CR/versions" -name '*.b2moved' 2>/dev/null); do
  t="${f%.b2moved}"; mv -f "$f" "$t"; echo "restored $t"
done
echo "残留 .b2moved: $(find $CR/versions -name '*.b2moved' | wc -l)"
# 新版把移走的 state.json 放到了 /tmp/b2/moved（不在版本目录里），也要复位
if [ -d /tmp/b2/moved ]; then
  for f in /tmp/b2/moved/v*-state.json /tmp/b2/moved/v*-state.json.s7; do
    [ -f "$f" ] || continue
    v=$(basename "$f" | sed 's/^v//; s/-state.json.*$//')
    t="$CR/versions/$NODE/v$v/state.json"
    if [ -f "$t" ]; then echo "SKIP (target exists) $t"; else mv -f "$f" "$t"; echo "restored $t"; fi
  done
  echo "剩余 /tmp/b2/moved: $(ls /tmp/b2/moved 2>/dev/null | wc -l)"
fi

login
echo "--- 2.1 删验收造的客户/业务 ---"
curl -s -b "$W/ck" "$API/api/customers" > "$W/cusall.json"
python3 -c "
import json;d=json.load(open('/tmp/b2/cusall.json'))
for c in (d.get('items') or []):
    if 'B2' in (c.get('name') or ''): print(c['id'])
" > "$W/cus_del.txt"
while read -r id; do
  [ -n "$id" ] || continue
  curl -s -b "$W/ck" -o /dev/null -w "del_customer($id)=%{http_code}\n" -X DELETE -H "X-CSRF-Token: $CSRF" "$API/api/customers/$id"
done < "$W/cus_del.txt"
curl -s -b "$W/ck" "$API/api/businesses" > "$W/bizall.json"
python3 -c "
import json;d=json.load(open('/tmp/b2/bizall.json'))
for b in (d.get('items') or []):
    if b['name'] != '验收业务': print(b['id'])
" > "$W/biz_del.txt"
while read -r id; do
  [ -n "$id" ] || continue
  curl -s -b "$W/ck" -o /dev/null -w "del_biz($id)=%{http_code}\n" -X DELETE -H "X-CSRF-Token: $CSRF" "$API/api/businesses/$id"
done < "$W/biz_del.txt"

echo "--- 2.2 删 9443 SNI 入口（产品无删除接口，改库；先备份）---"
DB=/var/lib/shengyu-edgelink/meta.db
cp -a "$DB" "$DB.before-cleanup-2"
python3 - "$DB" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
print("before:", list(c.execute("select id,bind_port,enabled from sni_entries")))
c.execute("delete from sni_entries where bind_port<>8443")
c.commit()
print("after :", list(c.execute("select id,bind_port,enabled from sni_entries")))
print("businesses:", list(c.execute("select id,name,mode,enabled from businesses")))
print("routes    :", list(c.execute("select id,business_id,origin_port,enabled from routes")))
c.close()
PY
systemctl restart shengyu-edgelink-server; sleep 2; systemctl is-active shengyu-edgelink-server

echo
echo "############ 3. 回滚到改动前的版本 ############"
login
V_ORIG=$(grep '^VERSION=' "$W/pass2-baseline.txt" | cut -d= -f2)
echo "V_ORIG(base) = $V_ORIG   VERSION_now = $(cat $CR/current/VERSION)"
if [ "$(cat $CR/current/VERSION)" != "$V_ORIG" ]; then
  curl -s -b "$W/ck" -o "$W/rb2.json" -w 'rollback_http=%{http_code}\n' \
    -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
    "$API/api/nodes/$NODE/rollback" -d "{\"version\":$V_ORIG,\"reason\":\"B2 第二轮收尾：恢复改动前版本\"}"
  python3 -c "
import json;d=json.load(open('/tmp/b2/rb2.json'))
print('status=',d.get('status'),'| version=',d.get('version'))
print('rollback_verified=',d.get('rollback_verified'),'| outcome=',d.get('rollback_outcome'))
print('message=',d.get('message'))"
fi

echo
echo "############ 4. 逐字节比对：current/ 是否回到改动前 ############"
echo "--- 4.1 文件清单 ---"
A=$(ls -1 "$CR/current" | grep -v '\.tmp$' | sort | tr '\n' ',')
# 用 sha256 行推导基线文件清单：那几行是逐文件记的，比单行的 files= 更可靠
# （files= 那行曾经因为 tr 的转义丢了反斜杠而被写坏，见 docs/08 §9.2）。
B=$(awk '$1=="sha256"{print $2}' "$W/pass2-baseline.txt" | sort | tr '\n' ',')
echo "now  = $A"
echo "base = $B"
[ "$A" = "$B" ] && echo "FILESET_SAME=PASS" || echo "FILESET_SAME=FAIL"
echo "--- 4.2 逐文件 sha256 ---"
allsame=1
while read -r kind f h; do
  [ "$kind" = "sha256" ] || continue
  now=$(sha256sum "$CR/current/$f" 2>/dev/null | cut -d' ' -f1)
  if [ "$now" = "$h" ]; then echo "  SAME $f"; else echo "  DIFF $f (now=$now base=$h)"; allsame=0; fi
done < "$W/pass2-baseline.txt"
[ "$allsame" = "1" ] && echo "BYTE_IDENTICAL=PASS" || echo "BYTE_IDENTICAL=FAIL"
echo "--- 4.3 VERSION ---"
echo "now=$(cat $CR/current/VERSION) base=$V_ORIG"
[ "$(cat $CR/current/VERSION)" = "$V_ORIG" ] && echo "VERSION_SAME=PASS" || echo "VERSION_SAME=FAIL"

echo
echo "############ 5. 终检 ############"
echo "--- systemd ---"
systemctl is-enabled shengyu-edgelink-server shengyu-edgelink-haproxy
systemctl is-active shengyu-edgelink-server shengyu-edgelink-haproxy
echo "haproxy_master_pid=$(systemctl show -p MainPID --value shengyu-edgelink-haproxy)"
echo "ExecReload=$(systemctl show -p ExecReload --value shengyu-edgelink-haproxy)"
echo "haproxy -c = $(haproxy -c -f $CR/current/haproxy.cfg 2>&1 | tail -1)"
echo "--- 端口 ---"; ss -lntp 2>/dev/null | grep -E '8443|9443|:443 ' || true
echo -n "8443 handshake = "; timeout 8 openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com -brief </dev/null 2>&1 | head -1
echo -n "9443 (应关闭) = "; timeout 5 openssl s_client -connect 127.0.0.1:9443 -brief </dev/null >/dev/null 2>&1 && echo "LISTENING" || echo "REFUSED"
echo "--- 统计套接字 ---"; ls -la "$SD"
echo "--- 页面与 meta ---"
curl -s -b "$W/ck" -o /tmp/b2/index2.html -w 'page_http=%{http_code} size=%{size_download}\n' "$API/"
echo "产品名出现次数 = $(grep -o '盛愈' /tmp/b2/index2.html | wc -l)"
curl -s -b "$W/ck" "$API/api/meta" | python3 -c "
import json,sys;d=json.load(sys.stdin);n=d['node']
print('initialized =', d.get('initialized'), '| business_count =', d.get('business_count'))
print('applied/expected/skew =', n['applied_version'],'/',n['expected_version'],'/',n['version_skewed'])
print('product/version =', d.get('product'), d.get('version'))"
curl -s -b "$W/ck" "$API/api/nodes/$NODE/sni-entries" | python3 -c "
import json,sys;d=json.load(sys.stdin)
print('sni entries =', [(e['bind_port'],e['enabled']) for e in (d.get('items') or [])])"
curl -s -b "$W/ck" "$API/api/businesses" | python3 -c "
import json,sys;d=json.load(sys.stdin)
print('businesses total =', d.get('total'))
for b in (d.get('items') or []): print('  ', b['id'], b['name'], b['mode'], b['enabled'], b.get('domains'))"
echo "--- 临时文件残留检查 ---"
echo "injector: $(ls /usr/local/bin/rbui-reload-inject.sh 2>&1 | head -1)"
echo "modefile: $(ls $SD/rbui-reload-* 2>&1 | head -1)"
echo "dropin  : $(ls $D 2>&1 | head -1)"
echo "CLEANUP2_DONE"
