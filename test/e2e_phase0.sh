#!/bin/bash
# e2e test для Phase 0.
# Запускает scheduler + lord, проверяет что Spawn работает.

set -e

cd "$(dirname "$0")/.."

SCHEDULER_BIN=./bin/scheduler
LORD_BIN=./bin/lord
ETRONIUM_BIN=./bin/etronium

if [ ! -x $SCHEDULER_BIN ] || [ ! -x $LORD_BIN ] || [ ! -x $ETRONIUM_BIN ]; then
  echo "ERROR: binaries not built. Run: go build -o bin/ ./cmd/..."
  exit 1
fi

# Cleanup
pkill -f "bin/scheduler" 2>/dev/null || true
pkill -f "bin/lord" 2>/dev/null || true
sleep 1

echo "=== Starting scheduler ==="
$SCHEDULER_BIN --addr=:50051 --log=info > /tmp/scheduler.log 2>&1 &
SCHED_PID=$!
sleep 1

echo "=== Starting lord ==="
$LORD_BIN --scheduler=localhost:50051 --log=info --log-format=tint > /tmp/lord.log 2>&1 &
LORD_PID=$!
sleep 2

cleanup() {
  echo "=== Cleanup ==="
  kill $SCHED_PID 2>/dev/null || true
  kill $LORD_PID 2>/dev/null || true
  sleep 1
}
trap cleanup EXIT

echo "=== Check scheduler log ==="
if ! grep -q "scheduler listening" /tmp/scheduler.log; then
  echo "FAIL: scheduler did not start"
  cat /tmp/scheduler.log
  exit 1
fi

echo "=== Check lord log ==="
if ! grep -q "registered with scheduler" /tmp/lord.log; then
  echo "FAIL: lord did not register"
  cat /tmp/lord.log
  exit 1
fi

echo "=== etronium lords ==="
$ETRONIUM_BIN --tenant=test lords

echo "=== etronium process spawn --exec=/bin/echo --arg=hello ==="
$ETRONIUM_BIN --tenant=test process spawn --exec=/bin/echo --arg=hello

echo "=== etronium process list ==="
sleep 1
$ETRONIUM_BIN --tenant=test process list

echo "=== etronium process spawn --exec=/bin/sleep --arg=2 ==="
SPAWN_OUT=$($ETRONIUM_BIN --tenant=test process spawn --exec=/bin/sleep --arg=2)
echo "$SPAWN_OUT"
PID=$(echo "$SPAWN_OUT" | grep "process_id:" | awk '{print $2}')
echo "PID=$PID"

echo "=== etronium process wait \$PID --timeout=10 ==="
$ETRONIUM_BIN --tenant=test process wait $PID --timeout=10

echo "=== etronium process get \$PID ==="
$ETRONIUM_BIN --tenant=test process get $PID

echo "=== etronium process spawn --exec=/bin/sleep --arg=60 ==="
LONG_OUT=$($ETRONIUM_BIN --tenant=test process spawn --exec=/bin/sleep --arg=60)
LONG_PID=$(echo "$LONG_OUT" | grep "process_id:" | awk '{print $2}')
echo "LONG_PID=$LONG_PID"

sleep 2
echo "=== etronium process kill \$LONG_PID --signal=15 ==="
$ETRONIUM_BIN --tenant=test process kill $LONG_PID --signal=15

sleep 1
echo "=== etronium process get \$LONG_PID ==="
$ETRONIUM_BIN --tenant=test process get $LONG_PID

echo "=== etronium process spawn --exec=/bin/echo --arg=hello-from-stdout ==="
IO_OUT=$($ETRONIUM_BIN --tenant=test process spawn --exec=/bin/echo --arg=hello-from-stdout)
IO_PID=$(echo "$IO_OUT" | grep "process_id:" | awk '{print $2}')
echo "IO_PID=$IO_PID"

sleep 1
echo "=== ALL TESTS PASSED ==="
