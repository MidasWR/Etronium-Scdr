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
}

// NewLordRegistry — конструктор.
func NewLordRegistry() *LordRegistry {
	return &LordRegistry{byID: make(map[string]*LordEntry)}
}

// Register — добавляет нового лорда или обновляет существующего.
func (r *LordRegistry) Register(info *etroniumv1.Lord) {
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

// Pick — выбирает лорда для нового процесса (Phase 0: trivial).
//
// Логика: первый здоровый лорд. Если preferLordID указан и он здоров —
// отдаём его. Если нет — fallback на любой здоровый.
//
// TODO Phase 2+: weighted score, memory pressure, affinity.
func (r *LordRegistry) Pick(preferLordID string) *etroniumv1.Lord {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if preferLordID != "" {
		if e, ok := r.byID[preferLordID]; ok && e.Healthy && !e.DrainRequested {
			return e.Info
		}
	}

	for _, e := range r.byID {
		if e.Healthy && !e.DrainRequested {
			return e.Info
		}
	}
	return nil
}

// SweepHeartbeats — вызывается периодически, помечает нездоровых.
func (r *LordRegistry) SweepHeartbeats(ttl time.Duration) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var lost []string
	for id, e := range r.byID {
		if time.Since(e.LastHeartbeat) > ttl && e.Healthy {
			e.Healthy = false
			lost = append(lost, id)
		}
	}
	return lost
}
