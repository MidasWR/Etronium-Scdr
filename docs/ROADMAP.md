# Roadmap — Etronium

> **Updated 2026-07-22 01:40**: **v0.3.0 released** — `tenant shell` (interactive TTY relay).
> Tenant terminal acts as if commands run locally, but they actually execute on a lord
> through scheduler. `apt-get`, `ls`, `bash` — full transparent relay.
>
> **v0.2.0** (2026-07-21): autoscale ABS_AUTO planner + flat tenant CLI.
> **v0.1.0** (2026-07-21): one-command installer (`curl | bash -s -- scheduler/lord/tenant`).
> **Sched_ext (F1/F1b) deferred** — все 10 путей BPF struct_ops register EINVAL на нашем
> kernel 7.0.0-28-generic. scx_bpfland 1.1.2 работает на этом ядре — это доказывает что
> kernel готов к sched_ext, но наш custom .o через cilium/bpftool/custom Go — нет.
> Подробности в `Etronium-Scdr-BPF/docs/F1-status.md`. ADR-040.

---

## Phase MVP — "Демо для PM" (это **завтра**)

**Цель:** 5-7 мин PM-демо, показывает что система работает как VPS-абстракция поверх army of lords.

**Что показывается:**
- 1 frontend + 3 lord-providers подключены
- Tenant делает `run` (auto-placement)
- `ps` показывает все процессы как на одной машине
- Failure одного lord'а → auto-recovery, клиент не замечает

**Что НЕ показывается:**
- Live migration процессов (Phase 2)
- eBPF scheduler (Phase 4)
- Multi-tenant SaaS (Phase 3)
- Lord-provider onboarding/anti-fraud (Phase 5)

**Acceptance:**
- [x] `./scripts/mvp/build-image.sh` собирает image без ошибок
- [x] `./scripts/mvp/up.sh -d` поднимает testbed
- [x] `./scripts/mvp/demo.sh` проходит до конца без hangup
- [x] `etronium ps` показывает ≥5 RUNNING процессов до и после failure
- [x] Cleanup через `./scripts/mvp/down.sh` — чистый
- [x] **Verified live 2026-07-21**: 8/8 RUNNING после lord-A kill, recovery <1s, 80s demo

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

## Phase 6 — "Interactive TTY relay" (✅ v0.3.0)

**Done 2026-07-22:** `tenant shell` + `tenant attach --follow`. End-user
can run interactive commands on a lord (`apt-get`, `ls`, `kubectl`,
`vim`) as if they were local.

- [x] `tenant shell --shell=/bin/sh` — TTY raw mode + stdin/stdout/stderr
      relay through scheduler via `WriteStdin` RPC + live `StreamProcessIO` follow
- [x] `tenant attach [--follow] <pid>` — kubectl-attach analog for dump / stream
- [x] Ctrl-D → graceful EOF → process exits normally
- [x] Live follow polling: 100ms, exits on `entry.ExitedChan()`

**Not done (Phase 2+ scope):**
- [ ] PID 1 hijack (etronium-init as container PID 1, full pty replacement) — ~1-2 weeks
- [ ] LD_PRELOAD + RPC relay for ELF binaries — ~1-2 months
- [ ] Multi-line TUI optimization (vim, htop, less with full winch/screen refresh)
- [ ] Shell history sync across sessions
- [ ] `etronium logs` (central log tail per tenant/process)
- [ ] File pull/push (S3-style on lord)

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
- **Phase 6** (✅ 2026-07-22, commit b4885f0) — interactive TTY relay (tenant shell)
- **v0.1.0** (✅ 2026-07-21) — one-command installer
- **v0.2.0** (✅ 2026-07-21, commit 6aed01b) — autoscale ABS_AUTO + flat CLI
- **v0.3.0** (✅ 2026-07-22, commit b4885f0) — `tenant shell` + `tenant attach`

Текущий focus — **v0.4.0** (TBD: log tail + file pull/push + history sync).
