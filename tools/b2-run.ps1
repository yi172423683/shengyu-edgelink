# 真机验收上传执行器（B2 重跑版）
#
# 与 rbui-run.ps1 的区别：额外支持上传若干"数据文件"（例如发布二进制），
# 因为真实验收必须先把新二进制送上去才能验证修复。
#
# 用法：
#   powershell -File b2-run.ps1 -Script <local.sh> [-Args "a b"] [-Upload "本地=远端;本地=远端"]
param(
  [Parameter(Mandatory=$true)][string]$Script,
  [string]$Args = "",
  [string]$Upload = ""
)
$ErrorActionPreference = "Continue"
$k = "C:\Users\Administrator\.ssh\shengyu_deploy"
$common = @("-i",$k,"-o","StrictHostKeyChecking=no","-o","UserKnownHostsFile=/dev/null","-o","BatchMode=yes","-o","ConnectTimeout=25")
$log = "$env:TEMP\b2-out.txt"
"### host 154.9.235.57:23722" | Set-Content -Encoding UTF8 $log

if ($Upload -ne "") {
  foreach ($pair in $Upload.Split(";")) {
    if ($pair.Trim() -eq "") { continue }
    $parts = $pair.Split("=")
    $lp = $parts[0]; $rp = $parts[1]
    "### scp $lp -> $rp" | Add-Content -Encoding UTF8 $log
    $o = scp @common -P 23722 $lp "root@154.9.235.57:$rp" 2>&1
    $o | Out-String | Add-Content -Encoding UTF8 $log
    if ($LASTEXITCODE -ne 0) { "SCP_FAILED($lp)" | Add-Content -Encoding UTF8 $log; Get-Content $log; exit 1 }
  }
}

"### scp script" | Add-Content -Encoding UTF8 $log
$o2 = scp @common -P 23722 $Script "root@154.9.235.57:/tmp/b2-run.sh" 2>&1
$o2 | Out-String | Add-Content -Encoding UTF8 $log
if ($LASTEXITCODE -ne 0) { "SCP_SCRIPT_FAILED" | Add-Content -Encoding UTF8 $log; Get-Content $log; exit 1 }

$tpl = "sed -i 's/\r`$//' /tmp/b2-run.sh; chmod +x /tmp/b2-run.sh; /tmp/b2-run.sh __ARGS__"
$cmd = $tpl.Replace("__ARGS__", $Args)
"### ssh run" | Add-Content -Encoding UTF8 $log
$out = ssh @common -p 23722 root@154.9.235.57 $cmd 2>&1
$code = $LASTEXITCODE
$out | Out-String | Add-Content -Encoding UTF8 $log
"EXIT=$code" | Add-Content -Encoding UTF8 $log
Write-Output "EXIT=$code log=$log"
