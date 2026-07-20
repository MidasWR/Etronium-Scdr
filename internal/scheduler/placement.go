// Package scheduler — placement.go
//
// Phase 0: trivial placement — первый здоровый лорд.
// Phase 2+: weighted score `rep × (1-load) × locality`.
package scheduler

import (
	"sync"
	"time"

	etroniumv1 "github.com/midas/Etronium-Scdr/internal/gen/etronium/v1"
)

// LordEntry — внутреннее представление лорда в scheduler'е.
type LordEntry struct {
	Info           *etroniumv1.Lord
	LastHeartbeat  time.Time
	Healthy        bool
	DrainRequested bool
}

// LordRegistry — глобальный реестр лордов.
type LordRegistry struct {
	mu    sync.RWMutex
	byID  map[string]*LordEntry
	algo  string // "trivial" или "weighted"
}

// NewLordRegistry — конструктор.
func NewLordRegistry(algo string) *LordRegistry {
	if algo != "weighted" {
		algo = "trivial"
	}
	return &LordRegistry{
		byID: make(map[string]*LordEntry),
		algo: algo,
	}
}

// Register — добавляет нового лорда или обновляет существующего.
//
// Логика advertised:
//  • Если RegisterRequest прислал advertised_cpu_shares > 0 — используем его (overcommit).
//  • Если 0 — оставляем 0 (= physical default).
func (r *LordRegistry) Register(info *etroniumv1.Lord) {
	// Поля advertised_cpu_shares/advertised_mem_bytes уже в info (проставлены lord'ом).
	// Если 0 — protojson их опустит (omitempty), что корректно: 0 = equals physical.

	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[info.LordId] = &LordEntry{
		Info:          info,
		LastHeartbeat: time.Now(),
		Healthy:       true,
	}
}

// UpdateStats — обновляет stats из Heartbeat.
func (r *LordRegistry) UpdateStats(lordID string, cpuUsedPct int32, memUsedBytes int64, activeProcs int32) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[lordID]
	if !ok {
		return false
	}
	e.LastHeartbeat = time.Now()
	e.Healthy = true
	e.Info.CpuUsedPct = cpuUsedPct
	e.Info.MemUsedBytes = memUsedBytes
	e.Info.ActiveProcesses = activeProcs
	return true
}

// MarkUnhealthy — пометить как нездорового (heartbeat timeout).
func (r *LordRegistry) MarkUnhealthy(lordID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.byID[lordID]; ok {
		e.Healthy = false
		if e.Info != nil {
			e.Info.Healthy = false
		}
	}
}

// SetDrain — запросить lazy death.
func (r *LordRegistry) SetDrain(lordID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.byID[lordID]; ok {
		e.DrainRequested = true
	}
}

// ListAll — все лорды (для ListLords).
func (r *LordRegistry) ListAll(onlyHealthy bool) []*etroniumv1.Lord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*etroniumv1.Lord, 0, len(r.byID))
	for _, e := range r.byID {
		if onlyHealthy && !e.Healthy {
			continue
		}
		out = append(out, e.Info)
	}
	return out
}

// Pick — выбирает лорда для нового процесса.
//
// Алгоритм определяется алгоритмом реестра:
//  • "trivial" (Phase 0): первый здоровый.
//  • "weighted" (Phase 2): weighted score с учётом NUMA-overcommit.
//
//  1. Soft affinity: preferLordID если он healthy и не drain и имеет capacity.
//  2. Иначе: weighted score = w_cpu * cpuFree + w_mem * memFree + w_invload * 1/(1+procs)
//  3. Отфильтровать unhealthy/drain.
func (r *LordRegistry) Pick(preferLordID string) *etroniumv1.Lord {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// 1. Soft affinity
	if preferLordID != "" {
		if e, ok := r.byID[preferLordID]; ok && e.Healthy && !e.DrainRequested {
			if hasCapacity(e.Info) {
				return e.Info
			}
		}
	}

	// 2. Выбор по алгоритму
	if r.algo == "weighted" {
		var best *etroniumv1.Lord
		var bestScore float64 = -1
		for _, e := range r.byID {
			if !e.Healthy || e.DrainRequested {
				continue
			}
			score := scoreLord(e.Info)
			if score > bestScore {
				bestScore = score
				best = e.Info
			}
		}
		return best
	}

	// trivial: первый здоровый
	for _, e := range r.byID {
		if e.Healthy && !e.DrainRequested {
			return e.Info
		}
	}
	return nil
}

// hasCapacity — лорд имеет хоть сколько-то ресурсов (в advertised терминах).
func hasCapacity(l *etroniumv1.Lord) bool {
	if l.CpuUsedPct >= 100 {
		return false
	}
	adv := l.GetAdvertisedMemBytes()
	if adv > 0 && l.MemUsedBytes >= adv {
		return false
	}
	return true
}

// scoreLord — weighted score [0..1], больше = лучше.
//
// w_cpu=0.5, w_mem=0.4, w_invload=0.1
// cpuFree ∈ [0..1], memFree ∈ [0..1], invLoad = 1/(1+procs)
func scoreLord(l *etroniumv1.Lord) float64 {
	const wCPU = 0.5
	const wMem = 0.4
	const wInvLoad = 0.1

	cpuFree := 1.0 - float64(l.CpuUsedPct)/100.0
	if cpuFree < 0 {
		cpuFree = 0
	}
	if cpuFree > 1 {
		cpuFree = 1
	}

	memFree := 1.0
	adv := l.GetAdvertisedMemBytes()
	if adv > 0 {
		memFree = 1.0 - float64(l.MemUsedBytes)/float64(adv)
	} else {
		// Fallback: используем physical mem
		if l.MemTotalBytesPhysical > 0 {
			memFree = 1.0 - float64(l.MemUsedBytes)/float64(l.MemTotalBytesPhysical)
		}
	}
	if memFree < 0 {
		memFree = 0
	}
	if memFree > 1 {
		memFree = 1
	}

	invLoad := 1.0 / float64(1+l.ActiveProcesses)

	return wCPU*cpuFree + wMem*memFree + wInvLoad*invLoad
}

// SweepHeartbeats — вызывается периодически, помечает нездоровых.
func (r *LordRegistry) SweepHeartbeats(ttl time.Duration) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var lost []string
	for id, e := range r.byID {
		if time.Since(e.LastHeartbeat) > ttl && e.Healthy {
			e.Healthy = false
			if e.Info != nil {
				e.Info.Healthy = false
			}
			lost = append(lost, id)
		}
	}
	return lost
}
