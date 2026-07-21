# Etronium — Operations Guide

Single-System-Image scheduler on top of heterogeneous compute resources
("lords"). Tenants see one Linux VPS; under the hood, the scheduler
distributes processes across N machines with custom sched_ext BPF
routing.

**Audience**: operator installing/operating Etronium, sysadmin onboarding
a new lord into the fleet, tenant using their VPS via `tenant` CLI.

---

## 0. Quick start (5-minute demo)

```bash
git clone https://github.com/midas/Etronium-Scdr.git
cd Etronium-Scdr
./scripts/mvp/build-image.sh                  # ~30s
./scripts/mvp/up.sh -d                         # bring up stack
sleep 8
cat /sys/kernel/sched_ext/state                # → enabled
./scripts/mvp/e2e-bpf.sh                       # smoke test → ALL GREEN
./scripts/mvp/demo-pm.sh                       # 5-minute PM demo
./scripts/mvp/down.sh                          # tear down
```

This brings up **1 frontend (scheduler)**, **5 lords**, **2 tenants**
on a single host. All containers share the host kernel via
`--privileged` + `/sys/fs/bpf` mount, so the BPF scheduler is the **same
scheduler for all 7 containers at once**.

---

## 1. Concepts

- **Frontend** = the VPS your tenants talk to. Holds the gRPC server
  the `tenant` CLI connects to. In production: 1 per deployment.
- **Scheduler** = the Go daemon that owns placement decisions and the
  process table. Runs **inside** the frontend container in MVP.
- **schedulerd** = the BPF loader daemon. Loads `sched_ext_final.o`
  into host kernel via `bpftool struct_ops register` and pins maps
  to bpffs. **Required on every host that runs lords.**
- **Lord** = a compute resource provider (your own laptop, school
  workstation, edge device). Runs `lord --scheduler=<frontend>` and
  registers its hostname + CPU shares. **No state**. The frontend
  is the source of truth.
- **Tenant** = end user. Has its own `tenant_id`. Talks to the
  scheduler via `tenant --scheduler=<frontend>`. Doesn't know or care
  how many lords exist.

```
                           ┌──────────────────┐
                           │     Frontend     │
                           │  scheduler +     │
                           │  schedulerd (BPF)│
                           └──────────────────┘
                                       │
                                       │  gRPC
                                       │
            ┌──────────┬───────────┬────┴────┬──────────┐
            ▼          ▼           ▼         ▼          ▼
       lord-01     lord-02    lord-03    lord-04    lord-05
       school-A    school-B   school-C   edge-X     iot-Y
```

---

## 2. Installation

### 2.1. Host kernel prerequisites

Etronium uses **sched_ext (scx)** which is in mainline kernel 7.0+ but
disabled by default.

Check your host:

```bash
uname -r                      # 7.0.0-28-generic or newer
test -d /sys/kernel/sched_ext && echo "sched_ext: OK" || echo "MISSING"
test -f /sys/kernel/btf/vmlinux && echo "BTF: OK" || echo "MISSING"
test -d /sys/fs/cgroup && echo "cgroup v2: OK" || echo "MISSING"
```

Required:
- kernel ≥ 7.0 with `CONFIG_SCHED_CLASS_EXT=y`
- BTF vmlinux (default in Ubuntu 24.04 / Debian 12)
- cgroup v2 unified hierarchy mounted
- kernel boot config at `/boot/config-$(uname -r)` (libbpf feature
  detection needs it)

### 2.2. Tooling on the host

```bash
sudo apt install -y build-essential golang-go \
    clang llvm libbpf-dev linux-tools-$(uname -r) docker.io docker-compose
```

### 2.3. Build

```bash
git clone https://github.com/midas/Etronium-Scdr-BPF.git    # BPF side
git clone https://github.com/midas/Etronium-Scdr.git         # userspace side

# Step 1: BPF side
cd Etronium-Scdr-BPF
make build                                   # produces bin/sched_ext_final.o + schedulerd

# Step 2: userspace side
cd ../Etronium-Scdr
make build                                   # produces bin/{scheduler, lord, etronium}

# Step 3: build the runtime image with BPF .o baked in
./scripts/mvp/build-image.sh                 # produces etronium-mvp:runtime
```

