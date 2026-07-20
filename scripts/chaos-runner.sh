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

echo "[chaos] creating queued lord containers (cold, не стартуют)..."
# Удаляем старые queued (если остались от предыдущего run'а).
for q in 4 5 6; do
    docker rm -f "etronium-lord-queued-$q" >/dev/null 2>&1 || true
done
docker compose -f "$COMPOSE" --profile queued create
# Queued lords созданы, но не запущены (sleep infinity). Chaos-runner S03 их поднимет.

echo "[chaos] waiting for scheduler to listen on :50051..."
for i in {1..30}; do
    if ss -tnlp 2>/dev/null | grep -q ":50051"; then
        echo "[chaos] scheduler listening after ${i}s"
        break
    fi
    sleep 1
done
sleep 3  # extra grace — gRPC server needs a moment after bind

echo "[chaos] waiting for lords to register..."
sleep 8
docker exec etronium-tenant /usr/local/bin/etronium lords 2>&1 | head

echo "[chaos] starting chaos-runner (host binary, docker socket mount)..."
# chaos-runner binary запускается на хосте — он Go-static, glibc не нужен.
# Docker socket уже доступен на хосте. tmp/etronium — общий volume.
export ETRONIUM_TENANT=etronium-tenant
./bin/chaos-runner 2>&1 | tee /tmp/chaos-stdout.log

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