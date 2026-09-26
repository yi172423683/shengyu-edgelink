#!/usr/bin/env bash
# B2 重跑 - 第 0 步：侦察现状 + 全量备份
#
# 目的：在改任何东西之前，先把"现在的现场"记下来，并且把所有会被本次验收碰到的
# 东西备份成一个可复原的目录。上次验收已经证明"不备份就动手"是不可接受的：
# 9443 SNI 入口**没有删除接口**，只能靠备份的 meta.db 复原。
set -u
API=http://127.0.0.1:8081
W=/tmp/b2
mkdir -p "$W"

TS=$(date -u +%Y%m%d-%H%M%S)
BK=/root/b2-accept-$TS
mkdir -p "$BK"

echo "############ 0. 基本信息 ############"
date -u
echo "hostname=$(hostname)"
uname -a
echo "installed_bin=$(sha256sum /usr/local/bin/shengyu-edgelink | cut -d' ' -f1)"
/usr/local/bin/shengyu-edgelink -version 2>&1 | head -2

echo
echo "############ 1. 服务与进程 ############"
systemctl is-enabled shengyu-edgelink-server shengyu-edgelink-haproxy 2>&1
systemctl is-active shengyu-edgelink-server shengyu-edgelink-haproxy 2>&1
echo "haproxy_master_pid=$(systemctl show -p MainPID --value shengyu-edgelink-haproxy)"
echo "server_main_pid=$(systemctl show -p MainPID --value shengyu-edgelink-server)"
echo "haproxy_procs=$(ps -o pid=,cmd= -C haproxy | tr '\n' '|')"

