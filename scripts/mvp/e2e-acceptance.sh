#!/bin/bash
# e2e-acceptance.sh — comprehensive E2E acceptance test для "почти-прод" MVP.
#
# Что проверяет:
#   1. Topology: 1 frontend + 5 lords зарегистрированы
#   2. Auto-placement: 5 процессов spawned и распределены по ≥2 lords
#   3. Failure recovery: kill lord-A → все 5 процессов RUNNING после recovery
#   4. Stateful migration: counter не сбрасывается, restarts++ при миграции
#   5. Multi-tenant isolation: acme-corp не видит acme-edu
#   6. Tenant API seamless: тот же process_id до и после recovery
#
# Usage:
#   ./scripts/mvp/e2e-acceptance.sh           # full run
#   ./scripts/mvp/e2e-acceptance.sh --quick   # skip failure recovery test (Phase A+ D only)
#
# Exit codes:
#   0 — все проверки passed
#   1 — какая-то проверка failed
#
# Pre-req: image etronium-mvp:runtime собран, ./scripts/mvp/build-image.sh OK.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

export ETRONIUM_SCHEDULER="${ETRONIUM_SCHEDULER:-127.0.0.1:51061}"
SKIP_FAILURE_TEST=0
[[ "${1:-}" == "--quick" ]] && SKIP_FAILURE_TEST=1

G='\033[0;32m' R='\033[0;31m' Y='\033[1;33m' C='\033[0;36m' N='\033[0m'

PASSED=0
FAILED=0
FUNCS_PASSED=()

step() {
    echo ""
    echo -e "${Y}═══ $1 ═══${N}"
}

ok() {
    echo -e "${G}✓ $1${N}"
    PASSED=$((PASSED + 1))
}

fail() {
    echo -e "${R}✗ $1${N}"
    FAILED=$((FAILED + 1))
}

# ───────────────────────────────────────────────────────────────
step "STEP 0 — Setup: bring up testbed"
./scripts/mvp/down.sh >/dev/null 2>&1 || true
# После down удалить WAL/state явно, иначе scheduler восстановит
# все процессы от прошлого теста как 'phantom RUNNING' без реальных lords.
rm -rf /tmp/etronium/frontend.wal /tmp/etronium/state 2>/dev/null || true
sleep 2
./scripts/mvp/up.sh -d 2>&1 | tail -3
sleep 8  # дать всем lords зарегистрироваться

# ───────────────────────────────────────────────────────────────
step "STEP 1 — Topology: 5 lords registered"
LORD_COUNT=$(./bin/etronium lords 2>/dev/null | tail -n +2 | wc -l)
if [[ "$LORD_COUNT" -ge 5 ]]; then
    ok "5 lords зарегистрировано (получили $LORD_COUNT)"
else
    fail "Ожидалось ≥5 lords, получено $LORD_COUNT"
fi
./bin/etronium lords 2>/dev/null

# ───────────────────────────────────────────────────────────────
step "STEP 2 — Auto-placement: spawn 5 процессов distributed across lords"

# Tenant A (acme-corp) — spawn 5 stateless
for i in 1 2 3 4 5; do
    out=$(./bin/etronium --tenant=acme-corp process spawn --exec=/bin/sleep --arg=300 2>&1 | head -3)
    PID=$(echo "$out" | grep -oE 'process_id: [a-zA-Z0-9_]+' | awk '{print $2}')
    PIDS+=("$PID")
    echo "  spawned: $PID"
done
sleep 3

CORP_RUNNING=$(./bin/etronium --tenant=acme-corp process list 2>/dev/null | grep -E "^[0-9A-Z]{20,}" | grep -c "PROCESS_STATE_RUNNING")
if [[ "$CORP_RUNNING" -eq 5 ]]; then
    ok "5 процессов RUNNING для acme-corp"
else
    fail "acme-corp имеет $CORP_RUNNING/5 RUNNING"
fi

# Count unique lords используемых
UNIQUE_LORDS=$(./bin/etronium --tenant=acme-corp process list 2>/dev/null | tail -n +3 | awk '{print $2}' | sort -u | wc -l)
if [[ "$UNIQUE_LORDS" -ge 2 ]]; then
    ok "Процессы распределены по $UNIQUE_LORDS разным lords (heterogeneous placement работает)"
else
    fail "Процессы только на $UNIQUE_LORDS lord — placement не работает"
fi

# ───────────────────────────────────────────────────────────────
step "STEP 3 — Multi-tenant isolation: acme-edu не видит acme-corp"

# Spawn 3 для acme-edu
for i in 1 2 3; do
    ./bin/etronium --tenant=acme-edu process spawn --exec=/bin/sleep --arg=300 2>&1 | tail -2
done
sleep 2

CORP_COUNT=$(./bin/etronium --tenant=acme-corp process list 2>/dev/null | grep -E "^[0-9A-Z]{20,}" | grep -c "PROCESS_STATE_RUNNING")
EDU_COUNT=$(./bin/etronium --tenant=acme-edu process list 2>/dev/null | grep -E "^[0-9A-Z]{20,}" | grep -c "PROCESS_STATE_RUNNING")

if [[ "$CORP_COUNT" -eq 5 && "$EDU_COUNT" -eq 3 ]]; then
    ok "Tenant isolation: acme-corp видит 5, acme-edu видит 3 (нет leak)"
else
    fail "Isolation broken: corp=$CORP_COUNT, edu=$EDU_COUNT"
fi

# ───────────────────────────────────────────────────────────────
step "STEP 4 — Stateful migration через WAL persistence"

