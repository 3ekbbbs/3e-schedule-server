# 构建 Windows 发行版（无控制台窗口）
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

New-Item -ItemType Directory -Force -Path dist | Out-Null
$env:CGO_ENABLED = "0"

go build -trimpath -ldflags "-s -w -H windowsgui" -o dist/schedule-server.exe ./cmd/schedule-server
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host "构建完成：dist/schedule-server.exe"
