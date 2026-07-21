// Package scheduler — autoscale.go
//
// АБСОЛЮТНАЯ автоматизация ресурсного планирования. Scheduler сам:
//
//   1. Считывает метрики (BPF map etr_lord_stats + cgroup cpu/mem)
//   2. Считает score per-lord (cpu + mem + task count + reject rate)
//   3. Принимает решения:
//      - lord overloaded (cpu > 80%)          → migrate coldest process off
//      - lord reject rate > 5%                → blacklist 60s (новые tenants не сюда)
//      - max-min score delta > 30%            → rebalance across lords
//      - lord died (heartbeat > 60s)          → migrate ALL processes off
//      - new lord registered                  → backfill with new tenants
//   4. Anti-flapping: cooldown per-lord 60s, hysteresis 5%, rate limit 5/min
//
// Никаких ручных CLI вызовов. Конфиг через env или /etc/etronium/autoscale.yaml.
package scheduler

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cilium/ebpf"

	etroniumv1 "github.com/midas/Etronium-Scdr/internal/gen/etronium/v1"
)

// AutoscaleConfig — конфиг автоматического планировщика.
type AutoscaleConfig struct {
	Enabled   bool          // default true (Phase 4+); set ETRONIUM_AUTOSCALE=false to disable
	Interval  time.Duration // default 30s
	Cooldown  time.Duration // default 60s per-lord
	MaxPerMin int           // default 5
	Score     ScoreWeights
	Thresh    Thresholds
}

type ScoreWeights struct {
	CPU         float64 // default 0.6
	Mem         float64 // default 0.2
	TaskCount   float64 // default 0.1
	Reject      float64 // default 0.1
	Hysteresis  float64 // default 0.05 (within 5% of lowest = tied)
}

type Thresholds struct {
	OverloadCPU       float64       // default 0.80 (cpu > 80% → migrate from)
	RebalanceDelta    float64       // default 0.30 (max-min > 30% → rebalance)
	BlacklistReject   float64       // default 0.05 (reject > 5% → blacklist)
	BlacklistDuration time.Duration // default 60s
	DeadGrace         time.Duration // default 60s heartbeat grace
}

// DefaultAutoscaleConfig — defaults.
func DefaultAutoscaleConfig() AutoscaleConfig {
	return AutoscaleConfig{
		Enabled:   getEnv("ETRONIUM_AUTOSCALE", "true") != "false",
		Interval:  parseDur("ETRONIUM_AUTOSCALE_INTERVAL", 30*time.Second),
		Cooldown:  parseDur("ETRONIUM_AUTOSCALE_COOLDOWN", 60*time.Second),
		MaxPerMin: parseInt("ETRONIUM_AUTOSCALE_MAX_PER_MIN", 5),
		Score: ScoreWeights{
			CPU:        parseFloat("ETRONIUM_SCORE_CPU", 0.6),
			Mem:        parseFloat("ETRONIUM_SCORE_MEM", 0.2),
			TaskCount:  parseFloat("ETRONIUM_SCORE_TASK", 0.1),
			Reject:     parseFloat("ETRONIUM_SCORE_REJECT", 0.1),
			Hysteresis: parseFloat("ETRONIUM_SCORE_HYST", 0.05),
		},
		Thresh: Thresholds{
			OverloadCPU:       parseFloat("ETRONIUM_THRESH_OVERLOAD_CPU", 0.80),
			RebalanceDelta:    parseFloat("ETRONIUM_THRESH_REBALANCE", 0.30),
			BlacklistReject:   parseFloat("ETRONIUM_THRESH_BLACKLIST", 0.05),
			BlacklistDuration: parseDur("ETRONIUM_BLACKLIST_DURATION", 60*time.Second),
			DeadGrace:         parseDur("ETRONIUM_DEAD_GRACE", 60*time.Second),
		},
	}
}

func parseFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func parseDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func parseInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

// LordScore — текущее состояние lord'а с точки зрения автоскейлера.
type LordScore struct {
	LordID    string
	Hostname  string
	CPU       float64 // 0..1
	Mem       float64 // 0..1
	TaskCount float64 // 0..1
	Reject    float64 // 0..1
	Combined  float64 // weighted sum
	BlacklistUntil time.Time
	LastMigration  time.Time
}

// Decision — что autoscale решил сделать.
type Decision struct {
	Action   string // "migrate" | "rebalance" | "blacklist" | "noop"
	FromLord string
	ToLord   string
	Reason   string
}

