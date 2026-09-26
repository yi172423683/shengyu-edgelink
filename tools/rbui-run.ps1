# Helper: upload a local .sh to the test VPS, normalize CRLF, run it, capture stdout.
# Usage: powershell -File rbui-run.ps1 -Script <local.sh> [-Args "arg string"]
param(
  [Parameter(Mandatory=$true)][string]$Script,
  [string]$Args = ""
)
$ErrorActionPreference = "Continue"
$k = "C:\Users\Administrator\.ssh\shengyu_deploy"
$common = @("-i",$k,"-o","StrictHostKeyChecking=no","-o","UserKnownHostsFile=/dev/null","-o","BatchMode=yes","-o","ConnectTimeout=25")
$remote = "/tmp/rbui-run.sh"
$up = scp @common -P 23722 $Script "root@154.9.235.57:$remote" 2>&1
if ($LASTEXITCODE -ne 0) { Write-Output "SCP_FAILED"; $up | Out-String | Set-Content -Encoding UTF8 "$env:TEMP\rbui-out.txt"; exit 1 }
$tpl = @'
sed -i 's/\r$//' /tmp/rbui-run.sh; chmod +x /tmp/rbui-run.sh; /tmp/rbui-run.sh __ARGS__
'@
$cmd = $tpl.Replace("__ARGS__", $Args)
$out = ssh @common -p 23722 root@154.9.235.57 $cmd 2>&1
$code = $script:LASTEXITCODE
$out | Out-String | Set-Content -Encoding UTF8 "$env:TEMP\rbui-out.txt"
Write-Output "EXIT=$code"
