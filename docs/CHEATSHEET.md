# Etronium Cheatsheet

> One page. If you forget everything else, remember the **installer**.

## Install

```bash
# Operator installs the control-plane on machine 1.
curl -fsSL https://github.com/MidasWR/Etronium-Scdr/releases/latest/download/installer.sh | \
  sudo bash -s -- scheduler

# A new machine wants to be a lord.
curl -fsSL https://github.com/MidasWR/Etronium-Scdr/releases/latest/download/installer.sh | \
  sudo bash -s -- lord \
    --scheduler=etronium.example.com:51061 \
    --hostname=my-laptop \
    --advertise-cpu=4

# A user installs the tenant CLI on their laptop.
curl -fsSL https://github.com/MidasWR/Etronium-Scdr/releases/latest/download/installer.sh | \
  bash -s -- tenant \
    --scheduler=etronium.example.com:51061
```

That's it. There's no "register", no token, no YAML, no certificate
flow. Each role knows what to do with one command.

## After install

```bash
sudo systemctl status etronium-scheduler
sudo journalctl -u etronium-scheduler -f

sudo systemctl status etronium-lord
sudo journalctl -u etronium-lord -f
```

## Day-2 commands

```bash
# Fleet status (operator or anyone).
tenant status --scheduler=etronium.example.com:51061

# Token: create one for a new tenant (Phase 3+ — placeholder for now).
curl -fsSL .../installer.sh | sudo bash -s -- token new \
    --scheduler=etronium.example.com:51061

# Update an existing install in place.
curl -fsSL .../installer.sh | sudo bash -s -- update \
    --role=lord \
    --scheduler=etronium.example.com:51061 \
    --version=v0.1.1

# Uninstall.
sudo /usr/local/bin/installer.sh uninstall
```

## Tenant usage

```bash
tenant lords                                               # what lords exist
tenant run /bin/sleep 3600                       # spawn a process (flat CLI, v0.2.0+)
tenant shell --shell=/bin/sh                     # interactive TTY relay to a lord (v0.3.0+)
tenant ps                                        # all your processes
tenant get <id>                                    # detailed state
tenant wait <id>                                   # block on exit
tenant kill <id>                                   # SIGTERM
tenant attach --follow <id>                       # live stream process IO (debug)

```

## Scheduler-side

```bash
scheduler stats                                           # /sys/kernel/sched_ext + BPF maps
(autoscale handles this — env: ETRONIUM_THRESH_REBALANCE)       # rebalance CPU mask
```

## Troubleshooting

```bash
sudo journalctl -u etronium-scheduler -n 200
sudo journalctl -u etronium-lord -n 200
curl -fsSL .../installer.sh | sudo bash -s -- status --scheduler=HOST:PORT
```

See [OPS-GUIDE.md](./OPS-GUIDE.md) for full detail.
