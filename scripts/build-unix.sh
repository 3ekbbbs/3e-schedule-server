#!/usr/bin/env bash
# 只编译，不打包。发布包用 node scripts/package.mjs。
# 产物写 build/：dist/ 是发给别人的 zip，两个目录都不放运行时数据。
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p build
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o build/schedule-server ./cmd/schedule-server
echo "构建完成：build/schedule-server"
