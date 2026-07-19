# Architecture — целевая картина

> Это "как должно быть" в финале. Поэтапно уточняется в `DECISIONS.md`.

## Класс задачи

**MOSIX-style Single System Image (SSI) поверх Linux, реализованный
в user-space через имитацию NUMA-архитектуры на сети машин.**

Не k8s. Не task-queue. Не "новый куб". Подробнее — `DECISIONS.md` ADR 016.

## NUMA-аналогия

```
Многоядерный CPU                Etronium-Scdr
─────────────────               ─────────────────────────
CPU scheduler                   Scheduler (1 binary, VPS)
Ядра (cores)                    Lords (N binaries, donor machines)
Нити (threads)                  Tenant processes
L1/L2 cache                     Lord local FS + file cache
Memory bus / QPI                Network (control: hier, data: P2P)
NUMA balancing                  Process migration + weight rebalancing
```

## Высокоуровневая схема

```
┌──────────────────────────────────────────────────────────────┐
│  Tenant (арендатор)                                          │
│  • etronium CLI (cobra) или будущий WebUI                    │
│  • Видит СВОИ процессы на ВСЕХ lord'ах где они сейчас        │
│  • Не знает про tenant'ов других                              │
└────────────────────────┬─────────────────────────────────────┘
                         │ gRPC (control plane)
                         │ + data plane через scheduler-routed streams
                         ▼
┌──────────────────────────────────────────────────────────────┐
│  Scheduler (single binary, VPS)                              │
│                                                              │
│  Глобальный view:                                            │
│    • все процессы всех tenant'ов на всех lord'ах             │
│    • все ресурсы всех lord'ов (advertised + local)           │
│    • placement decision                                      │
│                                                              │
│  Компоненты:                                                 │
│    • process_table.go    — глобальная таблица процессов      │
│    • placement.go        — выбор lord'а для Spawn/Migrate    │
│    • migrator.go         — оркестрация CRIU миграции         │
│    • io_router.go        — stdin/stdout/stderr ↔ tenant      │
│    • file_router.go      — Pull/Push + cache invalidation    │
│    • state_store.go      — in-memory + WAL                   │
└────────────────────────┬─────────────────────────────────────┘
                         │ gRPC (control plane)
              ┌──────────┼──────────┐
              ▼          ▼          ▼
        ┌─────────┐ ┌─────────┐ ┌─────────┐
        │ Lord 1  │ │ Lord 2  │ │ Lord N  │
        │         │ │         │ │         │
        │ Локальный view:                                      │
        │   • только свои процессы                             │
        │   • честные ресурсы (не overcommit)                  │
        │   • Linux kernel не знает про overcommit             │
        │                                                      │
        │ Компоненты:                                          │
        │   • cgroup_manager.go — управление cgroups v2        │
        │   • exec_linux.go    — fork/exec через libcontainer  │
        │   • criu_ops.go      — checkpoint/restore            │
        │   • local_cache.go   — file cache                    │
        └─────────┘ └─────────┘ └─────────┘
```

## Поток Spawn (от tenant до процесса)

```
1. tenant → scheduler:  gRPC Spawn(exec, argv, env, resources)
2. scheduler:
     a. process_table.create(process_id=ULID, state=NEW)
     b. placement.pick(resources) → lord_2
     c. process_table.update(state=READY, lord=lord_2)
     d. lord_2.ExecRemote(exec, argv, env, resources)
3. lord_2:
     a. cgroup_manager.create(process_id, resources)
     b. exec_linux.fork(exec, argv, env)
     c. process_table[local].add(pid)
     d. returns ProcessInfo { state: RUNNING, local_pid }
4. scheduler: process_table.update(state=RUNNING, local_pid)
5. scheduler → tenant: ProcessInfo
```

## Поток Migrate (pressure balancing)

```
1. scheduler.migrator.decide_to_migrate(process_X):
     • memory pressure на текущем lord'е, или
     • lord уходит в lazy death, или
     • tenant явно попросил
2. source_lord.Checkpoint(process_X, leave_running=false):
     • CRIU dump в /var/lib/etronium/checkpoints/process_X/
     • returns checkpoint_path
3. scheduler:
     a. target_lord = placement.pick_better(process_X.resources)
     b. source_lord передаёт dump target_lord'у (file transfer)
4. target_lord.Restore(process_X, dump_path, exec/argv/env/resources):
     • CRIU restore
     • cgroup_manager.attach(process_X)
     • stdin/stdout/stderr reconnect через scheduler
     • returns new local_pid
5. scheduler:
     a. process_table.update(lord=target_lord, local_pid=new_pid)
     b. source_lord.KillRemote(process_X) — cleanup old process
     c. process_table.update(state=RUNNING)
6. scheduler → tenant: MigrateResponse { new_lord_id, new_local_pid }
```

## Scheduler внутренняя структура

```
internal/scheduler/
├── server.go           # gRPC SchedulerService реализация
├── process_table.go    # глобальная таблица процессов (map + mutex + WAL)
├── placement/
│   ├── picker.go       # trivial в Phase 0, weighted score в Phase 1+
│   └── load.go         # memory/CPU pressure based
├── migrator/
│   ├── orchestrator.go # CRIU dump + transfer + restore
│   └── reconnect.go    # stdin/stdout/stderr restore
├── io_router.go        # stdin/stdout/stderr ↔ tenant (server streams)
├── file_router.go      # PullFile/PushFile/Invalidate
├── state_store/
│   ├── memory.go       # sync.Map / RWMutex map
│   └── wal.go          # append-only log (Phase 0.5+)
├── auth.go             # pre-shared token middleware
└── metrics/            # Prometheus (Phase 5)
```

