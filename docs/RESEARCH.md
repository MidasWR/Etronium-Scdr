# Etronium MVP — Исследование по минимальной реализации

> Цель: дать работающее демо концепции "single runtime поверх распределённого железа"
> Стек: Go (MVP) → Rust/C++ для hot path позже
> Дата: 2026-07-19

---

## TL;DR — 7 ключевых решений

| # | Решение | Обоснование |
|---|---------|-------------|
| 1 | Scheduler — single binary, in-memory state + WAL | HA/Raft это Phase 2+, для демо over-engineering |
| 2 | Container runtime = **containerd** (не runc напрямую) | Баланс контроля и готовых абстракций. rурц — слишком низкий уровень, Docker — слишком высокий |
| 3 | gRPC для scheduler↔lord и tenant↔scheduler | Контракт, streaming, codegen. Без message bus для MVP |
| 4 | Один тенант = одна сессия на одном лорде (для MVP) | Избегает многопользовательского шума на стороне лорда |
| 5 | Placement = weighted score: `rep × (1 - load) × locality` | Просто, детерминировано, легко расширять |
| 6 | OCI-совместимые контейнеры (Docker images) | Не изобретаем формат, пользуемся готовой инфраструктурой |
| 7 | Linux-only для MVP | macOS/Windows lords — Phase 3+ (через Lima/WSL2) |

---

## Reference projects — что изучать в первую очередь

### Tier 1 (must read, прямой relevance)

**HashiCorp Nomad** (Go, ~40k LOC scheduler)
Почему: ближайший архитектурный аналог, написан на Go, отличная документация, open source.
Что изучать:
- `nomad/scheduler/scheduler.go` — placement logic
- `nomad/scheduler/feasible.go` — filter phase
- `nomad/eval_broker.go` — очередь evaluations
- `nomad/state/` — in-memory state store pattern (хотя memdb — overkill для MVP)
- `client/allocrunner/taskrunner/` — как task живёт на клиенте
- `drivers/raw_exec/`, `drivers/exec/` — простейшие task drivers

Что НЕ копировать: Raft consensus, Consul integration, ACL system, Vault integration — это всё enterprise features, не нужны для MVP.

**containerd** (Go, ~150k LOC, но модульно)
Почему: золотая середина между runc и Docker. Используется K8s, production-ready.
Что изучать:
- Архитектура shim — как изолировать runtime от containerd daemon
- CRI plugin — если захочешь K8s-compatible API
- Snapshotter — как делать overlayfs для rootfs

**runc + libcontainer** (Go, ~20k LOC)
Почему: низкий уровень container execution. Полезно понять что под капотом, но напрямую использовать не надо.

### Tier 2 (для понимания domain'а)

**Fly.io FlyD** (Go, закрытый) + публичные посты
- Decentralized orchestrator без центрального scheduler
- Fly Machines = Firecracker microVMs
- Pilot = init process внутри VM
- Урок: scheduler может быть распределённым, не обязательно одна большая нода

**BOINC** (C++, открытый)
- Золотой стандарт volunteer computing
- HTTP RPC между client и server (вместо gRPC)
- Replication для verification результатов
- Урок: как работать с гетерогенными и ненадёжными донорами

### Tier 3 (концептуально)

- **Mesos** — offers/resources abstraction, two-level scheduling
- **Borg/Omega** (Google, по статьям) — placement, optimistic concurrency
- **Sparrow** (Berkeley) — power of two choices для batch
- **Ray** — distributed actor system, хорошие паттерны для task lifecycle

---

## Минимальная архитектура (MVP)

### Компоненты

```
┌─────────────────┐                ┌──────────────────┐
│  Tenant CLI     │  gRPC/REST     │  Scheduler (VPS) │
│  (etronium CLI) │ ─────────────► │  single binary   │
└─────────────────┘                │                  │
                                   │  • state store   │
                                   │  • placement     │
                                   │  • task queue    │
                                   │  • lord registry │
                                   └──────────────────┘
                                            │
                                            │ gRPC (bidi streaming)
                                            ▼
                                   ┌──────────────────┐
                                   │  Lord (donor)    │
                                   │  single binary   │
                                   │                  │
                                   │  • containerd    │
                                   │  • cgroups v2    │
                                   │  • task executor │
                                   └──────────────────┘
```

