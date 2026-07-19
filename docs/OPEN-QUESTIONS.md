# Open Questions — требуют решения до старта

> Источник: `docs/RESEARCH.md` + `docs/DECISIONS.md`. Здесь — нерешённые вопросы.

## Архитектурные (все решены на 2026-07-20)

| # | Вопрос | Решение | ADR |
|---|---|---|---|
| 1 | 1 tenant : 1 lord vs 1 : N | **1 tenant : N lords** | 009 |
| 2 | Persistent storage | **Local copy** (статика с scheduler'а, динамика — file transfer) | 019 |
| 3 | Image registry | Свой `midaswr` | 011 |
| 4 | Auth | Pre-shared token в gRPC metadata | 012 |
| 5 | Streaming | Стриминг IO с kill/restart через `Migrate`/`Kill` | 017 |
| 6 | Сколько лордов | Multi-lord с Phase 0, placement эволюционирует | 014 |
| 7 | Где запускать | Дизайн не зависит от физики, решим в Phase 5 (compose) | — |
| 8 | Proto контракт | POSIX-like process API, см. `proto/etronium/v1/etronium.proto` | 017 |
| 9 | Containerd | **Не используем** — перешли на cgroups v2 + libcontainer + CRIU | 016 |
| 10 | WAL | Отложено в Phase 5 (Persistence), Phase 0 = pure in-memory | 023 |

## Технические (решаются при старте соответствующей фазы)

### 11. Какой runtime для fork/exec в Phase 0?

**Статус:** 🤔 Phase 0. `os/exec` (просто) или libcontainer (сразу с namespace/cgroup)?
**Рекомендация:** `os/exec` для Phase 0. libcontainer в Phase 1 когда нужны namespaces.

### 12. Как лорд делает I/O capture?

**Статус:** 🤔 Phase 0. `cmd.StdoutPipe` + чтение в горутине? Или ring buffer в памяти и stream по запросу?
**Рекомендация:** ring buffer (8MB default) + stream по запросу через StreamProcessIO.

### 13. CRIU версия и зависимости?

**Статус:** 🤔 Phase 3.
- CRIU ~5MB, есть в стандартных Ubuntu репах (`apt install criu`)
- Нужен ли нам CRIU daemon или CLI mode? CLI проще для MVP.
- Какой процент реальных процессов поддерживается — надо проверять на демо-нагрузке.

### 14. Как измерять memory pressure?

**Статус:** 🤔 Phase 2. По heartbeat'ам (сэмплинг 10s) или PSI pressure stalls из `/proc/pressure/*`?
**Рекомендация:** heartbeat для MVP (проще), PSI в Phase 4.

### 15. Как реализовать file transfer peer-to-peer?

**Статус:** 🤔 Phase 4. Streaming по gRPC, или HTTP с chunked transfer, или просто scp?
**Рекомендация:** gRPC streaming с offset/size для resume. Если lord'ы в одной L2 — напрямую, иначе relay.

### 16. Как лорд узнаёт про invalidation кэша?

**Статус:** 🤔 Phase 4. Push от scheduler при `InvalidateFileCache` или pull от lord'а по TTL?
**Рекомендация:** push (explicit invalidation) + TTL safety net.

### 17. Multi-tenant в одном cgroup tree?

**Статус:** 🤔 Phase 1. Под-папки на каждый tenant внутри lord'а?
**Рекомендация:** да, `/sys/fs/cgroup/etronium/<tenant_id>/<process_id>/`.

### 18. Process group / session для spawn?

**Статус:** 🤔 Phase 0. POSIX требует PGID/SID. Создаём свою session или присоединяем к существующей?
**Рекомендация:** каждый Spawn = новая session (setsid), PGID = PID. Просто и предсказуемо.

### 19. Graceful shutdown lord'а?

**Статус:** 🤔 Phase 5. SIGTERM → drain активных → exit. За сколько секунд drain?
**Рекомендация:** 30s default, configurable.

### 20. Как scheduler восстанавливается после crash (WAL replay)?

**Статус:** 🤔 Phase 5. Если in-memory потерян, lord'ы должны заново зарегистрироваться, tenant'ы — переподключиться. Процессы RUNNING — orphan'ы, можно их re-discover через heartbeat.
**Рекомендация:** при старте scheduler помечает все процессы как UNKNOWN, ждёт heartbeat'ов от lord'ов чтобы понять какие живы. TENANT не замечает ничего (retry на любой RPC).
