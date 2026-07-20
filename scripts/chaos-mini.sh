#!/bin/bash
# chaos-mini.sh — быстрая проверка (3 сценария, ~3-4 мин).
# Без k3s, без долгих wait'ов.
#
# Прогоняет:
#   S01 Baseline (5 stateless)
#   S02 V5 stateful + SIGKILL lord
#   S05 Spawn storm (50 процессов)
set -uo pipefail
cd "$(dirname "$0")/.."

COMPOSE=test/chaos/docker-compose.mini.yml
IMG=etronium-test:chaos
TENANT=etronium-mini-tenant
PREFIX="etronium-mini-"

log() { printf "\033[1;36m[%s]\033[0m %s\n" "$(date +%H:%M:%S)" "$*"; }
ok()  { printf "  \033[32m✓\033[0m %s\n" "$*"; }
fail(){ printf "  \033[31m✗\033[0m %s\n" "$*"; }

trap cleanup EXIT
cleanup() {
    log "tear down..."
    docker compose -f "$COMPOSE" down --remove-orphans -v >/dev/null 2>&1 || true
    # kill any leftover mini containers
    for c in $(docker ps -a --format '{{.Names}}' | grep "$PREFIX"); do
        docker rm -f "$c" >/dev/null 2>&1 || true
    done
}

log "1. проверка RAM"
free -h | head -2 | tail -1

log "2. build image (cached, быстро)"
docker build -q -t "$IMG" -f test/Dockerfile.runtime . >/dev/null

log "3. teardown old run"
cleanup

log "4. up: scheduler + 3 lords + tenant"
docker compose -f "$COMPOSE" up -d

log "5. wait scheduler"
for i in {1..30}; do
    out=$(docker exec "$TENANT" ./bin/etronium lords 2>&1 || true)
    if echo "$out" | grep -q "etronium-mini-lord"; then
        ok "scheduler up, lords registered"
        break
    fi
    sleep 1
done

# show cluster state
log "6. cluster state"
docker exec "$TENANT" ./bin/etronium lords

PASSED=0
TOTAL=0

# ===== S01: Baseline =====
TOTAL=$((TOTAL+1))
log ""
log "=== S01: Baseline — 5 stateless процессов ==="
PIDS=()
for i in 1 2 3 4 5; do
    out=$(docker exec "$TENANT" ./bin/etronium process spawn --exec=/bin/sleep --arg=300 2>&1)
    pid=$(echo "$out" | grep -oE 'process_id[^=]*=[^ ]*' | head -1 | cut -d= -f2 | tr -d "',")
    PIDS+=("$pid")
done
sleep 2
LIST=$(docker exec "$TENANT" ./bin/etronium process list 2>&1)
echo "$LIST" | head -10
DISTINCT=$(echo "$LIST" | grep -oE 'lord-[0-9]+' | sort -u | wc -l)
if [ "$DISTINCT" -ge 2 ]; then
    ok "5 процессов размазаны по $DISTINCT lord'ам"
    PASSED=$((PASSED+1))
else
    fail "только $DISTINCT lord (ожидаем >=2)"
fi
# cleanup
for pid in "${PIDS[@]}"; do
    docker exec "$TENANT" ./bin/etronium process kill "$pid" >/dev/null 2>&1
done

# ===== S02: V5 stateful + kill lord =====
TOTAL=$((TOTAL+1))
log ""
log "=== S02: V5 stateful — SIGKILL lord mid-write ==="
STATE=/tmp/etronium/state/mini-test.json
rm -f "$STATE"

SPAWN=$(docker exec "$TENANT" ./bin/etronium process spawn \
    --exec=/usr/local/bin/example-stateful \
    --state-dump="$STATE" \
    --max-restarts=10 2>&1)
APP_PID=$(echo "$SPAWN" | grep -oE 'process_id[^=]*=[^ ]*' | head -1 | cut -d= -f2 | tr -d "',")
APP_LORD=$(echo "$SPAWN" | grep -oE 'lord_id[^=]*=[^ ]*' | head -1 | cut -d= -f2 | tr -d "',")
log "  spawned: pid=$APP_PID lord=$APP_LORD"
sleep 5

BEFORE=$(jq -r '.counter // 0' "$STATE" 2>/dev/null || echo 0)
log "  counter before kill: $BEFORE"

# Найти lord container
LORD_CONT="etronium-mini-lord-1"
log "  SIGKILL lord container: $LORD_CONT"
docker kill "$LORD_CONT" 2>&1 | head
T0=$(date +%s)
# wait for respawn (counter must increase)
RECOVERED=0
for i in $(seq 1 60); do
    AFTER=$(jq -r '.counter // 0' "$STATE" 2>/dev/null || echo 0)
    if [ "${AFTER%.*}" -gt "${BEFORE%.*}" ]; then
        T1=$(date +%s)
        RECOVERED=$((T1-T0))
        break
    fi
    sleep 1
done

if [ "$RECOVERED" -gt 0 ]; then
    ok "counter $BEFORE → $AFTER через ${RECOVERED}s после kill"
    PASSED=$((PASSED+1))
else
    fail "recovery timeout (>60s)"
fi
# cleanup
docker exec "$TENANT" ./bin/etronium process kill "$APP_PID" >/dev/null 2>&1

# ===== S05: Spawn storm =====
TOTAL=$((TOTAL+1))
log ""
log "=== S05: Spawn storm — 50 процессов ==="
N=50
T0=$(date +%s%N)
PIDS=()
for i in $(seq 1 $N); do
    out=$(docker exec "$TENANT" ./bin/etronium process spawn --exec=/bin/sleep --arg=60 2>&1)
    pid=$(echo "$out" | grep -oE 'process_id[^=]*=[^ ]*' | head -1 | cut -d= -f2 | tr -d "',")
    PIDS+=("$pid")
done
T1=$(date +%s%N)
ELAPSED_MS=$(( (T1 - T0) / 1000000 ))
log "  spawned $N in ${ELAPSED_MS}ms ($(( N * 1000 / (ELAPSED_MS+1) ))/s)"
sleep 2
LIST=$(docker exec "$TENANT" ./bin/etronium process list 2>&1)
ALIVE=$(echo "$LIST" | grep -c "RUNNING" || echo 0)
log "  $ALIVE из $N RUNNING"
if [ "$ALIVE" -ge $((N - 2)) ]; then
    ok "spawn storm ok: $ALIVE/$N RUNNING, throughput=$(( N * 1000 / (ELAPSED_MS+1) ))/s"
    PASSED=$((PASSED+1))
else
    fail "только $ALIVE/$N RUNNING"
fi
# cleanup
for pid in "${PIDS[@]}"; do
    docker exec "$TENANT" ./bin/etronium process kill "$pid" >/dev/null 2>&1
done

# ===== summary =====
log ""
log "================================"
log "  MINI CHAOS: $PASSED / $TOTAL scenarios passed"
log "================================"

if [ "$PASSED" -eq "$TOTAL" ]; then
    log "ALL GREEN — можем запускать полный chaos"
    exit 0
else
    log "FAIL — нужно чинить перед full run"
    exit 1
fi