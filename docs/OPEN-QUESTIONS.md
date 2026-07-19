# Open Questions — требуют решения до старта

> Источник: `docs/RESEARCH.md`, раздел "Open questions". Здесь — наш трекинг.

## 1. Один лорд = один тенант, или несколько тенантов на одном лорде?

**Рекомендация из research:** для MVP один тенант на лорд.
**Наш статус:** ⏳ принимаем рекомендацию, фиксируем в DECISIONS когда стартуем Phase 3.

## 2. Persistent storage для тенантских файлов?

**Рекомендация из research:** в MVP — нет. Stateless containers.
**Наш статус:** ⏳ принимаем.

## 3. Image registry — свой или публичный?

**Рекомендация из research:** публичный (Docker Hub). Lord делает `containerd.Pull(image)`.
**Наш статус:** ⏳ принимаем, но хотим проверить что containerd из коробки умеет Docker Hub.

## 4. Auth на API — есть или нет?

**Рекомендация из research:** для MVP — простой pre-shared token в gRPC metadata.
**Наш статус:** ⏳ принимаем, но фиксируем формат metadata когда определим proto.

## 5. Streaming vs batch output?

**Рекомендация из research:** streaming через gRPC bidirectional stream. Batch как fallback.
**Наш статус:** ⏳ принимаем, реализуем в Phase 2.

## 6. Какой минимум лордов для осмысленного демо?

**Рекомендация из research:** 3 лорда.
**Наш статус:** ⏳ принимаем для Phase 3+ (когда placement становится интересным). До этого — 1 lord.

## 7. Где запускать lord'ов для dev/demo?

**Варианты из research:**
- 3 VM на одной машине (VirtualBox/UTM)
- 3 контейнера через Docker-in-Docker
- 3 разных физических машины
- 3 процесса на одной машине с разными cgroup scopes

**Наш статус:** 🤔 для Phase 0–2 хватит 1 lord на этой машине. Для Phase 3 решим когда дойдём.

## 8. Какой минимальный proto-контракт для Phase 0?

**Статус:** 🛠 решаем сейчас при старте Phase 0.
- `Ping(scheduler) → Pong(lord)` — heartbeat
- `RunCommand(lord, command) → Result(scheduler)` — выполнение
- `RegisterLord(scheduler) → LordID(lord)` — registration

## 9. Containerd — как ставить и где хранить state?

**Статус:** 🤔 в Phase 1.
- Установка через пакет (`apt install containerd`) или статический бинарь?
- Default state dir `/var/lib/containerd` — ок?
- CRI plugin нужен? Скорее нет.

## 10. WAL формат — какой?

**Статус:** 🤔 в Phase 0.
- Append-only log файл с jsonl записями?
- Или сразу bolt/badgerDB (но это уже "external KV" по anti-patterns)?
- Скорее всего jsonl + replay at startup.