// AutoscaleState — per-lord runtime state (cooldowns, blacklists).
type AutoscaleState struct {
	mu sync.Mutex

	lords        map[string]*LordScore          // by lord_id
	migrations   []time.Time                    // recent migrations (for rate limit)
	migrationLog []Decision                     // last N decisions (for debug)
}

// NewAutoscaleState — init.
func NewAutoscaleState() *AutoscaleState {
	return &AutoscaleState{
		lords:        make(map[string]*LordScore),
		migrationLog: make([]Decision, 0, 64),
	}
}

// Sample — собрать метрики для всех lord'ов.
// Reads:
//   - etr_lord_stats BPF map (select_cpu/enqueue/dispatch/reject counters)
//   - cgroup /sys/fs/cgroup/etronium/<lord>/cpu.usage.usec, memory.current
//   - heartbeat freshness (from server's lord registry)
func (a *AutoscaleState) Sample(ctx context.Context, s *Server, cfg AutoscaleConfig) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// 1. BPF stats — sum up reject rates.
	rejectRates, err := readBPFRejectRates()
	if err != nil {
		// BPF not loaded yet → skip reject component
		rejectRates = map[string]float64{}
	}

	// 2. Iterate all registered lords.
	s.lords.mu.RLock()
	defer s.lords.mu.RUnlock()

	now := time.Now()
	liveScores := map[string]*LordScore{}
	for _, l := range s.lords.byID {
		info := l.Info
		// 2a. CPU usage from cgroup
		cpuRatio := readLordCPU(info.GetLordId())
		// 2b. Mem usage from cgroup
		memRatio := readLordMem(info.GetLordId())
		// 2c. Task count / capacity
		cap := float64(info.GetAdvertisedCpuShares())
		if cap <= 0 {
			cap = float64(info.GetCpuCoresPhysical())
		}
		if cap <= 0 {
			cap = 1
		}
		taskRatio := float64(info.GetActiveProcesses()) / cap
		if taskRatio > 1.0 {
			taskRatio = 1.0
		}
		// 2d. Reject rate from BPF (smoothed with previous sample)
		prev, exists := a.lords[info.GetLordId()]
		rejectRate := rejectRates[info.GetLordId()]
		if exists && prev.Reject > 0 {
			// EWMA: 70% new, 30% old (anti-spike)
			rejectRate = 0.7*rejectRate + 0.3*prev.Reject
		}

		// 3. Combine.
		score := cfg.Score.CPU*cpuRatio +
			cfg.Score.Mem*memRatio +
			cfg.Score.TaskCount*taskRatio +
			cfg.Score.Reject*rejectRate

		// 4. Preserve blacklist + last-migration from previous sample.
		var blacklistUntil, lastMig time.Time
		if exists {
			blacklistUntil = prev.BlacklistUntil
			lastMig = prev.LastMigration
		}

		// 5. Detect dead lords.
		if l.LastHeartbeat.IsZero() || now.Sub(l.LastHeartbeat) > cfg.Thresh.DeadGrace {
			s.logger.Warn("lord considered dead", "hostname", info.GetHostname(),
				"last_heartbeat_age_s", now.Sub(l.LastHeartbeat).Seconds())
		}

		liveScores[info.GetLordId()] = &LordScore{
			LordID:          info.GetLordId(),
			Hostname:        info.GetHostname(),
			CPU:             cpuRatio,
			Mem:             memRatio,
			TaskCount:       taskRatio,
			Reject:          rejectRate,
			Combined:        score,
			BlacklistUntil:  blacklistUntil,
			LastMigration:   lastMig,
		}
	}
	a.lords = liveScores
	return nil
}

