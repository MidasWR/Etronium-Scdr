#!/bin/bash
# up.sh — поднять MVP testbed.
#
# Использование:
#   ./scripts/mvp/up.sh          # foreground (Ctrl-C для остановки)
#   ./scripts/mvp/up.sh -d       # detached (background)
#
# Сначала вызывает build-image.sh если image ещё не построен.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

# Build image если его нет
if ! docker image inspect etronium-mvp:runtime >/dev/null 2>&1; then
    echo "Image etronium-mvp:runtime not found, building..."
    ./scripts/mvp/build-image.sh
fi

# Проверка что порт свободен
if ss -ltn 2>/dev/null | grep -q ":51061 "; then
    echo "ERROR: port 51061 already in use. Run ./scripts/mvp/down.sh first" >&2
    exit 1
fi

echo "=== Starting MVP testbed ==="
docker compose -f test/mvp/docker-compose.yml up "$@"

echo ""
echo "=== MVP testbed running ==="
echo "Frontend gRPC: 127.0.0.1:51061"
echo "Tenant commands:"
echo "  ./bin/etronium process spawn --exec=/bin/sleep --arg=300"
echo "  ./bin/etronium process list"
echo "  ./bin/etronium lords"
echo ""
echo "Demo flow: ./scripts/mvp/demo.sh"
