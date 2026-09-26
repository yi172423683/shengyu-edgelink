#!/usr/bin/env bash
#
# 在 Linux（Debian 12 / Ubuntu / Rocky 等）上装**官方** Go 工具链并跑完整验证。
#
# 用法（在源码树根目录下执行）：
#   bash scripts/linux-verify.sh
#   GO_MIRROR=https://golang.google.cn/dl bash scripts/linux-verify.sh   # 国内机器
#
# 为什么要有这个脚本：
#   开发环境（Windows）上的 Go 工具链坏过一次 —— src/ 目录残缺，连 hello world 都编不过。
#   用坏工具链跑出来的"通过/不通过"没有意义，而这类损坏**不会**报错说"我坏了"，
#   它只是在某些包上报一些看起来像代码问题的错。
#   所以验证链的第一环必须是：在一台刚装好系统的机器上，用官方包跑出一份可复现的结果。
#
# 这个脚本只做三件事：装官方 Go（带 sha256 校验）→ 跑 vet/test/race → 把结果写进文件。
# 它**不改系统 PATH 的持久配置**（只在本进程内 export），不删任何已有 Go 安装之外的数据。
set -euo pipefail

GO_VER="${GO_VER:-1.23.4}"
GO_MIRROR="${GO_MIRROR:-https://go.dev/dl}"
RESULT="${RESULT:-$(pwd)/verify-result.txt}"

# 官方 sha256（来源：https://go.dev/dl/?mode=json，可自行核对后更新）
SHA_AMD64="6924efde5de86fe277676e929dc9917d466efa02fb934197bc2eba35d5680971"
SHA_ARM64="16e5017863a7f6071363782b1b8042eb12c6ca4f4cd71528b2123f0a1275b13e"

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64 | amd64) GOARCH=amd64; WANT="$SHA_AMD64" ;;
  aarch64 | arm64) GOARCH=arm64; WANT="$SHA_ARM64" ;;
  *)
    echo "不支持的架构：$ARCH（只支持 amd64 / arm64）" >&2
    exit 1
    ;;
esac

TARBALL="go${GO_VER}.linux-${GOARCH}.tar.gz"
say() { printf '\n=== %s ===\n' "$1"; }

say "1. 下载官方 Go ${GO_VER} (linux/${GOARCH})"
echo "镜像：$GO_MIRROR"
if command -v curl >/dev/null 2>&1; then
  curl -fsSLO "${GO_MIRROR}/${TARBALL}"
elif command -v wget >/dev/null 2>&1; then
  wget -q "${GO_MIRROR}/${TARBALL}"
else
  echo "既没有 curl 也没有 wget，请先装一个。" >&2
  exit 1
fi

say "2. 校验 sha256（不做这一步，等于把供应链安全赌在镜像站上）"
echo "${WANT}  ${TARBALL}" | sha256sum -c -
echo "  校验通过：与官方发布值一致"

say "3. 安装到 /usr/local/go"
if [ -w /usr/local ]; then
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "$TARBALL"
else
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf "$TARBALL"
fi
export PATH=/usr/local/go/bin:$PATH
go version

say "4. 依赖与模块"
export GOFLAGS=-mod=mod
# 国内机器用 goproxy.cn；境外机器默认 proxy.golang.org。可用 GOPROXY=... 覆盖。
if [ -z "${GOPROXY:-}" ]; then
  export GOPROXY="https://goproxy.cn,direct"
fi
export GOSUMDB=off
echo "GOPROXY=$GOPROXY"
go mod download all

say "5. go vet ./..."
set +e
go vet ./... 2>&1 | tee /tmp/shengyu-vet.log
VET=${PIPESTATUS[0]}
set -e
echo "vet exit=$VET"

say "6. go test ./..."
set +e
go test ./... 2>&1 | tee /tmp/shengyu-test.log
TEST=${PIPESTATUS[0]}
set -e
echo "test exit=$TEST"

say "7. go test -race ./...（需要 gcc；没有会明确失败，不会静默跳过）"
if ! command -v gcc >/dev/null 2>&1; then
  echo "  未找到 gcc。Debian/Ubuntu: apt-get install -y gcc ；RHEL/Rocky: dnf install -y gcc"
fi
set +e
CGO_ENABLED=1 go test -race ./... 2>&1 | tee /tmp/shengyu-race.log
RACE=${PIPESTATUS[0]}
set -e
echo "race exit=$RACE"

say "8. 结果"
{
  echo "shengyu 验证结果"
  echo "时间(UTC): $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "主机: $(uname -a)"
  echo "系统: $(. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME")"
  echo "Go: $(go version)"
  echo "CPU: $(nproc)"
  echo "---"
  echo "go vet ./...        exit=$VET"
  echo "go test ./...       exit=$TEST"
  echo "go test -race ./... exit=$RACE"
  echo "---"
  echo "失败行（若有）："
  grep -hE '^(FAIL|--- FAIL|# |vet: )' /tmp/shengyu-vet.log /tmp/shengyu-test.log /tmp/shengyu-race.log 2>/dev/null | sort -u || true
} | tee "$RESULT"

echo
echo "结果已写入：$RESULT"
if [ "$VET" = 0 ] && [ "$TEST" = 0 ] && [ "$RACE" = 0 ]; then
  echo "全部通过（vet / test / race 退出码均为 0）。"
  echo "把这三条退出码与本文件内容一起贴进 docs/08-acceptance-linux.md §2.1 的实测栏。"
else
  echo "存在非 0 退出码：vet=$VET test=$TEST race=$RACE"
  echo "**带着失败的测试去做后面 17 项验收没有意义** —— 先修到全 0。"
  exit 1
fi
