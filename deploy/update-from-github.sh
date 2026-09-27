#!/usr/bin/env bash
# 从 GitHub main 拉取、构建并安全更新盛愈边缘网关。
# 用法：curl -fsSL https://raw.githubusercontent.com/yi172423683/shengyu-edgelink/main/deploy/update-from-github.sh | sudo bash
set -Eeuo pipefail

REPO_URL="${SHENGYU_REPO_URL:-https://github.com/yi172423683/shengyu-edgelink.git}"
BRANCH="${SHENGYU_BRANCH:-main}"
VERSION="${SHENGYU_VERSION:-v$(date -u +%Y%m%d.%H%M%S)}"
APP_ROOT="${SHENGYU_ROOT:-/opt/shengyu-edgelink}"
WORK="$APP_ROOT/.update-$$"
SOURCE_ROOT="$APP_ROOT/source"
TOOLCHAIN_ROOT="$APP_ROOT/toolchain"
BACKUP="$(mktemp -d /var/tmp/shengyu-backup.XXXXXX)"
BIN="$APP_ROOT/bin/shengyu-edgelink"
OLD_BIN="/usr/local/bin/shengyu-edgelink"
UNIT="/etc/systemd/system/shengyu-edgelink-server.service"
HEALTH_URL="${SHENGYU_HEALTH_URL:-http://127.0.0.1:8081/api/health}"
LISTEN=""
ROLLED_BACK=0

cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

fail() { echo "[失败] $*" >&2; exit 1; }
rollback() {
  [ "$ROLLED_BACK" -eq 1 ] && return 0
  ROLLED_BACK=1
  echo "[回滚] 新版本未通过检查，恢复旧管理程序"
  if [ -f "$BACKUP/binary" ]; then install -m 0755 "$BACKUP/binary" "$BIN"; fi
  if [ -f "$BACKUP/old-binary" ]; then install -m 0755 "$BACKUP/old-binary" "$OLD_BIN"; fi
  if [ -f "$BACKUP/unit" ]; then install -m 0644 "$BACKUP/unit" "$UNIT"; fi
  systemctl daemon-reload || true
  systemctl restart shengyu-edgelink-server || true
}
on_error() { rollback; echo "[失败] 更新未完成，现有数据和转发配置已保留。" >&2; exit 1; }
trap on_error ERR

[ "$(id -u)" -eq 0 ] || fail "请使用 sudo 执行此脚本"
command -v git >/dev/null || fail "缺少 git"
command -v systemctl >/dev/null || fail "缺少 systemd"
command -v curl >/dev/null || fail "缺少 curl"
[ -x /usr/sbin/haproxy ] || fail "未找到 /usr/sbin/haproxy，请先安装受支持的 HAProxy"

mkdir -p "$APP_ROOT"
if command -v go >/dev/null 2>&1; then
  GO_BIN="$(command -v go)"
else
  GO_VERSION="${SHENGYU_GO_VERSION:-1.23.4}"
  case "$(uname -m)" in
    x86_64|amd64) GO_ARCH=amd64 ;;
    aarch64|arm64) GO_ARCH=arm64 ;;
    *) fail "不支持的 CPU 架构：$(uname -m)" ;;
  esac
  echo "[准备] 未检测到 Go，自动安装 Go $GO_VERSION 到 $TOOLCHAIN_ROOT"
  mkdir -p "$TOOLCHAIN_ROOT" "$WORK"
  GO_TAR="$WORK/go.tar.gz"
  curl -fL --retry 3 "https://go.dev/dl/go${GO_VERSION}.linux-${GO_ARCH}.tar.gz" -o "$GO_TAR"
  rm -rf "$TOOLCHAIN_ROOT/go"
  tar -xzf "$GO_TAR" -C "$TOOLCHAIN_ROOT"
  GO_BIN="$TOOLCHAIN_ROOT/go/bin/go"
fi
export PATH="$(dirname "$GO_BIN"):$PATH"

if systemctl cat shengyu-edgelink-server.service >/dev/null 2>&1; then
  LISTEN="$(systemctl cat shengyu-edgelink-server.service | sed -n 's/.*-listen[[:space:]]\+\([^[:space:]\\]*\).*/\1/p' | head -1 || true)"
fi
LISTEN="${LISTEN:-${SHENGYU_LISTEN:-127.0.0.1:8081}}"

echo "[1/7] 拉取 GitHub：$BRANCH"
rm -rf "$WORK" "$SOURCE_ROOT"
mkdir -p "$WORK"
git clone --depth 1 --branch "$BRANCH" "$REPO_URL" "$WORK/source"
mv "$WORK/source" "$SOURCE_ROOT"
cd "$SOURCE_ROOT"
COMMIT="$(git rev-parse --short HEAD)"
echo "      提交：$COMMIT，版本：$VERSION"

echo "[2/7] 检查代码"
export CGO_ENABLED=0
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export GOMAXPROCS="${GOMAXPROCS:-1}"
go vet ./cmd/... ./internal/...
go test ./cmd/... ./internal/...

echo "[3/7] 构建 amd64"
mkdir -p "$WORK/package/deploy"
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.Version=$VERSION" \
  -o "$WORK/package/shengyu-edgelink-linux-amd64" ./cmd/shengyu-edgelink
cp -a deploy/. "$WORK/package/deploy/"
cp install.sh README.md "$WORK/package/"
chmod 0755 "$WORK/package/shengyu-edgelink-linux-amd64" "$WORK/package/install.sh" "$WORK/package/deploy/"*.sh

echo "[4/7] 备份当前管理程序"
if [ -x "$BIN" ]; then cp -p "$BIN" "$BACKUP/binary"; fi
if [ -x "$OLD_BIN" ]; then cp -p "$OLD_BIN" "$BACKUP/old-binary"; fi
if [ -f "$UNIT" ]; then cp -p "$UNIT" "$BACKUP/unit"; fi

echo "[5/7] 安装新版（监听地址保持为 $LISTEN）"
SHENGYU_ROOT="$APP_ROOT" bash "$WORK/package/install.sh" --listen "$LISTEN"

echo "[6/7] 重启管理服务"
systemctl restart shengyu-edgelink-server
sleep 2

echo "[7/7] 健康检查"
HEALTH="$(curl --fail --silent --show-error --max-time 10 "$HEALTH_URL")"
case "$HEALTH" in
  *'"ok":true'*) echo "[完成] 更新成功：$VERSION（$COMMIT）"; echo "      $HEALTH" ;;
  *) fail "健康检查返回异常：$HEALTH" ;;
esac
