#!/usr/bin/env bash
# B2 第六轮（v0.4.0 正式版）- 一键串联：部署 → 删除接口/悬空引用/socket 清理/摄入可见
#
# 用法：bash /tmp/b2-scripts/b2-99h-final.sh 2>&1 | tee /root/b2-accept6.log
set -u
S=/tmp/b2-scripts
LOG=/root/b2-accept6.log
: > "$LOG"
run() {
  echo | tee -a "$LOG"
  echo "############################################################" | tee -a "$LOG"
  echo "### $2   ($1)" | tee -a "$LOG"
  echo "############################################################" | tee -a "$LOG"
  bash "$S/$1" 2>&1 | tee -a "$LOG"
  echo "### <<< $1 结束 (exit=${PIPESTATUS[0]})" | tee -a "$LOG"
}
run b2-16-deploy40.sh    "第 16 步：部署 v0.4.0（只换管理面二进制）"
run b2-30-sni-delete.sh  "第 30 步：删除接口 / 悬空引用 / socket 清理 / 摄入可见"
echo | tee -a "$LOG"
echo "==================== 关键结论 ====================" | tee -a "$LOG"
grep -E '^(S3_HTTP=|S3=|S3B=|S5=|S5B=|S5C=|S6A=|S6B=|S6C=|S7=|S7B=|S7C=|S8=|S9=|DEPLOY40_DONE|V040_DONE)' "$LOG" | tee -a "$LOG"
echo | tee -a "$LOG"
echo "完整日志：$LOG" | tee -a "$LOG"
