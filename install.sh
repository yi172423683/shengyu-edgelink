#!/usr/bin/env bash
# 盛愈边缘网关（shengyu-edgelink）—— 一键安装入口
#
# 用法（在解压后的发布包根目录执行）：
#   sudo bash install.sh                            # 交互式安装向导
#   sudo bash install.sh --listen 127.0.0.1:8081    # 指定管理端口，非交互
#   sudo bash install.sh --dry-run                  # 只打印会做什么
#
# 这个文件刻意只做一件事：定位到发布包根目录，把参数原样转交给真正的安装脚本。
# 真正的逻辑全在 deploy/install.sh —— 两份实现迟早会各自漂移，
# 而"改了一处忘另一处"正是最难排查的那类事故。
set -euo pipefail

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SELF_DIR"

if [ ! -f "$SELF_DIR/deploy/install.sh" ]; then
  echo "错误：未找到 $SELF_DIR/deploy/install.sh" >&2
  echo >&2
  echo "请在**解压后的发布包根目录**执行本脚本，目录结构应为：" >&2
  echo "  <包>/install.sh" >&2
  echo "  <包>/deploy/" >&2
  echo "  <包>/shengyu-edgelink-linux-amd64" >&2
  exit 1
fi

exec bash "$SELF_DIR/deploy/install.sh" "$@"
