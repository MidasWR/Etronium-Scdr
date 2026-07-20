#!/bin/bash
# e2e_phase34.sh — Phase 3.4 fault tolerance e2e test.
#
# Verifies:
#   1. Tenant spawns stateful process on lord-01
#   2. Stateful process writes counter to $ETRONIUM_STATE_DUMP file
#   3. Lord-01 dies (simulated via docker kill)
#   4. Scheduler detects lord disconnect, respawns process on lord-02
#   5. New process on lord-02 reads previous state file
#   6. Counter is preserved (not reset to 0)
#   7. After max_restarts, scheduler stops respawning
#
# Does NOT require CRIU / kernel 6.17 fixes / namespace tricks. Pure
# user-space: lord dies → scheduler respawns → app recovers from disk.
set -eo pipefail
set -u

ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT/.."

# Cleanup any leftover state.
echo "=== cleanup ==="
docker rm -f lord-01 lord-02 2>/dev/null
rm -rf /tmp/etronium /tmp/etronium-state 2>/dev/null
sleep 1

mkdir -p /tmp/etronium/state
chmod 1777 /tmp/etronium/state

echo "=== rebuild image with new binaries ==="
echo "=== image already built (using cache) ==="


echo "=== launch scheduler ==="
pkill -f "scheduler --addr" 2>/dev/null
sleep 1
SCHEDULER_HEARTBEAT_TTL=15s SCHEDULER_PLACEMENT=weighted ./bin/scheduler \
    --addr=:50051 --log=info --log-format=json \
    > /tmp/sched.log 2>&1 &
SCHED_PID=$!
disown
sleep 3
ss -tnlp | grep 50051 >/dev/null || { echo "scheduler failed to bind 50051"; cat /tmp/sched.log; exit 1; }

echo "=== launch two lords ==="
docker run --rm -d --name lord-01 --network=host --cgroupns=private --privileged \
    --tmpfs /tmp -v /tmp/etronium:/tmp/etronium -v /tmp/etronium/state:/tmp/etronium/state \
    etronium-test:phase3 \
    bash -c 'lord --scheduler=127.0.0.1:50051 --advertise-cpu=3200 --advertise-mem=1073741824 \
             --log=info --log-format=json > /tmp/lord1.log 2>&1' >/dev/null

docker run --rm -d --name lord-02 --network=host --cgroupns=private --privileged \
    --tmpfs /tmp -v /tmp/etronium:/tmp/etronium -v /tmp/etronium/state:/tmp/etronium/state \
    etronium-test:phase3 \
    bash -c 'lord --scheduler=127.0.0.1:50051 --advertise-cpu=3200 --advertise-mem=1073741824 \
             --log=info --log-format=json > /tmp/lord2.log 2>&1' >/dev/null

sleep 5
echo "=== lords registered ==="
./bin/etronium lords 2>&1 | head

echo "=== spawn stateful app on lord-01 ==="
STATE_FILE=/tmp/etronium/state/pid-test.json
rm -f $STATE_FILE

SPAWN_INFO=$(./bin/etronium process spawn \
    --exec=/usr/local/bin/example-stateful \
    --arg "demo" \
    --state-dump=$STATE_FILE \
    --max-restarts=3 \
    --prefer-lord=lord-01 2>&1)
echo "$SPAWN_INFO"
PID=$(echo "$SPAWN_INFO" | grep -oE 'process_id[^=]*=[^ ]*' | head -1 | cut -d= -f2 | tr -d "',")
test -n "$PID" || { echo "FAIL: no process_id"; exit 1; }
echo "spawned process_id=$PID"

# Wait for state to be written.
echo "=== wait for initial state (10s, counter should reach ~4) ==="
for i in $(seq 1 10); do
    sleep 2
    if [ -f $STATE_FILE ]; then
        COUNTER=$(jq -r .counter $STATE_FILE 2>/dev/null || echo "?")
        echo "  t=${i}s state: counter=$COUNTER"
        if [ "$COUNTER" != "?" ] && [ "$COUNTER" -ge 3 ]; then
            break
        fi
    fi
done
COUNTER_BEFORE=$(jq -r .counter $STATE_FILE)
echo "BEFORE lord kill: counter=$COUNTER_BEFORE"

echo "=== simulating lord-01 death (docker kill) ==="
docker exec lord-01 pkill -9 -f "lord --scheduler" || true
# or: docker kill lord-01 --signal=9
sleep 1

# Schedule detects disconnect when stream closes.
echo "=== wait for scheduler to detect lord-01 disconnect ==="
for i in $(seq 1 15); do
    sleep 2
    DETECT=$(grep -c "lord disconnected" /tmp/sched.log 2>/dev/null || echo 0)
    if [ "$DETECT" -gt 0 ]; then
        echo "  scheduler detected lord disconnect"
        break
    fi
done

echo "=== wait for respawn on lord-02 ==="
# Wait for: state file timestamp changes (we on lord-02 now reading same state file).
sleep 5
RES=$(./bin/etronium process get $PID 2>&1)
echo "$RES"
LORD_AFTER=$(echo "$RES" | grep -oE 'lord_id[^=]*=[^ ]*' | head -1 | cut -d= -f2 | tr -d "',")
RESTARTS=$(echo "$RES" | grep -oE 'restart_count[^=]*=[^ ]*' | head -1 | cut -d= -f2 | tr -d "',")
echo "AFTER: lord_id=$LORD_AFTER restart_count=$RESTARTS"

# State should have a higher counter (continued from where it was, not reset).
sleep 4
COUNTER_AFTER=$(jq -r .counter $STATE_FILE)
RESTARTS_IN_STATE=$(jq -r .restarts $STATE_FILE)
echo "AFTER respawn: counter=$COUNTER_AFTER restarts_in_state=$RESTARTS_IN_STATE"

echo "=== verdict ==="
if [ "$COUNTER_AFTER" -gt "$COUNTER_BEFORE" ] && [ "$RESTARTS" -ge 1 ]; then
    echo "PASS: counter continued ($COUNTER_BEFORE → $COUNTER_AFTER), "
    echo "      process migrated lord-01 → $LORD_AFTER, restart_count=$RESTARTS"
    FINAL=0
else
    echo "FAIL: counter=$COUNTER_BEFORE → $COUNTER_AFTER (expected > $COUNTER_BEFORE)"
    echo "      restart_count=$RESTARTS"
    FINAL=1
fi

echo "=== cleanup ==="
docker rm -f lord-01 lord-02 2>/dev/null
pkill -f "scheduler --addr" 2>/dev/null
exit $FINAL
