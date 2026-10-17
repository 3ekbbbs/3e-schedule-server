# 只编译，不打包。发布包用 node scripts/package.mjs。
# 产物写 build/：dist/ 是发给别人的 zip，两个目录都不放运行时数据。
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

New-Item -ItemType Directory -Force -Path build | Out-Null
$env:CGO_ENABLED = "0"

go build -trimpath -ldflags "-s -w -H windowsgui" -o build/schedule-server.exe ./cmd/schedule-server
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host "构建完成：build/schedule-server.exe"
