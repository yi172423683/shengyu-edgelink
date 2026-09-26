param(
    [string]$Version = "v0.4.2"
)

$ErrorActionPreference = "Stop"
$Root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$Dist = Join-Path $Root "dist"
$Stage = Join-Path $Dist (".stage-" + $Version)

if (-not $Version.StartsWith("v")) { $Version = "v$Version" }

function Find-Go {
    $cmd = Get-Command go.exe -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    $candidates = @(
        (Join-Path $env:USERPROFILE ".workbuddy\binaries\go\official\go\bin\go.exe"),
        (Join-Path $env:USERPROFILE ".workbuddy\binaries\go-toolchain\go\bin\go.exe")
    )
    foreach ($path in $candidates) {
        if (Test-Path -LiteralPath $path) { return $path }
    }
    throw "找不到 go.exe。请安装 Go 1.22+，或把 Go 的 bin 目录加入 PATH。"
}

function Invoke-Go {
    param([string[]]$CommandArgs)
    & $script:Go @CommandArgs
    if ($LASTEXITCODE -ne 0) { throw "Go 命令失败（退出码 $LASTEXITCODE）：go $($CommandArgs -join ' ')" }
}

function Write-Utf8NoBom {
    param([string]$Path, [string]$Content, [bool]$Append = $false)
    $encoding = New-Object System.Text.UTF8Encoding($false)
    if ($Append -and (Test-Path -LiteralPath $Path)) {
        [System.IO.File]::AppendAllText($Path, $Content, $encoding)
    } else {
        [System.IO.File]::WriteAllText($Path, $Content, $encoding)
    }
}

function Build-Arch {
    param([string]$Arch)
    $out = Join-Path $Stage "shengyu-edgelink-linux-$Arch"
    $env:GOOS = "linux"
    $env:GOARCH = $Arch
    $env:CGO_ENABLED = "0"
    Invoke-Go @("build", "-trimpath", "-ldflags", "-s -w -X main.Version=$Version", "-o", $out, "./cmd/shengyu-edgelink")
    Copy-Item -LiteralPath $out -Destination (Join-Path $Dist "shengyu-edgelink-linux-$Arch") -Force
    Write-Host "已构建 $out"
}

try {
    $script:Go = Find-Go
    Set-Location $Root
    Write-Host "Go: $(& $script:Go version)"
    Write-Host "版本: $Version"

    Invoke-Go @("vet", "./...")
    Invoke-Go @("test", "./...")

    New-Item -ItemType Directory -Force -Path $Dist | Out-Null
    if (Test-Path -LiteralPath $Stage) { Remove-Item -LiteralPath $Stage -Recurse -Force }
    New-Item -ItemType Directory -Force -Path $Stage | Out-Null

    Build-Arch "amd64"
    Build-Arch "arm64"

    foreach ($arch in @("amd64", "arm64")) {
        $name = "shengyu-edgelink-linux-$arch-$Version"
        $dir = Join-Path $Stage $name
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        Copy-Item -LiteralPath (Join-Path $Stage "shengyu-edgelink-linux-$arch") -Destination (Join-Path $dir "shengyu-edgelink-linux-$arch")
        Copy-Item -LiteralPath (Join-Path $Root "deploy") -Destination $dir -Recurse
        Copy-Item -LiteralPath (Join-Path $Root "docs") -Destination $dir -Recurse
        Copy-Item -LiteralPath (Join-Path $Root "README.md") -Destination $dir
        Copy-Item -LiteralPath (Join-Path $Root "install.sh") -Destination $dir
        @"
shengyu-edgelink $Version
build_arch=$arch
build_go=$(& $script:Go env GOVERSION)
build_time=$([DateTime]::UtcNow.ToString("yyyy-MM-ddTHH:mm:ssZ"))
source_module=github.com/shengyu/edgelink

安装：
  sudo bash install.sh --dry-run
  sudo bash install.sh
"@ | ForEach-Object { Write-Utf8NoBom (Join-Path $dir "BUILD.txt") $_ }

        $tar = Join-Path $Dist "$name.tar.gz"
        tar -czf $tar -C $Stage $name
        if ($LASTEXITCODE -ne 0) { throw "打包失败：$tar" }
        Write-Host "已打包 $tar"
    }

    $sumFile = Join-Path $Dist "SHA256SUMS-$Version"
    Remove-Item -LiteralPath $sumFile -Force -ErrorAction SilentlyContinue
    $files = @(
        "shengyu-edgelink-linux-amd64",
        "shengyu-edgelink-linux-arm64",
        "shengyu-edgelink-linux-amd64-$Version.tar.gz",
        "shengyu-edgelink-linux-arm64-$Version.tar.gz"
    )
    foreach ($file in $files) {
        $hash = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $Dist $file)).Hash.ToLowerInvariant()
        Write-Utf8NoBom $sumFile ("$hash  $file" + [Environment]::NewLine) $true
    }
    Copy-Item -LiteralPath $sumFile -Destination (Join-Path $Dist "SHA256SUMS") -Force
    Remove-Item -LiteralPath $Stage -Recurse -Force
    Write-Host "发布完成，文件在 $Dist"
    Get-Content -LiteralPath $sumFile
}
finally {
    Remove-Item Env:GOOS -ErrorAction SilentlyContinue
    Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
    Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue
}
