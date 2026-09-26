#!/usr/bin/env bash
# B2 第五轮：清单外文件的处理边界（评审四条要求，见 docs/08 §10）
#
# 这一轮**不建任何业务/入口**，只操作版本目录里的文件，且每一步都有备份与复位。
#
#   S1  版本目录里存在清单之外的普通文件 → 发布**成功**，但结果里必须有 warning + 审计 + 界面提示
#   S2a 被配置引用的允许清单缺失          → 至少有一道防线拦住（HAProxy 语法检查 / 平台清单判据）
#   S2b 不被配置引用的清单内文件缺失      → 只能由平台的清单判据拦住（HAProxy 看不见 state.json）
#   S3  配置实际引用了清单外文件          → **拒绝**（那种文件运行时会被按绝对路径读到）
#   S4  current/ 最终只包含清单允许的文件 → 文件集合与清单完全一致，多余文件绝不进 current/
#
# 为什么 S1 不阻断、S3 必须拒绝：多余文件不会被写进 current/（写入以清单为准），
# 而当硬错误会**在事故中挡住回滚**；但"配置真的引用它"意味着那份配置会在运行时生效，
# 清单也就不再是"这一版由哪些文件组成"的权威说明。
#
# 第一次跑（上一轮）的三个 FAIL 全是本脚本自己的问题，已修：
#   ① 用 `mkdir -p` 预置版本目录 ⇒ 目录属主是 root，平台以 shengyu 跑 ⇒ 写不进去
#      （报 publish: 写入版本目录失败: ... permission denied）。改用 install -d -o shengyu。
#   ② S2 移走的是**被 cfg 引用**的允许清单 ⇒ HAProxy 的语法检查先拦下，走不到平台的清单判据。
#      拆成 S2a（两道防线任一即可）与 S2b（只能由平台判据拦，才是要验的那条）。
#   ③ 终检假设版本目录里有 VERSION —— 版本目录里没有它（VERSION 只属于 current/）。
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
login
NODE=$(cat "$W/node_id")
echo "NODE_ID=$NODE csrf_len=${#CSRF}"

