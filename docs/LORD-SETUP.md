# Lord Onboarding Checklist

When you add a new lord (a new machine that wants to contribute
compute capacity to the Etronium fleet), this is the canonical
sequence. For the high-level architecture or operator view, see
[OPS-GUIDE.md](./OPS-GUIDE.md).

---

## TL;DR

```bash
# On the new host, as root (or equivalent):
apt install golang-go libbpf-dev linux-tools-$(uname -r)          # or yum/apk
git clone https://github.com/MidasWR/Etronium-Scdr.git
cd Etronium-Scdr && make build
install bin/lord /usr/local/bin/lord
lord \
    --hostname=$(hostname -s) \
    --scheduler=etronium.example.com:51061 \
    --advertise-cpu=$(nproc) \
    --log=info
```

That's it. There's no agent to register, no certificate to issue,
no DB to connect to.

---

## 1. Pre-flight

### 1.1. Local requirements

| Component | Min | Why |
|-----------|-----|-----|
| kernel | 7.0.0 | sched_ext |
| Go | 1.22+ | Build `lord` |
| clang + libbpf-dev | 14+ | (Already built in MVP image — skip for binary install) |
| `linux-tools-$(uname -r)` | n/a | `bpftool` (only if running schedulerd, not lord) |
| `CAP_SYS_ADMIN` | required | mount bpffs / open BPF maps |

### 1.2. Network access

The lord opens **outbound** TCP to the scheduler. No need to expose
the lord externally. Outbound `etronium.example.com:51061` (or
wherever your scheduler is) must be reachable.

For the BPF scheduler to route the lord's tasks, the lord container
(via Docker) needs `--privileged` + `/sys/fs/bpf` mounted. Bare-metal
lords share the host kernel by definition.

### 1.3. The gRPC listener

Scheduler gRPC by default listens on `:51061`. Confirm before
onboarding:

```bash
echo "GRPC:          $(grep -E '^\s+- addr=' cmd/scheduler/main.go | head -1)"
# Default: ":50061" — but MVP run script overrides to :51061. Double
# check with your operator.
```

## 2. Build / install `lord`

### 2.1. From source

```bash
git clone https://github.com/MidasWR/Etronium-Scdr.git
cd Etronium-Scdr
go build -o bin/lord ./cmd/lord
sudo install -m 0755 bin/lord /usr/local/bin/lord
```

### 2.2. From prebuilt image (Docker case)

Just use the runtime image — it has `lord` baked in:

```yaml
services:
  lord-new-box:
    image: etronium-mvp:runtime
    command:
      - --hostname=lord-new-box
      - --scheduler=frontend:51061
      - --advertise-cpu=8
    privileged: true
    pid: host
    cgroupns: private
    volumes:
      - /sys/fs/bpf:/sys/fs/bpf:rw
      - /sys/kernel/debug:/sys/kernel/debug:ro
      - /boot:/boot:ro
```

```bash
docker compose up -d lord-new-box
```

## 3. Run lord

### 3.1. Modes of execution

**Foreground (debug, development):**

```bash
sudo lord \
    --hostname=lord-school-D \
    --scheduler=etronium.example.com:51061 \
    --advertise-cpu=4 \
    --log=debug
```

**systemd service (production):**

```bash
# /etc/systemd/system/etronium-lord.service
[Unit]
Description=Etronium Lord
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/lord \
    --hostname=lord-rack-01 \
    --scheduler=etronium.example.com:51061 \
    --advertise-cpu=8 \
    --advertise-mem=0 \
    --log=info
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now etronium-lord.service
sudo journalctl -u etronium-lord -f
```

### 3.2. Flags reference

```
--hostname           Override os.Hostname. (Set when running multiple lords
                     in different containers/hostnames that share a kernel.)
--scheduler          gRPC address of the frontend, default localhost:50061.
--advertise-cpu      CPU shares for placement, 1..16. Default 0 (treated as 1).
--advertise-mem      RAM bytes for placement, 0=use physical.
--log                log level: debug|info|warn|error.
--log-format         tint (default, human-readable) or json (machine-readable).
```

### 3.3. NUMA-overcommit philosophy

`--advertise-cpu` is **a hint, not a hard limit**. It's how many
"shares" the scheduler treats this lord as having. A 4-core machine
in a busy lab can advertise `--advertise-cpu=1` to stay a low-priority
participant. A 32-core server can advertise `--advertise-cpu=8` (or
`-cpu=16` — capped at 16 by Etronium design) to be seen as a top
contributor.

