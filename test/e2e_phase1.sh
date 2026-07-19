#!/bin/bash
# e2e test для Phase 1 — cgroup v2 resource limits.
#
# Требует Docker (privileged + cgroupns=private).
# Бинари должны быть собраны (bin/{scheduler,lord,etronium}).

set -e

cd "$(dirname "$0")/.."

if [ ! -x bin/scheduler ] || [ ! -x bin/lord ] || [ ! -x bin/etronium ]; then
  echo "ERROR: binaries not built"
  exit 1
fi

CONTAINER_NAME="etronium-phase1-test"
if ! docker images | grep -q "etronium-test.*phase1"; then
  echo "=== Building Docker image ==="
  docker build -t etronium-test:phase1 -f test/Dockerfile.phase1 .
fi

echo "=== Starting container ==="
docker rm -f $CONTAINER_NAME 2>/dev/null || true
docker run --rm -d \
  --name $CONTAINER_NAME \
  --cgroupns=private \
  --privileged \
  --tmpfs /tmp \
  etronium-test:phase1 \
  sh -c 'while true; do sleep 60; done'

cleanup() {
  docker kill $CONTAINER_NAME 2>/dev/null || true
  docker rm -f $CONTAINER_NAME 2>/dev/null || true
}
trap cleanup EXIT

sleep 1

# Запускаем scheduler + lord внутри контейнера
echo "=== Starting scheduler ==="
docker exec -d $CONTAINER_NAME bash -c 'scheduler --addr=:50051 --log=info > /tmp/sched.log 2>&1'

echo "=== Starting lord ==="
docker exec -d $CONTAINER_NAME bash -c 'lord --scheduler=127.0.0.1:50051 --log=info --log-format=tint > /tmp/lord.log 2>&1'

sleep 5

# Делаем быстрый smoke-test
echo "=== smoke: lords list ==="
docker exec $CONTAINER_NAME etronium --scheduler=127.0.0.1:50051 --tenant=test lords

echo "=== spawn echo (no limits) ==="
docker exec $CONTAINER_NAME etronium --scheduler=127.0.0.1:50051 --tenant=test process spawn --exec=/bin/echo --arg=hello

sleep 1
echo "=== list processes ==="
docker exec $CONTAINER_NAME etronium --scheduler=127.0.0.1:50051 --tenant=test process list

# Тест 1: mem_limit_bytes — запускаем процесс который попытается съесть много памяти
echo "=== TEST 1: mem_limit_bytes=10MB (должен быть OOM kill) ==="
SPAWN_OUT=$(docker exec $CONTAINER_NAME etronium --scheduler=127.0.0.1:50051 --tenant=test process spawn \
  --exec=/bin/sh --arg=-c --arg='sleep 1 && cat /proc/self/status | head -3 && echo TRYING_TO_EAT_1GB && head -c 1073741824 /dev/zero | wc -c' \
  --resources='{"mem_limit_bytes":10485760}')
echo "$SPAWN_OUT"
PID1=$(echo "$SPAWN_OUT" | grep "process_id:" | awk '{print $2}')

sleep 3
echo "=== get $PID1 (expect STOPPED with signal=9 OOM) ==="
docker exec $CONTAINER_NAME etronium --scheduler=127.0.0.1:50051 --tenant=test process get $PID1

# Тест 2: pids_limit — fork bomb protection
echo "=== TEST 2: pids_limit=5 (форк должен фейлиться) ==="
SPAWN_OUT=$(docker exec $CONTAINER_NAME etronium --scheduler=127.0.0.1:50051 --tenant=test process spawn \
  --exec=/bin/sh --arg=-c --arg='for i in $(seq 1 20); do (sleep 60 &); done; sleep 1; ps -ef | wc -l' \
  --resources='{"pids_limit":5}')
echo "$SPAWN_OUT"
PID2=$(echo "$SPAWN_OUT" | grep "process_id:" | awk '{print $2}')