# 现场查业务（不依赖上一轮遗留的 /tmp/b2/*.json）：脚本要能独立跑。
# 经验：POST/GET /api/businesses 返回的是**包装体** {"business":{...}}，
# 而 POST /api/customers 返回裸对象 —— 解析前先看清形状，这个坑踩过两次（docs/08 §8）。
curl -s -b "$W/ck" "$API/api/businesses" > "$W/bizs.json"
BIZID=$(python3 -c "
import json
d = json.load(open('/tmp/b2/bizs.json', encoding='utf-8'))
it = d.get('items') or []
print(it[0]['id'] if it else '')")
[ -n "$BIZID" ] || { echo "ABORT: 平台里没有业务，本用例需要一条（biz_mod 要改它来触发配置变化）"; exit 4; }
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
echo "BIZ=$BIZID CUS=$CUS SNI=$SNI_OF_BIZ DOMS=$DOMS"

biz_mod() { # $1=origin_port（端口变了配置文件内容才会变，否则会走 no_change）
  curl -s -b "$W/ck" -o /dev/null -w 'biz_put_http=%{http_code}\n' \
    -X PUT -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
    "$API/api/businesses/$BIZID" -d "{
      \"customer_id\":\"$CUS\",\"name\":\"验收业务\",\"remark\":\"B2 extra-files\",
      \"mode\":\"sni_tls\",\"domains\":$DOMS,\"primary_node_id\":\"$NODE\",
      \"sni_entry_id\":\"$SNI_OF_BIZ\",\"origin_host\":\"1.1.1.1\",\"origin_port\":$1}"
}

pub() { # $1=note $2=out
  curl -s -b "$W/ck" -o "$2" -w 'publish_http=%{http_code}\n' \
    -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
    "$API/api/nodes/$NODE/publish" -d "{\"note\":\"$1\"}"
}
rb() { # $1=version $2=out $3=reason
  curl -s -b "$W/ck" -o "$2" -w 'rollback_http=%{http_code}\n' \
    -X POST -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
    "$API/api/nodes/$NODE/rollback" -d "{\"version\":$1,\"reason\":\"$3\"}"
}
# rejectReason 打印响应体里可读的拒绝原因（不同接口的字段名不一样）。
rejectReason() {
  python3 - "$1" <<'PY'
import json, sys
raw = open(sys.argv[1], encoding='utf-8', errors='replace').read()
try:
    d = json.loads(raw)
except Exception:
    print("  body =", raw[:400]); raise SystemExit
for k in ("error", "detail", "message", "code"):
    if d.get(k):
        print("  %-8s = %s" % (k, d[k]))
PY
}

echo
echo "############ 0. 基线 ############"
V_BASE=$(cat "$CR/current/VERSION")
echo "V_BASE=$V_BASE"
# S3 要临时改坏基线的 haproxy.cfg，必须原样备份。
CFG_BAK="$W/v$V_BASE-haproxy.cfg.bak"
cp -a "$CR/versions/$NODE/v$V_BASE/haproxy.cfg" "$CFG_BAK"
CFG_SHA0=$(sha256sum "$CFG_BAK" | cut -d' ' -f1)
echo "cfg 备份 = $CFG_BAK  sha256=$CFG_SHA0"
echo "--- 清掉上一次失败留下的目录（owner=root，平台以 shengyu 跑，写不进去）---"
for d in "$CR"/versions/$NODE/v*; do
  [ -d "$d" ] || continue
  OWNER=$(stat -c '%U:%G' "$d")
  case "$OWNER" in
    *root*) echo "  发现 root 属主目录 $d（$(ls -1 "$d" | tr '\n' ' ')）"
            find "$d" -maxdepth 1 -type f -name 'haproxy.cfg.*' -delete
            rmdir "$d" 2>/dev/null && echo "  已删除 $d" || echo "  WARN: $d 非空，保留" ;;
  esac
done

echo
echo "############ S1：版本目录里存在清单之外的普通文件 ⇒ 不阻断 + warning ############"
NEXT=$(curl -s -b "$W/ck" "$API/api/nodes/$NODE/preview" | python3 -c 'import json,sys;print(json.load(sys.stdin)["version"])')
VDIR="$CR/versions/$NODE/v$NEXT"
# 关键：目录必须属于 shengyu —— 本平台带 root 跑脚本、以 shengyu 跑服务，
# 用 mkdir 造的 root 目录会让"写入版本目录"直接 permission denied（上一轮就是这么失败的）。
install -d -o shengyu -g shengyu -m 0750 "$VDIR"
install -o shengyu -g shengyu -m 0640 /dev/null "$VDIR/haproxy.cfg.orig"
echo "预置多余文件：$(ls -la "$VDIR/haproxy.cfg.orig")"

biz_mod 444
pub "S1: 版本目录里留了个备份" "$W/s1.json"
python3 - "$W/s1.json" "$NEXT" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding='utf-8')); nxt = int(sys.argv[2])
w = d.get("warnings") or []
print("  status   =", d.get("status"))
print("  version  =", d.get("version"), "(期望 %d)" % nxt)
print("  phase    =", d.get("phase"))
print("  warnings =", w)
print("  message  =", d.get("message"))
# status 允许两种：succeeded，或 applied_origin_unhealthy。
# 后者在本平台的语义是"**配置本身已生效**，只是源站探测没通，这不是发布失败"
# （见 publish.go 里 status := store.ReleaseAppliedUnhealthy 那段）。
# 本用例改了 origin_port=444，1.1.1.1:444 本来就不通 —— 这跟"清单外文件"无关。
ck = [("发布成功且配置已生效（%s）" % d.get("status"),
       d.get("status") in ("succeeded", "applied_origin_unhealthy")),
      ("产生了新版本 %d" % nxt, d.get("version") == nxt),
      ("结果里有 warnings", len(w) > 0),
      ("warning 点名 haproxy.cfg.orig", any("haproxy.cfg.orig" in x for x in w)),
      ("warning 说明性质（未纳入清单）", any("未纳入清单" in x for x in w)),
      ("warning 不影响结论（无 manual_fix）", not (d.get("manual_fix") or []))]
