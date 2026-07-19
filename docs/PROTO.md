# gRPC API — Etronium-Scdr

> Контракт API. Сгенерирован из `proto/etronium/v1/etronium.proto`.

## Как читать

- **`SchedulerService`** — клиентский API (для tenant CLI `etronium`).
  Тенант НЕ ходит к лордам напрямую — всё через scheduler.
- **`LordService`** — серверный API (для доноров-lord'ов).
  Лорды регистрируются, шлют heartbeat, принимают задачи от scheduler'а.
- **Common типы** — `ResourceSpec`, `Task`, `TaskStatus`, `Lord`, `TaskResult`.

## Потоковая модель

| RPC | Тип | Кто инициирует | Когда реализуется |
|---|---|---|---|
| `SubmitTask` | unary | tenant → scheduler | Phase 0 |
| `GetTask` | unary | tenant → scheduler | Phase 0 |
| `ListTasks` | unary | tenant → scheduler | Phase 0 |
| `CancelTask` | unary | tenant → scheduler | Phase 0 |
| `ControlTask` | unary | tenant/scheduler | Phase 1 (для cascade на fan-out) |
| `ListLords` | unary | tenant → scheduler | Phase 0 |
| `StreamTask` | server stream | tenant → scheduler | Phase 2 |
| `Register` | unary | lord → scheduler | Phase 0 |
| `Heartbeat` | unary (MVP) / bidi (Phase 3+) | lord → scheduler | Phase 0 (unary) |
| `RunTask` | server stream | scheduler → lord | Phase 0 (упрощённый), Phase 1 (полный) |
| `AcknowledgeLazyDeath` | unary | lord → scheduler | Phase 4 |

## Fan-out (1 tenant → N lords)

`SubmitTaskRequest.target_lord_count = N` (N > 1) → scheduler создаёт:
- parent task (status = RUNNING когда все children завершились)
- N child tasks, по одному на лорда (status = свой жизненный цикл)

`RunTaskRequest` у каждого child содержит `parent_task_id` и `fanout_index` (0..N-1).
`ControlTask` на parent cascade'ом применяется ко всем children (`ControlTaskResponse.affected_task_ids`).

## Volume mounts

Три типа (`VolumeType`):
- `STATIC` — статика со scheduler'а, read-only. `source: "static://path"`.
- `DFS` — защищённое хранилище с pre-shared ключом. `source: "dfs://bucket/key"`, опционально `dfs_key_id`.
- `TMPFS` — RAM-backed. `source: "tmpfs://100mb"`.

Монтируются при создании контейнера; hot-mount — Phase 5+.

## OpenAPI / Swagger

`docs/openapi/etronium.swagger.json` сгенерирован из proto.
Можно открыть в [Swagger Editor](https://editor.swagger.io/) для визуального просмотра.

**Важно:** OpenAPI покрывает только unary RPC. Server-streaming (`RunTask`, `StreamTask`)
через grpc-gateway не маппится — они остаются gRPC-only.

## HTTP endpoints (для отладки через curl)

| Method | Path | RPC |
|---|---|---|
| POST | `/api/v1/tasks` | `SubmitTask` |
| GET | `/api/v1/tasks` | `ListTasks` |
| GET | `/api/v1/tasks/{taskId}` | `GetTask` |
| POST | `/api/v1/tasks/{taskId}/cancel` | `CancelTask` |
| POST | `/api/v1/tasks/{taskId}/control` | `ControlTask` |
| GET | `/api/v1/lords` | `ListLords` |
| POST | `/api/v1/lords/register` | `Register` |
| POST | `/api/v1/lords/{lordId}/heartbeat` | `Heartbeat` |
| POST | `/api/v1/lords/{lordId}/lazy-death` | `AcknowledgeLazyDeath` |

## Task lifecycle (FSM)

```
QUEUED → SCHEDULED → PULLING → RUNNING → COMPLETED
                                       ↘ FAILED
                                       ↘ CANCELED
```

- **QUEUED** — задача в очереди scheduler'а, ждёт placement.
- **SCHEDULED** — placement выбрал лорда, готовится отправка.
- **PULLING** — лорд начал pull образа (Phase 1+).
- **RUNNING** — контейнер стартовал, идёт исполнение.
- **COMPLETED / FAILED / CANCELED** — финальные, переходов нет.

В Phase 0 минимум: QUEUED → RUNNING → COMPLETED/FAILED.

## Идентификаторы

- **`task_id`** — ULID (sortable, k8s-style). Можно сортировать по времени создания.
- **`lord_id`** — ULID, присваивается scheduler'ом при `Register`.
- **`tenant_id`** — строка, пока pre-shared per CLI install; auth — Phase 2+.

## Аутентификация

В MVP — pre-shared token в gRPC metadata (`authorization: Bearer <token>`).
Реализация откладывается в Phase 2.
