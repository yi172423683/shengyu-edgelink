#!/usr/bin/env bash
# B2 重跑 - 第 5 步：清理临时措施 + 把机器恢复到验收前的状态
#
# 用户前提要求："测试完成后恢复原 systemd 配置和原运行版本"。
# 本步骤逐条落实，并且用**逐字节比对**证明恢复到位，而不是只看版本号。
set -u
API=http://127.0.0.1:8081
W=/tmp/b2
CR=/etc/shengyu-edgelink/haproxy
SD=/run/shengyu-edgelink
D=/etc/systemd/system/shengyu-edgelink-haproxy.service.d
BK=$(cat "$W/backup_dir" 2>/dev/null || echo "")

echo "############ 0. 备份目录 ############"
echo "BK=$BK"
ls -la "$BK" 2>/dev/null | head -30

echo
echo "############ 1. 撤掉 reload 注入器与 drop-in ############"
printf 'off\n' > "$SD/rbui-reload-mode" 2>/dev/null || true
rm -f "$SD/rbui-reload-fail-once"
rm -f "$D/10-b2-reload-inject.conf"
rmdir "$D" 2>/dev/null || true
rm -f /usr/local/bin/rbui-reload-inject.sh
systemctl daemon-reload
echo "ExecReload(after) = $(systemctl show -p ExecReload --value shengyu-edgelink-haproxy)"
echo "drop-in dir: $(ls -la $D 2>&1 | head -3)"
echo "injector: $(ls -l /usr/local/bin/rbui-reload-inject.sh 2>&1)"
echo "flag: $(ls -l $SD/rbui-reload-fail-once 2>&1)"
echo "mode file: $(cat $SD/rbui-reload-mode 2>/dev/null || echo '(removed)')"
rm -f "$SD/rbui-reload-mode"

echo
echo "############ 2. 登录并清掉验收造的记录 ############"
PW=$(grep -oE '[A-Za-z0-9]{32}' /root/shengyu-install.log | head -1)
curl -s -c "$W/ck" -o "$W/login.json" -X POST "$API/api/login" \
  -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$PW\"}"
