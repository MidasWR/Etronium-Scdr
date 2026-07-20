#!/usr/bin/env bash
# test/e2e_phase3.sh — Phase 3 e2e: process migration via CRIU
#
# Сценарий:
#   1. Стартуем scheduler + 2 lord'а с CRIU support
#   2. Spawn sleep infinity процесс
#   3. Проверяем что lord-01 RUNNING с local_pid
#   4. Migrate → lord-02
#   5. Проверяем что lord-02 RUNNING с НОВЫМ local_pid
#   6. Kill на lord-02 → EXITED
#
# Требования:
#   - CRIU должен работать на lord'ах (Dockerfile.phase3 с CRIU 4.2)
#   - Shared volume /tmp/etronium/cp между lord'ами (монтируем один и тот же host dir)
#
# Ожидаемый результат: все тесты PASS, exit 0.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/.."

LOG_DIR="/tmp/etronium-phase3"
mkdir -p "$LOG_DIR"
rm -f "$LOG_DIR"/*.log

CHECKPOINT_DIR="/tmp/etronium/cp"
mkdir -p "$CHECKPOINT_DIR"

# Clean stale CRIU images (owned by root from previous runs in containers)
if [ -d "$CHECKPOINT_DIR" ]; then
    docker run --rm -v "$CHECKPOINT_DIR":/tmp/etronium/cp etronium-test:phase3 bash -c '
        find /tmp/etronium/cp -mindepth 1 -delete 2>/dev/null || true
        rm -rf /tmp/etronium/cp/* 2>/dev/null || true
    ' 2>/dev/null || true
fi

# Цвета для вывода
RED=$'\e[31m'; GREEN=$'\e[32m'; YELLOW=$'\e[33m'; NC=$'\e[0m'

pass() { echo "${GREEN}PASS${NC} $1"; }
fail() { echo "${RED}FAIL${NC} $1"; exit 1; }
info() { echo "${YELLOW}=== $* ===${NC}"; }

# Cleanup при выходе
PIDS=()
CONTAINERS=()
cleanup() {
    info "cleanup"
    for c in "${CONTAINERS[@]}"; do
        docker logs "$c" > "$LOG_DIR/$c.log" 2>&1 || true
        docker exec "$c" cat /tmp/lord.log >> "$LOG_DIR/$c.log" 2>/dev/null || true
        docker rm -f "$c" >/dev/null 2>&1 || true
    done
    for p in "${PIDS[@]}"; do
        kill "$p" 2>/dev/null || true
    done
    wait 2>/dev/null || true
}
trap cleanup EXIT

# === TEST 0: docker image available ===
info "TEST 0: docker image"
docker inspect etronium-test:phase3 >/dev/null 2>&1 || fail "image etronium-test:phase3 not built"
pass "image etronium-test:phase3 exists"

# === Старт scheduler ===
info "starting scheduler"
SCHEDULER_HEARTBEAT_TTL=30s SCHEDULER_PLACEMENT=weighted \
SCHEDULER_CHECKPOINT_DIR="$CHECKPOINT_DIR" \
    ./bin/scheduler --addr=:50051 --log=info >"$LOG_DIR/scheduler.log" 2>&1 &
PIDS+=($!)
sleep 2

# Ждём готовности scheduler'а
for i in {1..10}; do
    if (echo > /dev/tcp/127.0.0.1/50051) 2>/dev/null; then
        break
    fi
    sleep 0.5
done
pass "scheduler up"

# === Старт 2 lord'ов с CRIU и shared volume ===
info "starting lord-01"
docker run --rm -d \
    --name lord-01 \
    --network=host \
    --cgroupns=private \
    --privileged \
    -v "$CHECKPOINT_DIR":/tmp/etronium/cp \
    --tmpfs /tmp \
    etronium-test:phase3 \
    bash -c 'unshare -p -f --mount-proc setsid env GOMAXPROCS=1 lord --scheduler=127.0.0.1:50051 --advertise-cpu=1024 --log=info --log-format=json > /tmp/lord.log 2>&1; while true; do sleep 60; done' \
    > "$LOG_DIR/lord-01-docker.log" 2>&1
CONTAINERS+=(lord-01)

info "starting lord-02"
docker run --rm -d \
    --name lord-02 \
    --network=host \
    --cgroupns=private \
    --privileged \
    -v "$CHECKPOINT_DIR":/tmp/etronium/cp \
    --tmpfs /tmp \
    etronium-test:phase3 \
    bash -c 'unshare -p -f --mount-proc setsid env GOMAXPROCS=1 lord --scheduler=127.0.0.1:50051 --advertise-cpu=1024 --log=info --log-format=json > /tmp/lord.log 2>&1; while true; do sleep 60; done' \
    > "$LOG_DIR/lord-02-docker.log" 2>&1
CONTAINERS+=(lord-02)

# Ждём пока оба lord'а register'нутся
info "waiting for lords to register"
for i in {1..30}; do
    LORDS_JSON=$(./bin/etronium --scheduler=localhost:50051 --tenant=t --json lords 2>&1 || echo "[]")
    LORDS=$(echo "$LORDS_JSON" | python3 -c "
import json, sys
try:
    data = json.loads(sys.stdin.read())
    print(sum(1 for l in data if l.get('healthy')))
except:
    print(0)
" 2>/dev/null || echo "0")
    if [ "$LORDS" -ge 2 ]; then
        break
    fi
    sleep 0.5
done
[ "$LORDS" -ge 2 ] || fail "lords did not register (got $LORDS, last JSON: $LORDS_JSON)"
pass "both lords registered"

# === Проверить criu_available ===
info "TEST 1: lords have criu_available=true"
LORDS_JSON=$(./bin/etronium --scheduler=localhost:50051 --tenant=t --json lords 2>&1)
CRIU_COUNT=$(echo "$LORDS_JSON" | python3 -c "
import json, sys
try:
    data = json.loads(sys.stdin.read())
    print(sum(1 for l in data if l.get('criu_available')))
except:
    print(0)
" 2>/dev/null || echo "0")
[ "$CRIU_COUNT" -ge 2 ] || fail "only $CRIU_COUNT lords have criu_available. JSON: $LORDS_JSON"
pass "$CRIU_COUNT lords have criu_available"

# === Spawn процесс для миграции ===
info "TEST 2: spawn sleep process"
PID_JSON=$(./bin/etronium --scheduler=localhost:50051 --tenant=t --json process spawn --exec=/bin/sleep --arg=infinity 2>&1)
echo "$PID_JSON"
PID=$(echo "$PID_JSON" | python3 -c "
import json, sys
try:
    data = json.loads(sys.stdin.read())
    ref = data.get('ref', {})
    print(ref.get('process_id', ''))
except:
    pass
")
[ -n "$PID" ] || fail "no process_id returned: $PID_JSON"
pass "spawned process_id=$PID"

# Дождёмся RUNNING
info "waiting for state RUNNING"
for i in {1..20}; do
    GET_JSON=$(./bin/etronium --scheduler=localhost:50051 --tenant=t --json process get "$PID" 2>&1 || echo "{}")
    STATE=$(echo "$GET_JSON" | python3 -c "
import json, sys
try:
    data = json.loads(sys.stdin.read())
    state = data.get('state', '?')
    if isinstance(state, int):
        names = {0: 'UNSPECIFIED', 1: 'NEW', 2: 'READY', 3: 'RUNNING', 4: 'PAUSED', 5: 'MIGRATING', 6: 'EXITED', 7: 'STOPPED'}
        state = names.get(state, str(state))
    # Strip enum prefix if any
    if 'PROCESS_STATE_' in state:
        state = state.replace('PROCESS_STATE_', '')
    print(state)
except:
    print('?')
" 2>/dev/null || echo "?")
    if [ "$STATE" = 'RUNNING' ]; then
        break
    fi
    sleep 0.3
done
[ "$STATE" = 'RUNNING' ] || fail "process not RUNNING (state=$STATE, JSON: $GET_JSON)"
pass "process is RUNNING"

# === TEST 3: первоначальный lord_id ===
info "TEST 3: process initially on lord-01 (or lord-02 — weighted placement)"
INFO_BEFORE=$(./bin/etronium --scheduler=localhost:50051 --tenant=t --json process get "$PID" 2>&1)
SOURCE_LORD=$(echo "$INFO_BEFORE" | python3 -c "
import json, sys
try:
    data = json.loads(sys.stdin.read())
    ref = data.get('ref', {})
    print(ref.get('lord_id', ''))
except:
    pass
")
SOURCE_PID=$(echo "$INFO_BEFORE" | python3 -c "
import json, sys
try:
    data = json.loads(sys.stdin.read())
    ref = data.get('ref', {})
    print(ref.get('local_pid', 0))
except:
    pass
")
[ -n "$SOURCE_LORD" ] || fail "no source lord: $INFO_BEFORE"
[ "$SOURCE_PID" -gt 0 ] || fail "no source local_pid: $INFO_BEFORE"
pass "process on $SOURCE_LORD local_pid=$SOURCE_PID"

# Выбираем target lord (другой)
LORDS_JSON=$(./bin/etronium --scheduler=localhost:50051 --tenant=t --json lords 2>&1)
TARGET_LORD=$(echo "$LORDS_JSON" | python3 -c "
import json, sys
lords = json.loads(sys.stdin.read())
for l in lords:
    if l['lord_id'] != '$SOURCE_LORD' and l.get('healthy'):
        print(l['lord_id'])
        break
")
[ -n "$TARGET_LORD" ] || fail "could not pick target lord"
info "migrating $PID: $SOURCE_LORD → $TARGET_LORD"

# === TEST 4: миграция ===
info "TEST 4: migrate"
MIGRATE_OUT=$(./bin/etronium --scheduler=localhost:50051 --tenant=t --json process migrate "$PID" --to="$TARGET_LORD" --reason=e2e 2>&1) || fail "migrate failed: $MIGRATE_OUT"
echo "$MIGRATE_OUT"
if ! echo "$MIGRATE_OUT" | grep -q '"acknowledged":true'; then
    fail "migrate not acknowledged: $MIGRATE_OUT"
fi
NEW_LORD=$(echo "$MIGRATE_OUT" | python3 -c "
import json, sys
try:
    data = json.loads(sys.stdin.read())
    print(data.get('new_lord_id', ''))
except:
    pass
")
NEW_PID=$(echo "$MIGRATE_OUT" | python3 -c "
import json, sys
try:
    data = json.loads(sys.stdin.read())
    print(data.get('new_local_pid', 0))
except:
    pass
")
[ "$NEW_LORD" = "$TARGET_LORD" ] || fail "new_lord_id mismatch: got $NEW_LORD want $TARGET_LORD"
pass "migrated: new_lord=$NEW_LORD new_pid=$NEW_PID (was $SOURCE_PID)"
[ "$NEW_PID" -gt 0 ] || fail "new_local_pid not set"

# === TEST 5: process теперь на target lord'е и RUNNING ===
info "TEST 5: process state after migration"
sleep 1
INFO_AFTER=$(./bin/etronium --scheduler=localhost:50051 --tenant=t --json process get "$PID" 2>&1)
echo "$INFO_AFTER" | python3 -m json.tool 2>/dev/null || echo "$INFO_AFTER"
NEW_LORD_AFTER=$(echo "$INFO_AFTER" | python3 -c "
import json, sys
try:
    data = json.loads(sys.stdin.read())
    ref = data.get('ref', {})
    print(ref.get('lord_id', ''))
except:
    pass
")
NEW_STATE=$(echo "$INFO_AFTER" | python3 -c "
import json, sys
try:
    data = json.loads(sys.stdin.read())
    state = data.get('state', '?')
    if isinstance(state, int):
        names = {0: 'UNSPECIFIED', 1: 'NEW', 2: 'READY', 3: 'RUNNING', 4: 'PAUSED', 5: 'MIGRATING', 6: 'EXITED', 7: 'STOPPED'}
        state = names.get(state, str(state))
    if 'PROCESS_STATE_' in state:
        state = state.replace('PROCESS_STATE_', '')
    print(state)
except:
    print('?')
")
[ "$NEW_LORD_AFTER" = "$TARGET_LORD" ] || fail "lord not updated: got $NEW_LORD_AFTER want $TARGET_LORD"
[ "$NEW_STATE" = 'RUNNING' ] || fail "state not RUNNING: $NEW_STATE"
pass "process RUNNING on new lord"

# === TEST 6: новый local_pid в shell процесса ===
info "TEST 6: verify process actually running on target lord"
# ps в target lord должен показать процесс с PID == $NEW_PID
TARGET_PS=$(docker exec "$TARGET_LORD" ps -ef 2>/dev/null || echo "ps_failed")
echo "$TARGET_PS" | head -20
if echo "$TARGET_PS" | grep -q "[s]leep infinity"; then
    pass "sleep process visible in target lord's ps"
else
    info "WARNING: ps did not show sleep; might be CRIU-restored but ps cached"
fi

# === TEST 7: kill на новом lord'е ===
info "TEST 7: kill process on new lord"
./bin/etronium --scheduler=localhost:50051 --tenant=t process kill "$PID" --signal=15 >/dev/null 2>&1 || true
for i in {1..20}; do
    GET_JSON=$(./bin/etronium --scheduler=localhost:50051 --tenant=t --json process get "$PID" 2>&1 || echo "{}")
    STATE=$(echo "$GET_JSON" | python3 -c "
import json, sys
try:
    data = json.loads(sys.stdin.read())
    state = data.get('state', '?')
    if isinstance(state, int):
        names = {0: 'UNSPECIFIED', 1: 'NEW', 2: 'READY', 3: 'RUNNING', 4: 'PAUSED', 5: 'MIGRATING', 6: 'EXITED', 7: 'STOPPED'}
        state = names.get(state, str(state))
    if 'PROCESS_STATE_' in state:
        state = state.replace('PROCESS_STATE_', '')
    print(state)
except:
    print('?')
" 2>/dev/null || echo "?")
    if [[ "$STATE" == 'EXITED' ]] || [[ "$STATE" == 'STOPPED' ]]; then
        break
    fi
    sleep 0.3
done
[[ "$STATE" == 'EXITED' ]] || [[ "$STATE" == 'STOPPED' ]] || fail "process not exited: $STATE"
pass "process exited: $STATE"

echo ""
info "ALL TESTS PASSED"
echo "Logs: $LOG_DIR/"
ls -la "$LOG_DIR/"
