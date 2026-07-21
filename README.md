# Etronium-Scdr

> **MOSIX-style Single System Image (SSI) поверх Linux, в user-space.**
> Через имитацию NUMA-архитектуры на сети машин.
> Scheduler = NUMA scheduler, Lords = cores, Processes = threads.

Не k8s. Не task-queue. Не "новый куб". Решаем задачу класса MOSIX/OpenMosix/Kerrighed — то что пробовали в 90-х-2000-х, но без kernel patches.

## TL;DR

- **Scheduler** (VPS) — single Go binary, gRPC API, in-memory process table + WAL.
- **Lord** (donor machine) — single Go binary, gRPC server. Локальный view: только свои процессы, честные ресурсы.
- **Tenant** (арендатор) — CLI (`etronium` через cobra). Глобальный view: свои процессы на всех lord'ах.
- Стек: Go 1.22+, gRPC, protobuf, cgroups v2, libcontainer, опционально CRIU.
- **Никаких внешних БД** в MVP — in-memory state + WAL.
- **Никаких kernel patches** — всё в user-space.

## Архитектура (NUMA-аналогия)

```
Многоядерный CPU              Etronium-Scdr
─────────────────             ─────────────────────────
CPU scheduler                 Scheduler (1 binary, VPS)
Ядра (cores)                  Lords (N binaries, donor machines)
Нити (threads)                Tenant processes
L1/L2 cache                   Lord local FS + file cache
Memory bus / QPI              Network (hier control + P2P data)
NUMA balancing                Process migration + weight rebalancing
```

Подробнее — [`docs/ARCHITECTURE.md`](./docs/ARCHITECTURE.md).

## Документация

| Файл | Назначение |
|---|---|
| **[`docs/OPS-GUIDE.md`](./docs/OPS-GUIDE.md)** | **Operations: install / onboard / day-N troubleshooting** |
| **[`docs/LORD-SETUP.md`](./docs/LORD-SETUP.md)** | **Onboarding a new lord (container or bare-metal)** |
| **[`docs/TENANT-USAGE.md`](./docs/TENANT-USAGE.md)** | **Tenant CLI reference (run/ps/kill/wait)** |
| **[`docs/AUTOSCALE.md`](./docs/AUTOSCALE.md)** | **Autoscale: ABS_AUTO planner (scheduler decides, never you)** |
| [`docs/RESEARCH.md`](./docs/RESEARCH.md) | Исследование (ре-фрейм 2026-07-20) |
| [`docs/ARCHITECTURE.md`](./docs/ARCHITECTURE.md) | NUMA-аналогия, потоки Spawn/Migrate |
| [`docs/ROADMAP.md`](./docs/ROADMAP.md) | Phase 0–5: hello world → persistence |
| [`docs/STACK.md`](./docs/STACK.md) | Конкретные библиотеки, версии |
| [`docs/PROTO.md`](./docs/PROTO.md) | gRPC контракт v2, FSM процессов |
| [`docs/DECISIONS.md`](./docs/DECISIONS.md) | ADR-style журнал |
| [`docs/OPEN-QUESTIONS.md`](./docs/OPEN-QUESTIONS.md) | Нерешённые вопросы |
| `BPF-side` docs | [`../Etronium-Scdr-BPF/docs/`](../Etronium-Scdr-BPF/docs/) |

## Контракт

gRPC API в [`proto/etronium/v1/etronium.proto`](./proto/etronium/v1/etronium.proto).

Два сервиса:
- **`SchedulerService`** — tenant вызывает Spawn/Kill/Wait/Migrate.
- **`LordService`** — scheduler вызывает ExecRemote/KillRemote/Checkpoint/Restore.

Сгенерированный Go лежит в `internal/gen/etronium/v1/`. Swagger — в `docs/openapi/`.

**Регенерация:** `make proto` (нужен `protoc 24+` + Go плагины `protoc-gen-go`,
`protoc-gen-go-grpc`, `protoc-gen-grpc-gateway`, `protoc-gen-openapiv2`).

## Статус

✅ **v0.3.0 released.** `tenant shell` (TTY relay) + `tenant attach --follow` + autoscale ABS_AUTO planner + flat CLI.

