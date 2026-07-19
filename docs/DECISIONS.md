# Decisions Log (ADR-style)

> Каждое решение фиксируем здесь с обоснованием. Формат:
> ```
> ## NNN — Краткое название (YYYY-MM-DD)
> **Контекст:** ...
> **Решение:** ...
> **Альтернативы:** ...
> **Последствия:** ...
> ```

---

## 001 — Создание отдельного репо Etronium-Scdr (2026-07-19)

**Контекст:** В `../Etronium/` есть текущая реализация (HTTP pull, WebUI, Postgres). Research от 2026-07-19 предлагает другую архитектуру (gRPC, containerd, CLI). Хотим развивать новую архитектуру **без интеграции** с текущим кодом, чтобы не ломать то что уже работает.

**Решение:** Создать отдельное репо `Etronium-Scdr/` рядом с `Etronium/`. Общего кода нет, перенос только через документацию.

**Альтернативы:**
- Форкнуть `Etronium/` и переписать внутри — но это путает историю git и усложняет навигацию.
- Subdirectory в `Etronium/v2/` — но research явно говорит "без интеграции".
- Submodule — overkill для двух репо одного разработчика.

**Последствия:**
- ✅ Чистая история, можно в любой момент удалить/архивировать.
- ✅ Документы ссылаются на research как на первоисточник.
- ⚠️ Нужно явно следить чтобы решения из одного репо не "протекали" в другое без фиксации.

---

## 002 — Имя репо "Etronium-Scdr" (2026-07-19)

**Контекст:** "Scdr" — сокращение от "Scheduler". Отвечает на вопрос "что это за репо" одним словом.

**Решение:** `Etronium-Scdr` (не `Etronium2`, не `Etronium-v2`, не `Etronium-New`).

**Альтернативы:**
- `Etronium2` — звучит как продолжение, может сбить с толку.
- `Etronium-v2` — то же самое, плюс дефис лишний.
- `Etronium-New` — временное имя, придётся переименовывать.
- `etronium-scheduler` — слишком длинно, и в репо 3 бинаря.

**Последствия:**
- ✅ Короткое, запоминается.
- ⚠️ Не очевидно что "Scdr" = "Scheduler" для тех, кто не знает контекста — поэтому в README явно расшифровано.

---

(будет дополняться по мере принятия решений)

---

## 003 — gRPC как основной протокол (2026-07-19)

**Контекст:** Research предлагает gRPC для scheduler↔lord и tenant↔scheduler. Альтернативы: REST/JSON, message bus (NATS/Kafka), самописный TCP-протокол.

**Решение:** gRPC для всех межсервисных взаимодействий. HTTP/REST используется только как side-channel через grpc-gateway для отладки (`curl`), не как runtime API.

**Альтернативы:**
- REST/JSON — проще отлаживать, но нет streaming, нет codegen, нет строгого контракта.
- NATS/Kafka — overkill для MVP, добавляет внешнюю зависимость.
- Самописный TCP — никогда так не делаем.

**Последствия:**
- ✅ Codegen для Go-клиента и сервера.
- ✅ Bidirectional streaming из коробки (нужно для `RunTask`, `StreamTask`).
- ✅ Строгий proto-контракт — изменения версионируются.
- ⚠️ Нужен protoc + 4 плагина в dev-окружении (см. Makefile).
- ⚠️ Для отладки через curl нужен grpc-gateway или grpcurl.

---

## 004 — Два отдельных сервиса в одном proto (2026-07-19)

**Контекст:** Scheduler ↔ Tenant и Scheduler ↔ Lord — это разные потоки данных с разной семантикой (один — публичный, другой — service-to-service). Можно было бы сделать два proto-файла.

**Решение:** Один proto-файл `etronium/v1/etronium.proto` с двумя `service` блоками. Общие типы (`Task`, `Lord`, `TaskStatus`, `ResourceSpec`) переиспользуются.

**Альтернативы:**
- Два proto-файла (`scheduler.proto`, `lord.proto`) — но тогда общие типы пришлось бы дублировать или выносить в `common.proto` (три файла).
- Один service — смешивает семантику, плохо для codegen.

**Последствия:**
- ✅ Один источник правды для типов.
- ✅ Проще навигация.
- ⚠️ При желании разделить deploy (например, отдельный `lord-api` сервис) — будет сложно вычленить.

---

## 005 — ULID для task_id и lord_id (2026-07-19)

