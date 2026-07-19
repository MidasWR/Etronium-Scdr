# Roadmap — поэтапный план

> Источник: `docs/RESEARCH.md` (фазы 0–5). Здесь — наш адаптированный план с подфазами.

## Общий объём

~25–35 рабочих дней, 1 разработчик.

---

## Phase 0 — "Hello world" (3–5 дней)

**Цель:** scheduler + 1 lord общаются по gRPC, lord умеет выполнить фиксированную команду через `exec.Command`.

- [ ] Repo структура: `cmd/scheduler/`, `cmd/lord/`, `cmd/etronium/`, `internal/`, `proto/`
- [ ] `proto/v1/etronium.proto` — gRPC контракт (минимум: `Ping`, `RunCommand`)
- [ ] Scheduler: gRPC server, in-memory state, лог результата
- [ ] Lord: gRPC client, `exec.Command` для фиксированной команды
- [ ] Demo: `uptime` команда выполняется на lord, scheduler пишет результат в stdout

**Definition of done:**
```bash
$ ./bin/scheduler &
$ ./bin/lord &
$ ./bin/etronium task run -- cmd="uptime"
# scheduler stdout:
# task=<id> lord=<lord-id> exit=0 stdout=" 23:42:01 up 5 days, ..."
```

---

## Phase 1 — Container-based execution (5–7 дней)

**Цель:** lord тянет OCI image и запускает через containerd.

- [ ] Подключить containerd client к lord
- [ ] Pull alpine image, дефолтный OCI spec
- [ ] Scheduler шлёт "run alpine + command", lord возвращает stdout/stderr
- [ ] Resource limits через cgroups v2 (cpu.max, memory.max)

**Demo:** `etronium task run --image=alpine -- echo hi` — выполнение в контейнере alpine.

---

## Phase 2 — Tenant API (3–5 дней)

**Цель:** `etronium` CLI + gRPC API + live output.

- [ ] `etronium` CLI на cobra
- [ ] gRPC: `SubmitTask`, `GetTask`, `StreamLogs` (bidirectional stream)
- [ ] Tenant видит вывод в реальном времени

**Demo:** `etronium task submit --image=alpine -- sh -c "echo a; sleep 1; echo b"` — live output с двумя строками.

---

## Phase 3 — Multiple lords + placement (5–7 дней)

**Цель:** 3 lord'а, placement function, queue с приоритетами.

- [ ] Подключение нескольких lord'ов
- [ ] Heartbeat (gRPC stream), health monitoring
- [ ] Placement function: filter + weighted score (`rep × (1-load) × locality`)
- [ ] Task queue с приоритетами (heap-based)

**Demo:** 3 lord'а, scheduler распределяет задачи, tenant видит hostname лорда в выводе.

---

## Phase 4 — Reputation + lazy death (3–5 дней)

**Цель:** механизм мягкой смерти + репутация.

- [ ] Lord объявляет lazy death через gRPC
- [ ] Scheduler перестаёт давать новые задачи, ждёт завершения текущих
- [ ] Reputation scoring (latency, success rate, lazy death count)
- [ ] Если lord падает без lazy death → штраф репутации

**Demo:** `lord-1` послал "going down" → новые задачи идут на других, текущая дорабатывает.

---

## Phase 5 — Polish для demo (3–5 дней)

**Цель:** production-ready чувство для ПМ-демо.

- [ ] Pretty CLI output (lipgloss / tint)
- [ ] Metrics endpoint (Prometheus) на scheduler
- [ ] Graceful shutdown
- [ ] Health checks
- [ ] Docker-compose для локального запуска (1 scheduler + 2–3 lord'а)
- [ ] README с инструкцией "запусти за 5 минут"

---

## Что НЕ делаем (anti-patterns)

> Из `docs/RESEARCH.md`, повторяем здесь чтобы не забыть.

- ❌ Патчить ядро. Без kernel modules, без eBPF.
- ❌ Свой container runtime. containerd — production-ready.
- ❌ Свой consensus. Single scheduler, WAL для crash recovery.
- ❌ External DB. In-memory state + WAL.
- ❌ WebUI. CLI хватит.
- ❌ Billing.
- ❌ Tenant auth. Pre-shared token в gRPC metadata.
- ❌ GPU support.
- ❌ Persistent volumes.