echo
echo "############ 2. 当前生效配置 ############"
CR=/etc/shengyu-edgelink/haproxy
echo "current_dir=$CR/current"
echo "VERSION=$(cat $CR/current/VERSION 2>/dev/null)"
echo "--- ls -la current/ ---"
ls -la "$CR/current/"
echo "--- sha256 of current/ ---"
sha256sum "$CR/current/"* 2>/dev/null
echo "--- versions tree ---"
find "$CR/versions" -maxdepth 3 -type f | sort
echo "--- 每版的文件清单（manifest.json 的 files 字段）---"
for d in "$CR"/versions/*/v*; do
  [ -d "$d" ] || continue
  if [ -f "$d/manifest.json" ]; then
    echo "$d -> $(python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print(','.join(d.get('files',[])))" "$d/manifest.json" 2>/dev/null)"
  else
    echo "$d -> (no manifest.json; files: $(ls "$d" 2>/dev/null | tr '\n' ','))"
  fi
done

echo
echo "############ 3. 统计套接字（旧版本的会不会残留）############"
SD=/run/shengyu-edgelink
ls -la "$SD" 2>/dev/null
echo "--- 逐个探测是否有 worker 应答 ---"
for s in "$SD"/stats-v*.sock; do
  [ -e "$s" ] || continue
  printf '%s : ' "$(basename "$s")"
  python3 - "$s" <<'PY' 2>/dev/null || echo "(probe failed)"
import socket, sys
p = sys.argv[1]
try:
    c = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    c.settimeout(2)
    c.connect(p)
    c.sendall(b"show info\n")
    data = c.recv(4096)
    line = [l for l in data.decode(errors="replace").split("\n") if l.startswith("Version")]
    print("ALIVE", line[0] if line else "")
except Exception as e:
    print("DEAD(%s)" % e.__class__.__name__)
PY
done

echo
echo "############ 4. 端口与转发 ############"
ss -lntp 2>/dev/null | head -20
echo -n "8443 handshake = "
timeout 8 openssl s_client -connect 127.0.0.1:8443 -servername verify.example.com -brief </dev/null 2>&1 | head -1
echo -n "9443 handshake = "
timeout 8 openssl s_client -connect 127.0.0.1:9443 -servername rbui2-test.example.com -brief </dev/null 2>&1 | head -1
echo -n "haproxy -c = "
haproxy -c -f "$CR/current/haproxy.cfg" 2>&1 | tail -1

echo
echo "############ 5. 定位元数据库与单元文件 ############"
systemctl cat shengyu-edgelink-server 2>/dev/null | sed -n '1,40p'
echo "--- drop-in 目录 ---"
ls -la /etc/systemd/system/shengyu-edgelink-server.service.d/ 2>/dev/null
ls -la /etc/systemd/system/shengyu-edgelink-haproxy.service.d/ 2>/dev/null
echo "--- meta.db ---"
find /etc/shengyu-edgelink /var/lib/shengyu-edgelink -maxdepth 3 -name '*.db' 2>/dev/null

echo
echo "############ 6. 登录并读平台侧状态 ############"
PW=$(grep -oE '[A-Za-z0-9]{32}' /root/shengyu-install.log | head -1)
curl -s -c "$W/ck" -o "$W/login.json" -X POST "$API/api/login" \
  -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$PW\"}"
CSRF=$(sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p' "$W/login.json")
echo "$CSRF" > "$W/csrf"
echo "csrf_len=${#CSRF}"

curl -s -b "$W/ck" "$API/api/meta" > "$W/meta.json"
python3 -c "
import json;d=json.load(open('/tmp/b2/meta.json'))
print('product        =', d.get('product'), d.get('version'))
print('initialized    =', d.get('initialized'))
print('business_count =', d.get('business_count'))
print('node           =', json.dumps(d.get('node'), ensure_ascii=False))
"

curl -s -b "$W/ck" "$API/api/nodes" > "$W/nodes.json"
NODE=$(python3 -c "
import json;d=json.load(open('/tmp/b2/nodes.json'))
its=d.get('items') or d.get('nodes') or []
print(its[0]['id'] if its else '')
")
echo "$NODE" > "$W/node_id"
echo "NODE_ID=$NODE"

curl -s -b "$W/ck" "$API/api/businesses" > "$W/biz.json"
python3 -c "
import json;d=json.load(open('/tmp/b2/biz.json'))
print('total businesses =', d.get('total'))
for b in (d.get('items') or []):
    print('  ', b['id'], b['name'], b.get('mode'), b.get('enabled'), b.get('domains'))
"
curl -s -b "$W/ck" "$API/api/nodes/$NODE/sni-entries" > "$W/sni.json"
python3 -c "
import json;d=json.load(open('/tmp/b2/sni.json'))
its=d.get('items') or []
print('SNI entries =', len(its))
for e in its:
    print('  ', e['id'], e['bind_addr'], e['bind_port'], 'enabled=', e['enabled'], 'created=', e.get('created_at'))
"

echo
echo "############ 7. 备份 ############"
mkdir -p "$BK"
cp -a /etc/shengyu-edgelink "$BK/etc-shengyu-edgelink"
tar -czf "$BK/etc-shengyu-edgelink.tar.gz" -C /etc shengyu-edgelink
cp -a /etc/systemd/system/shengyu-edgelink-server.service "$BK/" 2>/dev/null
cp -a /etc/systemd/system/shengyu-edgelink-haproxy.service "$BK/" 2>/dev/null
cp -a /etc/systemd/system/shengyu-edgelink-server.service.d "$BK/server.service.d" 2>/dev/null
cp -a /etc/systemd/system/shengyu-edgelink-haproxy.service.d "$BK/haproxy.service.d" 2>/dev/null
cp -a /etc/polkit-1/rules.d/50-shengyu-edgelink.rules "$BK/" 2>/dev/null
cp -a /usr/local/bin/shengyu-edgelink "$BK/shengyu-edgelink.installed.bin"
for f in $(find /etc/shengyu-edgelink /var/lib/shengyu-edgelink -maxdepth 3 -name '*.db' 2>/dev/null); do
  cp -a "$f" "$BK/$(basename "$f").bak"
  echo "backed up db: $f -> $BK/$(basename "$f").bak"
done
cp "$W/meta.json" "$W/nodes.json" "$W/biz.json" "$W/sni.json" "$BK/" 2>/dev/null
{
  echo "ts_utc=$(date -u +%FT%TZ)"
  echo "installed_bin_sha256=$(sha256sum /usr/local/bin/shengyu-edgelink | cut -d' ' -f1)"
  echo "current_VERSION=$(cat $CR/current/VERSION 2>/dev/null)"
  echo "current_files=$(ls "$CR/current" | tr '\n' ',')"
  echo "current_cfg_sha256=$(sha256sum "$CR/current/haproxy.cfg" | cut -d' ' -f1)"
  echo "haproxy_master_pid=$(systemctl show -p MainPID --value shengyu-edgelink-haproxy)"
  echo "node_id=$NODE"
} > "$BK/facts-before.txt"
cat "$BK/facts-before.txt"
echo "$BK" > "$W/backup_dir"
ls -la "$BK"
echo "PREP_DONE backup=$BK"
