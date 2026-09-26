#!/usr/bin/env bash
# B2 第二轮 - 第 1 步：① 空 SNI 入口不再造成假失败  ② 缺陷二（陈旧辅助文件）真机复验
#
# ① 真机缺陷复现（v0.3.4 时发生，日志见 /root/b2-accept.log）：建了一个 9443 入口但业务没建成功，
#    渲染器按设计跳过空入口，验证却要求它监听 ⇒ 白等 30.1 秒（重试 61 次）判失败并回滚。
#    本步要证明 v0.3.5 下：**空入口 → 发布直接成功**，且该端口确实不监听、期望监听里也没有它。
#
# ② 给同一个入口挂业务 → 发布（current/ 出现 sni_allow_9443.lst）
#    → 删业务 + 删入口 → 再发布 → 该文件必须消失，且 current/ 与目标版本目录完全一致。
set -u
API=http://127.0.0.1:8081
W=/tmp/b2
CR=/etc/shengyu-edgelink/haproxy
mkdir -p "$W"

login() {
  PW=$(grep -oE '[A-Za-z0-9]{32}' /root/shengyu-install.log | head -1)
  curl -s -c "$W/ck" -o "$W/login.json" -X POST "$API/api/login" \
    -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$PW\"}"
  CSRF=$(sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p' "$W/login.json")
  echo "$CSRF" > "$W/csrf"
}
login
NODE=$(cat "$W/node_id")
echo "NODE_ID=$NODE  csrf_len=${#CSRF}"

pub() { # $1=note  $2=outfile
  curl -s -b "$W/ck" -o "$2" -w 'publish_http=%{http_code}\n' \
    -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
    "$API/api/nodes/$NODE/publish" -d "{\"note\":\"$1\"}"
  python3 - "$2" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
print("  status =", d.get("status"), "| phase =", d.get("phase"), "| version =", d.get("version"), "| prev =", d.get("prev_version"))
print("  rolled_back =", d.get("rolled_back"), "| outcome =", d.get("rollback_outcome"), "| note =", d.get("rollback_note"))
v = d.get("verify") or {}
def lg(l, k):
    return (l.get("expected") or l.get("Expected") or {}).get(k)
print("  verify.listeners =", [(lg(l,"addr"), lg(l,"port"), l.get("bound"), l.get("owned_by_us")) for l in (v.get("listeners") or [])])
print("  verify.problems  =", v.get("problems"))
print("  message =", d.get("message"))
PY
}

echo
echo "############ 0. 现状 ############"
echo "VERSION=$(cat $CR/current/VERSION)"
# 本轮验收的"改动前基线"：收尾要用它做逐字节比对。
# 必须在这一步（还没动任何东西）就抓下来——第一轮验收的备份里那个 current/ 还带着
# 旧代码留下的 sni_allow_9443.lst，拿它当基准会把"缺陷二被修好"误判成"恢复失败"。
{
  echo "ts_utc=$(date -u +%FT%TZ)"
  echo "VERSION=$(cat $CR/current/VERSION)"
  echo "files=$(ls -1 $CR/current | grep -v '\.tmp$' | sort | tr '\n' ',')"
  for f in $(ls -1 "$CR/current" | grep -v '\.tmp$' | sort); do
    echo "sha256 $f $(sha256sum "$CR/current/$f" | cut -d' ' -f1)"
  done
} > "$W/pass2-baseline.txt"
cat "$W/pass2-baseline.txt"
curl -s -b "$W/ck" "$API/api/nodes/$NODE/sni-entries" | python3 -c "
import json,sys;d=json.load(sys.stdin)
print('sni entries =', [(e['id'],e['bind_port'],e['enabled']) for e in (d.get('items') or [])])"
echo "--- 清掉上一轮可能残留的验收客户（pass1 脚本解析响应有 bug，可能漏删）---"
curl -s -b "$W/ck" "$API/api/customers" | python3 -c "
import json,sys;d=json.load(sys.stdin)
for c in (d.get('items') or []):
    if 'B2' in (c.get('name') or ''): print(c['id'], c['name'])"

echo
echo "############ 1. 建一个**空的** 9443 入口（故意不挂业务）############"
curl -s -b "$W/ck" -o "$W/sni_new.json" -w 'create_sni_http=%{http_code}\n' -X POST \
  -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/nodes/$NODE/sni-entries" -d '{"bind_addr":"0.0.0.0","bind_port":9443}'
cat "$W/sni_new.json"; echo
curl -s -b "$W/ck" "$API/api/nodes/$NODE/sni-entries" > "$W/sni2.json"
SNI9443=$(python3 -c "
import json;d=json.load(open('/tmp/b2/sni2.json'))
for e in (d.get('items') or []):
    if e['bind_port']==9443: print(e['id'])
" | head -1)
echo "SNI9443_ID=$SNI9443"
echo "$SNI9443" > "$W/sni9443_id"

echo
echo "--- 1.1 发布（空入口：期望**成功**，且 9443 不监听）---"
V_BEFORE=$(cat $CR/current/VERSION)
pub "S1: 空 9443 入口" "$W/s1.json"
V1=$(python3 -c "import json;print(json.load(open('/tmp/b2/s1.json')).get('version'))")
echo "--- 1.2 current/VERSION=$(cat $CR/current/VERSION)（发布前 $V_BEFORE，期望推进到 $V1）---"
if [ -f "$CR/versions/$NODE/v$V1/manifest.json" ]; then
  echo "--- 1.3 第 $V1 版清单（期望**不含** sni_allow_9443.lst）---"
  python3 -c "import json;print(json.load(open('$CR/versions/$NODE/v$V1/manifest.json'))['files'])"
  echo "--- 1.4 第 $V1 版 meta.json 的 expected_listeners（期望只有 8443）---"
  python3 -c "import json;m=json.load(open('$CR/versions/$NODE/v$V1/meta.json'));print(m.get('expected_listeners'))"
  if grep -q 'sni_allow_9443' "$CR/versions/$NODE/v$V1/haproxy.cfg"; then
    echo "CHECK_EMPTY_ENTRY_NOT_RENDERED=FAIL(正文里仍引用了 9443 清单)"
  else
    echo "CHECK_EMPTY_ENTRY_NOT_RENDERED=PASS(正文没有引用 9443 清单)"
  fi
fi
echo "--- 1.5 页面返回里 9443 的监听项（期望不出现）---"
python3 - <<'PY'
import json
d = json.load(open("/tmp/b2/s1.json"))
v = d.get("verify") or {}
ports = [(l.get("expected") or l.get("Expected") or {}).get("port") for l in (v.get("listeners") or [])]
st = d.get("status")
ok = st == "succeeded" and 9443 not in ports
print("listener ports =", ports)
print("CHECK_EMPTY_ENTRY_PUBLISH_OK=" + ("PASS" if ok else "FAIL"))
PY
echo -n "9443 实际监听情况: "
timeout 5 openssl s_client -connect 127.0.0.1:9443 -brief </dev/null >/dev/null 2>&1 && echo "LISTENING(异常)" || echo "REFUSED(期望：空入口不占端口)"

echo
echo "############ 2. 给 9443 入口挂一条业务，再发布（现在应该有 9443 清单）############"
CUS=$(curl -s -b "$W/ck" -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/customers" -d '{"name":"B2第二轮-客户"}' | python3 -c "import json,sys;print(json.load(sys.stdin).get('id',''))")
echo "CUSTOMER=$CUS"; echo "$CUS" > "$W/cus_id"
BIZRESP=$(curl -s -b "$W/ck" -w '\nHTTP=%{http_code}' -X POST \
  -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  "$API/api/businesses" -d "{
    \"customer_id\":\"$CUS\",
    \"name\":\"B2-9443-测试业务\",
    \"mode\":\"sni_tls\",
    \"domains\":[\"b2b-test.example.com\"],
    \"primary_node_id\":\"$NODE\",
    \"sni_entry_id\":\"$SNI9443\",
    \"origin_host\":\"1.1.1.1\",
    \"origin_port\":443
  }")
echo "$BIZRESP" | tail -1
echo "$BIZRESP" | head -1 > "$W/biz_new.json"
# 注意：POST /api/businesses 返回的是 **包装体** {"business":{...},"next_step":...}，
# 而 POST /api/customers 返回的是**裸对象**。两者形状不同，解析错一个就会拿到空 id，
# 后续 "删业务" 变成删空 id（404）→ 业务留着 → 它的路由还挂在入口上 →
# 我再 SQL 删入口，就留下悬空引用，之后每次发布都被 validate 拦成 422。
# 第二轮真机就是这么把测试机写坏的（详见 docs/08 §8）。
BIZID=$(python3 -c "
import json
d = json.load(open('/tmp/b2/biz_new.json'))
b = d.get('business') or d
print(b.get('id',''))
" 2>/dev/null || echo "")
echo "BIZ_ID=$BIZID"; echo "$BIZID" > "$W/biz9443_id"

pub "S2: 9443 入口挂上业务" "$W/s2.json"
V2=$(python3 -c "import json;print(json.load(open('/tmp/b2/s2.json')).get('version'))")
echo "V2=$V2  current/VERSION=$(cat $CR/current/VERSION)"
if [ -f "$CR/current/sni_allow_9443.lst" ]; then
  echo "CHECK_9443_ALLOWLIST_PRESENT=PASS ($(ls -l $CR/current/sni_allow_9443.lst))"
else
  echo "CHECK_9443_ALLOWLIST_PRESENT=FAIL(有业务的 9443 入口应当产出允许清单)"
fi
echo "current/ 清单:"; ls -1 "$CR/current" | sed 's/^/    /'

echo
echo "############ 3. 删业务 + 端口（入口）后重新发布 ############"
if [ -z "$BIZID" ]; then
  echo "ABORT: 没拿到 BIZ_ID —— 绝不能继续删入口（否则会留下悬空引用，之后所有发布都会被 422 拦下）"
  echo "STEP_S1S2_ABORTED"
  exit 9
fi
DELCODE=$(curl -s -b "$W/ck" -o /dev/null -w '%{http_code}' -X DELETE \
  -H "X-CSRF-Token: $CSRF" "$API/api/businesses/$BIZID")
echo "del_biz_http=$DELCODE"
if [ "$DELCODE" != "200" ]; then
  echo "ABORT: 删业务返回 $DELCODE（期望 200）—— 路由会级联删掉，所以这一步必须先成功。"
  echo "       绝不在业务还被路由引用时删入口。"
  echo "STEP_S1S2_ABORTED"
  exit 9
fi
# 双保险：确认已经没有任何路由引用这个入口
DBP=/var/lib/shengyu-edgelink/meta.db
REFS=$(python3 - "$DBP" "$SNI9443" <<'PY2'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
n = c.execute("select count(*) from routes where sni_entry_id=?", (sys.argv[2],)).fetchone()[0]
print(n)
PY2
)
echo "routes_referencing_9443=$REFS (必须为 0)"
[ "$REFS" = "0" ] || { echo "ABORT: 还有 $REFS 条路由引用该入口，删入口会留下悬空引用"; echo "STEP_S1S2_ABORTED"; exit 9; }
DB=$DBP
cp -a "$DB" "$DB.before-sni-delete-2"
python3 - "$DB" "$SNI9443" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
print("sni before:", list(c.execute("select id,bind_port,enabled from sni_entries")))
c.execute("delete from sni_entries where id=?", (sys.argv[2],))
c.commit()
print("sni after :", list(c.execute("select id,bind_port,enabled from sni_entries")))
c.close()
PY
systemctl restart shengyu-edgelink-server; sleep 2; systemctl is-active shengyu-edgelink-server
login   # 记住了：重启管理面会让会话失效
NODE=$(cat "$W/node_id")

echo "--- 3.3 先用 /preview 确认平台校验通过（这是发布前的同一套校验）---"
curl -s -b "$W/ck" "$API/api/nodes/$NODE/preview" > "$W/prev.json"
python3 -c "
import json;d=json.load(open('/tmp/b2/prev.json'))
print('validation_ok =', d.get('validation_ok'))
print('issues        =', d.get('issues'))
if d.get('validation_ok') is not True: raise SystemExit('PREVIEW_NOT_OK')" || { echo "ABORT: 平台校验未通过，先修数据再发布"; echo "STEP_S1S2_ABORTED"; exit 9; }

pub "S2: 去掉 9443 入口后重新发布" "$W/s3.json"
V3=$(python3 -c "import json;print(json.load(open('/tmp/b2/s3.json')).get('version'))")
echo
echo "############ 4. 判据 ############"
echo "--- 4.1 旧 sni_allow_9443.lst ---"
if [ -e "$CR/current/sni_allow_9443.lst" ]; then
  echo "CHECK_9443_REMOVED=FAIL (残留: $(ls -l $CR/current/sni_allow_9443.lst))"
else
  echo "CHECK_9443_REMOVED=PASS (current/ 已无 sni_allow_9443.lst)"
fi
echo "--- 4.2 current/ 文件集合 vs 第 $V3 版目录 ---"
A=$(ls -1 "$CR/current" | grep -v '\.tmp$' | grep -v '^VERSION$' | sort)
B=$(ls -1 "$CR/versions/$NODE/v$V3" | grep -v '\.tmp$' | sort)
echo "current(除VERSION): $(echo $A)"; echo "v$V3 目录        : $(echo $B)"
[ "$A" = "$B" ] && echo "CHECK_FILESET_EQUAL=PASS" || echo "CHECK_FILESET_EQUAL=FAIL"
echo "--- 4.3 逐文件 sha256 ---"
allsame=1
for f in $B; do
  a=$(sha256sum "$CR/versions/$NODE/v$V3/$f" | cut -d' ' -f1); b=$(sha256sum "$CR/current/$f" 2>/dev/null | cut -d' ' -f1)
  if [ "$a" = "$b" ] && [ -n "$a" ]; then echo "  SAME $f"; else echo "  DIFF $f"; allsame=0; fi
done
[ "$allsame" = "1" ] && echo "BYTE_IDENTICAL=PASS" || echo "BYTE_IDENTICAL=FAIL"
echo "--- 4.4 current/ 完整清单 + 多余残留审计 ---"
python3 - <<'PY'
import json, os
cur = "/etc/shengyu-edgelink/haproxy/current"
mf = json.load(open(os.path.join(cur, "manifest.json")))
want = set(mf["files"]) | {"manifest.json", "VERSION"}
got = set(n for n in os.listdir(cur) if not n.endswith(".tmp"))
print("manifest.files =", mf["files"])
print("current/       =", sorted(got))
print("多余残留       =", sorted(got - want), "->", "PASS" if not (got - want) else "FAIL")
print("缺少文件       =", sorted(want - got), "->", "PASS" if not (want - got) else "FAIL")
PY
echo "--- 4.5 current/VERSION = $(cat $CR/current/VERSION) ---"
echo "--- 4.6 8443 正常业务 ---"
timeout 8 openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com -brief </dev/null 2>&1 | head -1
echo "--- 4.7 haproxy 进程 / -c ---"
echo "haproxy -c = $(haproxy -c -f $CR/current/haproxy.cfg 2>&1 | tail -1)"
ps -o pid=,cmd= -C haproxy | head -3
echo "--- 4.8 meta ---"
curl -s -b "$W/ck" "$API/api/meta" | python3 -c "
import json,sys;d=json.load(sys.stdin);n=d['node']
print('applied/expected/skew =', n['applied_version'],'/',n['expected_version'],'/',n['version_skewed'])
print('business_count =', d.get('business_count'))"
echo "STEP_S1S2_DONE"
