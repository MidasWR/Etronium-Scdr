#!/bin/bash
# down.sh — остановить MVP testbed + очистить host volumes.

set -euo pipefail
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

echo "=== Stopping MVP testbed ==="
docker compose -f test/mvp/docker-compose.yml down -v
echo "=== Cleaning host volumes ==="
# /tmp/etronium — host bind mount, не удаляется docker compose down -v.
# Удаляем вручную для полного reset (WAL, state files).
# Требует sudo если /tmp/etronium root-owned (это типичный случай после
# того как scheduler контейнер первым записал туда root-owned файлы).
if [[ -d /tmp/etronium ]]; then
    sudo -n rm -rf /tmp/etronium 2>/dev/null || \
    rm -rf /tmp/etronium 2>/dev/null || \
    echo "warning: cannot remove /tmp/etronium — scheduler may show stale processes next run"
fi
echo "=== Done ==="