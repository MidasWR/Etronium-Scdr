// Package lord — config.go
package lord

import (
	"fmt"
	"os"
	"strconv"
)

// Config — runtime config lord'а.
type Config struct {
	SchedulerAddr      string // scheduler gRPC endpoint, default "localhost:50061"
	Hostname           string // опционально, иначе os.Hostname()
	HeartbeatSec       int    // интервал heartbeat, default 10
	LogLevel           string
	AdvertisedCpuShares int32 // NUMA-overcommit, 0 = equals cpu_cores_physical * 100
	AdvertisedMemBytes  int64 // NUMA-overcommit, 0 = equals mem_total_bytes_physical
}

// LoadConfig — грузит из env.
func LoadConfig() (*Config, error) {
	hostname, _ := os.Hostname()
	c := &Config{
		SchedulerAddr: getEnv("SCHEDULER_ADDR", "localhost:50061"),
		Hostname:      getEnv("LORD_HOSTNAME", hostname),
		HeartbeatSec:  10,
		LogLevel:      getEnv("LORD_LOG_LEVEL", "info"),
	}
	if v := os.Getenv("LORD_HEARTBEAT_SEC"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
			return nil, fmt.Errorf("LORD_HEARTBEAT_SEC: %v", err)
		}
		c.HeartbeatSec = n
	}
	if v := os.Getenv("LORD_ADVERTISE_CPU_SHARES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("LORD_ADVERTISE_CPU_SHARES: %v", err)
		}
		c.AdvertisedCpuShares = int32(n)
	}
	if v := os.Getenv("LORD_ADVERTISE_MEM_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("LORD_ADVERTISE_MEM_BYTES: %v", err)
		}
		c.AdvertisedMemBytes = n
	}
	if c.SchedulerAddr == "" {
		return nil, fmt.Errorf("SCHEDULER_ADDR required")
	}
	return c, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
