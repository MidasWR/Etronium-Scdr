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
| [`docs/RESEARCH.md`](./docs/RESEARCH.md) | Исследование (ре-фрейм 2026-07-20) |
| [`docs/ARCHITECTURE.md`](./docs/ARCHITECTURE.md) | NUMA-аналогия, потоки Spawn/Migrate |
| [`docs/ROADMAP.md`](./docs/ROADMAP.md) | Phase 0–5: hello world → persistence |
| [`docs/STACK.md`](./docs/STACK.md) | Конкретные библиотеки, версии |
| [`docs/PROTO.md`](./docs/PROTO.md) | gRPC контракт v2, FSM процессов |
| [`docs/DECISIONS.md`](./docs/DECISIONS.md) | ADR-style журнал (23 записи) |
| [`docs/OPEN-QUESTIONS.md`](./docs/OPEN-QUESTIONS.md) | Нерешённые вопросы |
| [`docs/openapi/etronium.swagger.json`](./docs/openapi/etronium.swagger.json) | Сгенерированный swagger (10 paths, 35 types) |

## Контракт

gRPC API в [`proto/etronium/v1/etronium.proto`](./proto/etronium/v1/etronium.proto).

Два сервиса:
- **`SchedulerService`** — tenant вызывает Spawn/Kill/Wait/Migrate.
- **`LordService`** — scheduler вызывает ExecRemote/KillRemote/Checkpoint/Restore.

Сгенерированный Go лежит в `internal/gen/etronium/v1/`. Swagger — в `docs/openapi/`.

**Регенерация:** `make proto` (нужен `protoc 24+` + Go плагины `protoc-gen-go`,
`protoc-gen-go-grpc`, `protoc-gen-grpc-gateway`, `protoc-gen-openapiv2`).

## Статус

🚧 **Phase 0 — в работе.** Подробности в `docs/ROADMAP.md`.

## Связь с другим репо

`../Etronium/` (TECH-MVP) — старая реализация: HTTP pull + cgroups v2 напрямую + WebUI + PostgreSQL.
**`Etronium-Scdr/` — независимый трек** на базе переосмысленного research от 2026-07-20. Общего кода нет.
