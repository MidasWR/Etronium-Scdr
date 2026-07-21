// Package scheduler — см. ../ARCHITECTURE.md.
//
// Config: env-based, простой для Phase 0.
package scheduler

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config — runtime config scheduler'а.
type Config struct {
	ListenAddr     string        // gRPC bind address, default ":50061"
	SharedToken    string        // pre-shared token для auth (опционально Phase 0.5)
	HeartbeatTTL   time.Duration // mark unhealthy после, default 30s
	PlacementAlgo  string        // "trivial" (Phase 0) или "weighted" (Phase 2+)
	LogLevel       string        // "debug"|"info"|"warn"|"error"
	BpftoolBin     string        // path to bpftool binary (Phase 3.3), default "bpftool"
}

// LoadConfig — грузит из env с дефолтами.
func LoadConfig() (*Config, error) {
	c := &Config{
		ListenAddr:    getEnv("SCHEDULER_LISTEN", ":50061"),
		SharedToken:   getEnv("ETRONIUM_SHARED_TOKEN", ""),
		PlacementAlgo: getEnv("SCHEDULER_PLACEMENT", "trivial"),
		LogLevel:      getEnv("SCHEDULER_LOG_LEVEL", "info"),
		HeartbeatTTL:  30 * time.Second,
		BpftoolBin:    getEnv("BPFTOOL_BIN", "bpftool"),
	}
	if v := os.Getenv("SCHEDULER_HEARTBEAT_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("SCHEDULER_HEARTBEAT_TTL: %w", err)
		}
		c.HeartbeatTTL = d
	}
	// sanity
	if _, err := strconv.Atoi(c.ListenAddr[1:]); err != nil && c.ListenAddr[0] != ':' {
		return nil, fmt.Errorf("SCHEDULER_LISTEN bad addr: %s", c.ListenAddr)
	}
	return c, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