Текущая функциональность:
- Multi-lord, weighted placement по CPU+RAM
- cgroup v2 isolation per process
- Fault tolerance: lord dies → respawn на здоровом lord'е (V4)
- Opt-in state serialization для переживания lord death (V5)
- WAL для process_table (cold start replay)
- Graceful shutdown с drain lords
- Prometheus-friendly JSON logs
- **Autoscale ABS_AUTO planner** — scheduler migrates coldest process off overloaded lords without any user action (v0.2.0+)
- **Sched_ext BPF scheduler** — kernel-level dispatch via `sched_ext` (Phase 4); `SCHED_EXT` policy applied to every spawned task
- **Interactive TTY relay** — `tenant shell` lets you run `apt-get`, `ls`, `kubectl` interactively with output streamed back (v0.3.0+)
- One-command installer (`curl | bash -s -- scheduler/lord/tenant`) with auto-generated systemd units

Не работает: live migration (CRIU) — отвергнута на kernel 6.17. Fault tolerance через V4+V5 вместо неё.

## CLI Reference

Three binaries. Scheduler + Lord are daemons; `tenant` is the
end-user CLI installed on every machine that wants to talk to the
fleet.

### `tenant` — арендаторский CLI (Go, cobra)

Global flags:

| Flag | Default | Notes |
|---|---|---|
| `--scheduler` | `localhost:51061` | scheduler gRPC address (env: `SCHEDULER_ADDR`) |
| `--tenant` | `anonymous` | tenant id |
| `--json` | false | machine-readable output |

Subcommands (flat — like supervisord/systemd-run):

| Subcommand | Purpose |
|---|---|
| `tenant run <exec> [args...]` | Spawn a new process (positional exec + args, `--cpu-shares`, `--mem-mb`, `--max-restarts`, `--state-dump`, `--prefer-lord`) |
| `tenant shell [--shell <path>]` | **Interactive TTY relay on a lord** (v0.3.0+) — write `apt-get` locally, runs on lord, output relayed back through scheduler |
| `tenant ps [--running]` | List this tenant's processes (alias: `ls`, `list`) |
| `tenant get <pid>` | Get state of one process |
| `tenant attach [--follow] <pid>` | **kubectl-attach analog** (v0.3.0+) — dump captured IO or stream live until process exits |
| `tenant wait <pid>` | Block until process exits (returns exit code) |
| `tenant kill <pid>` | Send signal (default `SIGTERM`, `--signal=…`, `--force=SIGKILL`) |
| `tenant status` | Fleet summary: lords count + healthy count + scheduler address |
| `tenant lords` | List registered lords (`hostname`, `advertised_cpu_shares`, `last_heartbeat`, …) |
| `tenant token new\|list\|revoke` | Tenant access tokens (Phase 3+ stubs) |
| `tenant format-fleet` | Pretty-print `lords --json` output as a table |
| `tenant version` | Print version + commit |

> **No `tenant migrate`** — autoscale inside scheduler handles all migration
> and rebalancing automatically. See [`docs/AUTOSCALE.md`](./docs/AUTOSCALE.md).

Examples:

```bash
# Spawn a 60-second sleep on the fleet:
tenant run /bin/sleep 60

# Spawn with explicit CPU/mem hint:
tenant run --cpu-shares=100 --mem-mb=100 /bin/sleep 300

# Spawn with restart budget + state-dump for fault tolerance:
tenant run \
  --state-dump=/tmp/etronium/state/demo.json \
  --max-restarts=10 \
  ./bin/example-stateful

# Soft-affinity (hint, autoscale may override):
tenant run --prefer-lord=lord-school-C /bin/sleep 600

# Watch fleet + processes:
tenant status --json | jq .
tenant ps --running

# Wait for completion:
tenant wait <pid>     # exit code 0..N

# Manual signal escalation:
tenant kill <pid>              # SIGTERM
tenant kill --signal=KILL <pid>  # SIGKILL

# Interactive shell on a lord (v0.3.0+ — write `apt-get` locally,
# it runs on a lord and you see the result as if it were local):
tenant shell --shell=/bin/sh
# [etronium shell] connected to lord=01KY3D75... pid=0
# /bin/sh: 0: can't access tty; job control turned off
$ echo HELLO_FROM_LORD
HELLO_FROM_LORD
$ uname -n
midas-ThinkPad-E14-Gen-5
$ ls -la / | head -3
$ exit

# Attach to a running process (dump buffer / stream live):
tenant attach <pid>               # dump captured IO once
tenant attach --follow <pid>      # live stream until process exits
```

