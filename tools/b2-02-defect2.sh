#!/usr/bin/env bash
# B2 重跑 - 第 2 步：缺陷二真机验收（陈旧辅助文件的清理）
#
# 场景：当前生效版本带 9443 入口（于是有 sni_allow_9443.lst）；
#       之后把 9443 入口去掉再发布，新版本不再需要那个清单。
# 期望：发布成功后 current/ 里**不能**再有 sni_allow_9443.lst，
#       且 current/ 的文件集合与目标版本目录**完全一致**（除 VERSION）。
set -u
API=http://127.0.0.1:8081
W=/tmp/b2
CR=/etc/shengyu-edgelink/haproxy
SD=/run/shengyu-edgelink
mkdir -p "$W"

echo "############ 0. 登录 ############"
PW=$(grep -oE '[A-Za-z0-9]{32}' /root/shengyu-install.log | head -1)
curl -s -c "$W/ck" -o "$W/login.json" -X POST "$API/api/login" \
  -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$PW\"}"
CSRF=$(sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p' "$W/login.json")
echo "$CSRF" > "$W/csrf"
echo "csrf_len=${#CSRF}"

NODE=$(cat "$W/node_id")
echo "NODE_ID=$NODE"

curl -s -b "$W/ck" "$API/api/nodes/$NODE" > "$W/n1.json"
VID=$(python3 -c "import json;d=json.load(open('/tmp/b2/n1.json'));print(d.get('node',{}).get('applied_version'))")
echo "起始生效版本 = v$VID"
V0=$VID

echo
echo "############ 1. 现状：current/ 与文件集合 ############"
echo "VERSION=$(cat $CR/current/VERSION)"
ls -la "$CR/current/"
echo "--- 已存在的 SNI 入口 ---"
curl -s -b "$W/ck" "$API/api/nodes/$NODE/sni-entries" | python3 -c "
import json,sys
d=json.load(sys.stdin)
for e in (d.get('items') or []):
    print('  ', e['id'], e['bind_addr'], e['bind_port'], 'enabled=', e['enabled'])
"

echo
echo "############ 2. 建 9443 入口 + 测试业务，发布 ############"
POST_SNI=$(curl -s -b "$W/ck" -w '\nHTTP=%{http_code}' -X POST \
  -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/$NODE/sni-entries" -d '{"bind_addr":"0.0.0.0","bind_port":9443}')
echo "$POST_SNI" | tail -1
echo "$POST_SNI" | head -1 > "$W/sni_new.json"
python3 -c "
import json
d=json.load(open('/tmp/b2/sni_new.json'))
e=d.get('sni_entry') or d.get('entry') or d
print('new_sni_id =', e.get('id'), e.get('bind_addr'), e.get('bind_port'))
" 2>/dev/null || cat "$W/sni_new.json"
curl -s -b "$W/ck" "$API/api/nodes/$NODE/sni-entries" > "$W/sni2.json"
SNI9443=$(python3 -c "
import json;d=json.load(open('/tmp/b2/sni2.json'))
for e in (d.get('items') or []):
    if e['bind_port']==9443: print(e['id'])
" | head -1)
echo "$SNI9443" > "$W/sni9443_id"
echo "SNI9443_ID=$SNI9443"

CUS=$(curl -s -b "$W/ck" -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/customers" -d '{"name":"B2验收客户"}' | python3 -c "import json,sys;print(json.load(sys.stdin).get('customer',{}).get('id',''))")
echo "CUSTOMER=$CUS"
echo "$CUS" > "$W/cus_id"

BIZRESP=$(curl -s -b "$W/ck" -w '\nHTTP=%{http_code}' -X POST \
  -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/businesses" -d "{
    \"customer_id\":\"$CUS\",
    \"name\":\"B2-9443-测试业务\",
    \"mode\":\"sni_tls\",
    \"domains\":[\"b2-test.example.com\"],
    \"primary_node_id\":\"$NODE\",
    \"sni_entry_id\":\"$SNI9443\",
    \"origin_host\":\"1.1.1.1\",
    \"origin_port\":443
  }")
echo "$BIZRESP" | tail -1
echo "$BIZRESP" | head -1 > "$W/biz_new.json"
BIZID=$(python3 -c "import json;print(json.load(open('/tmp/b2/biz_new.json')).get('business',{}).get('id',''))" 2>/dev/null || echo "")
echo "BIZ_ID=$BIZID"
echo "$BIZID" > "$W/biz9443_id"

echo "--- 发布（应产生带 sni_allow_9443.lst 的新版本）---"
curl -s -b "$W/ck" -o "$W/p2a.json" -w 'publish_http=%{http_code}\n' \
  -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/$NODE/publish" -d '{"note":"B2 缺陷二：带 9443 入口的版本"}'
python3 -c "
import json;d=json.load(open('/tmp/b2/p2a.json'))
print('status =', d.get('status'), '| version =', d.get('version'), '| phase =', d.get('phase'))
print('message =', d.get('message'))
print('listeners =', [(l['addr'],l['port'],l.get('owned_by_us')) for l in (d.get('verify') or {}).get('listeners',[])])
"
V1=$(python3 -c "import json;print(json.load(open('/tmp/b2/p2a.json')).get('version'))")
echo "V1=$V1"
echo "$V1" > "$W/v1"

echo
echo "---- 发布后 current/ 清单（此时**应当**有 sni_allow_9443.lst）----"
ls -la "$CR/current/"
if [ -f "$CR/current/sni_allow_9443.lst" ]; then
  echo "CHECK_V1_9443_ALLOWLIST=PRESENT(期望如此)"
else
  echo "CHECK_V1_9443_ALLOWLIST=ABSENT(异常：这一版应该有 9443 入口)"
fi
echo "--- current/manifest.json ---"
cat "$CR/current/manifest.json"
echo "--- 第 $V1 版目录 ---"
ls -la "$CR/versions/$NODE/v$V1/"

echo
echo "############ 3. 去掉 9443 入口（先删业务，再删入口）############"
echo "--- 3.1 删业务（产品有 DELETE 接口）---"
curl -s -b "$W/ck" -o /dev/null -w 'del_biz_http=%{http_code}\n' -X DELETE \
  -H "X-CSRF-Token: $CSRF" "$API/api/businesses/$BIZID"

echo "--- 3.2 删 9443 入口：产品**没有**删除接口，只能改库（先备份 meta.db）---"
DB=$(find /etc/shengyu-edgelink /var/lib/shengyu-edgelink -maxdepth 3 -name '*.db' 2>/dev/null | head -1)
echo "meta_db=$DB"
cp -a "$DB" "$DB.before-sni-delete"
python3 - "$DB" "$SNI9443" <<'PY'
import sqlite3, sys
db, sid = sys.argv[1], sys.argv[2]
c = sqlite3.connect(db)
before = c.execute("select count(*) from sni_entries").fetchone()[0]
c.execute("delete from sni_entries where id=?", (sid,))
c.commit()
after = c.execute("select count(*) from sni_entries").fetchone()[0]
print("sni_entries: %d -> %d" % (before, after))
for r in c.execute("select id,bind_port,enabled from sni_entries"):
    print("  remaining:", r)
c.close()
PY
systemctl restart shengyu-edgelink-server
sleep 2
systemctl is-active shengyu-edgelink-server

echo "--- 3.3 确认平台侧已无 9443 入口 ---"
curl -s -b "$W/ck" "$API/api/nodes/$NODE/sni-entries" | python3 -c "
import json,sys
d=json.load(sys.stdin)
print('sni entries =', [(e['bind_port'], e['enabled']) for e in (d.get('items') or [])])
"

echo
echo "############ 4. 重新发布（此时不应再需要 9443 清单）############"
curl -s -b "$W/ck" -o "$W/p2b.json" -w 'publish_http=%{http_code}\n' \
  -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/$NODE/publish" -d '{"note":"B2 缺陷二：去掉 9443 入口后重新发布"}'
python3 -c "
import json;d=json.load(open('/tmp/b2/p2b.json'))
print('status =', d.get('status'), '| version =', d.get('version'), '| phase =', d.get('phase'))
print('message =', d.get('message'))
print('rolled_back =', d.get('rolled_back'), '| outcome =', d.get('rollback_outcome'))
"
V2=$(python3 -c "import json;print(json.load(open('/tmp/b2/p2b.json')).get('version'))")
echo "V2=$V2"
echo "$V2" > "$W/v2"

echo
echo "############ 5. 判据 ############"
echo "--- 5.1 旧 sni_allow_9443.lst 是否已删除 ---"
if [ -e "$CR/current/sni_allow_9443.lst" ]; then
  echo "CHECK_9443_REMOVED=FAIL (残留: $(ls -l $CR/current/sni_allow_9443.lst))"
else
  echo "CHECK_9443_REMOVED=PASS (current/ 里已无 sni_allow_9443.lst)"
fi
echo "--- 5.2 current/ 文件集合 vs 第 $V2 版目录 ---"
A=$(ls -1 "$CR/current" | grep -v '\.tmp$' | grep -v '^VERSION$' | sort)
B=$(ls -1 "$CR/versions/$NODE/v$V2" | grep -v '\.tmp$' | sort)
echo "current(除VERSION):"; echo "$A" | sed 's/^/    /'
echo "v$V2 目录:";        echo "$B" | sed 's/^/    /'
if [ "$A" = "$B" ]; then
  echo "CHECK_FILESET_EQUAL=PASS (current/ 除 VERSION 外与第 $V2 版完全一致)"
else
  echo "CHECK_FILESET_EQUAL=FAIL"
fi
echo "--- 5.3 diff -r（应只有 VERSION 与 manifest 的时间戳差异；内容需一致）---"
diff -r "$CR/versions/$NODE/v$V2" "$CR/current" 2>&1 | grep -v '^Only in .*current: VERSION$' | head -20
echo "(上面若为空 = 无差异)"
echo "--- 5.4 逐字节 sha256 比对 ---"
for f in $(ls -1 "$CR/versions/$NODE/v$V2"); do
  a=$(sha256sum "$CR/versions/$NODE/v$V2/$f" | cut -d' ' -f1)
  b=$(sha256sum "$CR/current/$f" 2>/dev/null | cut -d' ' -f1)
  [ "$a" = "$b" ] && echo "  SAME $f" || echo "  DIFF $f"
done

echo
echo "--- 5.5 current/VERSION ---"
echo "current/VERSION=$(cat $CR/current/VERSION)  (期望 $V2)"

echo "--- 5.6 陈旧残留审计：current/ 里有没有"不在清单内"的文件 ---"
python3 - <<'PY'
import json, os
cur = "/etc/shengyu-edgelink/haproxy/current"
mf = json.load(open(os.path.join(cur, "manifest.json")))
want = set(mf["files"]) | {"manifest.json", "VERSION"}
got = set(n for n in os.listdir(cur) if not n.endswith(".tmp"))
extra = sorted(got - want)
missing = sorted(want - got)
print("manifest.files      =", mf["files"])
print("current/ 的完整集合  =", sorted(got))
print("多余残留            =", extra, "-> ", "PASS" if not extra else "FAIL")
print("缺少文件            =", missing, "-> ", "PASS" if not missing else "FAIL")
PY

echo
echo "--- 5.7 8443 正常业务 ---"
timeout 8 openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com -brief </dev/null 2>&1 | head -2
echo "--- 5.8 9443 应已不再监听 ---"
timeout 5 openssl s_client -connect 127.0.0.1:9443 -servername b2-test.example.com -brief </dev/null 2>&1 | head -2
echo "--- 5.9 统计套接字（旧版本 socket 是否残留、能否应答）---"
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
echo "--- 5.10 haproxy -c 与进程 ---"
echo "haproxy -c = $(haproxy -c -f $CR/current/haproxy.cfg 2>&1 | tail -1)"
echo "haproxy_master_pid=$(systemctl show -p MainPID --value shengyu-edgelink-haproxy)"
echo "--- 5.11 meta ---"
curl -s -b "$W/ck" "$API/api/meta" | python3 -c "
import json,sys;d=json.load(sys.stdin);n=d['node']
print('applied/expected/skew =', n['applied_version'],'/',n['expected_version'],'/',n['version_skewed'])
print('business_count =', d.get('business_count'))
"
echo "DEFECT2_DONE"