CSRF=$(sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p' "$W/login.json")
NODE=$(cat "$W/node_id")

echo "--- 2.1 删测试业务（B2-9443-测试业务）---"
for f in /tmp/b2/biz9443_id; do
  [ -f "$f" ] || continue
  ID=$(cat "$f")
  [ -n "$ID" ] || continue
  curl -s -b "$W/ck" -o /dev/null -w "del_biz($ID)=%{http_code}\n" -X DELETE \
    -H "X-CSRF-Token: $CSRF" "$API/api/businesses/$ID"
done
echo "--- 2.2 删测试客户（B2验收客户）---"
ID=$(cat /tmp/b2/cus_id 2>/dev/null || echo "")
if [ -n "$ID" ]; then
  curl -s -b "$W/ck" -o /dev/null -w "del_customer($ID)=%{http_code}\n" -X DELETE \
    -H "X-CSRF-Token: $CSRF" "$API/api/customers/$ID"
fi
echo "--- 2.3 删 9443 SNI 入口（无删除接口，改库；先备份）---"
DB=$(find /etc/shengyu-edgelink /var/lib/shengyu-edgelink -maxdepth 3 -name '*.db' 2>/dev/null | head -1)
cp -a "$DB" "$DB.before-cleanup"
python3 - "$DB" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
print("sni_entries before:", list(c.execute("select id,bind_port,enabled from sni_entries")))
c.execute("delete from sni_entries where bind_port=9443")
c.commit()
print("sni_entries after :", list(c.execute("select id,bind_port,enabled from sni_entries")))
print("businesses:", list(c.execute("select id,name,mode,enabled from businesses")))
print("routes    :", list(c.execute("select id,business_id,node_id,origin_port,enabled from routes")))
c.close()
PY
systemctl restart shengyu-edgelink-server
sleep 2
systemctl is-active shengyu-edgelink-server

echo
echo "############ 3. 回滚到验收前的版本 ############"
V_ORIG=$(grep '^current_VERSION=' "$BK/facts-before.txt" 2>/dev/null | cut -d= -f2)
echo "V_ORIG(from facts-before) = $V_ORIG"
echo "VERSION_now = $(cat $CR/current/VERSION)"
if [ -n "$V_ORIG" ] && [ "$(cat $CR/current/VERSION)" != "$V_ORIG" ]; then
  curl -s -b "$W/ck" -o "$W/rb.json" -w 'rollback_http=%{http_code}\n' \
    -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
    "$API/api/nodes/$NODE/rollback" -d "{\"version\":$V_ORIG,\"reason\":\"B2 验收收尾：恢复原运行版本\"}"
  python3 -c "
import json;d=json.load(open('/tmp/b2/rb.json'))
print('status=',d.get('status'),'| version=',d.get('version'),'| message=',d.get('message'))
print('rollback_verified=',d.get('rollback_verified'),'| outcome=',d.get('rollback_outcome'))
"
else
  echo "无需回滚（已在原版本上）或未取到原版本号"
fi

echo
echo "############ 4. 逐字节比对：current/ 是否与验收前一致 ############"
if [ -n "$BK" ] && [ -d "$BK/etc-shengyu-edgelink/haproxy/current" ]; then
  echo "--- 4.1 文件清单 ---"
  A=$(ls -1 "$CR/current" | sort)
  B=$(ls -1 "$BK/etc-shengyu-edgelink/haproxy/current" | sort)
  echo "now:"; echo "$A" | sed 's/^/    /'
  echo "before:"; echo "$B" | sed 's/^/    /'
  [ "$A" = "$B" ] && echo "FILESET_SAME=PASS" || echo "FILESET_SAME=FAIL"
  echo "--- 4.2 逐文件 sha256 ---"
  allsame=1
  for f in $B; do
    a=$(sha256sum "$CR/current/$f" 2>/dev/null | cut -d' ' -f1)
    b=$(sha256sum "$BK/etc-shengyu-edgelink/haproxy/current/$f" 2>/dev/null | cut -d' ' -f1)
    if [ "$a" = "$b" ] && [ -n "$a" ]; then echo "  SAME $f"; else echo "  DIFF $f (now=$a before=$b)"; allsame=0; fi
  done
  [ "$allsame" = "1" ] && echo "BYTE_IDENTICAL=PASS" || echo "BYTE_IDENTICAL=FAIL"
else
  echo "SKIP: 没有验收前的 current/ 备份"
fi

echo
echo "############ 5. 服务与端口终检 ############"
echo "--- systemd ---"
systemctl is-enabled shengyu-edgelink-server shengyu-edgelink-haproxy
systemctl is-active shengyu-edgelink-server shengyu-edgelink-haproxy
systemctl status shengyu-edgelink-haproxy --no-pager 2>&1 | head -12
echo "--- ExecReload 必须是原值 ---"
systemctl show -p ExecReload --value shengyu-edgelink-haproxy
grep -n 'ExecReload' /etc/systemd/system/shengyu-edgelink-haproxy.service
echo "--- haproxy -c ---"
haproxy -c -f "$CR/current/haproxy.cfg" 2>&1 | tail -1
echo "--- 端口 ---"
ss -lntp 2>/dev/null | head -20
echo "--- 8443 业务（应仍正常）---"
timeout 8 openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com -brief </dev/null 2>&1 | head -2
echo "--- 9443（应已关闭）---"
timeout 5 openssl s_client -connect 127.0.0.1:9443 -servername b2-test.example.com -brief </dev/null 2>&1 | head -2
echo "--- 统计套接字 ---"
ls -la "$SD"

echo
echo "############ 6. 页面与 meta ############"
curl -s -b "$W/ck" -o /tmp/b2/index.html -w 'page_http=%{http_code} size=%{size_download}\n' "$API/"
echo "产品名出现次数 = $(grep -o '盛愈' /tmp/b2/index.html | wc -l)"
curl -s -b "$W/ck" "$API/api/meta" | python3 -c "
import json,sys;d=json.load(sys.stdin);n=d['node']
print('initialized    =', d.get('initialized'))
print('business_count =', d.get('business_count'))
print('applied/expected/skew =', n['applied_version'],'/',n['expected_version'],'/',n['version_skewed'])
print('product/version =', d.get('product'), d.get('version'))
"
echo "--- SNI 入口 ---"
curl -s -b "$W/ck" "$API/api/nodes/$NODE/sni-entries" | python3 -c "
import json,sys;d=json.load(sys.stdin)
print('entries =', [(e['bind_port'],e['enabled']) for e in (d.get('items') or [])])
"
echo "--- 业务 ---"
curl -s -b "$W/ck" "$API/api/businesses" | python3 -c "
import json,sys;d=json.load(sys.stdin)
print('total =', d.get('total'))
for b in (d.get('items') or []): print('  ', b['id'], b['name'], b['mode'], b['enabled'], b.get('domains'))
"
echo "CLEANUP_DONE"