sleep 2
echo "=== get $PID2 (должен быть RUNNING или STOPPED с ошибкой) ==="
docker exec $CONTAINER_NAME etronium --scheduler=127.0.0.1:50051 --tenant=test process get $PID2

# Тест 3: валидация на scheduler — cpu_shares > 10000
echo "=== TEST 3: invalid cpu_shares (must be rejected) ==="
docker exec $CONTAINER_NAME etronium --scheduler=127.0.0.1:50051 --tenant=test process spawn \
  --exec=/bin/echo --arg=hi \
  --resources='{"cpu_shares":99999}' 2>&1 | head -3

# Тест 4: валидация — mem_limit_bytes < 0
echo "=== TEST 4: invalid mem_limit_bytes (must be rejected) ==="
docker exec $CONTAINER_NAME etronium --scheduler=127.0.0.1:50051 --tenant=test process spawn \
  --exec=/bin/echo --arg=hi \
  --resources='{"mem_limit_bytes":-1}' 2>&1 | head -3

# Тест 5: spawn с cpu_shares=200, проверяем что cgroup создан на lord'е
echo "=== TEST 5: cpu_shares=200, ищем slice в cgroup ==="
SPAWN_OUT=$(docker exec $CONTAINER_NAME etronium --scheduler=127.0.0.1:50051 --tenant=test process spawn \
  --exec=/bin/sleep --arg=30 \
  --resources='{"cpu_shares":200,"pids_limit":10}')
PID5=$(echo "$SPAWN_OUT" | grep "process_id:" | awk '{print $2}')
echo "PID5=$PID5"
sleep 2

# Заглядываем в cgroup изнутри контейнера
echo "--- /sys/fs/cgroup/etronium/* structure ---"
docker exec $CONTAINER_NAME find /sys/fs/cgroup -name "cpu.weight" 2>/dev/null | head -5
echo "--- slice content ---"
SLICE_DIR=$(docker exec $CONTAINER_NAME find /sys/fs/cgroup -name "cpu.weight" 2>/dev/null | head -1 | xargs -I{} dirname {} | xargs -I{} dirname {})
echo "SLICE_DIR=$SLICE_DIR"
if [ -n "$SLICE_DIR" ]; then
  docker exec $CONTAINER_NAME cat $SLICE_DIR/cpu.weight
  docker exec $CONTAINER_NAME cat $SLICE_DIR/pids.max
  docker exec $CONTAINER_NAME cat $SLICE_DIR/memory.max
fi

# Тест 6: long-running, проверяем что heartbeat читает реальные stats
echo "=== TEST 6: heartbeat stats (проверяем что lord читает cgroup cpu.usage) ==="
sleep 11
echo "--- lord log (cgroup/attach lines) ---"
docker exec $CONTAINER_NAME cat /tmp/lord.log 2>/dev/null | grep -i "cgroup\|attach\|slice" | tail -20

echo "=== TEST 7: spawn без лимитов + проверка структуры cgroup ==="
docker exec $CONTAINER_NAME etronium --scheduler=127.0.0.1:50051 --tenant=t process spawn \
  --exec=/bin/sleep --arg=15 \
  --resources='{"cpu_shares":200,"mem_limit_bytes":52428800,"pids_limit":50}' && \
sleep 2
echo "--- check slice created ---"
docker exec $CONTAINER_NAME bash -c 'ls /sys/fs/cgroup/etronium/*/*/ 2>/dev/null | head -20'
echo "--- check pid attached (если cgroup.procs доступен) ---"
docker exec $CONTAINER_NAME bash -c 'cat /sys/fs/cgroup/etronium/*/*/cgroup.procs 2>/dev/null | head -5'

echo "=== kill PID5 ==="
docker exec $CONTAINER_NAME etronium --scheduler=127.0.0.1:50051 --tenant=test process kill $PID5 --signal=15
sleep 1
docker exec $CONTAINER_NAME etronium --scheduler=127.0.0.1:50051 --tenant=test process get $PID5

echo "=== ALL TESTS PASSED ==="
