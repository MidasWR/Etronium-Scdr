# Etronium — MVP Demo (Phase 1.0)

> "VPS-абстракция поверх army of resource providers (lords)."
> Клиент видит одну машину. Под капотом — гетерогенная инфра.

## Что это (на 1 минуту)

```
                  [Tenant / B2B клиент]
                          │
                          │ SSH/API
                          ▼
              [Stateful Front-end VPS]
                iptables + process table
                          │
                          ▼
                    [Scheduler]
                  placement + recovery
                          │
        ┌─────────────────┼─────────────────┐
        ▼                 ▼                 ▼
   [Lord-school-A] [Lord-school-B] [Lord-school-C]
   (ноут школьника) (ноут школьника) (арендованный)
```

- **Tenant** — B2B клиент, покупает VPS-абстракцию.
- **Stateful Front-end** — на VPS хостера, держит "vps-view" клиента. iptables + process table.
- **Scheduler** — placement & recovery decisions (живёт в frontend).
- **Lord** — daemon у resource-provider'а (школьник/энтузиаст). Контейнер drop-in.

## Ключевая инвариантность

| Кто | Что от чего не зависит |
|---|---|
| Tenant | - от того где живут его процессы (lord-A или lord-B)<br>- от того что lord умер/включился<br>- от обновлений нашей системы |
| Lord-provider | - от того сколько клиентов подключено<br>- от обновлений нашей системы<br>- от того какие у других lord'ов нагрузки |

## Что показывается на demo (5-7 мин)

1. **Topology** — 1 frontend + 3 lord'а (представляющие разных providers).
2. **Spawn** — tenant делает 5 процессов, scheduler auto-placement.
3. **ps** — клиент видит все 5 как на одной машине (даже если они на разных lord'ах).
4. **Failure** — выключаем один lord (имитация того что школьник выключил ноут).
5. **Recovery** — через 35s все процессы живы на других lord'ах.
6. **VPS-view** — клиент ничего не заметил, VPS "работает".

## Быстрый старт (5 мин setup)

```bash
# 1. Собрать MVP image и binaries
./scripts/mvp/build-image.sh

# 2. Поднять testbed (detached)
./scripts/mvp/up.sh -d

# 3. Demo (foreground, 5-7 мин)
./scripts/mvp/demo.sh

# 4. Tear down
./scripts/mvp/down.sh
```

## Что НЕ входит в MVP (отложено в Phase 2+)

- ❌ **Live migration процессов** — клиент пока видит respawn-эффект, не миграцию. Phase 2.
- ❌ **Sched_ext eBPF** — пока placement без eBPF, через cgroup v2 + kernel CFS. Phase 4.
- ❌ **Multi-tenant SaaS pricing/billing** — один tenant в MVP. Phase 3.
- ❌ **Lord-provider onboarding/security** — drop-in, но без reputation/anti-fraud. Phase 3.
- ❌ **Cyber / anti-fraud / sandboxing** — Phase 5.

## Известные race-conditions (по mvp test)

Из 10 e2e сценариев chaos-runner'а:
- **8 из 11 PASS** — hello-world, lord-lag, spawn-storm, cgroup-OOM, zombies, cold-start, k8s-sidecar, final-state.
- **2 знаем что fail**: S04 (net partition race в recovery), S10 (slow lord reconnect).
- Ожидаемый результат MVP: **8-9/10 e2e scenarios проходят** — это достаточно для demo.

Детальный статус — `docs/ROADMAP.md` (Phase MVP).

## Структура

```
Etronium-Scdr/
├── cmd/
│   ├── scheduler/         # = "frontend" в новом naming
│   ├── lord/              # = resource provider daemon
│   └── etronium/          # = tenant CLI (= binary `tenant` через alias)
├── proto/etronium/v1/     # контракт
├── internal/
│   ├── gen/               # сгенерированный Go
│   ├── scheduler/         # placement, recovery, process_table
│   └── lord/              # cgroup, exec, bidi stream
├── test/
│   ├── mvp/               # ← MVP testbed (ЭТО ДЛЯ ДЕМО)
│   ├── e2e_phase{0,1,2}.sh
│   └── chaos/             # chaos testbed (Phase 2+, не demo)
├── scripts/
│   ├── mvp/               # ← MVP scripts (build-image/up/down/demo)
│   ├── chaos-runner/      # (Phase 2+)
│   └── demo-pm.sh         # (старое, можно удалить)
└── docs/
    └── ROADMAP.md         # обновлён под MVP
```

## Demo script detail

`scripts/mvp/demo.sh` подробно:
- 0. sanity: `etronium lords` показывает 3 lord'ов
- 1. Topology description (log output)
- 2. Spawn 5 sleep-процессов через `etronium process spawn`
- 3. `etronium process list` показывает где они живут
- 4. `docker stop mvp-lord-01` (имитация выключения ноута)
- 5. Ждём 35s (recovery TTL)
- 6. `etronium process list` снова — должны все 5 быть RUNNING, но на других lord'ах

## Build / Run

```bash
# Требования: Go 1.22+, Docker 24+, cgroup v2 на хосте
go version
docker --version

# Если нет — билд скрипт скажет
./scripts/mvp/build-image.sh
```
