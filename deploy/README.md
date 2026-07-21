# Deploy — Etronium MVP

Деплой Etronium MVP в двух вариантах:
1. **docker-compose** — для dev/demo на одной машине
2. **Kubernetes (kustomize)** — для multi-node "почти-прод"

## 🚀 Quick Start (docker-compose, dev/demo)

```bash
# Build образ
./scripts/mvp/build-image.sh

# Запустить testbed (1 frontend + 5 lords + 2 tenants)
./scripts/mvp/up.sh -d

# Run demo (auto-placement + lord-A failure + recovery)
./scripts/mvp/demo.sh

# Teardown
./scripts/mvp/down.sh
```

## 🎯 Quick Start (Kubernetes, multi-node)

### Требования
- Кластер: k3s ≥ 1.28, minikube ≥ 1.32, или любой distribution k8s ≥ 1.27
- `kubectl` с контекстом на этот кластер
- Image `etronium-mvp:dev` в локальном registry (или `etronium/mvp:v0.1.0` для prod)

### Dev (single-node, debug)
```bash
# Build и push в локальный registry
./scripts/mvp/build-image.sh
docker tag etronium-mvp:runtime etronium-mvp:dev
# Для k3s: docker save etronium-mvp:dev | k3s ctr images import -

# Apply
kubectl apply -k deploy/k8s/overlays/dev

# Проверить
kubectl -n etronium-mvp-dev get pods -w
kubectl -n etronium-mvp-dev exec -it dev-tenant-acme-corp-<hash> -- \
  /usr/local/bin/etronium process spawn --exec=/bin/sleep --arg=300
```

### Prod-like (multi-node, scaled)
```bash
# Push в registry
docker tag etronium-mvp:runtime etronium/mvp:v0.1.0
docker push etronium/mvp:v0.1.0

# Apply
kubectl apply -k deploy/k8s/overlays/prod

# Wait for all pods
kubectl -n etronium-mvp wait --for=condition=Ready pod -l app.kubernetes.io/part-of=etronium-mvp --timeout=120s

# Tenant CLI
kubectl -n etronium-mvp exec -it deploy/tenant-acme-corp -- \
  /usr/local/bin/etronium process spawn --exec=/usr/local/bin/example-stateful
```

## 📋 Что включено

✅ **Phase 1 (текущий MVP)**:
- 1 frontend (gRPC :51061, WAL persistence, recovery logic)
- 5 lords (heterogeneous resources, cgroup per-lord)
- 2 tenants (acme-corp, acme-edu) — изолированы по tenant_id
- Stateful migration через shared volume + ETRONIUM_STATE_DUMP
- docker-compose для dev
- k8s manifests (kustomize) для multi-node
- Dev overlay (1 replica frontend, 3 lords) + prod overlay (2 frontend, 5 lords)

❌ **Не включено (Phase 2+)**:
- mTLS / TLS termination
- RBAC policies для tenant isolation на уровне k8s API
- NetworkPolicy для east-west traffic
- PodSecurityStandards (cyber)
- cert-manager + Let's Encrypt
- OPA / Kyverno для admission control
- HorizontalPodAutoscaler для lords (auto-scale по CPU)
- observability: Prometheus exporter, Grafana dashboards, Loki logs
- distributed tracing (Jaeger / Tempo)
- chaos engineering (chaos-mesh, litmus)
- service mesh (Istio / Linkerd)
- secret management (Vault / Sealed Secrets)

## 🧪 Acceptance criteria

После `./scripts/mvp/up.sh -d && ./scripts/mvp/demo.sh`:

- [ ] 5 lords зарегистрированы (`etronium lords`)
- [ ] 5 процессов spawned и RUNNING
- [ ] Kill одного lord → recovery <60s, все 5 процессов RUNNING
- [ ] Stateful процесс (`example-stateful`) сохраняет counter после recovery
- [ ] Tenant acme-corp не видит процессы acme-edu и наоборот

## 🏗 Архитектура

```
                 ┌──────────────┐
                 │   Tenant A   │  (acme-corp)
                 │   CLI/SDK    │
                 └──────┬───────┘
                        │ gRPC
                        ▼
┌─────────────────────────────────────────────┐
│             Etronium Frontend                │
│         (gRPC :51061 + WAL)                  │
│  • Tenant registry     • Lord registry       │
│  • Process table       • Placement           │
│  • Recovery (heartbeat TTL)                  │
│  • Cgroup v2 isolation per tenant           │
└──────┬─────────────┬─────────────┬──────────┘
       │             │             │
   gRPC stream   gRPC stream   gRPC stream
       │             │             │
       ▼             ▼             ▼
   ┌───────┐    ┌───────┐    ┌───────┐
   │ lord- │    │ lord- │    │ lord- │  ... (heterogeneous)
   │  01   │    │  02   │    │  03   │
   │ cgroup│    │ cgroup│    │ cgroup│
   └───────┘    └───────┘    └───────┘
       │             │             │
       └─────────────┴─────────────┘
                     │
              shared state dir
          (PVC ReadWriteMany, NFS/cephfs)
```

## 🔧 Конфигурация

Через env vars:
- `SCHEDULER_LISTEN` — gRPC listen address (default `:50061`)
- `SCHEDULER_HEARTBEAT_TTL` — lord unhealthy timeout (default 30s)
- `SCHEDULER_PLACEMENT` — `trivial` или `weighted` (default `trivial`)
- `SCHEDULER_WAL_PATH` — путь к WAL file (default `/tmp/etronium/frontend.wal`)
- `SCHEDULER_RECOVERY_DEBOUNCE` — debounce между recovery passes
- `SCHEDULER_CGROUP_ROOT` — root для cgroup slices (Phase B)

Через CLI flags (`lord`):
- `--scheduler` — frontend address
- `--hostname` — override hostname
- `--advertise-cpu` — NUMA-overcommit shares (0 = physical)
- `--advertise-mem` — NUMA-overcommit bytes (0 = physical)

Через CLI flags (`etronium`):
- `--scheduler` — frontend address (env: `ETRONIUM_SCHEDULER`)
- `--tenant` — tenant id (env: `ETRONIUM_TENANT`)

## 📦 Что в Phase 2

После стабильного MVP e2e — добавим:
- Auto-scaling lords через HPA по CPU utilization
- Multi-region deploy (3 кластера, federation)
- Phase 3+: cyber stack (см. "Не включено")