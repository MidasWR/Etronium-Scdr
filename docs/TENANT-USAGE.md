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
# One-line install (v0.3.0+):
curl -fsSL https://github.com/MidasWR/Etronium-Scdr/releases/download/v0.3.0/installer.sh | \
  bash -s -- tenant --scheduler=etronium.example.com:51061

# Verify:
tenant version
# → etronium v0.3.0
```

## 2. Point at your scheduler

```bash
export ETRONIUM_SCHEDULER=etronium.example.com:51061
tenant lords                         # list lords (opaque IDs)
```

If you see a connection refused error:

```bash
# Test connectivity
nc -zv etronium.example.com 51061

# Default port is :51061 on both sides (server + tenant CLI).
# Override with --scheduler=<host>:<port> if your operator runs a non-default port.
tenant --scheduler=etronium.example.com:51061 lords
```

## 3. Spawn processes

### Basic

```bash
tenant run /bin/sleep 3600
# output: process_id=01K...Y5A state=PROCESS_STATE_RUNNING
```

### With arguments

```bash
tenant run /usr/bin/python3 -c "print('hello from the VPS')"
```

### With resource hints

```bash
# Reserve 1 CPU + 512 MB RAM (sizing hint, scheduler may override)
tenant run --cpu-shares=100 --mem-mb=512 /usr/bin/python3 -m http.server 8080
```

### Soft-affinity to a lord

```bash
tenant run --prefer-lord=01KY3CXN76XK3R7ZB5G6FWT0RY /bin/sleep 3600
```

### Output as JSON

```bash
tenant --json run /bin/sleep 60
```

## 4. Interactive shell on a lord (v0.3.0+)

This is the killer feature. Write `apt-get` in your terminal, it
runs on a lord and you see the result — as if it were local.

```bash
tenant shell --shell=/bin/sh
# [etronium shell] connected to lord=01KY3D75X6HT93SC0FSW11R3DP process_id=01K...Z pid=0
# /bin/sh: 0: can't access tty; job control turned off
$ echo HELLO_FROM_LORD
HELLO_FROM_LORD
$ uname -n
midas-ThinkPad-E14-Gen-5              # lord hostname (you see the lord it's on)
$ ls -la / | head
drwxr-xr-x   1 root root 4096 Jul 21 22:37 .
drwxr-xr-x   1 root root 4096 Jul 21 22:37 .dockerenv
lrwxrwxrwx   1 root root    7 Jun 27 02:04 bin -> usr/bin
$ apt-get install -y htop
Reading package lists...
Building dependency tree...
E: Unable to locate package htop      # alpine doesn't have htop, but the
                                     # command ran on the lord!
$ exit
# [etronium shell] disconnected
```

How it works:
- tenant reads your terminal stdin in raw mode (Ctrl-C / Ctrl-D go to the shell)
- Each chunk → `WriteStdin` RPC → scheduler → lord's `process.StdinPipe.Write`
- Process stdout/stderr → ring buffer → `StreamProcessIO` follow (100ms poll) → your terminal
- Ctrl-D → EOF → bash exits gracefully

Flags:
- `--shell <path>` — which shell to spawn on the lord (default `/bin/bash`)
- `--prefer-lord <id>` — soft-affinity to a lord (overrides scheduler choice)

## 5. Attach to a running process (debug)

Like `kubectl attach` — dump captured IO or stream live:

```bash
tenant attach <process_id>              # one-shot dump of ring buffer
tenant attach --follow <process_id>     # live stream until process exits
```

Useful for:
- Seeing output of a process you spawned via `tenant run`
- Debugging without modifying spawn flags
- Watching long-running processes (e.g., `tenant attach --follow <nginx-pid>`)

## 6. List / inspect / wait / kill

### List your processes

```bash
tenant ps
# PROCESS_ID                  STATE                EXEC
# 01KY381G9HFV95121HGDK42S2F  PROCESS_STATE_RUNNING  /bin/sleep
# 01KY381GCNGXECJN63X8RP4MF6  PROCESS_STATE_RUNNING  /bin/sleep
# ... (across all 5 lords, you don't need to know which)

tenant ps --running                      # only RUNNING/PAUSED states
```

### Get a specific process

```bash
tenant get 01KY381G9HFV95121HGDK42S2F
# state, lord_id (opaque ULID), local_pid, exec_path, argv, created_at
```

### Wait for completion (returns exit code)

```bash
tenant run /bin/sh -c "exit 42"
# process_id=01K...H
tenant wait 01K...H
# exit_code=42
```

### Kill

```bash
tenant kill 01K...H                  # SIGTERM (default)
tenant kill 01K...H --signal SIGKILL # SIGKILL
```

## 7. Tips

### Keep-alive style workloads

```bash
# Spawn a long-lived HTTP server
tenant run /usr/bin/python3 -m http.server 8080

# (Caveat: 0.0.0.0:8080 binds on whatever lord got picked — that
# lord's IP. To expose through scheduler, Phase 2+ proxy is needed.)
```

### Distributed training (1 lord = N processes)

```bash
# Spawn N workers — they'll likely end up spread across lords via
# trivial placement. Add a tiny sleep between spawns to give the
# scheduler time to balance.
for i in 1 2 3 4 5 6 7 8; do
    tenant run /usr/bin/python3 -c "import time; time.sleep(3600)" &
    sleep 0.5
done
wait
```

(Round-robin placement isn't built — trivial algorithm gives whatever
Go map iteration order produces. Phase 2 will add weighted placement.
v0.2.0+ autoscale balances them automatically over time.)

### State dumps survive lord death

```bash
# If your process writes state to a periodic file, the lord passes
# that path through $ETRONIUM_STATE_DUMP env var.
tenant run --state-dump /var/lib/state.json \
    /usr/bin/python3 -c "import os, json; print(os.environ['ETRONIUM_STATE_DUMP'])"
# → /var/lib/state.json (resolved on the lord side)
```

### Interactive work in your editor

```bash
# Open vim on a lord to edit a file there (works because TTY raw mode
# relay sends every keystroke through):
tenant shell --shell=/usr/bin/vim /etc/some-config.conf
```

(Caveat: large screen redraws may lag because of the 100ms polling
window. For full-screen TUI apps, prefer running them in `tenant run`
with `attach --follow` from another terminal.)

## 8. Common errors

| Error | Cause | Fix |
|-------|-------|-----|
| `connection refused` | wrong scheduler | check `ETRONIUM_SCHEDULER` / `--scheduler` flag |
| `process <id> not found` | bad ID or scheduler restart lost state | `tenant ps` to see live IDs |
| `unknown lord <id>` | `--prefer-lord` for a lord not in registry | `tenant lords` to see current |
| `[etronium shell] disconnected: <err>` | process exited before relay attached | spawn fresh |

## 9. Security

See [OPS-GUIDE.md §7](./OPS-GUIDE.md#7-security--phase-3) — current
implementation has no authn (Phase 3+). Any tenant can read any
process's IO if they know the process_id. Don't put secrets in
plain stdin/stdout.

For v0.3.0: `tenant shell` traffic is **not encrypted end-to-end** —
scheduler is a trusted hop. Phase 3+ will add mTLS.