`etr_lord_cpus` BPF map value will be `(1<<N) - 1`. So:

| advertise | mask | logical CPUs allowed |
|-----------|------|---------------------|
| 1 | 0x1 | cpu 0 only |
| 2 | 0x3 | cpus 0-1 |
| 4 | 0xF | cpus 0-3 |
| 8 | 0xFF | cpus 0-7 |
| 16 | 0xFFFF | cpus 0-15 |

The actual process threads still land on real cores per kernel scx
policy — they get the full host view inside the mask bits.

## 4. Verify the lord is registered

After lord is up (systemd or Docker), verify from the frontend:

```bash
# Check frontend logs for registration
docker logs mvp-frontend | grep "lord registered in BPF maps"
# {"time":"...","msg":"lord registered in BPF maps","lord_id":"01K...EH9",
#  "hostname":"lord-new-box","lord_id":3984688355,"cpu_mask":"0xF",
#  "dsq_id":"0xe77000000000e3"}

# Or via tenant CLI (operator uses the scheduler's admin tool)
docker exec mvp-frontend /usr/local/bin/scheduler stats
# Look at etr_lord_cpus entry count — it should grow by 1.
```

Or via BPF map directly:

```bash
docker exec mvp-schedulerd \
    /usr/local/bin/etronium-bpftool map dump pinned /sys/fs/bpf/etronium/maps/etr_lord_cpus
# [
#   { "key": 3984688355, "value": 15 }
# ]
```

## 5. Live-migration / reconfigure

The lord **does not need a restart** for capacity changes. The
operator runs:

```bash
docker exec mvp-frontend /usr/local/bin/
    --hostname lord-new-box \
    --shares 8
# → etr_lord_cpus[hash(lord-new-box)] = 0xFF
# → next spawn will route accordingly
```

## 6. Lord failure handling

Lord **does not persist anything**. On crash / restart:

1. TCP stream to scheduler dies.
2. Scheduler marks this lord `unhealthy` after keepalive timeout
   (default: 10s).
3. Processes that lived on this lord are **re-spawned on a healthy
   lord** (failure-tolerant restart, not CRIU).
4. Frontend WAL records the loss for audit.
5. When this lord comes back, it re-registers empty. Recovery
   restores only WAL-known in-flight processes that were targeting
   it (not arbitrary new ones).

For **graceful drain** (no current CLI; operator workflow):

1. Set `--advertise-cpu=1` (or lower) on this lord.
2. New spawns will prefer healthier lords.
3. When existing processes exit naturally, this lord empties out.
4. Eventually `kill -TERM lord` for hard drain.

## 7. Removing a lord

Just stop the lord process / docker container. Scheduler handles the
disconnect. No de-registration protocol needed.

If you want a clean removal from a fresh state:

```bash
# Stop the lord
docker stop mvp-lord-XX
# (or)
sudo systemctl stop etronium-lord.service

# Optionally clean BPF map entries for this lord (operator action):
docker exec mvp-schedulerd /usr/local/bin/etronium-bpftool \
    map delete pinned /sys/fs/bpf/etronium/maps/etr_lord_cpus key <hash>
```

## 8. Common issues

### 8.1. `connection refused` on lord startup

```bash
nc -zv etronium.example.com 51061
# Test the scheduler gRPC port from the lord
```

If OK but lord still fails, check scheduler logs:

```bash
docker logs mvp-frontend | grep -E "lord connection|lord=lord-new-box"
# Should see "lord registered"
```

### 8.2. Lord registered but no BPF map entry

Scheduler hasn't yet managed to call `RegisterLordBPF`. Check
schedulerd is running and `/sys/fs/bpf` is mounted.

### 8.3. Tasks spawned on this lord fail to get SCHED_EXT

`applySchedExt` in the lord exec.go uses raw syscall. If it errors
with `EINVAL`, the kernel doesn't recognize SCHED_EXT — kernel too
old or `/sys/kernel/sched_ext/state != enabled`. Verify:

```bash
cat /sys/kernel/sched_ext/state        # must be "enabled"
cat /sys/kernel/sched_ext/nr_rejected  # high = scheduler overloaded
```

---

Last updated: 2026-07-22.
