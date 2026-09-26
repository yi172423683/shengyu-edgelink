#!/usr/bin/env bash
# 回滚 UI 验收 - 第 9 步：清理临时注入、删除测试夹具、恢复原运行版本 v2
set -u
W=/tmp/rbui
API=http://127.0.0.1:8081
NODE=node_06GCGE2NPCC624J5AMKYQWC4C0
SD=/run/shengyu-edgelink
D=/etc/systemd/system/shengyu-edgelink-haproxy.service.d
BK=$(ls -d /root/rollback-ui-accept-* | head -1)
ORIG_SHA=a50b7f888d0a88e8f73b3693fe7d533fe99166364e0ec6306b17209f052c2b6e
echo "backup dir = $BK"

echo "##### 9.1 删除临时 systemd drop-in 与注入器 #####"
ls -la "$D" 2>&1
rm -f "$D/99-rbui-test.conf"
rmdir "$D" 2>/dev/null || true
rm -f /usr/local/bin/rbui-reload-inject.sh
rm -f "$SD/rbui-reload-mode" "$SD/rbui-reload-fail-once" /run/rbui-reload-fail-once
systemctl daemon-reload
echo "--- drop-in dir now ---"; ls -la "$D" 2>&1
echo "--- effective ExecReload (must be /bin/kill -USR2 \$MAINPID again) ---"
systemctl show -p ExecReload --value shengyu-edgelink-haproxy
echo "--- leftovers under /run/shengyu-edgelink ---"; ls "$SD"

echo "##### 9.2 备份元数据库后再删测试夹具 #####"
cp -a /var/lib/shengyu-edgelink/meta.db "$BK/meta.db.before-cleanup" 2>&1
ls -la "$BK/meta.db.before-cleanup"

echo "--- 删除测试业务（走产品自己的接口）---"
curl -s -b "$W/ck" -X DELETE -H "X-CSRF-Token: $(cat $W/csrf)" \
  -w '\nbiz_delete_http=%{http_code}\n' "$API/api/businesses/$(cat $W/biz_id)"

echo "--- 删除测试 SNI 入口（产品无删除接口，直接改库；已先备份 meta.db）---"
systemctl stop shengyu-edgelink-server
python3 - <<'PY'
import sqlite3, sys
db='/var/lib/shengyu-edgelink/meta.db'
c=sqlite3.connect(db)
cur=c.cursor()
cur.execute("SELECT id,bind_addr,bind_port FROM sni_entries")
print("before:", cur.fetchall())
cur.execute("DELETE FROM sni_entries WHERE bind_port=9443")
print("deleted rows:", cur.rowcount)
cur.execute("SELECT id,bind_addr,bind_port FROM sni_entries")
print("after :", cur.fetchall())
c.commit()
c.close()
PY
systemctl start shengyu-edgelink-server
sleep 2
systemctl is-active shengyu-edgelink-server

echo "##### 9.3 re-login (服务重启过) #####"
PW=$(grep -oE '[A-Za-z0-9]{32}' /root/shengyu-install.log | head -1)
curl -s -c "$W/ck" -o "$W/login.json" -X POST "$API/api/login" \
  -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$PW\"}"
CSRF=$(sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p' "$W/login.json"); echo "$CSRF" > "$W/csrf"

echo "##### 9.4 恢复原运行版本 v2（用产品自己的回滚接口）#####"
curl -s -b "$W/ck" -o "$W/rb2.json" -w 'rollback_http=%{http_code}\n' \
  -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/$NODE/rollback" -d '{"version":2,"reason":"RBUI 验收结束后恢复原运行版本"}'
python3 - <<'PY'
import json
d=json.load(open('/tmp/rbui/rb2.json'))
print("action       =", d.get("action"))
print("version/prev =", d.get("version"), "/", d.get("prev_version"))
print("status       =", d.get("status"))
print("phase        =", d.get("phase"))
print("message      =", d.get("message"))
print("verify.ok    =", (d.get("verify") or {}).get("ok"))
PY

echo "##### 9.5 恢复后核对 #####"
echo -n "current/VERSION = "; cat /etc/shengyu-edgelink/haproxy/current/VERSION
echo -n "current/cfg sha = "; sha256sum /etc/shengyu-edgelink/haproxy/current/haproxy.cfg | cut -d' ' -f1
echo    "original v2 sha = $ORIG_SHA"
echo -n "current/ files  = "; ls /etc/shengyu-edgelink/haproxy/current/ | tr '\n' ' '; echo
echo "--- 与原备份逐字节比对 current/ ---"
mkdir -p /tmp/rbui-orig && tar -C /tmp/rbui-orig -xzf "$BK/etc-shengyu-edgelink.tar.gz"
for f in haproxy.cfg meta.json sni_allow_8443.lst state.json VERSION; do
  if cmp -s "/tmp/rbui-orig/haproxy/current/$f" "/etc/shengyu-edgelink/haproxy/current/$f"; then
    echo "  同  $f"
  else
    echo "  不同 $f"
  fi
done

echo -n "haproxy -c      = "; haproxy -c -f /etc/shengyu-edgelink/haproxy/current/haproxy.cfg 2>&1 | tail -1
echo "--- systemctl status ---"
systemctl is-active shengyu-edgelink-server shengyu-edgelink-haproxy
systemctl is-enabled shengyu-edgelink-server shengyu-edgelink-haproxy
echo "--- haproxy 进程 ---"; ps -o user=,pid=,ppid=,args= -C haproxy
echo "--- 端口检查 ---"; ss -lntp | sort -k4
echo "--- stats sockets ---"; ls "$SD"

echo "--- 8443 业务 ---"
timeout 8 openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com -brief </dev/null 2>&1 | head -4
echo "--- 9443 应已不再监听 ---"
timeout 5 openssl s_client -connect 127.0.0.1:9443 -servername rbui-test.example.com -brief </dev/null 2>&1 | head -2

echo "--- 页面刷新（GET /）---"
curl -s -o "$W/page.html" -w 'GET / http=%{http_code} bytes=%{size_download}\n' "$API/"
grep -c "盛愈边缘网关" "$W/page.html" | sed 's/^/页面含产品名次数: /'

echo "--- meta ---"
curl -s -b "$W/ck" "$API/api/meta" -o "$W/m5.json"
python3 - <<'PY'
import json
d=json.load(open('/tmp/rbui/m5.json'))
n=d['node']
print("initialized      =",d['initialized'])
print("business_count   =",d['business_count'])
print("applied_version  =",n['applied_version']," expected_version =",n['expected_version']," skewed =",n['version_skewed'])
PY
echo "--- 业务与入口列表 ---"
curl -s -b "$W/ck" "$API/api/businesses" | python3 -c "import sys,json;d=json.load(sys.stdin);print('businesses:',[(b['name'],b['domains']) for b in d['items']])"
curl -s -b "$W/ck" "$API/api/nodes/$NODE/sni-entries" | python3 -c "import sys,json;d=json.load(sys.stdin);print('sni entries:',[(e['bind_addr'],e['bind_port']) for e in d['items']])"

echo "##### 9.6 清理 /tmp 传送物 #####"
rm -f /tmp/shengyu-edgelink-v0.3.2 /tmp/shengyu-edgelink-v0.3.3 /tmp/recon.sh
ls /tmp/rbui-run.sh 2>&1
echo "CLEANUP_DONE"
