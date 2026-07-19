# Stack — конкретные библиотеки и версии

> Источник: `docs/RESEARCH.md`. Здесь фиксируем **что именно** берём.

## Go версия

- **Go 1.26+** (на машине у нас 1.22.2 — ок).

## RPC и сериализация

| Библиотека | Зачем |
|---|---|
| `google.golang.org/grpc` | gRPC server/client |
| `google.golang.org/protobuf` | protobuf runtime |
| `google.golang.org/grpc/stream` | bidirectional streaming |

## CLI

| Библиотека | Зачем |
|---|---|
| `github.com/spf13/cobra` | CLI framework для `etronium` (tenant client) |
| `github.com/spf13/viper` | иерархия конфигов (пока не используем — `flag` + env хватает) |

## Container runtime

| Библиотека | Зачем |
|---|---|
| `github.com/containerd/cgroups/v3` | cgroups v2 управление (мы писали свой в `../Etronium` — здесь берём готовое) |
| `github.com/containerd/containerd/v2` | container runtime client |
| `github.com/opencontainers/runtime-spec/specs-go` | OCI spec типы |
| `github.com/opencontainers/go-digest` | image digests |
| `github.com/opencontainers/image-spec` | image manifest types |
| `github.com/containerd/continuity/fs` | fs utilities |

## Утилиты

| Библиотека | Зачем |
|---|---|
| `github.com/oklog/ulid/v2` | task IDs (sortable, k8s-style) |
| `github.com/google/uuid` | UUID v4 для session IDs |
| `go.uber.org/zap` | structured logging |
| `github.com/prometheus/client_golang` | метрики |
| `github.com/lmittmann/tint` | pretty colored logs для dev |
| `github.com/stretchr/testify` | тесты |

## Что НЕ используем в MVP

> Каждый пункт — осознанный отказ с обоснованием. Если захотим добавить — отдельное решение в `DECISIONS.md`.

- ❌ `github.com/hashicorp/raft` — Raft consensus не нужен
- ❌ `github.com/etcd-io/etcd` — external KV не нужен
- ❌ `github.com/nats-io/nats.go` — message bus не нужен
- ❌ `github.com/temporalio/sdk-go` — workflow engine overkill
- ❌ Любые ORM, DB драйверы — БД нет в MVP, in-memory state + WAL
- ❌ `github.com/go-chi/chi` — для gRPC server не нужен HTTP-роутер
- ❌ `github.com/jackc/pgx` — Postgres не используем
- ❌ WebUI — отдельный трек, CLI хватит для MVP

## Аналоги для референса

- **HashiCorp Nomad** — Go, ~40k LOC scheduler (placement, eval broker, alloc runner)
- **containerd** — Go, ~150k LOC, модульно (shim, CRI, snapshotter)
- **runc + libcontainer** — Go, ~20k LOC, низкий уровень
- **Fly.io FlyD** — закрытый, но публичные посты по архитектуре
- **BOINC** — золотой стандарт volunteer computing