**Контекст:** UUID v4 (random) vs ULID (sortable). В Phase 3+ понадобится сортировка задач по времени создания, чтобы placement мог учитывать "свежесть" и для эффективной пагинации.

**Решение:** ULID (`github.com/oklog/ulid/v2`) для `task_id` и `lord_id`. UUID v4 (`github.com/google/uuid`) — только для `session_id` если появится.

**Альтернативы:**
- UUID v4 — проще, но не сортируется (можно добавить `created_at` индекс, но всё равно не k8s-style).
- Snowflake (Twitter) — требует координации между нодами, overkill.
- KSUID (segmentio) — аналог ULID, но хуже поддержка в Go.

**Последствия:**
- ✅ Sortable by creation time → эффективная пагинация через `next_page_token`.
- ✅ K8s-style ID, привычно.
- ⚠️ Дополнительная зависимость `github.com/oklog/ulid/v2`.

---

## 006 — OpenAPI генерируется только для документации (2026-07-19)

**Контекст:** grpc-gateway умеет делать HTTP→gRPC маппинг автоматически. Можно поднять REST API параллельно с gRPC.

**Решение:** OpenAPI/swagger генерируется из proto-аннотаций, но **runtime gateway не поднимаем**. Используем только для документации и опциональной отладки через curl.

**Альтернативы:**
- Поднять grpc-gateway как отдельный бинарь — но это лишний процесс для MVP и второй API для поддержки.
- Не генерировать swagger вообще — но proto-аннотации (`summary`, `description`) полезны и для кода.

**Последствия:**
- ✅ Один API (gRPC), меньше поверхность для багов.
- ✅ Swagger полезен для онбординга и обсуждения контракта.
- ⚠️ Для отладки через curl придётся либо использовать grpcurl, либо писать тонкий debug-server (Phase 5+).

---

## 007 — StreamTask и RunTask остаются gRPC-only (2026-07-19)

**Контекст:** grpc-gateway не маппит server-streaming RPC на HTTP без хаков (chunked encoding, SSE). Можно попробовать, но это сложно и хрупко.

**Решение:** Server-streaming RPC (`RunTask`, `StreamTask`) — только gRPC. HTTP endpoints покрывают только unary RPC.

**Альтернативы:**
- Server-Sent Events (SSE) для стриминга через HTTP — можно, но это другой протокол, отдельная реализация. Отложено в Phase 5+.
- WebSocket — overkill, gRPC уже решает задачу.

**Последствия:**
- ✅ Простота: streaming = gRPC, unary = gRPC + HTTP gateway.
- ⚠️ Для демо в браузере (если когда-нибудь появится WebUI) придётся делать gRPC-Web или отдельный streaming endpoint.

---

## 008 — Формат command как repeated string (argv-style) (2026-07-19)

**Контекст:** В research написано `sh -c "echo a; sleep 1; echo b"` одной строкой. Можно было бы передавать command одной строкой, как в docker CLI.

**Решение:** `repeated string command = N;` — argv-style. Например `["sh", "-c", "echo a; sleep 1; echo b"]`. Это совпадает с тем, что containerd ожидает.

