# Etronium-Scdr

> Scheduler — single binary поверх распределённого железа.
> Реинкарнация идеи Etronium, **без интеграции** с `../Etronium`.
> Цель: минимально работающий MVP концепции "single runtime поверх распределённого железа".

## TL;DR

- **Scheduler** (VPS) — single Go binary, gRPC API, in-memory state + WAL.
- **Lord** (donor machine) — single Go binary, gRPC клиент к scheduler, исполняет задачи через **containerd** + cgroups v2.
- **Tenant** — CLI (`etronium` Go binary через cobra), общается со scheduler по gRPC.
- Стек: Go 1.22+, gRPC, protobuf, containerd, OCI spec.
- **Никаких внешних БД** в MVP — in-memory state + WAL.

## Документация

Вся документация — в [`docs/`](./docs/):

| Файл | Назначение |
|---|---|
| [`docs/RESEARCH.md`](./docs/RESEARCH.md) | стартовое исследование (импорт из исходного репо) |
| [`docs/ARCHITECTURE.md`](./docs/ARCHITECTURE.md) | целевая архитектура, диаграммы, контракты |
| [`docs/ROADMAP.md`](./docs/ROADMAP.md) | поэтапный план реализации (фазы 0–5) |
| [`docs/STACK.md`](./docs/STACK.md) | конкретные библиотеки, версии, что НЕ используем |
| [`docs/PROTO.md`](./docs/PROTO.md) | gRPC контракт, HTTP endpoints, FSM задач |
| [`docs/DECISIONS.md`](./docs/DECISIONS.md) | журнал принятых решений (ADR-style) |
| [`docs/OPEN-QUESTIONS.md`](./docs/OPEN-QUESTIONS.md) | нерешённые вопросы, требующие решения до старта |
| [`docs/openapi/etronium.swagger.json`](./docs/openapi/etronium.swagger.json) | сгенерированный swagger (открыть в editor.swagger.io) |

## Контракт

gRPC API определён в [`proto/etronium/v1/etronium.proto`](./proto/etronium/v1/etronium.proto).

Два сервиса:
- **`SchedulerService`** — клиентский (для tenant CLI).
- **`LordService`** — для доноров-lord'ов.

Сгенерированный Go лежит в `internal/gen/etronium/v1/`. Swagger — в `docs/openapi/`.

**Регенерация:** `make proto` (нужен `protoc 24+` + Go плагины `protoc-gen-go`,
`protoc-gen-go-grpc`, `protoc-gen-grpc-gateway`, `protoc-gen-openapiv2`).

## Статус

🚧 **Phase 0 — в работе.** Подробности в `docs/ROADMAP.md`.

## Связь с другим репо

`../Etronium/` (TECH-MVP) — текущая реализация через HTTP pull + cgroups v2 напрямую + WebUI + PostgreSQL.
**`Etronium-Scdr/` — независимый трек** на базе исследования от 2026-07-19. Общего кода нет. Знания и решения переносим только через документацию.
# Etronium-Scdr
