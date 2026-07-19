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
	)
	flag.Parse()

	logger := newLogger(*logLevel, *logFormat)

	cfg := &lord.Config{
		SchedulerAddr: *schedulerAddr,
		Hostname:      *hostname,
		HeartbeatSec:  10,
		LogLevel:      *logLevel,
		CriuAvailable: false,
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