### 2.4. Run the test stack

```bash
./scripts/mvp/up.sh -d                       # detached
sleep 8                                      # wait for BPF register
cat /sys/kernel/sched_ext/state              # → enabled
```

If state stays `disabled`, see [§6 Troubleshooting](#6-troubleshooting).

---

## 3. Onboarding a new lord

A **lord** is a container (or bare-metal host) that runs `lord`
binary, advertises a hostname + CPU shares, and executes tenant
processes when asked. No state, no auth, no persistent data.

### 3.1. Onboard a Docker container (the typical case)

Edit `test/mvp/docker-compose.yml` (or your own compose):

```yaml
  lord-my-new-box:
    image: etronium-mvp:runtime
    command:
      - --hostname=lord-my-new-box
      - --scheduler=frontend:51061
      - --advertise-cpu=8        # NUMA-overcommit CPU shares (1..16)
      - --advertise-mem=0        # 0 = use physical RAM
    privileged: true
    pid: host
    cgroupns: private
    volumes:
      - /sys/fs/bpf:/sys/fs/bpf:rw     # share host bpffs
      - /sys/kernel/debug:/sys/kernel/debug:ro
      - /boot:/boot:ro                  # libbpf feature detect
    restart: unless-stopped
```

Bring it up:

```bash
docker compose up -d lord-my-new-box
docker logs lord-my-new-box
# Look for "lord registered" lines — gRPC stream to frontend established
```

Verify from frontend:

```bash
docker exec mvp-frontend /usr/local/bin/scheduler stats
# Should now show 6 in etr_lord_cpus / etr_lord_dsq (was 5)
```

### 3.2. Onboard a bare-metal host

Bare-metal lords are first-class — but they need the BPF/host coupling:

```bash
# On the new host:
sudo apt install -y golang-go clang libbpf-dev linux-tools-$(uname -r)
git clone https://github.com/midas/Etronium-Scdr.git
cd Etronium-Scdr && make build
sudo cp bin/lord /usr/local/bin/lord
```

Run lord directly:

```bash
sudo /usr/local/bin/lord \
    --hostname=office-rig-01 \
    --scheduler=etronium.example.com:51061 \
    --advertise-cpu=8 \
    --log=info
```

The bare-metal lord will:
1. Connect gRPC stream to scheduler
2. Receive `RegisterLord` (sends hostname + CPU shares)
3. Scheduler writes BPF maps `etr_lord_cpus` and `etr_lord_dsq`
4. Start receiving `SpawnProcess` commands

Lord has NO persistent state. On restart, it re-registers and the
WAL replay restores any in-flight tenants.

### 3.3. NUMA-overcommit (advertising capacity)

Two flags control what the scheduler believes about this lord:

- `--advertise-cpu=N` — how many CPU shares (1..16). Scheduler uses
  this for the `etr_lord_cpus` BPF map (`(1<<N)-1` mask) and weighted
  placement. Default `0` → scheduler sees only 1 CPU (physical
  default).
- `--advertise-mem=N` — RAM bytes. Default `0` → use physical RAM.

Example — a 4-core school PC that's 25% loaded for Etronium:

```bash
lord --hostname=classroom-04 \
     --advertise-cpu=1 \         # treat as 1 share
     --advertise-mem=0           # but unlimited memory
```

A beefy workstation that wants the scheduler to consider it 8x:

```bash
lord --hostname=render-rig \
     --advertise-cpu=8 \
     --advertise-mem=0
```

### 3.4. Lord failure handling

Lords do not persist state. When a lord crashes:

1. TCP keepalive expires on scheduler side (default 10s).
2. Scheduler marks lord `unhealthy` in registry.
3. WAL replay for that lord becomes pending.
4. Tenant processes that lord had are **re-spawned on a healthy lord**
   (failure-tolerant restart, NOT CRIU live migration — see §7).
5. Once the failed lord comes back, it re-registers; processes stay
   where they were placed on the recovery.

### 3.5. Draining a lord (graceful drain)

Drain = mark lord as "don't place new tenants here; migrate existing
off":

```bash
# Phase 2+ feature — flag not implemented yet
# (See roadmap, [§7 Phase rollout](#7-phase-rollout))
```

---

## 4. Setting up a tenant

A **tenant** is anyone with access to the `tenant` CLI binary and
knows the scheduler gRPC address. Real production needs auth — see
[§6.3](#63-current-security-absence) for current state.

### 4.1. Install `tenant` CLI

```bash
# Option A — ship the same binary as frontend
cp bin/etronium /usr/local/bin/tenant

# Option B — minimal tenant only (built separately; Phase 2)
```

`tenant` is just the `etronium` binary renamed:

```bash
docker exec mvp-frontend /usr/local/bin/etronium process --help
# Rename to tenant for clarity (matches docs):
ln -sf /usr/local/bin/etronium /usr/local/bin/tenant
```

### 4.2. Point at the scheduler

The scheduler listens on `:51061` by default. Tenants need to know:

```bash
export ETRONIUM_SCHEDULER=etronium.example.com:51061
tenant --scheduler "$ETRONIUM_SCHEDULER" lords
```

Or per-call:

```bash
tenant --scheduler localhost:51061 process list
```

The default is `localhost:51061` for the `tenant` CLI and `:51061`
for the `scheduler` server. If you change the port on the server side
via `--addr=:50061`, tenants must override with `--scheduler=…:50061`.
Env var `SCHEDULER_LISTEN` controls server side,
`ETRONIUM_SCHEDULER` controls tenant side.

### 4.3. Run your first process

```bash
# Spawn a long-running task (just like on a Linux VPS)
tenant process spawn \
    --exec /bin/sleep --arg 3600

# JSON output for scripts
tenant process spawn \
    --exec /usr/bin/python3 --arg "-c" --arg "print('hi')" \
    --json

# Spawn on a specific lord (soft affinity)
tenant process spawn --exec /bin/sleep --arg 60 \
    --prefer-lord lord-edge-X
```

### 4.4. Inspect and manage

```bash
tenant process list                  # all my processes globally
tenant process get <process_id>      # state, lord_id, local_pid
tenant process wait <process_id>     # block until exit (returns exit code)
tenant process kill <process_id>     # SIGTERM
tenant process kill <process_id> --signal SIGKILL
tenant process migrate <process_id>  # re-spawn on a different lord
tenant lords                         # see registered lords (opaque IDs)
```

### 4.5. Resources (Phase 1+, partial)

```bash
# CPU/memory soft hint — scheduler passes to lord
tenant process spawn \
    --exec /bin/python3 \
    --resources '{"cpu_shares":100,"mem_limit_bytes":104857600}' \
    --arg "-c" --arg "import time; time.sleep(60)"
```

(Right now this is honored on the lord side via cgroups, but the BPF
scheduler doesn't yet account for it in `etr_lord_cpus` mask.)

---

## 5. Operating the scheduler

### 5.1. Run the BPF scheduler (schedulerd)

`schedulerd` runs **inside the frontend container** in MVP, but it
operates on the **host kernel**. That's the magic.

```bash
schedulerd \
    --bpf-obj=/opt/etronium/bpf/sched_ext_final.o \
    --link-dir=/sys/fs/bpf/etronium/sched \
    --v
```

Output on startup:

```
schedulerd starting bpf_obj=/opt/etronium/bpf/sched_ext_final.o
scheduler already registered (link pin exists)   # or "scheduler registered"
pinned map name=etr_task_lord id=3270 path=/sys/fs/bpf/etronium/maps/etr_task_lord
pinned map name=etr_lord_cpus  id=3271 path=/sys/fs/bpf/etronium/maps/etr_lord_cpus
pinned map name=etr_lord_dsq   id=3272 path=/sys/fs/bpf/etronium/maps/etr_lord_dsq
pinned map name=etr_lord_stats id=3290 path=/sys/fs/bpf/etronium/maps/etr_lord_stats
sched_ext state=enabled
```

### 5.2. Scheduler observability

`scheduler stats` — main observability entry point:

```bash
docker exec mvp-frontend /usr/local/bin/scheduler stats
# Output:
#   sched_ext state: enabled
#   nr_rejected:     0
#   enable_seq:      56
#   BPF map entries:
#     etr_task_lord        2     <- tenant processes currently routed
#     etr_lord_cpus        6     <- lords with masks (5 normal + 1 stale)
#     etr_lord_dsq         6
#     etr_lord_stats       1     <- global counter entries
#   Per-lord counters:
#     lord_id  select_cpu  enqueue  dispatch  reject
#     1        3275        0        0         0        <- global proxy
```

For JSON (Prometheus-friendly):

```bash
scheduler stats --json
```

### 5.3. Live migration (rebalance)

Triggered by the operator:

```bash
scheduler migrate --hostname lord-edge-X --shares 4
# → BPF map etr_lord_cpus[lord-edge-X] = 0x1 → 0xF (4 CPUs now available)

scheduler migrate --hostname lord-iot-Y --shares 16
# → full capacity
scheduler migrate --hostname lord-iot-Y --shares 1
# → contract back
```

Live migration is **immediate on the BPF map** but **lazy on the
lord side** — it changes the mask for new task placement and live
tasks will be re-dispatched on next wakeup. Full CRIU live migration
(not a restart) is Phase 2+.

### 5.4. Where state lives

| Data | Lives in | Lost on |
|------|----------|---------|
| Process table (process_id → lord + pid) | Frontend memory + WAL | Frontend crash (WAL replay restores) |
| BPF sched_ext registration | Host kernel + bpffs | Host reboot |
| BPF maps (`etr_*`) | Host kernel + bpffs | Host reboot |
| Lord state (what tasks it has) | Lord in-memory (ephemeral) | Lord crash — re-registers empty |
| Tenant data | Tenant's processes | Tenant process exit |
| schedulerd config | /sys/fs/bpf/etronium/ | bpffs no mounted |

---

## 6. Troubleshooting

### 6.1. sched_ext state stays `disabled`

```bash
cat /sys/kernel/sched_ext/state
# disabled
```

Common causes:
- `schedulerd` not running (or crashed). Check `docker logs mvp-schedulerd`.
- `/sys/fs/bpf` not mounted. `mount | grep bpf`.
- `sched_ext_final.o` missing or wrong arch. Check
  `file bin/sched_ext_final.o` → `ELF 64-bit LSB relocatable, eBPF`.

Fix:
```bash
docker rm -f mvp-schedulerd
docker run --rm --privileged --pid=host -v /sys/fs/bpf:/sys/fs/bpf \
    alpine sh -c "rm -rf /sys/fs/bpf/etronium && etronium-bpftool struct_ops unregister etronium_sched || true"
docker compose -f test/mvp/docker-compose.yml up -d schedulerd
sleep 5
cat /sys/kernel/sched_ext/state
```

### 6.2. Duplicate map IDs after restart

If you see `etr_lord_cpus` ID 3283 AND 3468 in `bpftool map list`
— that's an orphaned scheduler from a `kill -9` (without cleanup).
schedulerd's orphan-cleanup (ADR-043) handles this on next start.

### 6.3. Current security absence

**MVP has NO authentication, NO encryption, NO tenant isolation
beyond the optional `--tenant` flag.** Anyone with gRPC access to
scheduler can spawn processes as any tenant. **Internal deployment
only.** Production needs:

- mTLS between tenant↔scheduler and lord↔scheduler
- Tenant authn (token, OIDC)
- Tenant→cgroup isolation on each lord
- Network namespace isolation

These are Phase 3+ in the roadmap.

### 6.4. Lord doesn't show up in BPF map

```bash
docker logs mvp-lord-XX | grep "lord registered in BPF maps"
# If missing, scheduler couldn't update BPF — check:
docker logs mvp-frontend | grep -E "RegisterLordBPF|BPF"
docker exec mvp-schedulerd \
    /usr/local/bin/etronium-bpftool map list | grep etr_lord_cpus
```

Typical cause: scheduler hadn't yet mapped `etr_lord_cpus` (slight
race on cold-start). Restarting schedulerd after the first lord
registration fixes it.

### 6.5. Process spawn returns but no lord has it

```bash
# Check where the scheduler placed it
docker exec mvp-frontend /usr/local/bin/tenant process list
# → process_id, lord_id (opaque ULID)
# That's the scheduler's internal ID, not a hostname.

# Check real lords in BPF map
docker exec mvp-schedulerd \
    /usr/local/bin/etronium-bpftool map dump pinned /sys/fs/bpf/etronium/maps/etr_lord_cpus
```

If the lord_id list is empty but the process_id exists — the lord
crashed mid-spawn. WAL replay will respawn on a healthy lord.

---

## 7. Phase rollout

| Capability | Status | Notes |
|------------|--------|-------|
| `tenant process spawn/kill/list/wait/get` | ✅ working | MVP demo |
| `tenant process migrate` (re-spawn) | ✅ working | Phase 3.4 |
| `scheduler stats` | ✅ working | Phase 4 |
| `scheduler migrate --hostname X --shares N` | ✅ working | Phase 3.5 |
| BPF sched_ext enabled | ✅ working | kernel 7.0+ |
| SCHED_EXT policy on lord-spawned tasks | ✅ working | Phase 5 |
| CRIU live migration (no restart) | ❌ Phase 2+ | WIP design doc |
| weighted placement (multi-resource) | ⚠️ code exists, CLI flag TBD | Phase 2 |
| mTLS / authn / tenant isolation | ❌ Phase 3+ | |
| Lord pre-flight (CPU/mem/disk checks) | ❌ Phase 2+ | |
| Multi-frontend / sharded scheduler | ❌ Phase 4+ | |

See [docs/ROADMAP.md](./ROADMAP.md) for the full timeline.

---

## 8. Day-N ops checklist

- [ ] `docker ps` shows all containers UP
- [ ] `cat /sys/kernel/sched_ext/state` → `enabled`
- [ ] `scheduler stats` → `nr_rejected = 0`, ≥1 entry per `etr_*` map
- [ ] `tenant process spawn` works
- [ ] `scheduler migrate` works (BPF map updates visible)
- [ ] Lord crash test: `docker stop mvp-lord-XX` → `tenant process list`
      shows processes re-spawned on a healthy lord
- [ ] WAL recovery: `docker restart mvp-frontend` → processes
      restored from `etronium-mvp-data` volume

---

## Appendix A. Environment variables reference

| Var | Default | Used by | Purpose |
|-----|---------|---------|---------|
| `SCHEDULER_WAL_PATH` | `/tmp/etronium/frontend.wal` | frontend | Process table WAL |
| `SCHEDULER_RECOVERY_DEBOUNCE` | `10s` | frontend | Wait for lord register after crash |
| `--scheduler` flag | `localhost:50061` (note: tenant default is wrong, see §4.2) | tenant/lord | gRPC target |
| `--tenant` flag | `anonymous` | tenant | Tenant identifier |
| `--hostname` flag | `os.Hostname()` | lord | BPF-map hostname hash |

## Appendix B. Kernel version quirks

- **Kernel 7.0**: BPF `select_cpu`/`enqueue` callbacks are not called for
  SCHED_EXT policy tasks when `/sys/kernel/sched_ext/switch_all=1`.
  Counters increment via the `init_task` proxy instead. Full per-lord
  counters activate on kernel upgrades where this is fixed.
- **`/sys/kernel/sched_ext/switch_all`**: read-only on 7.0 (kernel
  design). Tasks opt-in via `sched_setscheduler(SCHED_EXT, 0,
  &param)` from userspace — that's what lord does.
- **`bpf_bpfel.go` bpf2go**: not used. We compile BPF via
  `clang -target bpf` directly (bpf2go was killing R_BPF_64_ABS64
  relocations in our version).

---

Last updated: 2026-07-22 (Phase 6 + cleanup complete).
