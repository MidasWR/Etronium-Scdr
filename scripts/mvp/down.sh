#!/bin/bash
# down.sh — остановить MVP testbed.

set -euo pipefail
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

echo "=== Stopping MVP testbed ==="
docker compose -f test/mvp/docker-compose.yml down -v
echo "=== Done ==="
