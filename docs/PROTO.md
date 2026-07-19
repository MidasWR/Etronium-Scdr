# gRPC API — Etronium-Scdr

> Контракт v2 (POSIX-подобный process API). Сгенерирован из `proto/etronium/v1/etronium.proto`.

## Два сервиса

- **`SchedulerService`** — то что вызывает tenant (арендатор).
  - Видит глобально: процессы тенанта на всех lord'ах где они сейчас живут.
  - Вызывает Spawn/Kill/Wait/Migrate, не знает на каком lord'е процесс.
- **`LordService`** — то что scheduler вызывает на lord'е.
  - Lord видит локально: только свои процессы.
  - Принимает ExecRemote/KillRemote/StatsRemote/Checkpoint/Restore от scheduler'а.

## RPC SchedulerService (tenant API)

| RPC | Тип | Назначение |
|---|---|---|
| `Spawn` | unary | Создать процесс |
| `Kill` | unary | Послать сигнал (default SIGTERM) |
| `Wait` | unary | Блокирующее ожидание exit |
| `GetProcess` | unary | Снимок состояния |
| `ListProcesses` | unary | Список процессов тенанта (все lord'ы) |
| `Migrate` | unary | Переместить процесс на другой lord (CRIU) |
| `ListLords` | unary | Дамп всех lord'ов (admin) |
| `StreamProcessIO` | server stream | stdin/stdout/stderr живого процесса |
| `WatchProcess` | server stream | Подписка на lifecycle events |
| `PullFile` | unary | Забрать файл с lord'а |
| `PushFile` | unary | Положить файл на lord'а |
| `InvalidateFileCache` | unary | Сбросить локальную копию |

## RPC LordService (scheduler → lord)

| RPC | Тип | Назначение |
|---|---|---|
| `Register` | unary | Регистрация lord'а |
| `Heartbeat` | unary | Keepalive + метрики |
| `ExecRemote` | server stream | Запустить процесс (stream IO до завершения) |
| `KillRemote` | unary | Послать сигнал процессу |
| `StatsRemote` | unary | Текущие метрики lord'а |
| `FilePull` | unary | Забрать файл |
| `FilePush` | unary | Положить файл |
| `Checkpoint` | unary | CRIU dump |
| `Restore` | unary | CRIU restore |
| `AcknowledgeLazyDeath` | unary | Объявить о завершении |

## Process lifecycle (FSM)

```
NEW ──► READY ──► RUNNING ──► EXITED
                │         │
                │         ├──► PAUSED ──► RUNNING
                │         │
                │         ├──► MIGRATING ──► RUNNING (на другом lord'е)
                │         │
                │         └──► STOPPED (killed by signal)
```

- **NEW** — создан в process_table, ещё не placement'нут
- **READY** — placement выбран, ExecRemote отправлен
- **RUNNING** — бежит на lord'е
- **PAUSED** — cgroup.freeze=1 + SIGSTOP
- **MIGRATING** — CRIU dump + restore в процессе
- **EXITED** — завершился нормально
- **STOPPED** — убит сигналом

## Resource model

`ResourceSpec`:
- `cpu_shares` — относительный вес (cgroup cpu.weight), default 100
- `cpu_quota_pct` — жёсткий лимит в % от ядра, 0 = только weight
- `mem_limit_bytes` — жёсткий лимит RAM, 0 = без лимита
- `io_weight` — blkio weight, default 100
- `pids_limit` — max процессов/тредов

Два уровня:
- **local_capacity** на lord'е — реальный cgroup лимит
- **advertised_capacity** в proto — что scheduler показывает тенанту (может быть больше)

Linux kernel на lord'е видит только local_capacity.

## HTTP endpoints (для отладки через curl)

Только unary RPC. Streaming остались gRPC-only.

| Method | Path | RPC |
|---|---|---|
| POST | `/api/v1/processes` | `Spawn` |
| GET | `/api/v1/processes` | `ListProcesses` |
| GET | `/api/v1/processes/{processId}` | `GetProcess` |
| POST | `/api/v1/processes/{processId}/kill` | `Kill` |
| POST | `/api/v1/processes/{processId}/wait` | `Wait` |
| POST | `/api/v1/processes/{processId}/migrate` | `Migrate` |
| POST | `/api/v1/processes/{processId}/files` | `PushFile` |
| GET | `/api/v1/lords` | `ListLords` |
| POST | `/api/v1/lords/register` | `Register` |
| POST | `/api/v1/lords/{lordId}/heartbeat` | `Heartbeat` |
| POST | `/api/v1/lords/{lordId}/lazy-death` | `AcknowledgeLazyDeath` |

## Идентификаторы

- **`process_id`** — ULID (sortable, stable across migration)
- **`lord_id`** — ULID
- **`local_pid`** — int, меняется при миграции

Глобально процесс адресуется через `(process_id)` ИЛИ `(lord_id, local_pid)`.

## Аутентификация

Pre-shared token в gRPC metadata (`authorization: Bearer <token>`).
Scheduler проверяет при каждом RPC. Lord'ы тоже авторизуются в scheduler'е.

## Файловые операции

Только локальный копи (ADR 019):
- `PullFile(process_id, path)` → scheduler просит lord прочитать → bytes
- `PushFile(process_id, path, data)` → обратно
- `InvalidateFileCache(process_id, path)` → сброс кэша
- На lord'е LRU кэш с TTL + size limit
- SHA-256 для integrity