### Scheduler — внутренняя структура

```
internal/scheduler/
├── server.go           # gRPC server, API endpoints
├── state/
│   ├── store.go        # in-memory map + WAL
│   ├── task.go         # task state machine
│   ├── lord.go         # lord registry, health
│   └── reputation.go   # scoring
├── placement/
│   ├── filter.go       # feasibility check
│   ├── score.go        # ranking
│   └── planner.go      # orchestrator
├── queue/
│   └── queue.go        # task queue (heap-based)
├── api/
│   ├── v1.proto        # gRPC definitions
│   └── handlers.go
└── metrics/            # prometheus
```

### Lord — внутренняя структура

```
internal/lord/
├── agent.go            # main loop, heartbeat
├── runtime/
│   ├── containerd.go   # containerd client wrapper
│   ├── oci.go          # OCI spec builder
│   └── cgroups.go      # cgroups v2 wrapper
├── executor/
│   └── task.go         # task lifecycle (start, stream, finish)
├── rpc/
│   └── client.go       # gRPC client to scheduler
└── resources/
    └── monitor.go      # CPU, RAM, IO usage
```

### Демо-флоу (что должно работать end-to-end)

1. `etronium task submit --image=alpine -- echo "hello from lord"`
2. CLI шлёт gRPC `SubmitTask` к scheduler
3. Scheduler выбирает лорда через placement function
4. Scheduler шлёт `RunTask` лорду по gRPC stream
5. Lord через containerd запускает контейнер с alpine + команду
6. stdout/stderr стримятся обратно через gRPC stream
7. По завершении лорд отдаёт exit code + resource usage
8. Scheduler помечает task completed, тенант видит результат
9. **WOW-moment:** тенант видит в логе "hello from lord-XXXXX", где XXXXX — hostname лорда, и понимает, что его код реально выполнился на чужой машине

---

## Go-стек — конкретные библиотеки

### RPC и сериализация
- `google.golang.org/grpc` — gRPC
- `google.golang.org/protobuf` — protobuf runtime
- `google.golang.org/grpc/stream` — bidirectional streaming

### CLI
- `github.com/spf13/cobra` — CLI framework (для всех 3 бинарей)
- `github.com/spf13/viper` — конфиг (если нужна иерархия)
- Для простого случая можно обойтись `flag` + JSON

### Container / runtime
- `github.com/containerd/cgroups/v3` — cgroups v2 управление
- `github.com/containerd/containerd/v2` — container runtime client
- `github.com/opencontainers/runtime-spec/specs-go` — OCI spec типы
- `github.com/opencontainers/go-digest` — image digests
- `github.com/opencontainers/image-spec` — image manifest types
- `github.com/containerd/continuity/fs` — fs utilities

### Утилиты
- `github.com/oklog/ulid/v2` — task IDs (sortable, k8s-style)
- `github.com/google/uuid` — UUID v4 для session IDs
- `go.uber.org/zap` — structured logging
- `github.com/prometheus/client_golang` — метрики
- `github.com/lmittmann/tint` — pretty colored logs для dev
- `github.com/stretchr/testify` — тесты

### Что НЕ использовать для MVP
- ❌ `github.com/hashicorp/raft` — Raft, не нужен
- ❌ `github.com/etcd-io/etcd` — external KV, не нужен
- ❌ `github.com/nats-io/nats.go` — message bus, не нужен
- ❌ `github.com/temporalio/sdk-go` — workflow engine, overkill
- ❌ Любые ORM, DB драйверы — нет БД в MVP

---

## Placement function (MVP версия)

```go
// Внутренний pseudocode, не финальный API
type Lord struct {
    ID           string
    Healthy      bool
    Capacity     Resources
    InUse        Resources
    Reputation   float64  // 0.0 - 1.0
    LastUsedFor  map[string]time.Time  // tenantID -> time
    AvgLatency   time.Duration
}

func (p *Planner) Select(task Task, lords []*Lord) *Lord {
    candidates := filter(lords, task)        // 1. Filter
    if len(candidates) == 0 { return nil }
    return best(candidates, task)            // 2. Score
}

func filter(lords []*Lord, task Task) []*Lord {
    var ok []*Lord
    for _, l := range lords {
        if !l.Healthy { continue }
        if !fits(l.Capacity.Sub(l.InUse), task.Resources) { continue }
        if !l.OS.Compatible(task.RequiredOS) { continue }
        ok = append(ok, l)
    }
    return ok
}

func best(lords []*Lord, task Task) *Lord {
    bestScore := -1.0
    var best *Lord
    for _, l := range lords {
        score := score(l, task)
        if score > bestScore {
            bestScore = score
            best = l
        }
    }
    return best
}

func score(l *Lord, task Task) float64 {
    // Простая формула, расширяемая
    loadFactor := 1.0 - float64(l.InUse.CPU) / float64(l.Capacity.CPU)
    locality := localityBoost(l, task.TenantID)  // 1.0 если недавно использовался, 0.5 иначе
    return l.Reputation * loadFactor * locality
}
```