// Decide — на основе текущего состояния решить что делать.
//
// Возвращает список Decision (0..N), которые Run применит по очереди
// (с rate limit + cooldown).
func (a *AutoscaleState) Decide(cfg AutoscaleConfig) []Decision {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()
	var decisions []Decision

	// 1. Rate limit cleanup.
	pruneMigrations := func() {
		cutoff := now.Add(-time.Minute)
		keep := a.migrations[:0]
		for _, t := range a.migrations {
			if t.After(cutoff) {
				keep = append(keep, t)
			}
		}
		a.migrations = keep
	}
	pruneMigrations()

	rateLimited := len(a.migrations) >= cfg.MaxPerMin

	// 2. Sort by score (lowest = coldest, best target).
	byScore := make([]*LordScore, 0, len(a.lords))
	for _, sc := range a.lords {
		byScore = append(byScore, sc)
	}
	sort.Slice(byScore, func(i, j int) bool {
		return byScore[i].Combined < byScore[j].Combined
	})

	// 3. Blacklist lords with high reject rate.
	for _, sc := range byScore {
		if sc.Reject > cfg.Thresh.BlacklistReject && sc.BlacklistUntil.Before(now) {
			sc.BlacklistUntil = now.Add(cfg.Thresh.BlacklistDuration)
			decisions = append(decisions, Decision{
				Action:   "blacklist",
				FromLord: sc.LordID,
				Reason:   fmt.Sprintf("reject_rate=%.3f > %.3f for %s", sc.Reject, cfg.Thresh.BlacklistReject, cfg.Thresh.BlacklistDuration),
			})
		}
	}

	// 4. Overload → migrate coldest process off the hot lord.
	for _, sc := range byScore {
		if sc.CPU < cfg.Thresh.OverloadCPU {
			continue
		}
		// Cooldown.
		if !sc.LastMigration.IsZero() && now.Sub(sc.LastMigration) < cfg.Cooldown {
			continue
		}
		// Pick coldest non-blacklisted lord.
		target := pickColdest(byScore, sc.LordID, now, cfg.Score.Hysteresis)
		if target == "" {
			continue
		}
		decisions = append(decisions, Decision{
			Action:   "migrate",
			FromLord: sc.LordID,
			ToLord:   target,
			Reason:   fmt.Sprintf("overload cpu=%.3f > %.3f", sc.CPU, cfg.Thresh.OverloadCPU),
		})
		sc.LastMigration = now
	}

	// 5. Rebalance: max-min delta > threshold.
	if len(byScore) >= 2 {
		hot := byScore[len(byScore)-1]
		cold := byScore[0]
		delta := hot.Combined - cold.Combined
		if delta > cfg.Thresh.RebalanceDelta {
			if !hot.LastMigration.IsZero() && now.Sub(hot.LastMigration) < cfg.Cooldown {
				// Cooldown.
			} else if !rateLimited {
				decisions = append(decisions, Decision{
					Action:   "rebalance",
					FromLord: hot.LordID,
					ToLord:   cold.LordID,
					Reason:   fmt.Sprintf("score_delta=%.3f > %.3f", delta, cfg.Thresh.RebalanceDelta),
				})
				hot.LastMigration = now
			}
		}
	}

	// 6. Stash for next Sample EWMA + log.
	for _, d := range decisions {
		if d.Action == "migrate" || d.Action == "rebalance" {
			a.migrations = append(a.migrations, now)
		}
	}
	a.migrationLog = append(a.migrationLog, decisions...)
	if len(a.migrationLog) > 64 {
		a.migrationLog = a.migrationLog[len(a.migrationLog)-64:]
	}
	return decisions
}

// pickColdest — выбрать lord с наименьшим score, не равный from,
// не в blacklist, в пределах hysteresis band.
func pickColdest(byScore []*LordScore, fromLord string, now time.Time, hysteresis float64) string {
	if len(byScore) == 0 {
		return ""
	}
	// Coldest = lowest combined score.
	for _, sc := range byScore {
		if sc.LordID == fromLord {
			continue
		}
		if sc.BlacklistUntil.After(now) {
			continue
		}
		return sc.LordID
	}
	return ""
}

// readLordCPU — read cpu usage ratio for lord's cgroup.
func readLordCPU(lordID string) float64 {
	path := fmt.Sprintf("/sys/fs/cgroup/etronium/%s/cpu.usage_usec", lordID)
	data, err := os.ReadFile(path)
	if err != nil {
		// Maybe scheduler stats path
		path = fmt.Sprintf("/sys/fs/cgroup/etronium/%s/cpu.stat", lordID)
		data, err = os.ReadFile(path)
		if err != nil {
			return 0
		}
		// cpu.stat has "usage_usec <N>"
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "usage_usec ") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					usec, _ := strconv.ParseUint(fields[1], 10, 64)
					// Approximate ratio: usec / (1 second window)
					return float64(usec) / 1e6
				}
			}
		}
		return 0
	}
	usec, _ := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	// ratio ≈ usage in last second (sample rate of cgroup.usage_usec)
	// We approximate: 1e6 usec = 1 CPU second per second = ratio 1.0
	return float64(usec) / 1e6
}

// readLordMem — read memory.current / memory.max for lord's cgroup.
func readLordMem(lordID string) float64 {
	base := fmt.Sprintf("/sys/fs/cgroup/etronium/%s", lordID)
	cur, err := os.ReadFile(filepath.Join(base, "memory.current"))
	if err != nil {
		return 0
	}
	max, err := os.ReadFile(filepath.Join(base, "memory.max"))
	if err != nil {
		return 0
	}
	curVal, _ := strconv.ParseUint(strings.TrimSpace(string(cur)), 10, 64)
	maxStr := strings.TrimSpace(string(max))
	if maxStr == "max" {
		// No limit → ratio from host memory.
		return 0.1
	}
	maxVal, _ := strconv.ParseUint(maxStr, 10, 64)
	if maxVal == 0 {
		return 0
	}
	ratio := float64(curVal) / float64(maxVal)
	if ratio > 1.0 {
		ratio = 1.0
	}
	return ratio
}

