# Open Questions — требуют решения до старта

> Источник: `docs/RESEARCH.md`, раздел "Open questions". Здесь — наш трекинг.

## 1. Один лорд = один тенант, или несколько тенантов на одном лорде?

**Рекомендация из research:** для MVP один тенант на лорд.
**Наш статус:** ⏳ принимаем рекомендацию, фиксируем в DECISIONS когда стартуем Phase 3.
ответ: 1 тенант : n лордов
## 2. Persistent storage для тенантских файлов?

**Рекомендация из research:** в MVP — нет. Stateless containers.
**Наш статус:** ⏳ принимаем.
Основа статика на стейтлесс сервере, и динамика защено DFS
## 3. Image registry — свой или публичный?

**Рекомендация из research:** публичный (Docker Hub). Lord делает `containerd.Pull(image)`.
**Наш статус:** ⏳ принимаем, но хотим проверить что containerd из коробки умеет Docker Hub.
свой, midaswr регистр, ключ на машине есть
## 4. Auth на API — есть или нет?

**Рекомендация из research:** для MVP — простой pre-shared token в gRPC metadata.
**Наш статус:** ⏳ принимаем, но фиксируем формат metadata когда определим proto.
для мвп да общий токен хватит
## 5. Streaming vs batch output?

**Рекомендация из research:** streaming через gRPC bidirectional stream. Batch как fallback.
**Наш статус:** ⏳ принимаем, реализуем в Phase 2.
стриминг баьчей с убийством стримов и обновлений их
## 6. Какой минимум лордов для осмысленного демо?

**Рекомендация из research:** 3 лорда.
**Наш статус:** ⏳ принимаем для Phase 3+ (когда placement становится интересным). До этого — 1 lord.
сразу закладываем динамику под n 
## 7. Где запускать lord'ов для dev/demo?

**Варианты из research:**
- 3 VM на одной машине (VirtualBox/UTM)
- 3 контейнера через Docker-in-Docker
- 3 разных физических машины
- 3 процесса на одной машине с разными cgroup scopes

**Наш статус:** 🤔 для Phase 0–2 хватит 1 lord на этой машине. Для Phase 3 решим когда дойдём.
разберемся, мы работаем под динамику по этому это не важно 
## 8. Какой минимальный proto-контракт для Phase 0?

**Статус:** ✅ **РЕШЕНО 2026-07-19.** См. `proto/etronium/v1/etronium.proto` и `docs/PROTO.md`.
- В proto определён полный контракт на все фазы.
- Phase 0 будет реализовывать подмножество: `Register`, `Heartbeat`, `SubmitTask`, `GetTask`, `ListTasks` (unary).
- Streaming RPC (`RunTask`, `StreamTask`) — Phase 1+.
основа для общений и интерфейс управления скедулером
## 9. Containerd — как ставить и где хранить state?

**Статус:** 🤔 в Phase 1.
- Установка через пакет (`apt install containerd`) или статический бинарь?
- Default state dir `/var/lib/containerd` — ок?
- CRI plugin нужен? Скорее нет.
да не важно
## 10. WAL формат — какой?

**Статус:** 🤔 в Phase 0.
- Append-only log файл с jsonl записями?
- Или сразу bolt/badgerDB (но это уже "external KV" по anti-patterns)?
- Скорее всего jsonl + replay at startup.
тоже не думаю что важно