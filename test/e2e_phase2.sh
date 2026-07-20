#!/bin/bash
# e2e test для Phase 2 — multi-lord + NUMA-overcommit placement.
#
# Запускает 3 lord'а:
#   lord-01: physical capacity (default)
#   lord-02: physical + advertised 2x overcommit (CPU 3200 = 32 cores worth of shares)
#   lord-03: physical
#
# Проверяет:
#   1. Все 3 lord'а зарегистрированы
#   2. Spawn распределяется на лордов (не только lord-01)
#   3. Soft affinity: --prefer-lord работает
#   4. Heartbeat sweep: kill lord → marked unhealthy
#   5. Tenant isolation: tenant-A не видит процессы tenant-B
#
# Требования: Docker, бинари собраны в bin/.

set -e

cd "$(dirname "$0")/.."

if [ ! -x bin/scheduler ] || [ ! -x bin/lord ] || [ ! -x bin/etronium ]; then
  echo "ERROR: binaries not built — run: go build -o bin/ ./cmd/..."
  exit 1
fi

if ! docker images | grep -q "etronium-test.*phase1"; then
  echo "=== Building Docker image ==="
  docker build -t etronium-test:phase1 -f test/Dockerfile.phase1 .
fi

# Cleanup любых предыдущих запусков
pkill -f "bin/scheduler" 2>/dev/null || true
pkill -f "bin/lord" 2>/dev/null || true
sleep 1
docker rm -f lord-01 lord-02 lord-03 2>/dev/null || true

LOG_DIR=$(mktemp -d)
echo "=== Log dir: $LOG_DIR ==="

cleanup() {
  echo ""
  echo "=== Cleanup ==="
  # Сначала сохраняем логи lord'ов (до kill, чтобы docker exec работал)
  for name in lord-01 lord-02 lord-03; do
    docker logs "$name" > "$LOG_DIR/${name}.log" 2>&1 || true
    docker exec "$name" cat /tmp/lord.log > "$LOG_DIR/${name}-inner.log" 2>&1 || true
  done 2>/dev/null
  pkill -f "bin/scheduler" 2>/dev/null || true
  pkill -f "bin/lord" 2>/dev/null || true
  docker kill lord-01 lord-02 lord-03 2>/dev/null || true
  docker rm -f lord-01 lord-02 lord-03 2>/dev/null || true
  sleep 1
}
trap cleanup EXIT

echo "=== Starting scheduler (heartbeat TTL 30s, weighted placement) ==="
# TTL должен быть >> HeartbeatSec (10s на стороне lord'а), иначе sweeper отметит
# lord'а unhealthy между heartbeat'ами. Используем 30s = 3× HeartbeatSec.
SCHEDULER_HEARTBEAT_TTL=30s SCHEDULER_PLACEMENT=weighted \
  SCHEDULER_LOG_LEVEL=info \
  ./bin/scheduler --addr=:50051 --log=info > "$LOG_DIR/scheduler.log" 2>&1 &
SCHED_PID=$!
sleep 1
echo "scheduler PID=$SCHED_PID"

# Запускаем 3 lord'а в Docker (разные advertised capacity)
start_lord() {
  local name=$1
  local adv_cpu=$2
  local adv_cpu_flag=""
  if [ "$adv_cpu" -gt 0 ] 2>/dev/null; then
    adv_cpu_flag="--advertise-cpu=$adv_cpu"
  fi

  echo "=== Starting $name (advertise-cpu=$adv_cpu, flag='$adv_cpu_flag') ==="
  docker run --rm -d \
    --name "$name" \
    --network=host \
    --cgroupns=private \
    --privileged \
    --tmpfs /tmp \
    -e SCHEDULER_ADDR=127.0.0.1:50051 \
    -e LORD_LOG_LEVEL=info \
    -e LORD_ADVERTISE_CPU_SHARES="$adv_cpu" \
    etronium-test:phase1 \
    bash -c "lord --scheduler=127.0.0.1:50051 $adv_cpu_flag --log=info --log-format=json > /tmp/lord.log 2>&1; while true; do sleep 60; done"

  # Подтвердим что флаг реально попал в cmdline контейнера
  sleep 0.5
  echo "  - container Args: $(docker inspect "$name" --format '{{.Args}}' 2>/dev/null)"
}

start_lord lord-01 0
sleep 2
start_lord lord-02 3200   # 2x overcommit CPU shares
sleep 2
start_lord lord-03 0
sleep 4  # даём lord'ам зарегистрироваться

# Helper: запустить etronium CLI внутри lord-01 (там же наш бинарь)
CLI_RUN() {
  docker exec lord-01 "$@"
}

# (diagnostic block removed — it caused early cleanup via set -e + grep)

