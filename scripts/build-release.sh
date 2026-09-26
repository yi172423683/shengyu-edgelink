#!/usr/bin/env bash
#
# 构建发布产物。
#
# 用法：
#   ./scripts/build-release.sh [版本号]        # 例：./scripts/build-release.sh v0.1.0
#
# 产物（都在 dist/ 下）：
#   shengyu-edgelink-linux-amd64          linux/amd64 二进制
#   shengyu-edgelink-linux-arm64          linux/arm64 二进制
#   shengyu-edgelink-linux-amd64-<版本>.tar.gz    amd64 交付包（二进制 + deploy/ + docs/ + README）
#   shengyu-edgelink-linux-arm64-<版本>.tar.gz    arm64 交付包
#   SHA256SUMS                          上面所有文件的 sha256
#
# 这个脚本刻意**先跑 vet 与测试再构建**：发布产物必须来自"测试通过的那份代码"，
# 否则"构建成功"没有意义——它能证明的只是代码能编译。
#
# 另一个闸门是"生产二进制里不得出现测试用数据面"（internal/testplane，前身 gorelay）。
# 它只允许出现在测试里，绝不能随交付物发出去：
# 一旦有人在发布路径上把它接成了降级内核，转发行为就会和验收时的不一致。
# 检查方式是 go tool nm 扫符号表，所以**先编一份不裁剪符号的副本做检查**，
# 通过后再编真正的发布产物（带 -s -w 裁剪）。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
  if git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1; then
    VERSION="$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)"
  else
    VERSION="dev"
  fi
fi
# 版本号统一带 v 前缀，避免出现 "0.1.0" 和 "v0.1.0" 两种写法混在验收记录里。
case "$VERSION" in v*) ;; *) VERSION="v$VERSION" ;; esac

DIST="$ROOT/dist"
STAGE_ROOT="$DIST/.stage-$VERSION"
rm -rf "$STAGE_ROOT"
mkdir -p "$DIST" "$STAGE_ROOT"

export CGO_ENABLED=0
export GOFLAGS=-mod=mod
export GOSUMDB=off
: "${GOPROXY:=https://goproxy.cn,direct}"
export GOPROXY

echo "== 工具链 =="
go version
echo "版本: $VERSION"

echo
echo "== go vet ./... =="
go vet ./...

echo
echo "== go test ./... =="
go test ./...

build_one() {
  local arch="$1"
  local stripped="$STAGE_ROOT/shengyu-edgelink-linux-$arch"
  local plain="$STAGE_ROOT/.plain-linux-$arch"

  # 1) 不裁剪符号的副本：只为做符号检查
  GOOS=linux GOARCH="$arch" go build -trimpath \
    -ldflags "-X main.Version=$VERSION" \
    -o "$plain" ./cmd/shengyu-edgelink

  if go tool nm "$plain" 2>/dev/null | grep -qi 'testplane'; then
    echo "拒绝发布：$arch 二进制里出现了测试用数据面符号（internal/testplane）。" >&2
    echo "它只允许出现在测试里，不能进入交付物。" >&2
    exit 1
  fi
  rm -f "$plain"

  # 2) 真正的发布产物
  GOOS=linux GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X main.Version=$VERSION" \
    -o "$stripped" ./cmd/shengyu-edgelink
  # 同时留一份"裸二进制"在 dist 根目录。
  #
  # 为什么必须显式拷过去：SHA256SUMS 里列了 shengyu-edgelink-linux-<arch> 四个文件，
  # 而它们原先只存在于 stage 目录（末尾会被删）。如果这里不拷，
  # dist 根目录留下的就是**上一次构建的旧二进制**，而 SHA256SUMS 会一本正经地
  # 给那个旧文件算摘要 —— 交付物与校验值对不上，且从字节上看不出来。
  cp "$stripped" "$DIST/shengyu-edgelink-linux-$arch"
  echo "  已构建 $stripped"
  echo "  已输出 $DIST/shengyu-edgelink-linux-$arch"
}

echo
echo "== 构建 =="
build_one amd64
build_one arm64

package_one() {
  local arch="$1"
  local name="shengyu-edgelink-linux-$arch-$VERSION"
  local dir="$STAGE_ROOT/$name"
  mkdir -p "$dir"
  cp "$STAGE_ROOT/shengyu-edgelink-linux-$arch" "$dir/shengyu-edgelink-linux-$arch"
  # 显式加可执行位：在 Windows 上打包时，文件系统本身没有"可执行"这个概念，
  # tar 会记成 0666。到 Linux 上解开后直接 `./shengyu-edgelink-linux-amd64` 就会
  # "Permission denied"。install.sh 用的是 `install -m 0755`，不受影响，
  # 但手工直接跑二进制的人会踩到，所以在打包前先补上。
  chmod +x "$dir/shengyu-edgelink-linux-$arch"
  cp -r "$ROOT/deploy" "$dir/deploy"
  cp -r "$ROOT/docs" "$dir/docs"
  cp "$ROOT/README.md" "$dir/README.md"
  # 根目录的一键安装入口**必须**进包：目标流程是"解压后 sudo bash install.sh"，
  # 少了它用户就得自己去 deploy/ 里翻，而"翻错了路径"会以找不到 deploy/ 的形式失败。
  cp "$ROOT/install.sh" "$dir/install.sh"
  chmod +x "$dir/install.sh"
  cat > "$dir/BUILD.txt" <<EOF
shengyu-edgelink $VERSION
build_arch=$arch
build_go=$(go env GOVERSION)
build_time=$(date -u +%Y-%m-%dT%H:%M:%SZ)
source_module=github.com/shengyu/edgelink

校验：sha256sum -c SHA256SUMS

安装（在本目录执行）：
  sudo bash install.sh --dry-run     # 先看会做什么
  sudo bash install.sh               # 一条命令：依赖检查 / 目录 / 单元 / 管理员 / 启动
EOF
  tar -czf "$DIST/$name.tar.gz" -C "$STAGE_ROOT" "$name"
  echo "  已打包 dist/$name.tar.gz"
}

echo
echo "== 打包 =="
package_one amd64
package_one arm64

# 交付包里只放二进制与部署文件，SHA256SUMS 放在 dist 根目录：
# 它要覆盖 tar.gz 本身，而 tar.gz 不可能包含自己的摘要。
cd "$DIST"
rm -f "SHA256SUMS-$VERSION"
for f in "shengyu-edgelink-linux-amd64" "shengyu-edgelink-linux-arm64" \
         "shengyu-edgelink-linux-amd64-$VERSION.tar.gz" "shengyu-edgelink-linux-arm64-$VERSION.tar.gz"; do
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$f" >> "SHA256SUMS-$VERSION"
  else
    shasum -a 256 "$f" >> "SHA256SUMS-$VERSION"
  fi
done
cp "SHA256SUMS-$VERSION" SHA256SUMS

rm -rf "$STAGE_ROOT"

echo
echo "== 产物 =="
ls -l "$DIST"
echo
cat "SHA256SUMS-$VERSION"