for k, v in ck: print(("PASS  " if v else "FAIL  ") + k)
print("S1=" + ("PASS" if all(v for _, v in ck) else "FAIL"))
PY

echo "--- S1b：warning 必须写进审计 ---"
python3 - "$DB" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
n = c.execute("select count(*) from audit where action='config.version_dir_extra_files'").fetchone()[0]
print("  audit(config.version_dir_extra_files) =", n)
row = c.execute("select ts, result, summary from audit where action='config.version_dir_extra_files' order by rowid desc limit 1").fetchone()
if row: print("  最近一条 =", row[0], row[1], "|", row[2])
print("S1B=" + ("PASS" if n > 0 else "FAIL"))
PY

echo "--- S1c：界面必须带这条提醒的文案 ---"
# 真机证据（不只信单元测试里的字面量断言）：页面是内嵌 HTML，直接取来看。
if curl -s -b "$W/ck" "$API/" | grep -q "存在未纳入清单的额外文件"; then
  echo "S1C=PASS（页面含提醒文案）"
else
  echo "S1C=FAIL"
fi

echo
echo "############ S4：current/ 只应含清单允许的文件 ############"
if [ -f "$VDIR/manifest.json" ]; then
  python3 - "$CR/current" "$VDIR" <<'PY'
import json, os, sys
cur, vdir = sys.argv[1], sys.argv[2]
mf = json.load(open(os.path.join(vdir, "manifest.json"), encoding="utf-8"))
want = set(mf["files"]) | {"manifest.json", "VERSION"}
got = {n for n in os.listdir(cur)
       if not n.endswith(".tmp") and os.path.isfile(os.path.join(cur, n))}
print("  manifest.files =", sorted(mf["files"]))
print("  current/       =", sorted(got))
print("  多余残留       =", sorted(got - want))
print("  缺少文件       =", sorted(want - got))
ck = [("current/ 与清单完全一致", got == want),
      ("多余文件没进 current/", "haproxy.cfg.orig" not in got)]
for k, v in ck: print(("PASS  " if v else "FAIL  ") + k)
print("S4=" + ("PASS" if all(v for _, v in ck) else "FAIL"))
PY
else
  echo "  SKIP: v$NEXT 没有清单（S1 未成功）"
  echo "S4=SKIP"
fi

echo
echo "############ S2a：被配置引用的允许清单缺失 ⇒ 至少一道防线要拦住 ############"
V_NOW=$(cat "$CR/current/VERSION")
VD="$CR/versions/$NODE/v$V_NOW"
ALLOW=$(ls -1 "$VD" | grep -E '^sni_allow_[0-9]+\.lst$' | head -1)
echo "把 v$V_NOW 的 $ALLOW 移出版本目录（cfg 与清单都还引用着它）"
mv "$VD/$ALLOW" "$W/moved-v$V_NOW-$ALLOW"
rb "$V_NOW" "$W/s2a.json" "S2a: 被引用的允许清单缺失"
rejectReason "$W/s2a.json"
python3 - "$W/s2a.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
blob = json.dumps(d, ensure_ascii=False)
ck = [("回滚被拒绝", d.get("status") != "succeeded" and not d.get("rolled_back")),
      ("未声称已回滚、未进入 verified", not d.get("rolled_back") and not d.get("rollback_verified")),
      ("原因可读（平台清单判据 或 HAProxy 语法检查）",
       ("版本目录不完整" in blob) or ("语法检查未通过" in blob) or ("failed to open" in blob))]
for k, v in ck: print(("PASS  " if v else "FAIL  ") + k)
print("S2A=" + ("PASS" if all(v for _, v in ck) else "FAIL"))
PY
mv "$W/moved-v$V_NOW-$ALLOW" "$VD/$ALLOW"
echo "已复位 $ALLOW"