Это **не** production-grade, но достаточно для демо. Расширения:
- Добавить jitter для разных лордов с одинаковым score (предотвращение "thundering herd")
- Добавить географическую близость (когда будет telemetry)
- Добавить специализацию (compute-heavy → лорды с GPU, memory-heavy → с RAM)

---

## Phased implementation roadmap

### Phase 0: "Hello world" (~3-5 дней)
- [ ] Repo structure, базовые Go modules
- [ ] Один бинарь scheduler, один бинарь lord
- [ ] gRPC ping между ними
- [ ] Lord может запустить фиксированную команду через `exec.Command`
- [ ] Scheduler видит результат в логе
**Demo:** scheduler + 1 lord, `uptime` команда, вывод в stdout scheduler'а

### Phase 1: Container-based execution (~5-7 дней)
- [ ] Подключить containerd к lord
- [ ] Pull alpine image, run с дефолтным OCI spec
- [ ] Scheduler шлёт "run alpine + command", lord возвращает stdout/stderr
**Demo:** `docker run alpine echo hi` через Etronium

### Phase 2: Tenant API (~3-5 дней)
- [ ] `etronium` CLI
- [ ] gRPC API: SubmitTask, GetTask, StreamLogs
- [ ] Tenant видит task в реальном времени
**Demo:** `etronium task submit --image=alpine -- sh -c "echo a; sleep 1; echo b"` с live output

### Phase 3: Multiple lords + placement (~5-7 дней)
- [ ] Подключение нескольких lord'ов
- [ ] Heartbeat, health monitoring
- [ ] Placement function (filter + score)
- [ ] Task queue с приоритетами
**Demo:** 3 lord'а, scheduler распределяет задачи, tenant видит на каком лорде выполнилось

### Phase 4: Reputation + lazy death (~3-5 дней)
- [ ] Lord объявляет lazy death через gRPC
- [ ] Scheduler перестаёт давать новые задачи, ждёт завершения
- [ ] Reputation scoring (latency, success rate, lazy death count)
- [ ] Если lord падает без lazy death → штраф репутации
**Demo:** `lord-1` послал "going down" → новые задачи идут на других, текущая дорабатывает

### Phase 5: Polish для demo (~3-5 дней)
- [ ] Pretty CLI output
- [ ] Metrics endpoint (Prometheus)
- [ ] Graceful shutdown
- [ ] Health checks
- [ ] Docker-compose для локального запуска
- [ ] README с инструкцией "запусти за 5 минут"
**Demo:** полноценный скрипт, который поднимает 1 scheduler + 2-3 lord'а и прогоняет сценарий

**Итого MVP: ~25-35 рабочих дней** (1 разработчик, без отвлечений)

---

## Демо-сценарий для PM / первых пользователей

**Setup (5 минут):**
1. На VPS запускается `etronium-scheduler`
2. На 2-3 донорах (можно VMs на той же машине) запускается `etronium-lord`
3. Scheduler показывает "3 lord'а connected, pool ready"

**Сценарий (5 минут):**
1. PM вводит: `etronium task submit --image=alpine --name=demo -- sh -c "echo Started on \$(hostname); for i in 1 2 3 4 5; do echo working \$i; sleep 1; done; echo Done"`
2. PM видит live output: "Started on lord-MacBook-Pro.local", потом "working 1", "working 2", ...
3. PM вводит: `etronium lords list` — видит нагрузку, репутацию, сколько задач обработал каждый
4. PM вводит: `etronium task submit --image=alpine --parallel=5 -- echo "concurrent test"`
5. PM видит что 5 задач распределились по разным лордам (в выводе видны разные hostname)
6. PM останавливает один lord через Ctrl+C — следующая задача уходит на оставшиеся
7. PM: "вот так это работает, ваши задачи реально крутятся на чужих машинах, и вам не надо об этом думать"

