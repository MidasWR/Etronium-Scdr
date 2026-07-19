// Command scheduler — main entry point.
//
// Запускает gRPC server с реализацией SchedulerService и LordService.
// Архитектура: один процесс держит ОБА сервиса, потому что lord'ы
// открывают bidi stream к scheduler'у, и этот stream реализует
// LordService.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	etroniumv1 "github.com/midas/Etronium-Scdr/internal/gen/etronium/v1"
	"github.com/midas/Etronium-Scdr/internal/scheduler"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

func main() {
	var (
		addr     = flag.String("addr", ":50051", "gRPC listen address")
		logLevel = flag.String("log", "info", "log level (debug|info|warn|error)")
	)
	flag.Parse()

	logger := newLogger(*logLevel)

	cfg, err := scheduler.LoadConfig()
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}
	if *addr != ":50051" {
		cfg.ListenAddr = *addr
	}

	processes := scheduler.NewProcessTable()
	lords := scheduler.NewLordRegistry()
	srv := scheduler.NewServer(cfg, processes, lords, logger)

	// Периодический sweep heartbeat'ов
	go heartbeatSweeper(srv, cfg, logger)

	// Graceful shutdown
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// gRPC server
	grpcServer := grpc.NewServer(
		grpc.ConnectionTimeout(5 * time.Second),
	)
	etroniumv1.RegisterSchedulerServiceServer(grpcServer, srv)
	etroniumv1.RegisterLordServiceServer(grpcServer, srv)
	reflection.Register(grpcServer)

	lis, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		logger.Error("listen", "addr", cfg.ListenAddr, "err", err)
		os.Exit(1)
	}

	go func() {
		<-ctx.Done()
		logger.Info("shutdown signal received, draining...")
		// Даём активным stream'ам до 10 сек на graceful close
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		done := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-shutCtx.Done():
			grpcServer.Stop()
		}
	}()

	logger.Info("scheduler listening", "addr", cfg.ListenAddr, "placement", cfg.PlacementAlgo)
	if err := grpcServer.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		logger.Error("grpc serve", "err", err)
		os.Exit(1)
	}
	logger.Info("scheduler stopped")
}

// heartbeatSweeper — периодически проверяет lord'ов на heartbeat timeout.
func heartbeatSweeper(srv *scheduler.Server, cfg *scheduler.Config, logger *slog.Logger) {
	t := time.NewTicker(cfg.HeartbeatTTL / 3)
	defer t.Stop()
	for range t.C {
		lost := srv.SweepHeartbeats(cfg.HeartbeatTTL)
		if len(lost) > 0 {
			logger.Warn("lords marked unhealthy (heartbeat timeout)", "count", len(lost))
		}
	}
}

// newLogger — JSON handler для прода, текст для дева.
func newLogger(level string) *slog.Logger {
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
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
