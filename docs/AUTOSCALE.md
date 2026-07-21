# Etronium-Scdr — Autoscale (ABS_AUTO planner)

**Все миграции и ребалансы делает scheduler сам.** Никаких ручных
команд `tenant migrate` или `scheduler migrate` нет — всё на
автомате через periodic loop внутри scheduler'а.

## Что делает autoscale

Каждые 30 секунд (default) scheduler:

1. Считывает метрики для каждого lord'а:
   - **CPU**: `/sys/fs/cgroup/etronium/<lord_id>/cpu.usage_usec`
   - **Memory**: `/sys/fs/cgroup/etronium/<lord_id>/memory.current / memory.max`
   - **Reject rate**: `etr_lord_stats` BPF map (per-lord u64 counter)
   - **Heartbeat freshness**: in-memory `LastHeartbeat` time
   - **Task count**: `Lord.ActiveProcesses` from Register message

2. Считает **score** каждого lord'а:

   ```
   score = 0.6 * cpu_ratio
         + 0.2 * mem_ratio
         + 0.1 * task_count_ratio
         + 0.1 * reject_rate
   ```

   где `cpu_ratio` ∈ [0, 1], `mem_ratio` ∈ [0, 1] и т.д.

3. Принимает **decisions**:

   | Триггер | Действие |
   |---|---|
   | `cpu_ratio > 0.80` на lord X | migrate coldest process с X на coldest lord |
   | `reject_rate > 0.05` на lord X | blacklist X на 60s (новые tenants не сюда) |
   | `max(score) - min(score) > 0.30` | rebalance across all lords |
   | `heartbeat_age > 60s` для X | mark X dead, WAL replay переезжает процессы |
   | new lord registered | backfill (новые tenants предпочитают этот lord) |

4. **Anti-flapping** (чтобы не пинг-понг):
   - **Cooldown**: после миграции с X следующая миграция с X только через 60s
   - **Hysteresis**: target не "lowest score", а "within 5% of lowest"
   - **Rate limit**: max 5 миграций в минуту (глобально)

## Конфигурация (через ENV)

| Переменная | Default | Описание |
|---|---|---|
| `ETRONIUM_AUTOSCALE` | `true` | `false` чтобы полностью выключить |
| `ETRONIUM_AUTOSCALE_INTERVAL` | `30s` | Период цикла |
| `ETRONIUM_AUTOSCALE_COOLDOWN` | `60s` | Минимальный интервал между миграциями с одного lord'а |
| `ETRONIUM_AUTOSCALE_MAX_PER_MIN` | `5` | Глобальный rate limit миграций |
| `ETRONIUM_SCORE_CPU` | `0.6` | Вес CPU в score |
| `ETRONIUM_SCORE_MEM` | `0.2` | Вес memory |
| `ETRONIUM_SCORE_TASK` | `0.1` | Вес task count |
| `ETRONIUM_SCORE_REJECT` | `0.1` | Вес BPF reject rate |
| `ETRONIUM_SCORE_HYST` | `0.05` | Hysteresis band (5% of lowest = tied) |
| `ETRONIUM_THRESH_OVERLOAD_CPU` | `0.80` | CPU > 80% → migrate from |
| `ETRONIUM_THRESH_REBALANCE` | `0.30` | max-min score delta > 30% → rebalance |
| `ETRONIUM_THRESH_BLACKLIST` | `0.05` | reject > 5% → blacklist 60s |
| `ETRONIUM_BLACKLIST_DURATION` | `60s` | Срок blacklist |
| `ETRONIUM_DEAD_GRACE` | `60s` | Heartbeat grace period |

Все настройки через ENV. Никаких YAML / TOML / ConfigMap — потому что
production deploy = Kubernetes, и там конфиги приходят через env
от container orchestrator'а. Если нужно больше гибкости, можно
прокинуть env через `env:` в docker-compose или `env:` в K8s manifest.

## Что НЕ делает autoscale (намеренно)

- **Не убивает процессы** tenant'а. Tenant сам решает когда `tenant kill`.
- **Не трогает** процессы с affinity pinning (только tenant знает почему).
- **Не делает** live migration через CRIU. Только fault-tolerant restart.
- **Не масштабирует** lord'ов (нет смысла — это external machines).

## Логи

Каждое решение логируется как `INFO` с полями:

```json
{
  "level": "INFO",
  "msg": "autoscale decision",
  "action": "migrate",
  "from": "01KY...",
  "to": "01KY...",
  "reason": "overload cpu=0.870 > 0.800"
}
```

`action` ∈ `{migrate, rebalance, blacklist, noop}`. Ищите в логах
scheduler'а:

```bash
docker logs mvp-frontend 2>&1 | grep autoscale
```

## Проверка autoscale активен

```bash
docker logs mvp-frontend --tail=30 2>&1 | grep "autoscale started"
# → {"time":"...","level":"INFO","msg":"autoscale started",
#    "interval":30000000000,"overload_cpu":0.8,"rebalance_delta":0.3}

# E2E тест:
make e2e-bpf
# → Phase 5: autoscale enabled + scheduler aware ✓
```

## Unit тесты

```bash
go test ./internal/scheduler/ -count=1
# → ok  github.com/midas/Etronium-Scdr/internal/scheduler  0.009s
```

Покрывает:
- `TestDefaultAutoscaleConfig` — все дефолты совпадают с этой докой
- `TestDecideOverloadTriggersMigrate` — overload → migrate decision
- `TestDecideNoActionWhenBalanced` — баланс → noop
- `TestDecideBlacklistOnHighReject` — reject > threshold → blacklist
- `TestDecideRespectsCooldown` — cooldown подавляет миграцию
- `TestDecideRateLimit` — rate limit работает
- `TestPickColdestSkipsBlacklistedAndSelf` — coldest pick фильтрует blacklist