**Что НЕ показывать на этом демо:**
- Internal architecture, gRPC streams, containerd
- Миграцию задач между лордами (Phase 4+)
- Speculative batching (Phase 6+)
- Cross-OS lords (Phase 8+)

---

## Open questions (требуют решения до старта)

### 1. Один лорд = один тенант, или несколько тенантов на одном лорде?
**Рекомендация:** для MVP один тенант на лорд. Избегает шума с изоляцией между тенантами, проще тестировать. На проде — мульти-тенант через cgroups (разные tenant_id = разные cgroup namespaces).

### 2. Persistent storage для тенантских файлов?
**Рекомендация:** в MVP — нет. Контейнер запускается с image rootfs, результаты возвращаются через stdout/files API. Если тенанту нужны persistent files — это Phase 2+ с overlayfs + sync на VPS.

### 3. Image registry — свой или публичный?
**Рекомендация:** использовать публичный (Docker Hub). Lord делает `containerd.Pull(image)`. Сложность с auth и private registry — потом.

### 4. Auth на API — есть или нет?
**Рекомендация:** для MVP — простой pre-shared token в gRPC metadata. Не user-facing auth, а service-to-service.

### 5. Streaming vs batch output?
**Рекомендация:** streaming через gRPC bidirectional stream. Tenant видит output по мере появления. Batch (дождаться завершения, отдать всё разом) — это fallback для клиентов без streaming support.

### 6. Какой минимум лордов для осмысленного демо?
**Рекомендация:** 3 лорда. Один — банально мало, не показывает распределение. Два — ок, но скучно. Три — placement function становится интересной.

### 7. Где запускать lord'ов для dev/demo?
**Варианты:**
- 3 VM на одной машине (VirtualBox/UTM)
- 3 контейнера через Docker-in-Docker (некрасиво, но быстро)
- 3 разных физических машины (идеально, но не всегда доступно)
- 3 процесса на одной машине с разными cgroup scopes (хак, но работает для smoke test)

---

## Что НЕ делать в MVP (anti-patterns)

- ❌ **Не патчить ядро.** Без `kernel modules`, без `eBPF`, без `/dev/etrX`. Всё через стандартные API: cgroups v2, namespaces, seccomp (через containerd).
- ❌ **Не делать свой container runtime.** containerd уже есть, он production-ready.
- ❌ **Не изобретать consensus.** Один scheduler, WAL для crash recovery, без Raft.
- ❌ **Не делать external DB.** In-memory state + лог-файл. Если упадёт scheduler — последние N секунд state теряются, но lord'ы переподключатся.
- ❌ **Не делать WebUI.** CLI хватит для демо. WebUI — это отдельный проект.
- ❌ **Не делать billing.** Потом. Для MVP "время работы лорда" логируется, но не считаются деньги.
- ❌ **Не делать tenant auth.** Pre-shared token. Real auth — Phase 2+.
- ❌ **Не делать GPU support.** Containerd + nvidia-container-toolkit — это отдельный зоопарк, не для MVP.
- ❌ **Не делать persistent volumes.** Stateless containers. Если надо — overlayfs на VPS, sync при lazy death.

---

## Sources

Изученные проекты:
- HashiCorp Nomad: https://github.com/hashicorp/nomad (architecture docs, scheduler package)
- containerd: https://github.com/containerd/containerd
- containerd/cgroups: https://github.com/containerd/cgroups
- opencontainers/runc: https://github.com/opencontainers/runc
- fly.io architecture: https://fly.io/docs/reference/architecture/
- BOINC paper: https://arxiv.org/pdf/1903.01699.pdf
- k8s scheduler scoring: https://kubernetes.io/docs/concepts/scheduling-eviction/resource-bin-packing/

Паттерны:
- Eval broker / scheduler workers (Nomad)
- Filter + Score + Bind (k8s)
- Streaming RPC с bidirectional stream (gRPC)
- cgroups v2 unified hierarchy (Linux 4.5+)
- OCI Runtime Spec (opencontainers/runtime-spec)