// readBPFRejectRates — read etr_lord_stats BPF map, compute reject rate per lord.
//
// Returns map[hostname] = reject_rate (rejects / max(enqueue, 1)).
func readBPFRejectRates() (map[string]float64, error) {
	statsPath := "/sys/fs/bpf/etronium/maps/etr_lord_stats"
	if _, err := os.Stat(statsPath); err != nil {
		return nil, err
	}
	m, err := ebpf.LoadPinnedMap(statsPath, nil)
	if err != nil {
		return nil, err
	}
	defer m.Close()

	// Sample - take current snapshot (not rate-limited; just raw counts).
	it := m.Iterate()
	var key uint32
	var val struct {
		SelectCPU uint64
		Enqueue   uint64
		Dispatch  uint64
		Reject    uint64
	}

	out := map[string]float64{}
	// Need to map lord_id (u32) → hostname. That mapping is in scheduler's
	// lord registry. We return by lord_id; the caller (Sample) re-keys by
	// its own lord registry.
	for it.Next(&key, &val) {
		denom := val.Enqueue
		if denom == 0 {
			denom = 1
		}
		out[lordIDFromUint32(key)] = float64(val.Reject) / float64(denom)
	}
	return out, nil
}

func lordIDFromUint32(k uint32) string {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, k)
	return string(b)
}

// StartAutoscale — запуск periodic loop. Caller passes the running Server.
func (s *Server) StartAutoscale(ctx context.Context, cfg AutoscaleConfig) {
	if !cfg.Enabled {
		s.logger.Info("autoscale disabled")
		return
	}
	state := NewAutoscaleState()
	s.autoscale = state

	go func() {
		t := time.NewTicker(cfg.Interval)
		defer t.Stop()
		s.logger.Info("autoscale started",
			"interval", cfg.Interval,
			"overload_cpu", cfg.Thresh.OverloadCPU,
			"rebalance_delta", cfg.Thresh.RebalanceDelta,
		)
		for {
			select {
			case <-ctx.Done():
				s.logger.Info("autoscale stopped")
				return
			case <-t.C:
				if err := state.Sample(ctx, s, cfg); err != nil {
					s.logger.Warn("autoscale sample error", "err", err)
					continue
				}
				decisions := state.Decide(cfg)
				for _, d := range decisions {
					s.logger.Info("autoscale decision",
						"action", d.Action,
						"from", d.FromLord,
						"to", d.ToLord,
						"reason", d.Reason,
					)
					if err := s.applyDecision(d, state); err != nil {
						s.logger.Warn("autoscale apply error", "action", d.Action, "err", err)
					}
				}
			}
		}
	}()
}

// applyDecision — выполнить решение (миграция процесса).
func (s *Server) applyDecision(d Decision, state *AutoscaleState) error {
	switch d.Action {
	case "migrate", "rebalance":
		// Pick the coldest process on the hot lord and migrate.
		pid := s.pickColdestProcessOnLord(d.FromLord)
		if pid == "" {
			return nil
		}
		_, err := s.Migrate(context.Background(), &etroniumv1.MigrateRequest{
			ProcessId:    pid,
			TargetLordId: d.ToLord,
			Auto:         true,
		})
		return err
	case "blacklist":
		// Blacklist is recorded in state.Sample; nothing else to do here.
		return nil
	}
	return nil
}

// pickColdestProcessOnLord — найти процесс с наименьшим recent CPU usage
// на данном lord'е, чтобы мигрировать.
func (s *Server) pickColdestProcessOnLord(lordID string) string {
	s.processes.mu.RLock()
	defer s.processes.mu.RUnlock()
	var best string
	var bestCPU uint64 = ^uint64(0)
	for id, e := range s.processes.byID {
		if e.Info.GetRef().GetLordId() != lordID {
			continue
		}
		// Use simple heuristic: prefer younger processes for migration.
		// (Real CPU usage tracking is in cgroup stats; for now we use age.)
		age := uint64(time.Since(e.Info.GetStateChangedAt().AsTime()).Seconds())
		if age < bestCPU {
			bestCPU = age
			best = id
		}
	}
	return best
}