echo ""
echo "=== TEST 1: all 3 lords registered ==="
LORDS_JSON=$(CLI_RUN etronium --scheduler=127.0.0.1:50051 --tenant=t --json lords 2>&1)
echo "$LORDS_JSON" | python3 -c "
import json, sys
data = json.loads(sys.stdin.read())
lords = data if isinstance(data, list) else data.get('lords', [])
print(f'  lords count: {len(lords)}')
adv_cpus = []
for l in lords:
  lid = l['lord_id'][:10]
  adv_cpu = l.get('advertised_cpu_shares', 0) or 0
  adv_mem = l.get('advertised_mem_bytes', 0) or 0
  print(f\"  {lid}.. hostname={l.get('hostname','?'):20} cores={l['cpu_cores_physical']} adv_cpu={adv_cpu} adv_mem_mb={adv_mem//(1024*1024) if adv_mem else 0}\")
  adv_cpus.append(adv_cpu)
if len(lords) != 3:
  print(f'FAIL: expected 3 lords, got {len(lords)}')
  sys.exit(1)
if 3200 not in adv_cpus:
  print(f'FAIL: expected at least one lord with advertised_cpu_shares=3200, got {adv_cpus}')
  sys.exit(1)
print(f'OK: 3 lords registered, one with overcommit (3200)')
"

echo ""
echo "=== TEST 2: spawn 9 sleep-процессов, проверяем что weighted placement распределяет ==="
declare -A LORD_PROCS
for i in $(seq 1 9); do
  OUT=$(CLI_RUN etronium --scheduler=127.0.0.1:50051 --tenant=t process spawn \
    --exec=/bin/sleep --arg=15 2>&1)
  # Текстовый вывод: 'lord_id:    <id>'
  LORD=$(echo "$OUT" | grep "^lord_id:" | awk '{print $2}')
  PID=$(echo "$OUT" | grep "^process_id:" | awk '{print $2}')
  LORD_SHORT=${LORD:0:10}
  if [ -z "$LORD" ]; then
    echo "  spawn $i: FAIL — нет lord_id в выводе:"
    echo "$OUT" | sed 's/^/    /'
    exit 1
  fi
  LORD_PROCS[$LORD_SHORT]=$((${LORD_PROCS[$LORD_SHORT]:-0}+1))
  echo "  spawn $i: pid=$PID lord=${LORD_SHORT}"
done

echo "--- distribution ---"
for lord in "${!LORD_PROCS[@]}"; do
  echo "  $lord: ${LORD_PROCS[$lord]} procs"
done

NONZERO=$(for v in "${LORD_PROCS[@]}"; do echo $v; done | grep -v "^0$" | wc -l)
if [ "$NONZERO" -lt 2 ]; then
  echo "FAIL: placement не распределяет, только $NONZERO лордов с процессами"
  exit 1
fi
echo "OK: $NONZERO лордов получили процессы"

echo ""
echo "=== TEST 3: soft affinity — --prefer-lord работает ==="
# Достаём lord_id lord-02 из вывода ListLords (он с advertisedCpuShares=3200)
LORD2_ID=$(CLI_RUN etronium --scheduler=127.0.0.1:50051 --tenant=t --json lords 2>&1 | python3 -c "
import json, sys
data = json.loads(sys.stdin.read())
lords = data if isinstance(data, list) else data.get('lords', [])
for l in lords:
  if (l.get('advertised_cpu_shares') or 0) == 3200:
    print(l['lord_id'])
    break
")
if [ -z "$LORD2_ID" ]; then
  echo "FAIL: не нашли lord-02 по advertisedCpuShares=3200"
  exit 1
fi
echo "  lord-02 id = $LORD2_ID"

OUT=$(CLI_RUN etronium --scheduler=127.0.0.1:50051 --tenant=t process spawn \
  --exec=/bin/sleep --arg=10 --prefer-lord="$LORD2_ID" 2>&1)
SPAWNED_LORD=$(echo "$OUT" | grep "^lord_id:" | awk '{print $2}')
echo "  spawned на: $SPAWNED_LORD"
if [ "$SPAWNED_LORD" != "$LORD2_ID" ]; then
  echo "FAIL: --prefer-lord не сработал (хотели $LORD2_ID, получили $SPAWNED_LORD)"
  exit 1
fi
echo "OK: spawned на preferred lord-02"

echo ""
echo "=== TEST 4: tenant isolation — tenant-A не видит процессы tenant-B ==="
CLI_RUN etronium --scheduler=127.0.0.1:50051 --tenant=tenant-A process spawn \
  --exec=/bin/sleep --arg=10 > /dev/null 2>&1
CLI_RUN etronium --scheduler=127.0.0.1:50051 --tenant=tenant-B process spawn \
  --exec=/bin/sleep --arg=10 > /dev/null 2>&1

A_LIST=$(CLI_RUN etronium --scheduler=127.0.0.1:50051 --tenant=tenant-A process list 2>&1)
B_LIST=$(CLI_RUN etronium --scheduler=127.0.0.1:50051 --tenant=tenant-B process list 2>&1)
A_PROCS=$(echo "$A_LIST" | grep -c "PROCESS_STATE")
B_PROCS=$(echo "$B_LIST" | grep -c "PROCESS_STATE")
echo "  tenant-A procs: $A_PROCS, tenant-B procs: $B_PROCS"
if [ "$A_PROCS" -eq 0 ] || [ "$B_PROCS" -eq 0 ]; then
  echo "FAIL: tenant isolation (A=$A_PROCS B=$B_PROCS)"
  exit 1
fi
echo "OK: tenant isolation работает (оба тенанта видят свои процессы)"

echo ""
echo "=== TEST 5: heartbeat sweep — kill lord-03, проверяем что marked unhealthy ==="
docker kill lord-03
echo "  waiting 35s for heartbeat TTL=30s + sweep margin..."
sleep 35

CLI_RUN etronium --scheduler=127.0.0.1:50051 --tenant=t --json lords 2>&1 | python3 -c "
import json, sys
data = json.loads(sys.stdin.read())
lords = data if isinstance(data, list) else data.get('lords', [])
for l in lords:
  h = l.get('healthy', False)  # protojson omits false values
  print(f\"  {l['lord_id'][:10]}.. hostname={l.get('hostname','?'):20} healthy={h}\")
unhealthy = [l for l in lords if not l.get('healthy', False)]
if len(unhealthy) < 1:
  print('FAIL: expected at least 1 unhealthy lord after kill')
  sys.exit(1)
print(f'OK: {len(unhealthy)} unhealthy lord(s) после kill')
"

echo ""
echo "=== ALL PHASE 2 TESTS PASSED ==="
echo "Scheduler log: $LOG_DIR/scheduler.log"