#!/usr/bin/env bash
# shellcheck shell=bash
#
# shengyu 的 Go 构建环境。用法：  source scripts/goenv.sh
#
# 本机装有 360 主动防御，会拦截「新落盘文件的首次写入/执行」，表现为：
#   package runtime is not in std (...)
#   open ...\_pkg_.a: Access is denied.
#   fork/exec ...pkg.test.exe: Access is denied.
# 这些**不是编译错误**，同一命令重试即过。go_retry 负责这一层。
# 详见 skill: windows-go-release-build-with-av

# --- Git Bash 必备：这些工具在 PortableGit 里，默认 PATH 里没有 ---
_PG=/c/Users/Administrator/.workbuddy/binaries/PortableGit/versions/1.2.0
export PATH="$_PG/usr/bin:$_PG/bin:/c/Windows/System32:/c/Windows:$PATH"

# --- Go 工具链（隔离安装，不污染系统） ---
# 注意两条路径风格不能混：
#   * 给 Go 自己的变量（GOROOT/GOPATH/GOCACHE）必须 **Windows 风格**，否则 Go 会当成相对路径；
#   * 进 PATH 的条目必须是 **/c/... 风格**，否则 Git Bash 解析不到可执行文件（曾踩：go: command not found）。
export GOROOT='C:/Users/Administrator/.workbuddy/binaries/go-toolchain/go'
export GOPATH='C:/Users/Administrator/.workbuddy/binaries/go-path'
export GOCACHE='C:/Users/Administrator/.workbuddy/binaries/go-cache'
# PATH 用 /c/... 形式（Git Bash 只认这个）
export PATH="/c/Users/Administrator/.workbuddy/binaries/go-toolchain/go/bin:/c/Users/Administrator/.workbuddy/binaries/go-path/bin:$PATH"

# GOTMPDIR 必须是 **Windows 风格** 路径且目录已存在，否则报
# "go: creating work dir: ... The system cannot find the file specified"
export GOTMPDIR='C:\Users\Administrator\.workbuddy\binaries\go-tmp'
mkdir -p /c/Users/Administrator/.workbuddy/binaries/go-tmp

# 固定工具链版本，禁止 Go 自作主张去下载别的版本
export GOTOOLCHAIN=local
export GOFLAGS=-mod=mod
export GOSUMDB=off

# 默认离线（模块缓存已齐）；需要拉新依赖时用 GOPROXY_ON
export GOPROXY=off
export GOPROXY_ON='https://goproxy.cn,direct'

# --- 噪声重试 ---
# 注意：Go 报的路径是 C:\go\src\hash（单个反斜杠），ERE 里必须写 \\ 而不是 \\\\
AV_NOISE='_pkg_\.a: Access is denied|is not in std|Access is denied|being used by another process|internal compiler error|cannot find the file specified'

# go_retry <次数> <命令...>
go_retry() {
  local max=$1; shift
  local i=1 out rc
  while :; do
    out=$("$@" 2>&1); rc=$?
    if [ $rc -eq 0 ]; then
      [ -n "$out" ] && printf '%s\n' "$out"
      return 0
    fi
    if printf '%s' "$out" | grep -Eq "$AV_NOISE" && [ $i -lt "$max" ]; then
      echo "[go_retry] 第 $i 次命中安全软件噪声，重试：$*" >&2
      i=$((i + 1)); sleep 1; continue
    fi
    printf '%s\n' "$out"
    return $rc
  done
}

# 预热标准库：把「新增 .a 最多、最易被拦」的 std 编译放在这里一次性消化掉，
# 而不是让它发生在正式构建/测试流水线内部。
go_prewarm_std() {
  echo "=== 预热 std (host) ==="
  go_retry 6 go build std
  for t in "$@"; do
    echo "=== 预热 std ($t) ==="
    go_retry 6 env GOOS=${t%%/*} GOARCH=${t##*/} go build std
  done
}

# 切到项目根目录。
#
# 这里刻意**不写死项目路径**：项目会被移动位置（例如从 WorkBuddy 工作区挪到桌面），
# 写死路径的后果是"换台机器/换个目录就构建不了"，而且报错通常是一句
# "no Go files in ..."，很难一眼看出是路径问题。
# 用脚本自身的位置反推根目录，脚本跟着项目走，搬到哪都能用。
#
# 同样注意：本机 shim 环境里没有 `dirname`，只能用纯 bash 的参数展开。
_goenv_self="${BASH_SOURCE[0]}"
_goenv_dir="${_goenv_self%/*}"
[ "$_goenv_dir" = "$_goenv_self" ] && _goenv_dir="."
cd "$_goenv_dir/.." 2>/dev/null || true
