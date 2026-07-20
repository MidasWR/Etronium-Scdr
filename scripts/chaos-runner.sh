#!/bin/bash
# chaos runner driver — поднять compose + запустить orchestrator + напечатать отчёт.
set -euo pipefail
cd "$(dirname "$0")/.."

COMPOSE=test/chaos/docker-compose.yml
IMG=etronium-test:chaos

echo "[chaos] building image..."
docker build -q -t "$IMG" -f test/Dockerfile.runtime . >/dev/null

echo "[chaos] tearing down any prior run..."
docker compose -f "$COMPOSE" down --remove-orphans -v >/dev/null 2>&1 || true

echo "[chaos] up: scheduler + 3 active lords + tenant + k3s..."
docker compose -f "$COMPOSE" up -d scheduler lord-active-1 lord-active-2 lord-active-3 tenant k3s

echo "[chaos] waiting for scheduler healthcheck..."
for i in {1..30}; do
    if curl -fsS http://127.0.0.1:50051 >/dev/null 2>&1; then
        echo "[chaos] scheduler up after ${i}s"
        break
    fi
    sleep 1
done

echo "[chaos] waiting for lords to register..."
sleep 8
docker exec etronium-tenant ./bin/etronium lords 2>&1 | head

echo "[chaos] starting chaos-runner container..."
# Запускаем chaos-runner как foreground-контейнер с подключённым docker socket.
docker run --rm \
    --name etronium-chaos \
    --network=host \
    --privileged \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "$(pwd)/bin/chaos-runner:/usr/local/bin/chaos-runner:ro" \
    -v /tmp/etronium:/tmp/etronium \
    "$IMG" \
    chaos-runner 2>&1 | tee /tmp/chaos-stdout.log

EXIT=$?

echo "[chaos] runner exit=$EXIT"
echo "[chaos] report at /tmp/chaos-report.json"
if [ -f /tmp/chaos-report.json ]; then
    jq '{passed: [.scenarios[] | select(.success)] | length, total: (.scenarios | length), duration_sec: ((.finished_at | fromdateiso8601) - (.started_at | fromdateiso8601))}' \
        /tmp/chaos-report.json
fi

echo "[chaos] tearing down..."
docker compose -f "$COMPOSE" down --remove-orphans -v >/dev/null 2>&1 || true

exit $EXIT