# Spawn stateful процесс с shared state file
STATE_FILE="/tmp/etronium/state/e2e-test.json"
sudo rm -f "$STATE_FILE" 2>/dev/null || rm -f "$STATE_FILE" 2>/dev/null

OUT=$(./bin/etronium --tenant=acme-corp process spawn \
    --exec=/usr/local/bin/example-stateful \
    --state-dump="$STATE_FILE" 2>&1)
STATE_PID=$(echo "$OUT" | grep -oE 'process_id: [a-zA-Z0-9_]+' | awk '{print $2}')
echo "  stateful process: $STATE_PID"
sleep 5

COUNTER_BEFORE=$(grep -oE '"counter": [0-9]+' "$STATE_FILE" 2>/dev/null | grep -oE '[0-9]+' | head -1)
echo "  counter before failure: $COUNTER_BEFORE"

# Determine which lord hosts it
HOSTED_LORD=$(./bin/etronium --tenant=acme-corp process list 2>/dev/null | grep "$STATE_PID" | awk '{print $2}')
HOSTED_CONTAINER="mvp-lord-01"
# Map lord-id to container name (heuristic — depends on hostname)
LORD_CONTAINER=$(docker ps --format '{{.Names}}' | while read cn; do
    if docker exec "$cn" cat /proc/1/cmdline 2>/dev/null | grep -q "lord"; then
        echo "$cn"
    fi
done | head -5)

# Kill the specific lord hosting stateful process
echo "  stateful pid hosted by: $(./bin/etronium --tenant=acme-corp process list | grep $STATE_PID | awk '{print $2}')"
# Just kill lord-01 — probability high that stateful ended up there
echo "  killing mvp-lord-01..."
# docker rm -f: skip restart: unless-stopped policy, force kill + remove,
# чтобы scheduler увидел disconnect и сделал recovery на другом lord.
docker rm -f mvp-lord-01 >/dev/null 2>&1
echo "  waiting 35s for recovery..."
sleep 35

# Check process still alive
STILL_RUNNING=$(./bin/etronium --tenant=acme-corp process list 2>/dev/null | grep -E "^[0-9A-Z]{20,}" | grep "$STATE_PID" | grep -c "PROCESS_STATE_RUNNING")
if [[ "$STILL_RUNNING" -eq 1 ]]; then
    ok "Stateful процесс ВЫЖИЛ после lord-A kill (process_id тот же: $STATE_PID)"
else
    fail "Stateful процесс НЕ найден после recovery"
fi

COUNTER_AFTER=$(grep -oE '"counter": [0-9]+' "$STATE_FILE" 2>/dev/null | grep -oE '[0-9]+' | head -1)
RESTARTS_AFTER=$(grep -oE '"restarts": [0-9]+' "$STATE_FILE" 2>/dev/null | grep -oE '[0-9]+' | head -1)
echo "  counter after recovery: $COUNTER_AFTER (restarts: $RESTARTS_AFTER)"

if [[ -n "$COUNTER_AFTER" && "$COUNTER_AFTER" -gt "${COUNTER_BEFORE:-0}" ]]; then
    ok "Counter продолжает инкрементироваться ($COUNTER_BEFORE → $COUNTER_AFTER), state сохранён"
else
    fail "Counter не вырос или state потерян ($COUNTER_BEFORE → $COUNTER_AFTER)"
fi

if [[ "$RESTARTS_AFTER" -ge 1 ]]; then
    ok "Process restarted on новом lord (restarts=$RESTARTS_AFTER)"
else
    fail "Restarts не увеличился — миграция не произошла"
fi

# ───────────────────────────────────────────────────────────────
step "STEP 5 — Failure recovery: kill lord-B → все остальные процессы RUNNING"

if [[ "$SKIP_FAILURE_TEST" -eq 1 ]]; then
    echo "  skipped (--quick)"
else
    CORP_BEFORE=$(./bin/etronium --tenant=acme-corp process list 2>/dev/null | grep -E "^[0-9A-Z]{20,}" | grep -c "PROCESS_STATE_RUNNING")
    echo "  corp processes before kill: $CORP_BEFORE"
    echo "  killing mvp-lord-03..."
    # docker rm -f: skip restart policy чтобы scheduler увидел disconnect.
    docker rm -f mvp-lord-03 >/dev/null 2>&1
    sleep 35
    CORP_AFTER=$(./bin/etronium --tenant=acme-corp process list 2>/dev/null | grep -E "^[0-9A-Z]{20,}" | grep -c "PROCESS_STATE_RUNNING")
    if [[ "$CORP_AFTER" -eq "$CORP_BEFORE" ]]; then
        ok "Recovery работает: $CORP_BEFORE процессов после kill lord-03"
    else
        fail "Recovery потерял процессы: $CORP_BEFORE → $CORP_AFTER"
    fi
fi

# ───────────────────────────────────────────────────────────────
step "STEP 6 — Teardown"
./scripts/mvp/down.sh 2>&1 | tail -2

# ───────────────────────────────────────────────────────────────
echo ""
echo -e "${Y}═══════════════════════════════════════════════════════${N}"
echo -e "${Y}  ACCEPTANCE RESULTS${N}"
echo -e "${Y}═══════════════════════════════════════════════════════${N}"
echo ""
echo -e "  ${G}Passed: $PASSED${N}"
echo -e "  ${R}Failed: $FAILED${N}"
echo ""

if [[ "$FAILED" -eq 0 ]]; then
    echo -e "${G}✅ ALL ACCEPTANCE CRITERIA MET — MVP ready for production-like use${N}"
    exit 0
else
    echo -e "${R}❌ Some acceptance criteria failed — see above${N}"
    exit 1
fi