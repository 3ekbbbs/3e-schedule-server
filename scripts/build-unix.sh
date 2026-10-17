#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p dist
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/schedule-server ./cmd/schedule-server
echo "构建完成：dist/schedule-server"
