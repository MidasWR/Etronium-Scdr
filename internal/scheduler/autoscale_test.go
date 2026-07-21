// Package scheduler — autoscale_test.go
//
// Unit tests for the autoscale scoring + decision algorithms.
// No I/O — only pure-Go logic.

package scheduler

import (
	"testing"
	"time"
)

func TestDefaultAutoscaleConfig(t *testing.T) {
	cfg := DefaultAutoscaleConfig()
	if !cfg.Enabled {
		t.Fatal("autoscale should be enabled by default")
	}
	if cfg.Interval != 30*time.Second {
		t.Errorf("default interval: got %s, want 30s", cfg.Interval)
	}
	if cfg.Score.CPU != 0.6 {
		t.Errorf("default cpu weight: got %f, want 0.6", cfg.Score.CPU)
	}
	if cfg.Score.Mem != 0.2 {
		t.Errorf("default mem weight: got %f, want 0.2", cfg.Score.Mem)
	}
	if cfg.Score.TaskCount != 0.1 {
		t.Errorf("default task_count weight: got %f, want 0.1", cfg.Score.TaskCount)
	}
	if cfg.Score.Reject != 0.1 {
		t.Errorf("default reject weight: got %f, want 0.1", cfg.Score.Reject)
	}
	if cfg.Thresh.OverloadCPU != 0.80 {
		t.Errorf("default overload threshold: got %f, want 0.80", cfg.Thresh.OverloadCPU)
	}
	if cfg.Thresh.RebalanceDelta != 0.30 {
		t.Errorf("default rebalance threshold: got %f, want 0.30", cfg.Thresh.RebalanceDelta)
	}
	if cfg.Thresh.BlacklistReject != 0.05 {
		t.Errorf("default blacklist reject: got %f, want 0.05", cfg.Thresh.BlacklistReject)
	}
}

func TestDecideOverloadTriggersMigrate(t *testing.T) {
	cfg := DefaultAutoscaleConfig()
	state := NewAutoscaleState()
	state.lords = map[string]*LordScore{
		"hot":  {LordID: "hot", CPU: 0.90, Combined: 0.90, LastMigration: time.Time{}}, // overload
		"cold": {LordID: "cold", CPU: 0.10, Combined: 0.10},
	}

	decisions := state.Decide(cfg)
	if len(decisions) == 0 {
		t.Fatal("expected at least one decision for overloaded lord")
	}
	foundMigrate := false
	for _, d := range decisions {
		if d.Action == "migrate" && d.FromLord == "hot" && d.ToLord == "cold" {
			foundMigrate = true
		}
	}
	if !foundMigrate {
		t.Errorf("expected migrate hot→cold, got decisions: %+v", decisions)
	}
}

func TestDecideNoActionWhenBalanced(t *testing.T) {
	cfg := DefaultAutoscaleConfig()
	state := NewAutoscaleState()
	state.lords = map[string]*LordScore{
		"a": {LordID: "a", CPU: 0.40, Combined: 0.40},
		"b": {LordID: "b", CPU: 0.45, Combined: 0.45},
		"c": {LordID: "c", CPU: 0.50, Combined: 0.50},
	}
	// Delta = 0.10 < threshold 0.30 → no rebalance.
	decisions := state.Decide(cfg)
	for _, d := range decisions {
		if d.Action == "rebalance" || d.Action == "migrate" {
			t.Errorf("unexpected action on balanced load: %+v", d)
		}
	}
}

func TestDecideBlacklistOnHighReject(t *testing.T) {
	cfg := DefaultAutoscaleConfig()
	state := NewAutoscaleState()
	state.lords = map[string]*LordScore{
		"bad": {LordID: "bad", Reject: 0.10, Combined: 0.10}, // > 0.05 threshold
	}
	decisions := state.Decide(cfg)
	foundBlacklist := false
	for _, d := range decisions {
		if d.Action == "blacklist" && d.FromLord == "bad" {
			foundBlacklist = true
		}
	}
	if !foundBlacklist {
		t.Errorf("expected blacklist for high reject rate, got: %+v", decisions)
	}
	if state.lords["bad"].BlacklistUntil.IsZero() {
		t.Error("blacklist_until should be set after blacklist decision")
	}
}

func TestDecideRespectsCooldown(t *testing.T) {
	cfg := DefaultAutoscaleConfig()
	state := NewAutoscaleState()
	now := time.Now()
	state.lords = map[string]*LordScore{
		"hot":  {LordID: "hot", CPU: 0.95, Combined: 0.95, LastMigration: now.Add(-30 * time.Second)}, // cooldown 60s
		"cold": {LordID: "cold", CPU: 0.05, Combined: 0.05},
	}
	// Cooldown not expired (30s < 60s), so no migration decision.
	decisions := state.Decide(cfg)
	for _, d := range decisions {
		if d.Action == "migrate" && d.FromLord == "hot" {
			t.Errorf("expected cooldown to suppress migration, got: %+v", d)
		}
	}
}

func TestDecideRateLimit(t *testing.T) {
	cfg := DefaultAutoscaleConfig()
	cfg.MaxPerMin = 2
	state := NewAutoscaleState()
	// Pre-fill migration log with 2 entries in last 60s.
	state.migrations = []time.Time{time.Now().Add(-30 * time.Second), time.Now().Add(-10 * time.Second)}
	state.lords = map[string]*LordScore{
		"hot":  {LordID: "hot", CPU: 0.95, Combined: 0.95},
		"cold": {LordID: "cold", CPU: 0.05, Combined: 0.05},
	}
	decisions := state.Decide(cfg)
	for _, d := range decisions {
		if d.Action == "rebalance" {
			t.Errorf("rate limit should suppress rebalance, got: %+v", d)
		}
	}
}

func TestPickColdestSkipsBlacklistedAndSelf(t *testing.T) {
	now := time.Now()
	scores := []*LordScore{
		{LordID: "self", Combined: 0.5},
		{LordID: "bl", Combined: 0.1, BlacklistUntil: now.Add(time.Minute)},
		{LordID: "good", Combined: 0.3},
	}
	got := pickColdest(scores, "self", now, 0.05)
	if got != "good" {
		t.Errorf("pickColdest: got %q, want 'good'", got)
	}
}

func TestReadLordCPUZeroOnMissingCgroup(t *testing.T) {
	// Should not panic; returns 0 when cgroup not found.
	got := readLordCPU("nonexistent-lord-id-12345")
	if got != 0 {
		t.Errorf("expected 0 for missing lord, got %f", got)
	}
}

func TestReadLordMemZeroOnMissingCgroup(t *testing.T) {
	got := readLordMem("nonexistent-lord-id-12345")
	if got != 0 {
		t.Errorf("expected 0 for missing lord, got %f", got)
	}
}