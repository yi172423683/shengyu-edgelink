#!/usr/bin/env bash
# B2 第二轮 - 一键串联：10 部署 v0.3.5 → 11 空入口+缺陷二 → 12 五种回滚结论 → 13 清理恢复
#
# 前置：/tmp/shengyu-edgelink-v0.3.5 已上传；脚本在 /tmp/b2-scripts/
# 用法：bash /tmp/b2-scripts/b2-99c-second.sh 2>&1 | tee /root/b2-accept2.log
set -u
S=/tmp/b2-scripts
LOG=/root/b2-accept2.log
: > "$LOG"
run() {
  echo | tee -a "$LOG"
  echo "############################################################" | tee -a "$LOG"
  echo "### $2   ($1)" | tee -a "$LOG"
  echo "############################################################" | tee -a "$LOG"
  bash "$S/$1" 2>&1 | tee -a "$LOG"
  echo "### <<< $1 结束 (exit=${PIPESTATUS[0]})" | tee -a "$LOG"
}
run b2-10-deploy35.sh "第 10 步：部署 v0.3.5"
run b2-11-fixes.sh    "第 11 步：空 SNI 入口修复 + 缺陷二复验"
run b2-12-rollback.sh "第 12 步：五种回滚结论（S3-S8）"
run b2-13-cleanup.sh  "第 13 步：清理临时措施 + 恢复改动前版本"
echo | tee -a "$LOG"
echo "==================== 关键结论 ====================" | tee -a "$LOG"
grep -E '^(S[3-8]=|CHECK_|FILESET_SAME|BYTE_IDENTICAL|VERSION_SAME|DEPLOY35_DONE|CLEANUP2_DONE)' "$LOG" | tee -a "$LOG"
echo | tee -a "$LOG"
echo "完整日志：$LOG" | tee -a "$LOG"
