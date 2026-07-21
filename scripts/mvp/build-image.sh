#!/bin/bash
# build-image.sh — собрать MVP runtime image для docker-compose.
#
# Шаги:
#   1. Собрать Go binaries (scheduler, lord, etronium, example-stateful)
#   2. docker build с тегом etronium-mvp:runtime
#
# Использование: ./scripts/mvp/build-image.sh

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

echo "=== [1/2] Building Go binaries ==="
mkdir -p bin
go build -o bin/scheduler ./cmd/scheduler
go build -o bin/lord ./cmd/lord
go build -o bin/etronium ./cmd/etronium
go build -o bin/example-stateful ./cmd/example-stateful
echo "=== binaries OK ==="
ls -lh bin/

echo ""
echo "=== [2/2] Building docker image etronium-mvp:runtime ==="
docker build -f test/mvp/Dockerfile.runtime -t etronium-mvp:runtime .

echo ""
echo "=== Done ==="
echo "Run: ./scripts/mvp/up.sh"
