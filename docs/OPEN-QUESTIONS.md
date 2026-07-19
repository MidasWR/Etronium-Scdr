# Open Questions — требуют решения до старта

> Источник: `docs/RESEARCH.md`, раздел "Open questions". Здесь — наш трекинг.
> После ответа на вопрос — статус ✅ и ссылка на решение в DECISIONS.

---

## 1. Один лорд = один тенант, или несколько тенантов на одном лорде?

**Рекомендация из research:** для MVP один тенант на лорд.
**Наш статус:** ✅ **РЕШЕНО 2026-07-19.** Tenant → N lords (fan-out), не lord → N tenants.
См. ADR 009 — `1 tenant : N lords` placement.

## 2. Persistent storage для тенантских файлов?

**Рекомендация из research:** в MVP — нет. Stateless containers.
**Наш статус:** ✅ **РЕШЕНО 2026-07-19.**
Основа (статика) — на stateless сервере scheduler'а, отдаётся через volume mount в lord'а.
Динамика — защищённая DFS, доступ по pre-shared ключу.
См. ADR 010 + `VolumeMount` в proto.

## 3. Image registry — свой или публичный?

**Рекомендация из research:** публичный (Docker Hub). Lord делает `containerd.Pull(image)`.
**Наш статус:** ✅ **РЕШЕНО 2026-07-19.** Свой — `midaswr`, ключ на машине есть.
См. ADR 011 — приватный registry.

## 4. Auth на API — есть или нет?

**Рекомендация из research:** для MVP — простой pre-shared token в gRPC metadata.
**Наш статус:** ✅ **РЕШЕНО 2026-07-19.** MVP — общий токен хватит.
См. ADR 012 — формат pre-shared token.

## 5. Streaming vs batch output?

**Рекомендация из research:** streaming через gRPC bidirectional stream. Batch как fallback.
**Наш статус:** ✅ **РЕШЕНО 2026-07-19.** Стриминг батчей с kill/restart/replace.
См. ADR 013 + `ControlTask` RPC.

## 6. Какой минимум лордов для осмысленного демо?

**Рекомендация из research:** 3 лорда.
**Наш статус:** ✅ **РЕШЕНО 2026-07-19.** Сразу закладываем динамику под N.
Multi-lord и placement — с Phase 0, не Phase 3.
См. ADR 014.

## 7. Где запускать lord'ов для dev/demo?

**Варианты из research:**
- 3 VM на одной машине (VirtualBox/UTM)
- 3 контейнера через Docker-in-Docker
- 3 разных физических машины
- 3 процесса на одной машине с разными cgroup scopes

**Наш статус:** ✅ **РЕШЕНО 2026-07-19.** Дизайн не зависит от физики, решим по месту в Phase 5 (compose).

## 8. Какой минимальный proto-контракт для Phase 0?

**Статус:** ✅ **РЕШЕНО 2026-07-19.** См. `proto/etronium/v1/etronium.proto` и `docs/PROTO.md`.
- В proto определён полный контракт на все фазы.
- Phase 0 будет реализовывать подмножество: `Register`, `Heartbeat`, `SubmitTask`, `GetTask`, `ListTasks` (unary).
- Streaming RPC (`RunTask`, `StreamTask`) — Phase 1+.
- Основа общения и интерфейс управления scheduler'ом.

## 9. Containerd — как ставить и где хранить state?

**Статус:** ⏳ **ОТЛОЖЕНО.** Не блокирует Phase 0. Решим в Phase 1 когда дойдём до runtime.

## 10. WAL формат — какой?

**Статус:** ⏳ **ОТЛОЖЕНО.** Не блокирует Phase 0. Решим перед стартом Phase 0.
