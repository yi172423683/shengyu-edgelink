#!/usr/bin/env bash
# B2 第三轮 - 一键串联：19 诊断+修复 → 11 空入口+缺陷二 → 12 五种回滚结论 → 13 清理恢复
#
# 用法：bash /tmp/b2-scripts/b2-99d-third.sh 2>&1 | tee /root/b2-accept3.log
set -u
S=/tmp/b2-scripts
LOG=/root/b2-accept3.log
: > "$LOG"
run() {
  echo | tee -a "$LOG"
  echo "############################################################" | tee -a "$LOG"
  echo "### $2   ($1)" | tee -a "$LOG"
  echo "############################################################" | tee -a "$LOG"
  bash "$S/$1" 2>&1 | tee -a "$LOG"
  echo "### <<< $1 结束 (exit=${PIPESTATUS[0]})" | tee -a "$LOG"
}
run b2-19-repair.sh   "第 19 步：诊断 + 修复悬空引用 + 确认发布链路恢复"
run b2-11-fixes.sh    "第 11 步：空 SNI 入口修复 + 缺陷二复验"
run b2-12-rollback.sh "第 12 步：五种回滚结论（S3-S8）"
run b2-13-cleanup.sh  "第 13 步：清理临时措施 + 恢复改动前版本"
echo | tee -a "$LOG"
echo "==================== 关键结论 ====================" | tee -a "$LOG"
grep -E '^(S[3-8]=|CHECK_|FILESET_SAME|BYTE_IDENTICAL|VERSION_SAME|PREVIEW_OK|PUBLISH_UNBLOCKED|REPAIR_DONE|CLEANUP2_DONE|STEP_S1S2_ABORTED)' "$LOG" | tee -a "$LOG"
echo | tee -a "$LOG"
echo "完整日志：$LOG" | tee -a "$LOG"
