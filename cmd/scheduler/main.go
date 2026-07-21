// Command scheduler — main entry point.
//
// Запускает gRPC server с реализацией SchedulerService и LordService.
// Архитектура: один процесс держит ОБА сервиса, потому что lord'ы
// открывают bidi stream к scheduler'у, и этот stream реализует
// LordService.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cilium/ebpf"

	etroniumv1 "github.com/midas/Etronium-Scdr/internal/gen/etronium/v1"
	"github.com/midas/Etronium-Scdr/internal/scheduler"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

// version is overridden at link time via -ldflags="-X main.version=...".
var version = "dev"

func main() {
	// Phase 3.5 subcommands: "scheduler serve" (default), "scheduler migrate", "scheduler stats".
	if len(os.Args) >= 2 && os.Args[1] == "--version" {
		fmt.Fprintf(os.Stderr, "scheduler %s\n", version)
		os.Exit(0)
	}
	if len(os.Args) >= 2 && os.Args[1] == "version" {
		fmt.Fprintf(os.Stderr, "scheduler %s\n", version)
		os.Exit(0)
	}
	switch {
	case len(os.Args) >= 2 && os.Args[1] == "migrate":
		migrateCmd(os.Args[2:])
		return
	case len(os.Args) >= 2 && os.Args[1] == "stats":
		statsCmd(os.Args[2:])
		return
	}

	var (
		addr     = flag.String("addr", ":51061", "gRPC listen address")
		logLevel = flag.String("log", "info", "log level (debug|info|warn|error)")
	)
	flag.Parse()

	logger := newLogger(*logLevel)

	cfg, err := scheduler.LoadConfig()
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}
	if *addr != ":50061" {
		cfg.ListenAddr = *addr
	}

	processes := scheduler.NewProcessTable()
	lords := scheduler.NewLordRegistry(cfg.PlacementAlgo)

	// Phase 5: WAL replay at startup. Failures are non-fatal — just log.
	walPath := os.Getenv("SCHEDULER_WAL_PATH")
	if walPath == "" {
		walPath = "/tmp/etronium/scheduler.wal"
	}
	if err := scheduler.WriteHeader(walPath); err != nil {
		logger.Warn("wal header write failed", "err", err)
	}
	if rep, err := scheduler.ReplayWAL(walPath, processes); err != nil {
		logger.Warn("wal replay failed", "err", err)
	} else if rep.Creates > 0 {
		logger.Info("wal replay done", "creates", rep.Creates, "states", rep.States, "results", rep.Results)
	}
	wal, err := scheduler.OpenWAL(walPath)
	if err != nil {
		logger.Warn("wal open failed, continuing without WAL", "err", err)
		wal = nil
	} else {
		processes.AttachWAL(wal)
		defer wal.Close()
	}

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

		// Phase 5 graceful shutdown:
		//   1. Tell all lords to drain (refuse new spawns, kill active procs).
		//   2. Wait up to drainTimeout for them to ack.
		//   3. Then grpcServer.GracefulStop() closes bidi streams cleanly.
		const drainTimeout = 15 * time.Second
		drainCtx, drainCancel := context.WithTimeout(context.Background(), drainTimeout)
		defer drainCancel()
		if err := srv.Shutdown(drainCtx, drainTimeout); err != nil {
			logger.Warn("shutdown: drain incomplete", "err", err)
		}

		// Закрываем gRPC. Active streams получают ~5 сек на graceful close.
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
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


// migrateCmd — Phase 3.5 live migration: update lord CPU mask in BPF map.
// Triggers scx select_cpu to re-evaluate first_cpu_in_mask on next wakeup.
//
// Usage:
//   scheduler migrate --hostname <hostname> --shares <N>
func migrateCmd(args []string) {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	hostname := fs.String("hostname", "", "lord hostname (required)")
	shares := fs.Uint("shares", 1, "new advertised CPU shares (1..16)")
	_ = fs.Parse(args)
	if *hostname == "" {
		fmt.Fprintln(os.Stderr, "scheduler migrate: --hostname is required")
		fs.Usage()
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	newMask, err := scheduler.UpdateLordCPUMask(ctx, *hostname, uint32(*shares), logger)
	if err != nil {
		logger.Error("live migration failed", "err", err)
		os.Exit(1)
	}
	fmt.Printf("OK hostname=%s shares=%d new_cpu_mask=0x%x\n", *hostname, *shares, newMask)
}

// statsCmd — Phase 4 observability: dump scx kernel stats + BPF map sizes.
//
// Usage:
//   scheduler stats
func statsCmd(args []string) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "JSON output")
	_ = fs.Parse(args)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Kernel-side scx stats from /sys/kernel/sched_ext/.
	scxStats, err := readSCXSysfs()
	if err != nil {
		logger.Warn("read sched_ext sysfs failed", "err", err)
	}

	// BPF map sizes via cilium/ebpf.
	bpfStats, err := readBPFMapStats(ctx)
	if err != nil {
		logger.Warn("read BPF map stats failed", "err", err)
	}

	perLord, _ := readLordStats(ctx)

	out := statsOutput{
		SCX:     scxStats,
		BPFMaps: bpfStats,
		PerLord: perLord,
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	} else {
		fmt.Printf("sched_ext state: %s\n", scxStats.State)
		fmt.Printf("nr_rejected:     %d\n", scxStats.NrRejected)
		fmt.Printf("enable_seq:      %d\n", scxStats.EnableSeq)
		fmt.Printf("hotplug_seq:     %d\n", scxStats.HotplugSeq)
		fmt.Printf("switch_all:      %d\n", scxStats.SwitchAll)
		fmt.Println()
		fmt.Printf("BPF map entries:\n")
		for name, cnt := range bpfStats {
			fmt.Printf("  %-20s %d\n", name, cnt)
		}
		fmt.Println()
		if len(perLord) > 0 {
			fmt.Printf("Per-lord counters:\n")
			fmt.Printf("  %-12s %-12s %-12s %-12s %-12s\n", "lord_id", "select_cpu", "enqueue", "dispatch", "reject")
			// Sort by lord_id for stable output
			keys := make([]uint32, 0, len(perLord))
			for k := range perLord {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
			for _, lid := range keys {
				sv := perLord[lid]
				fmt.Printf("  %-12d %-12d %-12d %-12d %-12d\n",
					lid, sv.NrSelectCPU, sv.NrEnqueue, sv.NrDispatch, sv.NrReject)
			}
		} else {
			fmt.Printf("Per-lord counters: (none — no SCHED_EXT policy tasks yet)\n")
		}
	}
}

