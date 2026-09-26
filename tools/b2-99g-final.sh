#!/usr/bin/env bash
# B2 第五轮 - 一键串联：部署 v0.3.7 → 20 清单外文件的处理边界（S1-S4）
#
# v0.3.7 相对 v0.3.6 的改动（评审确认的四条边界）：
#   A) 版本目录里"清单之外的普通文件" **不再阻断**发布/回滚 ⇒ warning + 审计 + 页面提示；
#   B) 但"配置**实际引用**了清单外文件" ⇒ **必须拒绝**（那种文件运行时会被按绝对路径读到）；
#   C) 清单列出的文件缺失 ⇒ 仍**必须拒绝**（且不得进入 verified）；
#   D) current/ 仍然只包含清单允许的文件（多余文件绝不进入）。
#
# 用法：bash /tmp/b2-scripts/b2-99g-final.sh 2>&1 | tee /root/b2-accept5.log
set -u
S=/tmp/b2-scripts
LOG=/root/b2-accept5.log
: > "$LOG"
run() {
  echo | tee -a "$LOG"
  echo "############################################################" | tee -a "$LOG"
  echo "### $2   ($1)" | tee -a "$LOG"
  echo "############################################################" | tee -a "$LOG"
  bash "$S/$1" 2>&1 | tee -a "$LOG"
  echo "### <<< $1 结束 (exit=${PIPESTATUS[0]})" | tee -a "$LOG"
}
run b2-15-deploy37.sh    "第 15 步：部署 v0.3.7（只换管理面二进制）"
run b2-20-extra-files.sh "第 20 步：清单外文件的处理边界（S1-S4）"
echo | tee -a "$LOG"
echo "==================== 关键结论 ====================" | tee -a "$LOG"
grep -E '^(S1=|S1B=|S1C=|S2A=|S2B=|S3=|S4=|FINAL_FILESET=|CFG_RESTORED=|cfg_syntax_after_inject=|S3_BLOCKED_BY_SYNTAX|DEPLOY37_DONE|EXTRA_FILES_DONE)' "$LOG" | tee -a "$LOG"
echo | tee -a "$LOG"
echo "完整日志：$LOG" | tee -a "$LOG"