**Альтернативы:**
- Одна строка + shell-парсинг — хрупко (экранирование), небезопасно (нельзя нормально denylist'ить).
- JSON-массивом строкой — то же самое, но без type-safety.

**Последствия:**
- ✅ Type-safe, нет shell-injection.
- ✅ Совпадает с containerd API напрямую.
- ⚠️ Менее удобно в CLI — `etronium task submit -- sh -c "..."` придётся мапить в argv.


---

## 009 — 1 tenant : N lords (fan-out placement) (2026-07-19)

**Контекст:** Ответ на Open Question #1. Решили НЕ делать `1 lord = 1 tenant` (как в research),
а вместо этого `1 tenant → N lords`. То есть тенант масштабируется **вширь** (несколько лордов
параллельно), а не вглубь (мульти-тенант на одном лорде).

**Решение:**
- Placement по умолчанию = "один лучший лорд" (по score function).
- Тенант может запросить fan-out: `SubmitTaskRequest.target_lord_count = N` (N > 1).
- Scheduler создаёт parent task + N child tasks, по одному на лорда. Stream идёт от parent'а
  ко всем child'ам параллельно. Результат parent'а агрегируется из children.
- Мульти-тенант на одном лорде — поддерживается через cgroups v2 isolation, разные tenant_id
  в одном контейнере НЕ живут.

**Альтернативы:**
- 1 lord = 1 tenant (research recommendation) — проще, но не масштабируется. Если у тенанта
  задача CPU-heavy и требует 100 cores, ему надо 100 лордов, а не 1 lord с 100 cores.
- Multiplex tenants в один контейнер — слишком шумно на стороне изоляции.

**Последствия:**
- ✅ Естественное масштабирование тенанта.
- ✅ Placement функция становится сразу интересной.
- ⚠️ Parent/child state machine сложнее. Появляется `ControlTask` для cascade-операций.
- ⚠️ Агрегация результата из N лордов: ждём всех или хотя бы majority?
  → MVP: ждём всех (consensus позже).

**Proto изменения:**
- `SubmitTaskRequest.target_lord_count`
- `SubmitTaskResponse.child_task_ids`
- `RunTaskRequest.parent_task_id`, `fanout_index`
- `Task.parent_task_id` (для children)

---

## 010 — Volume mounts: статика с scheduler'а + DFS для динамики (2026-07-19)

**Контекст:** Ответ на Open Question #2. Тенанту нужны файлы:
- "Основа" (статика) — общие бинарники, конфиги, референсные данные. Не меняется часто.
- "Динамика" — тенантские файлы, рантайм-артефакты. Меняется, защищённая.

**Решение:** Три типа volume mounts:
- `STATIC` — scheduler отдаёт файл/папку через внутренний канал (gRPC stream или NFS).
  Read-only по умолчанию. Кэшируется на lord'е.
- `DFS` — защищённое distributed storage. Auth через pre-shared ключ, id которого
  указан в `VolumeMount.dfs_key_id`. Lord делает mount при старте контейнера.
- `TMPFS` — RAM-backed, ephemeral. Для `/tmp`, scratch space.

`source` — opaque URI: `static://path`, `dfs://bucket/key`, `tmpfs://size_mb`.

**Альтернативы:**
- S3 / MinIO как одно хранилище на всё — проще в разработке, но смешивает security boundaries.
- Всё через NFS — единая точка отказа.
- Копировать в lord перед стартом — медленно для больших статик.

**Последствия:**
- ✅ Чёткое разделение: что публичное (static), что защищённое (DFS).
- ✅ Гибкость: тенант сам решает что ему нужно.
- ⚠️ Три имплементации вместо одной. DFS — отдельный микросервис или часть scheduler'а?
  → MVP: DFS = in-memory mock с pre-shared ключом, в проде — выделенный сервис.
- ⚠️ Mount во время выполнения (Phase 5+) — отдельная задача, пока mounts только при создании.

**Proto изменения:**
- `VolumeType` enum
- `VolumeMount` message
- `SubmitTaskRequest.volumes`, `RunTaskRequest.volumes`

---

## 011 — Приватный image registry `midaswr` (2026-07-19)

**Контекст:** Ответ на Open Question #3. Ключ на машине уже есть, свой регистр.

**Решение:** Lord конфигурируется с endpoint'ом `midaswr` registry и pre-shared TLS key.
Containerd пулит образы оттуда. Default `image` в `SubmitTaskRequest` не имеет префикса —
scheduler дописывает `midaswr/` автоматически.

Пример:
- Тенант пишет `--image=myimage:tag`
- Scheduler интерпретирует как `midaswr/myimage:tag`
- Lord делает pull `https://midaswr.local/v2/myimage/manifests/tag` с TLS key

Публичные образы (alpine, ubuntu) — по-прежнему работают, явно через `docker.io/alpine` или
`library/alpine`.

**Альтернативы:**
- Docker Hub — отказано в OpenQ #3.
- Per-tenant registries — избыточно для MVP.

**Последствия:**
- ✅ Контроль над тем, какие образы доступны.
- ✅ Безопасность: чужой тенант не зальёт malicious image.
- ⚠️ Один registry на всех — single point of failure. Зеркало — Phase 5+.
- ⚠️ Конфиг lord'а: где хранить TLS key? `/etc/etronium/midaswr-tls.key` с 0600 perms.

---

## 012 — Pre-shared token в gRPC metadata (MVP auth) (2026-07-19)

**Контекст:** Ответ на Open Question #4. Для MVP общий токен хватит.

**Решение:**
- Один pre-shared token на всё Etronium-Scdr развёртывание, прописан в конфиге scheduler'а
  и lord'ов через переменную `ETRONIUM_SHARED_TOKEN`.
- Передаётся в gRPC metadata: `authorization: Bearer <token>`.
- Tenant CLI получает токен из конфига при установке (`~/.etronium/config.yaml`).
- Lord получает токен из env при старте.

**Альтернативы:**
- mTLS — production-grade, но сложнее в setup для MVP.
- Per-tenant tokens — не нужно пока (один тенант = одна установка CLI).
- OAuth/JWT — overkill для MVP.

**Последствия:**
- ✅ Просто: один env var, одна строка в metadata.
- ✅ Достаточно для изоляции scheduler↔lord и tenant↔scheduler.
- ⚠️ Компрометация ключа = компрометация всего. Для MVP ок, для prod — mTLS.
- ⚠️ Нет per-tenant authorization — любой с токеном может submit от любого tenant_id.
  → Phase 2+: tenant_id из metadata сверяется с заявленным в Request.

**Proto:** без изменений, формат хардкодится в middleware.

---

## 013 — Stream lifecycle: kill/restart/replace через ControlTask (2026-07-19)

**Контекст:** Ответ на Open Question #5. Нужно управлять жизненным циклом стримов,
не только live logs. Уточнил: стримы могут умирать, перезапускаться, переезжать.

**Решение:** Отдельный RPC `ControlTask(task_id, action)` с действиями:
- `PAUSE` — `cgroup.freeze=1` + SIGSTOP контейнеру. State сохраняется.
- `RESUME` — обратное.
- `RESTART` — убить контейнер, пересоздать, перезапустить процесс. Тот же `task_id`,
  свежие счётчики ресурсов.
- `RELOCATE` — остановить + вернуть в очередь с новым placement. Полезно если лорд
  деградирует (CPU steal, OOM killer).
- `CANCEL` — алиас `CancelTask` для единого API.

Применяется к parent task — каскадно на всех child'ов при fan-out (`ControlTaskResponse.affected_task_ids`).

**Альтернативы:**
- Расширить `CancelTask` enum'ом — но семантика другая, лучше отдельный RPC.
- WebSocket control channel — отдельный стрим для команд. Сложнее, не нужно в MVP.

**Последствия:**
- ✅ Один endpoint для управления lifecycle.
- ✅ Cascade на fan-out children.
- ⚠️ Relocate требует переигровки placement — увеличивает latency на cancel.
- ⚠️ Pause/Resume требует поддержки `cgroup.freeze` (есть в v2).

**Proto изменения:**
- `ControlAction` enum
- `ControlTaskRequest` / `ControlTaskResponse`
- HTTP: `POST /api/v1/tasks/{task_id}/control`

---

## 014 — Multi-lord и placement с Phase 0 (2026-07-19)

**Контекст:** Ответ на Open Question #6. Решили закладывать динамику под N лордов
с самого начала, а не откладывать в Phase 3.

**Решение:**
- С Phase 0 — scheduler умеет регистрировать нескольких лордов (`LordService.Register`).
- В Phase 0 placement = "первый зарегистрированный лорд со свободными ресурсами"
  (trivially fair). Fan-out (`target_lord_count > 1`) тоже работает, но placement trivial.
- В Phase 3 placement = weighted score `rep × (1-load) × locality` (по research).
- `Lord` уже содержит все нужные поля: `capacity`, `reputation`, `last_seen`, `healthy`.

**Альтернативы:**
- Phase 0 = 1 лорд (как в research) — проще, но придётся переписывать в Phase 3.
- Phase 0 = placement function сразу — over-engineering для "hello world".

**Последствия:**
- ✅ Phase 0 → Phase 3 = эволюция placement, а не переписывание.
- ⚠️ Phase 0 placement тривиальный — может давать плохое распределение при N лордах,
  но это и не важно в Phase 0 (там один тенант с одной задачей).

---

## 015 — Управление scheduler'ом через тот же proto (2026-07-19)

**Контекст:** Ответ на Open Question #8. Proto = и контракт, и admin-интерфейс к scheduler'у.

**Решение:** Отдельного admin API нет. Все операции — через `SchedulerService`:
- `ControlTask` для lifecycle (см. ADR 013).
- `ListLords` для мониторинга.
- `ListTasks` для аудита.
- Для будущего admin (Phase 5+): добавим RPC в тот же сервис с tag'ом `admin`,
  не отдельный proto.

**Альтернативы:**
- Отдельный `AdminService` — можно, но преждевременно.
- REST admin endpoint — дублирование API.

**Последствия:**
- ✅ Один источник правды.
- ⚠️ Если admin-операций станет много — отдельный сервис, но не сейчас.