### `lord` — donor machine (Go daemon)

The `lord` binary opens a long-lived bidi gRPC stream to the scheduler,
auto-registers on connect, and accepts Exec/Kill/Checkpoint/Restore
commands. It applies `SCHED_EXT` (policy=7) to every spawned child so
the BPF scheduler can dispatch lord-side tasks per-cgroup.

Flags:

| Flag | Default | Notes |
|---|---|---|
| `--scheduler` | `localhost:50061` | scheduler gRPC address (env: `SCHEDULER_ADDR`) |
| `--hostname` | `os.Hostname()` | override hostname |
| `--advertise-cpu` | 0 | NUMA-overcommit: CPU shares to advertise (0 = physical count) |
| `--advertise-mem` | 0 | NUMA-overcommit: memory bytes to advertise (0 = physical) |
| `--log` | info | debug/info/warn/error |
| `--log-format` | tint | tint (TTY) or json (production) |
| `--version` | — | print version and exit |

```bash
# Bare-metal example (recommended via installer.sh):
sudo /usr/local/bin/installer.sh lord \
  --scheduler=etronium.example.com:51061 \
  --hostname=my-laptop \
  --advertise-cpu=4
```

### `scheduler` — control plane (Go daemon)

Two modes: long-running daemon (`scheduler serve` is implicit), and
operator subcommands that exit after one operation.

Daemon flags:

| Flag | Default | Notes |
|---|---|---|
| `--addr` | `:51061` | gRPC listen address (env: `SCHEDULER_LISTEN`) |
| `--log` | info | log level |

Operator subcommands (positional, before `--addr`):

| Subcommand | Purpose |
|---|---|
| `scheduler stats` | Dump SCHED_EXT kernel state + BPF map sizes (per-lord counters) |
| `scheduler stats --json` | Machine-readable for Prometheus |
| `scheduler --version` / `scheduler version` | Print version |

> **No `scheduler migrate`** — autoscale handles all rebalancing automatically.
> Tune via env vars (`ETRONIUM_AUTOSCALE_*`).

```bash
# Observability only:
scheduler stats
scheduler stats --json | jq .
```

Full tenant reference: [`docs/TENANT-USAGE.md`](./docs/TENANT-USAGE.md).

## Quick Start (5 минут)

```bash
# 1. Build (нужен Go 1.22+):
make build

# 2. Поднять scheduler + 3 lords (Docker):
docker compose -f test/docker-compose.yml up -d
sleep 5

# 3. С tenant CLI:
./bin/etronium lords                              # видим 3 lord'а
./bin/etronium run /bin/sleep 60                  # создать процесс (flat CLI)
./bin/etronium ps                                  # список

# 4. 5-минутное демо для PM:
./scripts/demo-pm.sh

# Остановить:
docker compose -f test/docker-compose.yml down
```

## Сценарий fault tolerance вручную

```bash
# Spawn stateful app:
./bin/etronium run \
    --state-dump=/tmp/etronium/state/demo.json \
    --max-restarts=10 \
    ./bin/example-stateful

# Kill lord container:
docker kill etronium-lord-02

# Через 5–15 сек:
./bin/etronium ps                              # процесс снова RUNNING, на другом lord'е
cat /tmp/etronium/state/demo.json | jq .counter   # counter сохранился
```



## Связь с другим репо

`../Etronium/` (TECH-MVP) — старая реализация: HTTP pull + cgroups v2 напрямую + WebUI + PostgreSQL.
**`Etronium-Scdr/` — независимый трек** на базе переосмысленного research от 2026-07-20. Общего кода нет.