echo
echo "############ S2b：不被配置引用的清单内文件缺失 ⇒ 只能由平台的清单判据拦住 ############"
# 这一条才是真正在验"清单完整性"：HAProxy 的语法检查看不见 state.json，
# 如果平台不查清单，这次回滚会"成功"，而版本目录其实已经残缺（下次就回不去了）。
mv "$VD/state.json" "$W/moved-v$V_NOW-state.json"
rb "$V_NOW" "$W/s2b.json" "S2b: 清单里的 state.json 缺失"
rejectReason "$W/s2b.json"
python3 - "$W/s2b.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
blob = json.dumps(d, ensure_ascii=False)
ck = [("回滚被拒绝", d.get("status") != "succeeded" and not d.get("rolled_back")),
      ("原因提到「版本目录不完整」", "版本目录不完整" in blob),
      ("原因点名缺失的文件 state.json", "state.json" in blob),
      ("未声称已回滚", not d.get("rolled_back"))]
for k, v in ck: print(("PASS  " if v else "FAIL  ") + k)
print("S2B=" + ("PASS" if all(v for _, v in ck) else "FAIL"))
PY
mv "$W/moved-v$V_NOW-state.json" "$VD/state.json"
echo "已复位 state.json"
echo "current/VERSION 仍为 $(cat "$CR/current/VERSION")（期望 $V_NOW）"

echo
echo "############ S3：配置引用了清单外的文件 ⇒ 必须拒绝 ############"
VD="$CR/versions/$NODE/v$V_BASE"
printf 'x.example.com\n' > "$VD/extra_allow.lst"
python3 - "$VD" <<'PY'
import os, sys
vd = sys.argv[1]
p = os.path.join(vd, "haproxy.cfg")
s = open(p, encoding="utf-8").read()
if not s.endswith("\n"):
    s += "\n"
# 形状与渲染器一致（tcp-request ... -f <版本目录>/<文件>），只是引用的文件没进清单。
s += ("\nfrontend s3_probe\n  bind 127.0.0.1:19000\n"
      "  tcp-request content reject unless { var(txn.sni) -m str -i -f " +
      vd.replace("\\", "/") + "/extra_allow.lst }\n")
open(p, "w", encoding="utf-8").write(s)
print("  注入引用 ->", vd + "/extra_allow.lst")
PY
echo "--- 先单独确认：注入后的 cfg **语法合法** ---"
echo "（避免假阳性：如果它本身语法就不合法，后面的「拒绝」就分不清是语法还是引用检查）"
if haproxy -c -f "$VD/haproxy.cfg" >/dev/null 2>&1; then
  echo "cfg_syntax_after_inject=VALID"
else
  echo "cfg_syntax_after_inject=INVALID  <-- 本用例无效，请检查注入内容"
  echo "S3_BLOCKED_BY_SYNTAX"
fi

rb "$V_BASE" "$W/s3.json" "S3: 目标版本配置引用了清单外文件"
rejectReason "$W/s3.json"
python3 - "$W/s3.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
blob = json.dumps(d, ensure_ascii=False)
ck = [("回滚被拒绝", d.get("status") != "succeeded" and not d.get("rolled_back")),
      ("原因点名 extra_allow.lst", "extra_allow.lst" in blob),
      ("原因说明是「不在清单内」", "不在清单" in blob),
      ("未声称已回滚", not d.get("rolled_back"))]
for k, v in ck: print(("PASS  " if v else "FAIL  ") + k)
print("S3=" + ("PASS" if all(v for _, v in ck) else "FAIL"))
PY

echo "--- 复位被改坏的基线版本目录 ---"
cp -a "$CFG_BAK" "$VD/haproxy.cfg"
rm -f "$VD/extra_allow.lst"
SHA1=$(sha256sum "$VD/haproxy.cfg" | cut -d' ' -f1)
echo "cfg sha256 = $SHA1"
echo "cfg sha256 备份 = $CFG_SHA0"
[ "$SHA1" = "$CFG_SHA0" ] && echo "CFG_RESTORED=PASS" || echo "CFG_RESTORED=FAIL"

