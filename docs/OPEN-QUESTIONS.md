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

**Статус:** ✅ Решено. `os/exec` в Phase 0/1/2. libcontainer не используется (cgroups v2 пишем напрямую). См. ADR 016.

### 12. Как лорд делает I/O capture?

**Статус:** ✅ Решено в Phase 0. `cmd.StdoutPipe` + ring buffer (8MB default), stream по запросу через StreamProcessIO. См. `internal/lord/exec.go`.

### 13. CRIU версия и зависимости?

**Статус:** ✅ Решено (ADR 024). CLI mode, CRIU 3.x из `apt install criu`, требует CAP_CHECKPOINT_RESTORE. См. ADR 024 для cgroup-нюансов.

### 14. Как измерять memory pressure?

**Статус:** ✅ Частично решено (ADR 025). Phase 3.0 — без auto-pressure, только explicit migrate.
Phase 3.2 — memory pressure через heartbeat'ы (MemUsed/AdvertisedMem > 0.85). PSI pressure stalls остаётся в Phase 4 как enhancement.

### 15. Как реализовать file transfer peer-to-peer?

**Статус:** ✅ Phase 3 checkpoint transfer решено (ADR 026) — relay через scheduler,
gRPC streaming с backpressure. Полный file API (PullFile/PushFile) — в Phase 4.

### 16. Как лорд узнаёт про invalidation кэша?

**Статус:** 🤔 Phase 4. Push от scheduler при `InvalidateFileCache` или pull от lord'а по TTL?
**Рекомендация:** push (explicit invalidation) + TTL safety net.

### 17. Multi-tenant в одном cgroup tree?

**Статус:** ✅ Решено в Phase 1: cgroup slice `/sys/fs/cgroup/etronium/<lord_id>/<process_id>/`
(без tenant_id в пути — tenant_id в metadata, cgroup delegation просто). Если multi-tenant на
lord'е станет реальной нагрузкой, добавим `<tenant_id>/` позже.

### 18. Process group / session для spawn?

**Статус:** ✅ Решено в Phase 0: `SysProcAttr.Setpgid = true` в `internal/lord/exec.go`,
PGID = PID.

### 19. Graceful shutdown lord'а?

**Статус:** 🤔 Phase 5. SIGTERM → drain активных → exit. За сколько секунд drain?
**Рекомендация:** 30s default, configurable.

### 20. Как scheduler восстанавливается после crash (WAL replay)?

**Статус:** 🤔 Phase 5. Если in-memory потерян, lord'ы должны заново зарегистрироваться, tenant'ы — переподключиться. Процессы RUNNING — orphan'ы, можно их re-discover через heartbeat.
**Рекомендация:** при старте scheduler помечает все процессы как UNKNOWN, ждёт heartbeat'ов от lord'ов чтобы понять какие живы. TENANT не замечает ничего (retry на любой RPC).
