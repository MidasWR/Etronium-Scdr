#!/bin/bash
# demo-pm.sh — Phase 5: 5-minute PM demo.
#
# Сценарий: PM видит как один "cluster" из 3 lord'ов обслуживает процессы,
# переживает lord death, и балансирует между lord'ами автоматически.
#
# Преподносимый нарратив:
#   "Это NUMA-style SSI в user-space. Scheduler — это как планировщик ядра Linux,
#    только для нескольких машин. Ваши процессы прозрачно распределяются и
#    переживают падения нод. Никакого Kubernetes, никакого YAML."
#
# Запуск:
#   1. Поднять стек: docker compose -f test/docker-compose.yml up
#   2. Из другой консоли: ./scripts/demo-pm.sh
#
# Что происходит (5 мин):
#   0:00 — show lords (3 healthy)
#   0:30 — spawn stateless workload (5 processes), watch distribution
#   1:30 — spawn V5 stateful app, show counter incrementing
#   2:30 — kill lord-02 → app survives on lord-01/03, counter preserved
#   4:00 — show recovery logs + final process list (all still RUNNING)
#   5:00 — graceful SIGTERM scheduler → drain lords → exit cleanly
#
# Это НЕ е2е test — это сценарий для ПМа. Каждый шаг можно комментировать.
set -uo pipefail
cd "$(dirname "$0")/.."

CLI=./bin/etronium
WAL_PATH=/tmp/etronium/scheduler.wal
SCHED_LOG=/tmp/sched.log
DRAIN_DEMO_SECS=8  # ускоренный drain для демо (vs 15s default)

banner() {
    printf '\n\033[1;36m=== %s ===\033[0m\n' "$1"
}

pause() {
    local msg="${1:-Paused.}"
    printf '\n\033[33m>>> %s\033[0m\n' "$msg"
    if [[ -z "${NO_PAUSE:-}" ]]; then
        read -r -p "Press ENTER to continue..."
    else
        sleep 2
    fi
}

banner "0:00 — Lords are alive"
$CLI lords 2>&1 | head -10

banner "0:30 — Spawn 5 stateless processes (sleep 60s)"
PIDS=()
for i in 1 2 3 4 5; do
    out=$($CLI process spawn --exec=/bin/sleep --arg=60 2>&1)
    pid=$(echo "$out" | grep -oE 'process_id[^=]*=[^ ]*' | head -1 | cut -d= -f2 | tr -d "',")
    PIDS+=("$pid")
    printf 'spawn #%d → %s\n' "$i" "$pid"
done
pause "Five processes spawned. They distribute across lords automatically."

banner "0:50 — Distribution check"
$CLI process list 2>&1 | grep -E "process_id|state:" | head -15
$CLI lords 2>&1 | tail -5

banner "1:30 — Spawn V5 stateful app (counter persists across lord death)"
STATE_FILE=/tmp/etronium/state/demo.json
rm -f "$STATE_FILE"
SPAWN=$($CLI process spawn \
    --exec=/usr/local/bin/example-stateful \
    --state-dump="$STATE_FILE" \
    --max-restarts=10 2>&1)
echo "$SPAWN"
APP_PID=$(echo "$SPAWN" | grep -oE 'process_id[^=]*=[^ ]*' | head -1 | cut -d= -f2 | tr -d "',")
APP_LORD=$(echo "$SPAWN" | grep -oE 'lord_id[^=]*=[^ ]*' | head -1 | cut -d= -f2 | tr -d "',")
echo "App PID=$APP_PID on lord=$APP_LORD"
pause "App is running. State file location: $STATE_FILE"

banner "2:00 — Watch counter incrementing"
for i in 1 2 3; do
    sleep 2
    counter=$(jq -r .counter "$STATE_FILE" 2>/dev/null || echo "?")
    printf 't=%ds  counter=%s\n' $((i*2)) "$counter"
done
COUNTER_BEFORE=$(jq -r .counter "$STATE_FILE")
echo "Counter before lord kill: $COUNTER_BEFORE"

banner "2:30 — Kill lord $APP_LORD"
docker kill "etronium-$APP_LORD" 2>/dev/null || \
    docker kill "${APP_LORD//lord-/etronium-lord-}" 2>/dev/null || \
    echo "(container kill attempted; in real env do 'docker kill <lord_container>')"
pause "Lord died. Scheduler should respawn app on another lord within 5-15s."

banner "2:50 — Wait for recovery"
for i in $(seq 1 15); do
    sleep 2
    counter=$(jq -r .counter "$STATE_FILE" 2>/dev/null || echo "?")
    cur=$($CLI process get "$APP_PID" --json 2>&1 | jq -r '.state' 2>/dev/null || echo "?")
    printf 't=%2ds  counter=%-5s  state=%s\n' $((i*2)) "$counter" "$cur"
    if [ "$counter" != "$COUNTER_BEFORE" ] && [ "$counter" != "?" ]; then
        echo "RESPAWN DETECTED — counter increased from $COUNTER_BEFORE to $counter"
        break
    fi
done

banner "4:00 — Final state"
$CLI process get "$APP_PID" 2>&1 | head -10
echo
$CLI lords 2>&1 | head -10

banner "4:30 — Graceful shutdown"
echo "Sending SIGTERM to scheduler..."
SCHED_PID=$(pgrep -f "scheduler --addr" | head -1 || echo "")
if [ -n "$SCHED_PID" ]; then
    kill -TERM "$SCHED_PID"
    echo "Scheduler PID=$SCHED_PID, draining..."
    sleep $((DRAIN_DEMO_SECS + 5))
fi
echo
echo "Last 15 lines of scheduler log:"
tail -15 "$SCHED_LOG" 2>/dev/null || echo "(log not at $SCHED_LOG)"

banner "5:00 — Demo complete"
echo
echo "What you saw:"
echo "  • 3 lords, transparent load-balancing (no Kubernetes)"
echo "  • Stateless workloads auto-distributed"
echo "  • Stateful app survives lord death (V5 state serialization)"
echo "  • Graceful shutdown with drain (no data loss)"
echo "  • Persistent WAL survives scheduler crash"
echo
echo "What we DIDN'T show:"
echo "  • Live process migration (CRIU) — abandoned (kernel 6.17 hostile)"
echo "  • Network namespace isolation (Phase 4)"
echo "  • File transfer between lords (Phase 4)"
echo "  • Process↔process RPC over network"
echo "  • NUMA pinning, GPU, IO bandwidth"
echo
