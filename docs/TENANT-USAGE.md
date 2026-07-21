# Tenant Usage Guide

If you're a tenant using the Etronium VPS, this is your reference.
If you're an operator setting up tenants, see
[OPS-GUIDE.md](./OPS-GUIDE.md).

---

## What you get

A Linux VPS, plain and simple. From your terminal, it looks like
your processes run on one machine. They don't — the scheduler
distributes them across 5+ lord nodes transparently.

What you **don't** need to do:
- ❌ Pick a lord
- ❌ Set CPU affinities
- ❌ Worry about lord failures
- ❌ Rebalance after a lord dies

What you **don't** have that a real VPS has (Phase 2+ for these):
- ⚠️ Persistent home directory (yet)
- ⚠️ External IP bind (yet)
- ⚠️ Cross-host shared FS (yet)

---

## 1. Install the CLI

```bash
# Single binary — ~17 MB static Go binary
sudo install -m 0755 etronium /usr/local/bin/tenant
```

Or use the same binary the fronted uses:

```bash
docker cp mvp-frontend:/usr/local/bin/etronium /usr/local/bin/tenant
```

(If the binary is named `etronium`, create a `tenant` alias:
`ln -sf etronium /usr/local/bin/tenant`)

## 2. Point at your scheduler

```bash
export ETRONIUM_SCHEDULER=etronium.example.com:51061
tenant lords                         # list lords (opaque IDs)
```

If you see a connection refused error:

```bash
# Test connectivity
nc -zv etronium.example.com 51061

# Default port is now :51061 on both sides (server + tenant CLI).
# Override with --scheduler=<host>:<port> if your operator runs a non-default port.
tenant --scheduler=etronium.example.com:51061 lords
```

## 3. Spawn processes

### Basic

```bash
tenant process spawn --exec /bin/sleep --arg 3600
# output: process_id=01K...Y5A state=PROCESS_STATE_PENDING
```

### With arguments

```bash
tenant process spawn \
    --exec /usr/bin/python3 \
    --arg "-c" \
    --arg "print('hello from the VPS')"
```

### With output as JSON

```bash
tenant process spawn \
    --exec /bin/sleep --arg 30 \
    --json | jq .
```

### Soft-affinity to a specific lord

```bash
tenant process spawn \
    --exec /bin/python3 \
    --prefer-lord lord-edge-X \
    --arg "-c" --arg "import time; time.sleep(60)"
```

(Soft = the scheduler will prefer that lord but will move elsewhere
if it's not healthy or has no capacity.)

### With resource hints

```bash
tenant process spawn \
    --exec /usr/bin/python3 \
    --resources '{"cpu_shares":200,"mem_limit_bytes":1073741824}' \
    --arg "-c" --arg "import time; time.sleep(60)"
```

(`cpu_shares` = cgroup cpu.shares, `mem_limit_bytes` = cgroup
memory.max in bytes. 1 GiB = 1073741824.)

### Stateful: re-execute through crashes

```bash
tenant process spawn \
    --exec /usr/local/bin/example-stateful \
    --state-dump /var/lib/state.json \
    --max-restarts 5
```

`example-stateful` increments a counter and dumps to `--state-dump`
periodically. On lord failure, the process is re-spawned on a
different lord and reads the state back. `--max-restarts` caps the
restart count (default 10; -1 = unlimited).

## 4. List / inspect / wait / kill

### List your processes

```bash
tenant process list
# PROCESS_ID                  STATE                EXEC
# 01KY381G9HFV95121HGDK42S2F  PROCESS_STATE_RUNNING  /bin/sleep
# 01KY381GCNGXECJN63X8RP4MF6  PROCESS_STATE_RUNNING  /bin/sleep
# ... (across all 5 lords, you don't need to know which)
```

### Get a specific process

```bash
tenant process get 01KY381G9HFV95121HGDK42S2F
# state, lord_id (opaque ULID), local_pid, exec_path, argv, created_at
```

### Wait for completion (returns exit code)

```bash
tenant process spawn --exec /bin/sh --arg -c --arg "exit 42"
# process_id=01K...H
tenant process wait 01K...H
# exit_code=42
```

### Kill

```bash
tenant process kill 01K...H                  # SIGTERM (default)
tenant process kill 01K...H --signal SIGKILL # SIGKILL
```

### Force re-spawn on another lord

```bash
tenant process migrate 01K...H
# → kills the process on its current lord, re-spawns on a different lord.
# Note: this is NOT a transparent live migration — the process restarts
# from scratch. State-preserving migration (CRIU) is Phase 2+.
```

## 5. Tips

### Keep-alive style workloads

```bash
# Spawn a long-lived HTTP server
tenant process spawn \
    --exec /usr/bin/python3 \
    --arg -m --arg http.server --arg 8080

# (Caveat: 0.0.0.0:8080 binds on whatever lord got picked — that
# lord's IP. To expose through scheduler, Phase 2+ proxy is needed.)
```

### Distributed training (1 lord = N processes)

```bash
# Spawn N workers — they'll likely end up spread across lords via
# trivial placement. Add a tiny sleep between spawns to give the
# scheduler time to balance.
for i in 1 2 3 4 5 6 7 8; do
    tenant process spawn \
        --exec /usr/bin/python3 \
        --arg -c --arg "import time; time.sleep(3600)" &
    sleep 0.5
done
wait
```

(Round-robin placement isn't built — trivial algorithm gives whatever
Go map iteration order produces. Phase 2 will add weighted
placement.)

### State dumps survive lord death

```bash
# If your process writes state to a periodic file, the lord passes
# that path through $ETRONIUM_STATE_DUMP env var.
tenant process spawn \
    --exec /usr/bin/python3 \
    --arg -c --arg "import os, json; print(os.environ['ETRONIUM_STATE_DUMP'])" \
    --state-dump /var/lib/state.json
# → /var/lib/state.json (resolved on the lord side)
```

## 6. Common errors

| Error | Cause | Fix |
|-------|-------|-----|
| `connection refused` | Wrong `--scheduler` | Use `--scheduler=…:51061` |
| `rpc error: Unavailable` after a minute | Scheduler crashed | Check `docker logs mvp-frontend` |
| `no lords healthy` | All lords disconnected | Check `tenant lords`, retry lords |
| Process state `PROCESS_STATE_RESTARTING` repeatedly | Lord keeps crashing on this process | Check `--max-restarts` or process logic |
| `process not found` | Wrong `process_id` (case-sensitive ULID) | Re-run `tenant process list` and copy fresh |

## 7. Security

**MVP has no auth.** Anyone who can reach the scheduler gRPC port
can act as any tenant. The `--tenant` flag is set per-call but is
**not verified**. Production deployment requires:

- mTLS termination in front of the scheduler (nginx, envoy)
- Per-tenant API tokens at the proxy
- Per-tenant cgroup on each lord (Phase 3+)

---

Last updated: 2026-07-22.