type scxSysfsStats struct {
	State      string `json:"state"`
	NrRejected uint64 `json:"nr_rejected"`
	EnableSeq  uint64 `json:"enable_seq"`
	HotplugSeq uint64 `json:"hotplug_seq"`
	SwitchAll  int    `json:"switch_all"`
}

type statsOutput struct {
	SCX     scxSysfsStats        `json:"scx"`
	BPFMaps map[string]int       `json:"bpf_maps"`
	PerLord map[uint32]lordStats `json:"per_lord,omitempty"`
}

type lordStats struct {
	NrSelectCPU uint64 `json:"nr_select_cpu"`
	NrEnqueue   uint64 `json:"nr_enqueue"`
	NrDispatch  uint64 `json:"nr_dispatch"`
	NrReject    uint64 `json:"nr_reject"`
}

func readSCXSysfs() (scxSysfsStats, error) {
	var s scxSysfsStats
	var err error
	if s.State, err = readSysfsFile("/sys/kernel/sched_ext/state"); err != nil {
		return s, err
	}
	if s.NrRejected, err = readSysfsUint("/sys/kernel/sched_ext/nr_rejected"); err != nil {
		return s, err
	}
	s.EnableSeq, _ = readSysfsUint("/sys/kernel/sched_ext/enable_seq")
	s.HotplugSeq, _ = readSysfsUint("/sys/kernel/sched_ext/hotplug_seq")
	s.SwitchAll, _ = readSysfsInt("/sys/kernel/sched_ext/switch_all")
	return s, nil
}

func readSysfsFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func readSysfsUint(path string) (uint64, error) {
	s, err := readSysfsFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(s, 10, 64)
}

func readSysfsInt(path string) (int, error) {
	s, err := readSysfsFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(s)
}

func readBPFMapStats(ctx context.Context) (map[string]int, error) {
	result := map[string]int{}
	for _, p := range []string{
		scheduler.BPFMaps.TaskLord,
		scheduler.BPFMaps.LordCpus,
		scheduler.BPFMaps.LordDSQ,
		scheduler.StatsMap,
	} {
		info, err := mapInfo(p)
		if err != nil {
			return result, err
		}
		result[filepath.Base(p)] = int(info.Live)
	}
	return result, nil
}

// readLordStats — read per-lord counter values from etr_lord_stats map.
// Mirrors BPF struct etr_lord_stats_value layout (4 x u64 little-endian).
func readLordStats(ctx context.Context) (map[uint32]lordStats, error) {
	out := map[uint32]lordStats{}
	m, err := ebpf.LoadPinnedMap(scheduler.StatsMap, nil)
	if err != nil {
		return out, fmt.Errorf("load stats map: %w", err)
	}
	defer m.Close()
	key := make([]byte, 4)
	nextKey := make([]byte, 4)
	valueBuf := make([]byte, 32) // 4 x u64
	for {
		err := m.NextKey(key, nextKey)
		if err != nil {
			break
		}
		if err := m.Lookup(nextKey, valueBuf); err != nil {
			copy(key, nextKey)
			continue
		}
		var ls lordStats
		ls.NrSelectCPU = binary.LittleEndian.Uint64(valueBuf[0:8])
		ls.NrEnqueue = binary.LittleEndian.Uint64(valueBuf[8:16])
		ls.NrDispatch = binary.LittleEndian.Uint64(valueBuf[16:24])
		ls.NrReject = binary.LittleEndian.Uint64(valueBuf[24:32])
		lid := binary.LittleEndian.Uint32(nextKey)
		out[lid] = ls
		copy(key, nextKey)
	}
	return out, nil
}

// mapInfo — read BPF map max + live count via NextKey traversal.
type mapInfoResult struct {
	Max  uint32
	Live uint32
}

func mapInfo(pinPath string) (mapInfoResult, error) {
	var r mapInfoResult
	m, err := ebpf.LoadPinnedMap(pinPath, nil)
	if err != nil {
		return r, fmt.Errorf("load %s: %w", pinPath, err)
	}
	defer m.Close()
	info, err := m.Info()
	if err != nil {
		return r, err
	}
	if info == nil {
		return r, nil
	}
	r.Max = info.MaxEntries
	keySize := uint32(info.KeySize)
	if keySize == 0 {
		return r, nil
	}
	key := make([]byte, keySize)
	nextKey := make([]byte, keySize)
	count := uint32(0)
	for {
		err := m.NextKey(key, nextKey)
		if err != nil {
			break // ENOENT = end of map
		}
		count++
		copy(key, nextKey)
	}
	r.Live = count
	return r, nil
}
