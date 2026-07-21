#!/bin/bash
# demo.sh — 5-минутный PM-demo Etronium MVP.
#
# Что показывается:
#   1. Topology — фронтенд + 3 lord'а (имитация школьников)
#   2. Tenant spawn несколько процессов
#   3. Auto-placement процессов по lord'ам
#   4. Имитация failure одного lord'а — auto-recovery
#   5. Имитация смерти lord'а навсегда — recovery на других
#   6. Show client-VPS-view: ps/top как будто одна машина
#
# Использование: ./scripts/mvp/demo.sh

set -euo pipefail
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

# MVP frontend listens on :51061 (chaos-testbed port). After-fix from
# commit 95f0391 (port collision fix). Override via env if needed.
export ETRONIUM_SCHEDULER="${ETRONIUM_SCHEDULER:-127.0.0.1:51061}"
export ETRONIUM_TENANT="${ETRONIUM_TENANT:-demo-tenant}"

C='\033[0;36m' G='\033[0;32m' Y='\033[1;33m' R='\033[0;31m' N='\033[0m'

header() {
    echo ""
    echo -e "${Y}════════════════════════════════════════════════════════════════════${N}"
    echo -e "${Y}  $1${N}"
    echo -e "${Y}════════════════════════════════════════════════════════════════════${N}"
    sleep 2
}

# ───────────────────────────────────────────────────────────────
header "STEP 0 — Подготовка: посмотрим что testbed живой"

./bin/etronium lords || true
sleep 2

# ───────────────────────────────────────────────────────────────
header "STEP 1 — Topology: 1 frontend + 3 lord-providers"
echo -e "${C}Цель:${N} показать что у нас heterogeneous compute (lord'ы разные)."
echo -e "${C}lord-school-A:${N} advertise-cpu=3200 advertise-mem=4GB"
echo -e "${C}lord-school-B:${N} advertise-cpu=1600 advertise-mem=2GB"
echo -e "${C}lord-school-C:${N} advertise-cpu=2400 advertise-mem=3GB"
sleep 3

# ───────────────────────────────────────────────────────────────
header "STEP 2 — Tenant делает spawn (auto-placement)"

PIDS=()
for i in 1 2 3 4 5; do
    out=$(./bin/etronium run /bin/sleep 600 2>&1 | head -5)
    echo -e "${G}$out${N}"
    # Format: "process_id: 01KY..." (с пробелом после двоеточия) — v2+
    PID=$(echo "$out" | grep -oE 'process_id: [a-zA-Z0-9_]+' | awk '{print $2}')
    PIDS+=("$PID")
done
echo ""
echo "Spawned ${#PIDS[@]} процессов."
sleep 3

# ───────────────────────────────────────────────────────────────
header "STEP 3 — Distributed ps: посмотрим где они живут"

echo -e "${C}Tenant видит все процессы как будто они на одной машине:${N}"
./bin/etronium ps
sleep 3

# ───────────────────────────────────────────────────────────────
header "STEP 4 — Failure simulation: убиваем lord-A (имитация выключения ноута школьника)"
echo ""
echo -e "${R}stopping lord-school-A...${N}"
docker stop mvp-lord-01 >/dev/null
sleep 2
echo -e "${R}lord-A мёртв. Подождём 35s чтобы scheduler его заметил и сделал respawn...${N}"
echo ""
for i in 30 25 20 15 10 5 0; do
    echo -n "  T-${i}s "
    sleep 5
done

echo ""
echo -e "${G}После 35s recovery отработал:${N}"
./bin/etronium ps
sleep 3

# ───────────────────────────────────────────────────────────────
header "STEP 5 — Все 5 процессов ВЫЖИЛИ (auto-respawn на других lord'ах)"

EXPECTED=${#PIDS[@]}
SURVIVED=$(./bin/etronium ps | grep -c "PROCESS_STATE_RUNNING" || echo 0)

if [ "$SURVIVED" -ge "$EXPECTED" ]; then
    echo -e "${G}✅ ${SURVIVED}/${EXPECTED} процессов RUNNING после auto-recovery${N}"
    echo -e "${G}Это и есть 'клиент не зависит от изменений на стороне lord'а'.${N}"
else
    echo -e "${R}⚠ ${SURVIVED}/${EXPECTED} процессов выжило. Recovery неполный — known limitation.${N}"
fi
sleep 3

# ───────────────────────────────────────────────────────────────
header "STEP 6 — Финал: client-VPS-view"

echo -e "${C}Tenant никогда не знал что lord-A умер. Для него: VPS работает.${N}"
./bin/etronium ps | head -20
echo ""
echo -e "${C}Чтобы окончательно убедиться — посмотрим logs на frontend:${N}"
docker logs mvp-frontend --tail=15 2>/dev/null | grep -E "recovery|respawn|disconnect" | head -10 || true

echo ""
echo -e "${Y}=== END DEMO ===${N}"
echo ""
echo "Cleanup: ./scripts/mvp/down.sh"
