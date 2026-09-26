#!/usr/bin/env bash
# B2 第四轮 - 一键串联：部署 v0.3.6 → 11 空入口+缺陷二 → 12 五种回滚结论 → 13 清理恢复
#
# v0.3.6 相对 v0.3.5 的一处产品修复 + 三处验收脚本修正：
#   产品：ListenerStatus 补 json 标签（原先界面把嵌套的 expected 当扁平读，
#         发布成功气泡显示"监听：undefined:undefined"）。
#   脚本：① S6/S7 把 state.json 移出**版本目录**（在目录里改名会被清单一致性检查先挡住）；
#         ② 基线文件清单改从 sha256 行推导（原先那行被 tr 的转义写坏了）；
#         ③ 监听取值改读嵌套的 expected（原先全读到 None，使一条断言变成空断言）。
#
# 用法：bash /tmp/b2-scripts/b2-99e-final.sh 2>&1 | tee /root/b2-accept4.log
set -u
S=/tmp/b2-scripts
LOG=/root/b2-accept4.log
: > "$LOG"
run() {
  echo | tee -a "$LOG"
  echo "############################################################" | tee -a "$LOG"
  echo "### $2   ($1)" | tee -a "$LOG"
  echo "############################################################" | tee -a "$LOG"
  bash "$S/$1" 2>&1 | tee -a "$LOG"
  echo "### <<< $1 结束 (exit=${PIPESTATUS[0]})" | tee -a "$LOG"
}
run b2-14-deploy36.sh  "第 14 步：部署 v0.3.6（只换管理面二进制）"
run b2-11-fixes.sh     "第 11 步：空 SNI 入口修复 + 缺陷二复验"
run b2-12-rollback.sh  "第 12 步：五种回滚结论（S3-S8）"
run b2-13-cleanup.sh   "第 13 步：清理临时措施 + 恢复改动前版本"
echo | tee -a "$LOG"
echo "==================== 关键结论 ====================" | tee -a "$LOG"
grep -E '^(S[3-8]=|CHECK_|FILESET_SAME|BYTE_IDENTICAL|VERSION_SAME|DEPLOY36_DONE|CLEANUP2_DONE|STEP_S1S2_ABORTED)' "$LOG" | tee -a "$LOG"
echo | tee -a "$LOG"
echo "完整日志：$LOG" | tee -a "$LOG"
