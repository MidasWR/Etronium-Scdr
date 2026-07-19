// Package lord — config.go
package lord

import (
	"fmt"
	"os"
)

// Config — runtime config lord'а.
type Config struct {
	SchedulerAddr  string  // scheduler gRPC endpoint, default "localhost:50051"
	Hostname       string  // опционально, иначе os.Hostname()
	HeartbeatSec   int     // интервал heartbeat, default 10
	CriuAvailable  bool    // поддерживает ли CRIU (для Register)
	LogLevel       string
}

// LoadConfig — грузит из env.
func LoadConfig() (*Config, error) {
	hostname, _ := os.Hostname()
	c := &Config{
		SchedulerAddr: getEnv("SCHEDULER_ADDR", "localhost:50051"),
		Hostname:      getEnv("LORD_HOSTNAME", hostname),
		HeartbeatSec:  10,
		LogLevel:      getEnv("LORD_LOG_LEVEL", "info"),
		CriuAvailable: false, // TODO: проверить что criu есть в PATH
	}
	if v := os.Getenv("LORD_HEARTBEAT_SEC"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
			return nil, fmt.Errorf("LORD_HEARTBEAT_SEC: %v", err)
		}
		c.HeartbeatSec = n
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
