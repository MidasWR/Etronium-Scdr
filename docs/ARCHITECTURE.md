# Architecture — целевая картина

> Это "как должно быть" в финале Phase 5. Поэтапно уточняется в `DECISIONS.md`.

## Высокоуровневая схема

```
┌─────────────────┐                ┌──────────────────┐
│  etronium CLI   │  gRPC          │  Scheduler (VPS) │
│  (tenant)       │ ─────────────► │  single binary   │
└─────────────────┘                │                  │
                                   │  • state store   │
                                   │  • placement     │
                                   │  • task queue    │
                                   │  • lord registry │
                                   └──────────────────┘
                                            │
                                            │ gRPC (bidi streaming)
                                            ▼
                                   ┌──────────────────┐
                                   │  Lord (donor)    │
                                   │  single binary   │
                                   │                  │
                                   │  • containerd    │
                                   │  • cgroups v2    │
                                   │  • task executor │
                                   └──────────────────┘
```

## Scheduler — внутренняя структура

```
internal/scheduler/
├── server.go           # gRPC server, API endpoints
├── state/
│   ├── store.go        # in-memory map + WAL
│   ├── task.go         # task state machine
│   ├── lord.go         # lord registry, health
│   └── reputation.go   # scoring
├── placement/
│   ├── filter.go       # feasibility check
│   ├── score.go        # ranking
│   └── planner.go      # orchestrator
├── queue/
│   └── queue.go        # task queue (heap-based)
├── api/
│   ├── v1.proto        # gRPC definitions
│   └── handlers.go
└── metrics/            # prometheus
```

## Lord — внутренняя структура

```
internal/lord/
├── agent.go            # main loop, heartbeat
├── runtime/
│   ├── containerd.go   # containerd client wrapper
│   ├── oci.go          # OCI spec builder
│   └── cgroups.go      # cgroups v2 wrapper
├── executor/
│   └── task.go         # task lifecycle (start, stream, finish)
├── rpc/
│   └── client.go       # gRPC client to scheduler
└── resources/
    └── monitor.go      # CPU, RAM, IO usage
```

## Демо-флоу (что должно работать end-to-end в Phase 5)

1. `etronium task submit --image=alpine -- echo "hello from lord"`
2. CLI шлёт gRPC `SubmitTask` к scheduler
3. Scheduler выбирает лорда через placement function
4. Scheduler шлёт `RunTask` лорду по gRPC stream
5. Lord через containerd запускает контейнер с alpine + команду
6. stdout/stderr стримятся обратно через gRPC stream
7. По завершении лорд отдаёт exit code + resource usage
8. Scheduler помечает task completed, тенант видит результат

## Сравнение с `../Etronium`

| Аспект | `../Etronium` (TECH-MVP) | `Etronium-Scdr` (здесь) |
|---|---|---|
| Scheduler ↔ Lord | HTTP pull (long-poll) | gRPC bidi stream |
| Container runtime | Прямые cgroups v2 файлы | containerd + OCI spec |
| Что запускается | `bash -c <command>` | OCI image + command |
| Tenant client | WebUI (HTML/JS) | CLI (`etronium` через cobra) |
| State | PostgreSQL | in-memory + WAL |
| Lords | один | минимум 3 |
| Placement | FIFO из БД | weighted score function |
| Auth | `LANDLORD_TOKEN` статичный | gRPC metadata + pre-shared token |
| Task IDs | UUID v4 | ULID (sortable) |
| Logging | slog | zap + tint |
| External DB | Postgres 16 | нет |

## Потоки данных (в процессе уточнения)

> Будет дополнено после Phase 0, когда определим реальные контракты.