echo
echo "############ 收尾：回到基线版本 + 业务端口改回 443 ############"
biz_mod 443
V_NOW=$(cat "$CR/current/VERSION")
if [ "$V_NOW" != "$V_BASE" ]; then
  rb "$V_BASE" "$W/rb_final.json" "B2 第五轮收尾：恢复改动前版本"
  python3 -c "
import json;d=json.load(open('/tmp/b2/rb_final.json'))
print('  status=',d.get('status'),'| version=',d.get('version'))
print('  rollback_verified=',d.get('rollback_verified'),'| outcome=',d.get('rollback_outcome'))
print('  message=',d.get('message'))"
fi
# 删掉 S1 造的"多余文件"（它在版本目录里，删掉不影响 current/）
rm -f "$CR/versions/$NODE/v$NEXT/haproxy.cfg.orig"
echo "已删除 v$NEXT/haproxy.cfg.orig（目录保留：里面有平台自己写的文件）"

echo
echo "############ 终检 ############"
echo "--- current/ 与基线逐字节比对（VERSION 是 current/ 独有的提交标记，不在版本目录里）---"
python3 - "$CR/current" "$CR/versions/$NODE/v$V_BASE" "$V_BASE" <<'PY'
import hashlib, json, os, sys
cur, vdir, ver = sys.argv[1], sys.argv[2], sys.argv[3]
mf = json.load(open(os.path.join(vdir, "manifest.json"), encoding="utf-8"))
want = set(mf["files"]) | {"manifest.json"}
got = {n for n in os.listdir(cur)
       if not n.endswith(".tmp") and os.path.isfile(os.path.join(cur, n))}
got_wo_ver = got - {"VERSION"}
same = []
for n in sorted(want & got_wo_ver):
    a = hashlib.sha256(open(os.path.join(cur, n), "rb").read()).hexdigest()
    b = hashlib.sha256(open(os.path.join(vdir, n), "rb").read()).hexdigest()
    same.append((n, a == b))
vnow = open(os.path.join(cur, "VERSION")).read().strip()
print("  current/VERSION      =", vnow, "(期望 %s)" % ver)
print("  current/(除 VERSION) =", sorted(got_wo_ver))
print("  清单期望             =", sorted(want))
for n, ok in same: print("  %s %s" % ("SAME" if ok else "DIFF", n))
ck = [("current/ 除 VERSION 外与第 %s 版清单一致" % ver, got_wo_ver == want),
      ("全部文件逐字节一致", all(ok for _, ok in same)),
      ("VERSION == %s" % ver, vnow == ver)]
for k, v in ck: print(("PASS  " if v else "FAIL  ") + k)
print("FINAL_FILESET=" + ("PASS" if all(v for _, v in ck) else "FAIL"))
PY
echo "--- 8443 业务 ---"
timeout 8 openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com </dev/null 2>&1 | grep -E "CONNECTION ESTABLISHED|Protocol version|Verify return code" | head -3
echo "--- haproxy -c / 进程 / meta ---"
haproxy -c -f "$CR/current/haproxy.cfg" 2>&1 | tail -1
pgrep -af 'haproxy -Ws' | head -2
curl -s -b "$W/ck" "$API/api/meta" | python3 -c "
import json,sys;d=json.load(sys.stdin);n=d.get('node') or {}
print('  applied/expected/skew =', n.get('applied_version'), '/', n.get('expected_version'), '/', n.get('version_skewed'))
print('  business_count =', d.get('business_count'), '| product =', d.get('product'))"
echo "--- 验收残留检查 ---"
echo "版本目录里的 .orig/.bak/extra_allow: $(find "$CR/versions" \( -name '*.orig' -o -name '*.bak' -o -name 'extra_allow.lst' \) | wc -l)"
echo "临时移动的文件: $(ls -1 $W/moved-* 2>/dev/null | wc -l) 个"
echo "EXTRA_FILES_DONE"
