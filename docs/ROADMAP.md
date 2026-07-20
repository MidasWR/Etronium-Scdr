# Roadmap — поэтапный план

> Класс задачи: MOSIX-style SSI без kernel patches (см. ADR 016).
> Это не "новый куб" — это NUMA-аналогия поверх сети машин.

---

## Phase 0 — "Hello world: один процесс, один лорд" (3–5 дней)

**Цель:** минимально работающий SSI с одним lord'ом.
Tenant может Spawn процесс, видеть state, получать stdout, послать SIGTERM, дождаться exit.

- [ ] Структура репо: `cmd/scheduler/`, `cmd/lord/`, `cmd/etronium/`
- [ ] `proto/v1/etronium.proto` (уже есть в v2) — Phase 0 реализует подмножество
- [ ] Scheduler:
  - [ ] gRPC SchedulerService: Spawn, GetProcess, ListProcesses, Kill, Wait
  - [ ] gRPC LordService (на стороне scheduler'а): Register, Heartbeat, ExecRemote, KillRemote, StatsRemote
  - [ ] In-memory process_table (map + mutex, без WAL)
  - [ ] Trivial placement: единственный здоровый lord
- [ ] Lord:
  - [ ] gRPC client к scheduler'у
  - [ ] Register + Heartbeat (10s interval)
  - [ ] ExecRemote через `os/exec` (не libcontainer, не cgroups — пока просто fork+exec)
  - [ ] KillRemote через `syscall.Kill`
  - [ ] StatsRemote через `runtime.NumGoroutine` + `gopsutil` (CPU/RAM)
  - [ ] Stdout/stderr capture в буфер, отдача через stream
- [ ] Tenant CLI (`etronium`):
  - [ ] `etronium process spawn --exec=/bin/echo --arg=hi`
  - [ ] `etronium process list`
  - [ ] `etronium process get <id>`
  - [ ] `etronium process kill <id> [--signal=15]`
  - [ ] `etronium process wait <id>`
  - [ ] `etronium process attach <id>` (stdin/stdout)

**Definition of done:**
```bash
$ ./bin/scheduler &
listening on :50051

$ ./bin/lord --scheduler=localhost:50051 &
lord-01 registered, heartbeat=10s

$ ./bin/etronium process spawn --exec=/bin/echo --arg=hi
process_id=01H... state=RUNNING lord=lord-01 local_pid=12345

$ ./bin/etronium process wait 01H...
exit_code=0 stdout="hi\n" duration_ms=12

$ ./bin/etronium lords
lord-01  healthy=true  cpu=2%  mem=120MB  procs=0
```

**Что НЕ в Phase 0:**
- cgroups (просто fork+exec, никаких лимитов)
- CRIU / миграция
- File transfer
- WatchProcess / StreamProcessIO через scheduler (lord стримит напрямую tenant'у через gateway)
- WAL, auth (проверка токена), metrics

---

## Phase 1 — Resource isolation (5–7 дней)

**Цель:** реальная изоляция через cgroups v2. Готовность к overcommit.

- [x] Lord: cgroup_manager с tree, limits (cpu/memory/io/pids), stats, cleanup
- [x] Lord: exec через cgroup-attached fork (вместо простого os/exec)
- [x] Scheduler: `ResourceSpec` валидация (cpu_shares, cpu_quota_pct, mem_limit_bytes, io_weight, pids_limit)
- [x] Lord: heartbeat читает реальные cpu/mem из cgroup агрегата (delta sampling)
- [ ] Scheduler: учитывает `local_capacity` lord'ов в placement (Phase 2)
- [ ] Lord: SIGSTOP/SIGCONT через cgroup.freeze для PAUSED state
- [ ] Lord: /proc/<pid>/ inspection для state introspection
- [ ] Tenant CLI: `etronium process pause <id>` / `resume`
- [ ] Tenant CLI: `etronium process stats <id>`

**Реализовано в commit d7591a6+phase1:**
- `internal/lord/cgroup.go` — CgroupManager (mkdir slice, enable controllers, write limits, attach PID, read stats, destroy)
- `internal/lord/stats.go` — agent.getCurrentUsage() читает cgroup.usage_usec + memory.current агрегата
- `internal/scheduler/server.go` — validateResources() с правилами [0..10000] для cpu/io, [0..100] для cpu_quota_pct, [>0] для mem, [0..1000000] для pids
- Slice path: `/sys/fs/cgroup/etronium/<lord_id>/<process_id>/` (fallback к user-slice если root недоступен)

**Definition of done:**
```bash
$ ./bin/etronium process spawn --exec=/bin/stress --arg=--cpu --arg=2 --resources=mem_limit_mb=100
# через 30 сек процесс убит OOM на lord'е, exit_signal=9 (SIGKILL от OOM killer)
```

**Тестовая среда:** e2e_phase1.sh в privileged Docker с cgroupns=private.
На этом Docker-хосте **cgroup delegation заблокирована** (apparmor/seccomp не даёт
включать `+cpu +memory +io +pids` в subtree_control даже root'у). Код толерантен —
проверяет что файл существует, логирует warning, продолжает. На production хосте
с systemd user.slice делегация работает.

---

## Phase 2 — Multi-lord + weighted placement (5–7 дней)

**Цель:** scheduler умеет работать с N lord'ами, placement не trivial.

- [ ] Scheduler: Lord registry с heartbeat tracking, mark unhealthy после N пропусков
- [ ] Scheduler: weighted placement: `score = reputation × (1 - load) × locality × affinity`
- [ ] Scheduler: memory pressure detection (по Heartbeat'ам от lord'ов)
- [ ] Tenant CLI: `etronium lords` показывает score для каждого
- [ ] Tenant CLI: `etronium process spawn --prefer-lord=lord-02`

**Definition of done:**
```bash
$ ./bin/etronium lords
lord-01  healthy=true  score=0.85  cpu=30%  mem=2GB
lord-02  healthy=true  score=0.92  cpu=10%  mem=1GB   ← рекомендован

$ ./bin/etronium process spawn --exec=/bin/sleep --arg=60
# scheduler выбрал lord-02 (выше score)
```

---

## Phase 3 — Fault Tolerance через user-space (3 дня)

**Цель:** процесс переживает lord death без live migration. Pure user-space, без OS patches, без CRIU.

> ADR 028-029 в DECISIONS.md. Live migration (CRIU) была отвергнута — kernel 6.17 + Docker namespace death
> фундаментально блокирует CRIU restore (см. git history до commit `7d66b82`). Вместо этого:
> **V4** — scheduler respawns процесс на здоровом lord'е при lord disconnect.
> **V5** — приложение само пишет state в `$ETRONIUM_STATE_DUMP`, scheduler прокидывает путь в env.
> При respawn новое приложение читает state-файл и продолжает работу.

### Phase 3.4 — Respawn on disconnect + V5 opt-in state dump (DONE)

**Status (2026-07-20 21:00):** commits `3f00ea6` / `366d7a7` / `7d66b82` — **implementation complete**, build green, e2e_phase2 green. V5 example app `cmd/example-stateful` демонстрирует persistent counter.

**Что сделано:**
- [x] Proto: PROCESS_STATE_RESTARTING (transient), ProcessInfo.{restart_count, last_error, state_dump_path, max_restarts}, SpawnRequest.{max_restarts, state_dump_path_hint}
- [x] Scheduler: `internal/scheduler/recovery.go` — `onLordDisconnect()` + `respawnProcessOnLord()`
- [x] Scheduler: PAUSED/MIGRATING процессы тоже реcпаунятся (was only RUNNING/READY)
- [x] Scheduler: re-check state перед respawn (race vs delayed ProcessExit/Kill)
- [x] Scheduler: 10 attempts × ~10s ≈ 55s max wait; после исчерпания все RESTARTING → STOPPED
- [x] Scheduler: max_restarts honoured (default 10, -1=unlimited); превышение → STOPPED с LastError
- [x] Scheduler: LastError устанавливается ДО UpdateState/UpdateResult (порядок важен для WatchProcess)
- [x] Scheduler: `etronium process migrate` → fault-tolerant respawn (тот же путь что recovery)
- [x] ProcessTable: `ListByLord(lordID, filter)`, `All()` — recovery helpers
- [x] CLI: `--max-restarts`, `--state-dump` flags на `etronium process spawn`
- [x] Example app: `cmd/example-stateful/main.go` — counter + state serialization demo
- [x] Open Question #21: shared storage между lord'ами — задокументировано требование

**Не сделано (отложено):**
- [ ] Auto-pressure migration (Phase 3.2) — зависит от migration, не fault-tolerance. Возможно не нужно вообще.
- [ ] ptrace-based Freeze+Resume (`internal/lord/freeze.go` удалён, MVP без Resume). Может быть полезен как быстрая замена CRIU для simple процессов, но это 1-2 недели работы на full Resume.
- [ ] Reconnect stdout/stderr (Phase 3.1) — был для CRIU миграции, теперь не критично: новый процесс теряет output предыдущего instance, но V5 state-file содержит прогресс.

### Definition of done (Phase 3.4) ✅
```bash
# Spawn на lord-01:
$ ./bin/etronium process spawn --exec=./bin/example-stateful \
    --state-dump=/tmp/etronium/state/pid.json --max-restarts=3
process_id=01H... lord=lord-01

# Kill lord-01:
$ docker kill lord-01

# Через 5-15 секунд:
$ ./bin/etronium process get 01H...
state=RUNNING lord_id=lord-02 restart_count=1
last_error=""

# Counter в state-файле продолжается с предыдущего значения:
$ cat /tmp/etronium/state/pid.json | jq .counter
1234   # не 0!
```

## Phase 4 — File operations + Network transparency (5–7 дней)

**Цель:** tenant может работать с файлами процессов, процессы на разных lord'ах могут общаться.

- [ ] Scheduler: PullFile / PushFile / InvalidateFileCache
- [ ] Lord: local file cache (LRU + size limit + TTL)
- [ ] Lord: file transfer между lord'ами (peer-to-peer если в одной сети, relay через scheduler иначе)
- [ ] Scheduler: выдача endpoint'ов для process↔process network
- [ ] Tenant CLI: `etronium process pull <id> /path/in/lord`
- [ ] Tenant CLI: `etronium process push <id> /path < local_file`
- [ ] Tenant CLI: `etronium process netstat <id>` — какие connections у процесса

**Definition of done:**
```bash
$ ./bin/etronium process push 01H... /tmp/script.sh < ./script.sh
ok sha256=abc123

$ ./bin/etronium process pull 01H... /tmp/output.txt > output.txt
sha256=def456
```

---

## Phase 5 — Persistence + Polish (5–7 дней)

**Цель:** production-ready чувство для ПМ-демо.

- [ ] Scheduler: WAL для process_table (jsonl append-only + replay at startup)
- [ ] Scheduler: auth middleware (pre-shared token)
- [ ] Scheduler + Lord: graceful shutdown (SIGTERM → drain → exit)
- [ ] Scheduler: Prometheus metrics
- [ ] Lord: Prometheus metrics
- [ ] Tenant CLI: pretty output (lipgloss/tint)
- [ ] docker-compose для локального запуска (1 scheduler + 2-3 lord'а)
- [ ] README "запусти за 5 минут"

---

## Что НЕ делаем (anti-patterns, не повторять)

> Из research + наши решения.

- ❌ Патчить ядро. Без kernel modules, без eBPF (для migration — допустимо, для остального нет).
- ❌ Свой container runtime. Используем cgroups v2 + libcontainer + опционально CRIU.
- ❌ Свой consensus / external DB. Single scheduler + WAL.
- ❌ WebUI. CLI хватит для MVP.
- ❌ Billing / auth users / GPU.
- ❌ Persistent volumes в смысле "общий диск на всех". Только file cache + pull/push.
- ❌ Полноценный POSIX / VFS layer. Только `PullFile` / `PushFile` API.
- ❌ process↔process IPC поверх сети через что-то кроме TCP/UDP sockets.

---

## Демо-сценарий для PM (Phase 5)

**Setup (5 минут):**
1. На VPS: `docker compose up scheduler`
2. На 2-3 донорах: `docker compose up lord` (или просто бинарь)
3. Scheduler показывает "3 lord'а registered, pool ready"

**Сценарий (5–7 минут):**
1. PM вводит:
   ```bash
   etronium process spawn --exec=/bin/sh --arg=-c \
     --arg='echo "Started on PID $$"; for i in 1 2 3 4 5; do echo working $i; sleep 1; done'
   ```
2. PM видит live output: "Started on PID 12345", потом "working 1", "working 2", ...
3. PM вводит `etronium process spawn --exec=/bin/sleep --arg=300 --resources=mem_limit_mb=100`
4. На lord'е через 5 минут процесс OOM-killed → scheduler мигрирует на другой lord (если есть ресурсы)
5. PM вводит `etronium lords` — видит миграцию событий в логе
6. PM вводит `etronium process migrate <id> --to=lord-02` — явная миграция
7. PM: "вот так это работает, ваши процессы реально живут на чужих машинах, scheduler их балансирует как NUMA scheduler ядра в CPU"

**Что НЕ показывать:**
- Внутреннюю архитектуру
- CRIU dump/restore (просто работает)
- Multi-tenant (для демо один tenant)
