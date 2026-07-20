// Command lord — машина-донор.
//
// Открывает долгоживущий bidi stream к scheduler'у, регистрируется,
// шлёт heartbeat'ы, принимает команды на запуск/убийство процессов.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/lmittmann/tint"

	"github.com/midas/Etronium-Scdr/internal/lord"
)

func main() {
	var (
		schedulerAddr = flag.String("scheduler", envOr("SCHEDULER_ADDR", "localhost:50051"), "scheduler gRPC address")
		hostname      = flag.String("hostname", "", "override hostname (default: os.Hostname)")
		logLevel      = flag.String("log", "info", "log level (debug|info|warn|error)")
		logFormat     = flag.String("log-format", "tint", "log format (tint|json)")
		advCPU        = flag.Int("advertise-cpu", 0, "NUMA-overcommit: advertise CPU shares to scheduler (0=physical)")
		advMem        = flag.Int64("advertise-mem", 0, "NUMA-overcommit: advertise mem bytes to scheduler (0=physical)")
	)
	flag.Parse()

	logger := newLogger(*logLevel, *logFormat)

	cfg := &lord.Config{
		SchedulerAddr:       *schedulerAddr,
		Hostname:            *hostname,
		HeartbeatSec:        10,
		LogLevel:            *logLevel,
		CriuAvailable:       false,
		AdvertisedCpuShares: int32(*advCPU),
		AdvertisedMemBytes:  *advMem,
	}

	logger.Info("lord starting",
		"advertise_cpu", cfg.AdvertisedCpuShares,
		"advertise_mem", cfg.AdvertisedMemBytes,
		"scheduler", cfg.SchedulerAddr,
		"hostname", cfg.Hostname,
	)

	// CRIU detection (Phase 3)
	criuProbe := lord.NewCriuOps(logger)
	if criuProbe.Available() {
		cfg.CriuAvailable = true
		logger.Info("criu detected, migration supported", "version", criuProbe.Version())
	} else {
		logger.Warn("criu not available, migration disabled (Phase 3 will no-op)")
	}

	// Pre-fork dummy sleep процессов чтобы PID counter на lord'е вырос выше thread'ов.
	// Это нужно для Phase 3 — CRIU restore пробует создать target с PID из images
	// (где он был на source lord'е). На target lord'е тот же PID должен быть свободен.
	// Pre-fork гарантирует alignment между source и target lords (оба стартуют с
	// одинаковым количеством dummy → оба начинают user PIDs с одинакового номера).
	if cfg.CriuAvailable {
		prewarmPIDCounter(logger, 25)
	}

	// Graceful shutdown
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	agent := lord.NewAgent(cfg, logger)
	if err := agent.Run(ctx); err != nil && err != context.Canceled {
		logger.Error("agent run", "err", err)
		os.Exit(1)
	}
	logger.Info("lord stopped")
}

// prewarmPIDCounter — fork-выет N dummy `sleep infinity` процессов чтобы
// kernel PID counter вырос выше thread'ов lord'а (~14). После этого user-land
// процессы (spawn, restore target) получают PID ≥ N+1, что совпадает между
// source и target lord'ами. NB: `sleep infinity` не делает ничего полезного —
// они просто держат PID slot. При exit lord'а (SIGTERM) kernel их соберёт.
func prewarmPIDCounter(logger *slog.Logger, n int) {
	for i := 0; i < n; i++ {
		cmd := exec.Command("sleep", "infinity")
		// Setsid чтобы не получить SIGINT/SIGTERM cascade от lord'а.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			logger.Warn("prewarm fork failed", "err", err, "i", i)
			return
		}
		// Release — не дожидаемся, процесс живёт в фоне.
		go func(c *exec.Cmd) { _ = c.Wait() }(cmd)
	}
	logger.Info("pid counter prewarmed", "n", n)
}

func newLogger(level, format string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
	}
	return slog.New(tint.NewHandler(os.Stderr, &tint.Options{
		Level:   lvl,
		TimeFormat: time.Kitchen,
	}))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
