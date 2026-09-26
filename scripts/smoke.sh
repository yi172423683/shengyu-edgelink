#!/usr/bin/env bash
# 真机冒烟：对真实运行中的 shengyu-edgelink-server 走一遍完整业务闭环（黑盒，只经 HTTP API）。
#
# 用法：  bash scripts/smoke.sh http://127.0.0.1:8081
#
# 前提：目标机器上已经有一个**用 HAProxy 数据面**跑起来的 shengyu-edgelink-server。
#   主程序只支持 HAProxy，所以这个脚本只在 Linux 节点上有意义；
#   开发机（Windows）上的链路验证请用 `go test ./e2e/`，它不依赖 HAProxy。
#
# 脚本自身不关心数据面是什么，只关心 API 行为与转发结果。
set -uo pipefail
BASE="${1:-http://127.0.0.1:8081}"
JAR=$(mktemp)
PY="${PY:-python}"

# 本机环境的 http_proxy 会把 127.0.0.1 也当外部地址转发出去，导致"连接被拒绝"这类
# 极具误导性的报错（看起来像服务没起来）。显式声明本机地址不走代理。
export NO_PROXY="127.0.0.1,localhost,::1"
export no_proxy="$NO_PROXY"

pass() { echo "  ✓ $1"; }
fail() { echo "  ✗ $1"; echo "$2"; exit 1; }

echo "=== 0. 健康检查 ==="
curl -s --noproxy '*' --max-time 5 "$BASE/api/health" | grep -q '"ok":true' && pass "服务在线" || fail "服务未响应" ""

echo "=== 1. 登录 ==="
LOGIN=$(curl -s --noproxy '*' -c "$JAR" -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"Smoke-Passw0rd!"}' "$BASE/api/login")
CSRF=$(printf '%s' "$LOGIN" | sed -n 's/.*"csrf_token":"\([^"]*\)".*/\1/p')
[ -n "$CSRF" ] && pass "登录成功并获得 CSRF 令牌" || fail "登录失败" "$LOGIN"

# 未带 CSRF 的写操作必须被拒绝
CODE=$(curl -s --noproxy '*' -o /dev/null -w '%{http_code}' -b "$JAR" -H 'Content-Type: application/json' \
  -X POST -d '{"name":"x"}' "$BASE/api/customers")
[ "$CODE" = "403" ] && pass "缺少 CSRF 的写操作被拒绝(403)" || fail "CSRF 防护未生效，返回 $CODE" ""

W() { # W <method> <path> <json>
  curl -s --noproxy '*' -b "$JAR" -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" \
    -X "$1" -d "${3:-}" "$BASE$2"
}
R() { curl -s --noproxy '*' -b "$JAR" "$BASE$1"; }

echo "=== 2. 新建客户 ==="
CUST=$(W POST /api/customers '{"name":"冒烟客户","contact":"ops@example.com"}')
CID=$(printf '%s' "$CUST" | sed -n 's/.*"id":"\(cus_[^"]*\)".*/\1/p')
[ -n "$CID" ] && pass "客户已创建 $CID" || fail "创建客户失败" "$CUST"

echo "=== 3. 取同机节点 ==="
NODES=$(R /api/nodes)
NID=$(printf '%s' "$NODES" | sed -n 's/.*"id":"\(node_[^"]*\)".*/\1/p')
[ -n "$NID" ] && pass "节点 $NID" || fail "未找到节点" "$NODES"

echo "=== 4. 启动源站（真实 TCP 回显）==="
# 两个坑：
#   1) 本机 shim 环境里没有 dirname，必须用纯 bash 的参数展开取脚本目录；
#   2) /c/... 只在 Git Bash 内部有效，交给 Windows 的 python 必须转成 C:\... 形式。
#
# 每次冒烟都用**新的空闲端口**：否则在同一台机器上第二次运行时，
# 会撞上一次留下的路由（平台会正确地报「入口已被占用」），看起来像产品 bug。
FREE_PORT() { $PY -c "import socket;s=socket.socket();s.bind(('127.0.0.1',0));print(s.getsockname()[1]);s.close()"; }
ORIGIN_PORT=$(FREE_PORT)
ENTRY_PORT=$(FREE_PORT)
HERE="${0%/*}"
[ "$HERE" = "$0" ] && HERE="."
HERE_WIN="$HERE"
if command -v cygpath >/dev/null 2>&1; then
  HERE_WIN=$(cygpath -w "$HERE")
