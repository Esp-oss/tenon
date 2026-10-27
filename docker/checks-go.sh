#!/bin/sh
# Go 工具链检查：gofmt 无差异 → go vet → go build。
# 失败即非零退出，便于 `docker compose run --rm go` 与 CI 直接使用。
set -eu

echo "== gofmt =="
out=$(gofmt -l . || true)
if [ -n "$out" ]; then
  echo "以下文件需要 gofmt："
  echo "$out"
  exit 1
fi
echo "ok"

echo "== go vet =="
go vet ./...
echo "ok"

echo "== go build =="
go build ./...
echo "ok"

echo "Go 工具链检查全部通过"
