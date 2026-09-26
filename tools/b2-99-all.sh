#!/usr/bin/env bash
# B2 重跑 - 一键串联（00 侦察备份 → 01 部署 → 02 缺陷二 → 03 缺陷一 → 04 中间态 → 05 清理恢复）
#
# 前置（必须在服务器上已有）：
#   /tmp/shengyu-edgelink-v0.3.4     本地 dist/shengyu-edgelink-linux-amd64 上传后的副本
#   对应脚本文件已放在 /tmp/b2-scripts/ 下（b2-0*.sh）
#
# 用法：
#   sudo bash /tmp/b2-scripts/b2-99-all.sh 2>&1 | tee /root/b2-accept.log
#
# 为什么要串起来：这五步之间有**顺序依赖**（备份必须在改动前、清理必须在最后），
# 手动一步步敲很容易在中途忘了某一步，而"忘了清理"会把临时 systemd override
# 留在生产机上 —— 那正是本轮最不能犯的错。
set -u
# ⚠️ 本脚本是第一轮的版本，**已被 b2-99d-third.sh 取代**，不要再用：
# 它的 b2-03/b2-04 两个场景的期望已按第一轮真机结论修正（数据面会自愈 → not_needed；
# 4B 的构造方式也已改正），继续用旧的会把结论带偏。这里直接拒绝执行。
echo "REFUSED: b2-99-all.sh 是第一轮的脚本，已过时。请改用："
echo "  bash /tmp/b2-scripts/b2-99d-third.sh 2>&1 | tee /root/b2-accept3.log"
exit 1

S=/tmp/b2-scripts
LOG=/root/b2-accept.log
: > "$LOG"

run() { # $1=script  $2=label
  echo | tee -a "$LOG"
  echo "############################################################" | tee -a "$LOG"
  echo "### $2   ($1)" | tee -a "$LOG"
  echo "############################################################" | tee -a "$LOG"
  bash "$S/$1" 2>&1 | tee -a "$LOG"
  echo "### <<< $1 结束 (exit=${PIPESTATUS[0]})" | tee -a "$LOG"
}

run b2-00-prep.sh      "第 0 步：侦察 + 全量备份"
run b2-01-deploy.sh    "第 1 步：部署 v0.3.4（只换管理面二进制）"
run b2-02-defect2.sh   "第 2 步：缺陷二（陈旧辅助文件清理）"
run b2-03-defect1.sh   "第 3 步：缺陷一 B2（reload 只失败一次）"
run b2-04-notverified.sh "第 4 步：中间态（已恢复但未验证 / 回滚验证失败）"
run b2-05-cleanup.sh   "第 5 步：清理临时措施 + 恢复原运行版本"

echo | tee -a "$LOG"
echo "==================== 关键结论汇总（从日志里 grep） ====================" | tee -a "$LOG"
grep -E 'B2_VERDICT|SCENARIO_4A|SCENARIO_4B|CHECK_9443|CHECK_FILESET|BYTE_IDENTICAL|FILESET_SAME|CHECK_VERSION_NOT_ADVANCED|DEPLOY_DONE|CLEANUP_DONE' "$LOG" | tee -a "$LOG"
echo | tee -a "$LOG"
echo "完整日志：$LOG" | tee -a "$LOG"