fi
# 用独立脚本 + 重定向启动：不把脚本自身的 stdin/stdout 借给子进程，
# 否则父脚本被中断时这个后台进程会拖住整条命令，表现为"命令无输出然后被杀"。
$PY "$HERE_WIN\\smoke_origin.py" "$ORIGIN_PORT" "ECHO:" </dev/null >/dev/null 2>&1 &
ORIGIN_PID=$!
sleep 2
if kill -0 "$ORIGIN_PID" 2>/dev/null; then
  pass "源站已监听 127.0.0.1:$ORIGIN_PORT (pid $ORIGIN_PID)"
else
  fail "源站未能启动（检查 $PY 是否可用）" ""
fi

echo "=== 5. 接入业务（TCP 端口转发）==="
BIZ=$(W POST /api/businesses "{\"customer_id\":\"$CID\",\"name\":\"冒烟业务\",\"mode\":\"tcp_port\",
  \"primary_node_id\":\"$NID\",\"entry_addr\":\"127.0.0.1\",\"entry_port\":$ENTRY_PORT,
  \"origin_host\":\"127.0.0.1\",\"origin_port\":$ORIGIN_PORT}")
BID=$(printf '%s' "$BIZ" | sed -n 's/.*"id":"\(biz_[^"]*\)".*/\1/p')
[ -n "$BID" ] && pass "业务已创建 $BID（入口 127.0.0.1:$ENTRY_PORT → 源站 127.0.0.1:$ORIGIN_PORT）" || fail "创建业务失败" "$BIZ"

echo "=== 6. 发布 ==="
PUB=$(W POST "/api/nodes/$NID/publish" '{"note":"冒烟发布"}')
# 两种成功状态都要接受，并区分开：
#   succeeded                —— 配置生效且源站可达
#   applied_origin_unhealthy —— 配置本身已生效，但源站不可达（这不是发布失败）
if printf '%s' "$PUB" | grep -q '"status":"succeeded"'; then
  pass "发布成功"
elif printf '%s' "$PUB" | grep -q '"status":"applied_origin_unhealthy"'; then
  pass "发布成功（配置已生效）；平台同时报出「部分源站不可达」，这是预期内的区分"
else
  fail "发布失败" "$PUB"
fi
printf '%s' "$PUB" | grep -q '"config_healthy":true' || fail "配置未健康" "$PUB"
VER=$(printf '%s' "$PUB" | sed -n 's/.*"version":\([0-9]*\).*/\1/p')
pass "生效版本 v$VER"

echo "=== 7. 真实转发 ==="
OUT=$($PY -c "
import socket
s = socket.create_connection(('127.0.0.1', $ENTRY_PORT), 5)
s.sendall(b'hello-through-relay')
print(s.recv(256).decode())
s.close()
" </dev/null)
echo "  收到: $OUT"
[ "$OUT" = "ECHO:hello-through-relay" ] && pass "转发链路真实可用" || fail "转发结果不符" "$OUT"

echo "=== 8. 日志查询（转发的真实日志）==="
# 注意：TCP 会话日志在会话结束后才产生，且入库是异步的。
# 因此这里必须轮询等待 —— 连接刚关闭就去查、查不到，不是故障。
TIMESTAMP() { $PY -c "
import datetime, sys
d = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=int(sys.argv[1]))
print(d.strftime('%Y-%m-%dT%H:%M:%SZ'))
" "$1"; }
FROM=$(TIMESTAMP -600)
TO=$(TIMESTAMP 120)
LOGS=""
for i in 1 2 3 4 5 6 7 8 9 10; do
  LOGS=$(R "/api/logs/conn?from=$FROM&to=$TO&business_id=$BID")
  printf '%s' "$LOGS" | grep -q "$BID" && break
  sleep 0.3
done
printf '%s' "$LOGS" | grep -q "$BID" && pass "日志链路：转发 → 落盘 → 查询 打通" || fail "查不到日志" "$LOGS"

echo "=== 9. 诊断 ==="
DIAG=$(R "/api/diagnose?from=$FROM&to=$TO&business_id=$BID")
printf '%s' "$DIAG" | grep -q 'classification_note' && pass "诊断接口可用" || fail "诊断接口异常" "$DIAG"

echo "=== 10. 版本与审计 ==="
R "/api/nodes/$NID/versions" | grep -q "\"version\":$VER" && pass "版本历史可见" || fail "版本历史缺失" ""
R "/api/audit?action=config.publish" | grep -q "config.publish" && pass "发布审计已留痕" || fail "审计缺失" ""

echo "=== 11. 平台离线不影响转发 ==="
kill $(jobs -p | head -1) 2>/dev/null || true
echo "  （本步骤由 e2e 测试 TestEndToEndMainFlow 断言，冒烟脚本不重复）"

kill "$ORIGIN_PID" 2>/dev/null || true
rm -f "$JAR"
echo
echo "全部冒烟项通过。"