## Lord внутренняя структура

```
internal/lord/
├── agent.go            # main loop, register, heartbeat
├── cgroup_manager/
│   ├── tree.go         # создание cgroup v2 иерархии
│   ├── limits.go       # cpu/memory/io/pids (LOCAL capacity, не overcommit)
│   ├── stats.go        # cpu.stat, memory.current, memory.peak
│   └── freeze.go       # cgroup.freeze для PAUSED state
├── exec_linux/
│   ├── fork.go         # через libcontainer или os/exec
│   ├── signals.go      # POSIX signal delivery
│   └── procfs.go       # /proc/<pid>/ для state introspection
├── criu_ops/
│   ├── checkpoint.go   # criu dump
│   ├── restore.go      # criu restore
│   └── transfer.go     # local file mgmt
├── file_cache/
│   ├── store.go        # LRU + size limit
│   └── invalidator.go  # explicit + TTL
├── stats_monitor.go    # CPU/mem/active_procs для heartbeat
└── rpc/
    ├── server.go       # gRPC LordService реализация
    └── client.go       # client к scheduler'у
```

## Сетевая топология (ADR 018)

```
Control plane (иерархическая):
  tenant ↔ scheduler ↔ lord     (НЕ lord ↔ lord)

Data plane (peer-to-peer где можно):
  tenant ↔ lord:                I/O streams, file transfer
  lord   ↔ lord:                checkpoint transfer, process↔process network
  relay через scheduler если lord'ы в разных L2-сетях
```

## Process lifecycle (POSIX-подобная FSM)

```
NEW ──► READY ──► RUNNING ──► EXITED
                │         │
                │         ├──► PAUSED ──► RUNNING
                │         │
                │         ├──► MIGRATING ──► RUNNING (на другом lord'е)
                │         │
                │         └──► STOPPED (killed by signal)
```

| State | Кто владеет | Что происходит |
|---|---|---|
| NEW | scheduler | Только что создан в process_table |
| READY | scheduler | Placement выбран, ExecRemote отправлен, ждём ack |
| RUNNING | lord | Процесс бежит, ресурсы считаются |
| PAUSED | lord | cgroup.freeze=1 + SIGSTOP. I/O streams pause |
| MIGRATING | scheduler | CRIU dump+restore, новый lord ещё не ack'нул |
| EXITED | scheduler | exit_code != null, финальное состояние |
| STOPPED | scheduler | killed by signal, exit_signal != 0 |

## Resource модель

```
advertised_capacity (в proto)    →  что тенант видит / просит
   ↓ scheduler решает что реально доступно
local_capacity (на lord'е)        →  реальный лимит cgroup
   ↓ Linux kernel на lord'е
hardware                          →  физические CPU/RAM
```

Принцип: `advertised ≥ local`, scheduler гарантирует что сумма `local`
на lord'е не превышает hardware. Если тенант хочет больше — может
попросить через `Migrate(auto=true)`.

## Сравнение с `../Etronium`

| Аспект | `../Etronium` (TECH-MVP) | `Etronium-Scdr` (здесь, v2) |
|---|---|---|
| Класс задачи | WebUI-orchestrator для cgroup-задач | MOSIX-class SSI поверх Linux |
| Scheduler ↔ Lord | HTTP pull | gRPC bidi stream + push |
| Гранулярность | bash-команда в cgroup | POSIX-процесс |
| Container runtime | Прямые cgroups v2 файлы | cgroups v2 + libcontainer + опционально CRIU |
| Что запускается | `bash -c <command>` | exec + argv (POSIX) |
| Tenant client | WebUI (HTML/JS) | CLI (`etronium` через cobra) |
| State | PostgreSQL | in-memory + WAL |
| Lords | один | N (NUMA-cores) |
| Placement | FIFO из БД | weighted score + memory pressure + affinity |
| Auth | `LANDLORD_TOKEN` статичный | pre-shared token в metadata + tenant ID |
| Task IDs | UUID v4 | ULID для process_id / lord_id |
| Migration | Нет | CRIU checkpoint/restore + reconnect |
| Filesystem | Нет | Local cache + pull/push через scheduler |
| Network | N/A | Hierarchical control + P2P data |
| Logging | slog | zap + tint |
| External DB | Postgres 16 | нет |

## Reference projects

Прямые аналоги (исторические):
- **MOSIX** (1999–2008, Bar-Ilan Univ) — Linux kernel patches для process migration
- **OpenMosix** — форк MOSIX
- **Kerrighed** (INRIA) — SSI на Linux, cluster-aware scheduler
- **Bproc** (Beowulf) — process migration across cluster
- **Sprite** (Berkeley, 80-е) — оригинальная distributed OS с process migration
- **Locus** (UCSB, 80-е) — distributed Unix
- **CRIU** — современный userspace checkpoint/restore

Отвергнутые как wrong abstraction level:
- ❌ HashiCorp Nomad — k8s-class, task-queue based
- ❌ BOINC — volunteer computing, слишком высокоуровневый
- ❌ k8s — оркестратор контейнеров, не процессов
