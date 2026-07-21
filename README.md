# Etronium-Scdr

> **MOSIX-style Single System Image (SSI) поверх Linux, в user-space.**
> Через имитацию NUMA-архитектуры на сети машин.
> Scheduler = NUMA scheduler, Lords = cores, Processes = threads.

Не k8s. Не task-queue. Не "новый куб". Решаем задачу класса MOSIX/OpenMosix/Kerrighed — то что пробовали в 90-х-2000-х, но без kernel patches.

## TL;DR

- **Scheduler** (VPS) — single Go binary, gRPC API, in-memory process table + WAL.
- **Lord** (donor machine) — single Go binary, gRPC server. Локальный view: только свои процессы, честные ресурсы.
- **Tenant** (арендатор) — CLI (`etronium` через cobra). Глобальный view: свои процессы на всех lord'ах.
- Стек: Go 1.22+, gRPC, protobuf, cgroups v2, libcontainer, опционально CRIU.
- **Никаких внешних БД** в MVP — in-memory state + WAL.
- **Никаких kernel patches** — всё в user-space.

## Архитектура (NUMA-аналогия)

```
Многоядерный CPU              Etronium-Scdr
─────────────────             ─────────────────────────
CPU scheduler                 Scheduler (1 binary, VPS)
Ядра (cores)                  Lords (N binaries, donor machines)
Нити (threads)                Tenant processes
L1/L2 cache                   Lord local FS + file cache
Memory bus / QPI              Network (hier control + P2P data)
NUMA balancing                Process migration + weight rebalancing
```

Подробнее — [`docs/ARCHITECTURE.md`](./docs/ARCHITECTURE.md).

## Документация

| Файл | Назначение |
|---|---|
| **[`docs/OPS-GUIDE.md`](./docs/OPS-GUIDE.md)** | **Operations: install / onboard / day-N troubleshooting** |
| **[`docs/LORD-SETUP.md`](./docs/LORD-SETUP.md)** | **Onboarding a new lord (container or bare-metal)** |
| **[`docs/TENANT-USAGE.md`](./docs/TENANT-USAGE.md)** | **Tenant CLI reference (spawn/kill/list/migrate)** |
| [`docs/RESEARCH.md`](./docs/RESEARCH.md) | Исследование (ре-фрейм 2026-07-20) |
| [`docs/ARCHITECTURE.md`](./docs/ARCHITECTURE.md) | NUMA-аналогия, потоки Spawn/Migrate |
| [`docs/ROADMAP.md`](./docs/ROADMAP.md) | Phase 0–5: hello world → persistence |
| [`docs/STACK.md`](./docs/STACK.md) | Конкретные библиотеки, версии |
| [`docs/PROTO.md`](./docs/PROTO.md) | gRPC контракт v2, FSM процессов |
| [`docs/DECISIONS.md`](./docs/DECISIONS.md) | ADR-style журнал |
| [`docs/OPEN-QUESTIONS.md`](./docs/OPEN-QUESTIONS.md) | Нерешённые вопросы |
| `BPF-side` docs | [`../Etronium-Scdr-BPF/docs/`](../Etronium-Scdr-BPF/docs/) |

## Контракт

gRPC API в [`proto/etronium/v1/etronium.proto`](./proto/etronium/v1/etronium.proto).

Два сервиса:
- **`SchedulerService`** — tenant вызывает Spawn/Kill/Wait/Migrate.
- **`LordService`** — scheduler вызывает ExecRemote/KillRemote/Checkpoint/Restore.

Сгенерированный Go лежит в `internal/gen/etronium/v1/`. Swagger — в `docs/openapi/`.

**Регенерация:** `make proto` (нужен `protoc 24+` + Go плагины `protoc-gen-go`,
`protoc-gen-go-grpc`, `protoc-gen-grpc-gateway`, `protoc-gen-openapiv2`).

## Статус

✅ **Phase 0–4 готовы**, Phase 5 (WAL + graceful drain) done, live migration отвергнута.

Текущая функциональность:
- Multi-lord, weighted placement по CPU+RAM
- cgroup v2 isolation per process
- Fault tolerance: lord dies → respawn на здоровом lord'е (V4)
- Opt-in state serialization для переживания lord death (V5)
- WAL для process_table (cold start replay)
- Graceful shutdown с drain lords
- Prometheus-friendly JSON logs

Не работает: live migration (CRIU) — отвергнута на kernel 6.17. Fault tolerance через V4+V5 вместо неё.

## Quick Start (5 минут)

```bash
# 1. Build (нужен Go 1.22+):
make build

# 2. Поднять scheduler + 3 lords (Docker):
docker compose -f test/docker-compose.yml up -d
sleep 5

# 3. С tenant CLI:
./bin/etronium lords                                    # видим 3 lord'а
./bin/etronium process spawn --exec=/bin/sleep --arg=60  # создать процесс
./bin/etronium process list                             # список

# 4. 5-минутное демо для PM:
./scripts/demo-pm.sh

# Остановить:
docker compose -f test/docker-compose.yml down
```

## Сценарий fault tolerance вручную

```bash
# Spawn stateful app:
./bin/etronium process spawn \
    --exec=./bin/example-stateful \
    --state-dump=/tmp/etronium/state/demo.json \
    --max-restarts=10

# Kill lord container:
docker kill etronium-lord-02

# Через 5–15 сек:
./bin/etronium process list                  # процесс снова RUNNING, на другом lord'е
cat /tmp/etronium/state/demo.json | jq .counter   # counter сохранился
```



## Связь с другим репо

`../Etronium/` (TECH-MVP) — старая реализация: HTTP pull + cgroups v2 напрямую + WebUI + PostgreSQL.
**`Etronium-Scdr/` — независимый трек** на базе переосмысленного research от 2026-07-20. Общего кода нет.
