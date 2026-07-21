# Roadmap — Etronium

> **Phase MVP** = demo завтра. Phase 2+ = production-grade.
> MVP редизайн (jul-2026): отказались от варианта A (PRU 6.12+ timeline) ради quick-wins.

---

## Phase MVP — "Демо для PM" (это **завтра**)

**Цель:** 5-7 мин PM-демо, показывает что система работает как VPS-абстракция поверх army of lords.

**Что показывается:**
- 1 frontend + 3 lord-providers подключены
- Tenant делает `process spawn` (auto-placement)
- `process list` показывает все процессы как на одной машине
- Failure одного lord'а → auto-recovery, клиент не замечает

**Что НЕ показывается:**
- Live migration процессов (Phase 2)
- eBPF scheduler (Phase 4)
- Multi-tenant SaaS (Phase 3)
- Lord-provider onboarding/anti-fraud (Phase 5)

**Acceptance:**
- [ ] `./scripts/mvp/build-image.sh` собирает image без ошибок
- [ ] `./scripts/mvp/up.sh -d` поднимает testbed
- [ ] `./scripts/mvp/demo.sh` проходит до конца без hangup
- [ ] `etronium process list` показывает ≥5 RUNNING процессов до и после failure
- [ ] Cleanup через `./scripts/mvp/down.sh` — чистый

**Структура:**
```
test/mvp/
├── docker-compose.yml     # 1 frontend + 3 lords + tenant
├── Dockerfile.runtime     # контейнер с binaries
└── README.md              # этот файл-инструкция

scripts/mvp/
├── build-image.sh
├── up.sh
├── down.sh
└── demo.sh
```

**Test result expectation:** 8-9/10 e2e scenarios pass на demo (из старого chaos-runner 11 — S04/S10 race conditions отложены).

---

## Phase 2 — "Production-ready basic" (после demo)

**Цель:** закрыть race conditions, добавить live migration опционально.

- [ ] Phase MVP + S04 fix (PickWithSession retry + debounce tuning)
- [ ] Phase MVP + S10 fix (lord reconnect poll, не sleep)
- [ ] Live migration через sched_ext + CRIU (отложено из-за kernel 7.0 hostility, но пробуем v2)
- [ ] Auto-scale: больше lord-providers auto-register

---

## Phase 3 — "Multi-tenant SaaS" (после Phase 2)

- [ ] Multi-tenant isolation (cgroup per-tenant)
- [ ] Lord-provider onboarding flow (signup, resource advertise, payout)
- [ ] Reputation/anti-fraud (multi-lord consensus for trust)
- [ ] Pricing/billing (per-CPU-hour, per-GB-RAM-hour)
- [ ] Client-side SSH/API: каждый tenant = свой VPS-view, изолированные

---

## Phase 4 — "Sched_ext eBPF scheduler" (долгосрок)

- [ ] BPF scheduler на Linux ≥ 6.12 (твоё ядро 7.0 уже подходит)
- [ ] Fine-grained CPU placement per-tenant
- [ ] CPU-affinity через sched_setaffinity
- [ ] NUMA-aware через libnuma distance map
- [ ] Замена kernel CFS scheduler на user-space'овый для наших tenant'ов

---

## Phase 5 — "Cyber/anti-fraud" (security)

- [ ] Multi-lord consensus для billing
- [ ] Reputation система для lord-providers
- [ ] Watchdog на frontend (consistency checks)
- [ ] Sandbox для tenant кода (cgroup + capability drops)
- [ ] Resource limit enforcement at runtime

---

## Out of scope (для будущих major)

- ❌ Kernel patches для host'а (Win/Mac не должны страдать)
- ❌ Replacement init (systemd replacement)
- ❌ Мulti-cluster federation
- ❌ Persistent state (WAL) → заменено на in-memory + recovery

## Reference: историческое

- **Phase 0** (✅ 2026-07-20, commit d7591a6) — hello world
- **Phase 1** (✅ 2026-07-20, commit ce9fea1) — cgroup v2 isolation
- **Phase 2** (✅ 2026-07-20, commit 5ef4376) — multi-lord weighted placement
- **Phase 3.0** (✅ затем отменён 7d66b82) — CRIU live migration (kernel 6.17 hostile)
- **Phase 3.4** (✅ 3f00ea6) — recovery + V5 state dump
- **Phase 5** (✅ bfbd474) — WAL + graceful shutdown
- **Chaos testbed** (✅ 8045d12 — 95f0391) — 11 scenario chaos, 8/11 pass

Текущий focus — **Phase MVP** для demo.
