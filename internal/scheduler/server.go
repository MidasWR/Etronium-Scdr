// Package scheduler — server.go
//
// Реализация etroniumv1.SchedulerServiceServer (то что вызывает tenant).
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	etroniumv1 "github.com/midas/Etronium-Scdr/internal/gen/etronium/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Server — реализация SchedulerService.
//
// Server держит:
//   • processTable — глобальная таблица процессов
//   • lords — реестр лордов
//   • lordSessions — активные bidi stream'ы (lord_id → session)
//   • logger — структурный логгер
//
// Архитектура Phase 0: lord'ы инициируют соединение, scheduler пушит
// команды через их долгоживущие stream'ы. Не требует публичного IP у lord'а.
type Server struct {
	etroniumv1.UnimplementedSchedulerServiceServer
	etroniumv1.UnimplementedLordServiceServer

	config         *Config
	processes      *ProcessTable
	lords          *LordRegistry
	lordSessionsMu sync.RWMutex
	lordSessions   map[string]*lordSession
	logger         *slog.Logger

	// Checkpoint wait registry (Phase 3): keyed by (lord_id, process_id).
	cpWaitsMu sync.Mutex
	cpWaits   map[checkpointWaitKey]chan *etroniumv1.CheckpointResponse
}

// NewServer — конструктор.
func NewServer(cfg *Config, processes *ProcessTable, lords *LordRegistry, logger *slog.Logger) *Server {
	return &Server{
		config:       cfg,
		processes:    processes,
		lords:        lords,
		lordSessions: make(map[string]*lordSession),
		cpWaits:      make(map[checkpointWaitKey]chan *etroniumv1.CheckpointResponse),
		logger:       logger,
	}
}

// SetLordClient — устарело в Phase 0 (lord инициирует stream сам).
// Оставлено для совместимости со старым кодом, не используется.
func (s *Server) SetLordClient(lordID string, client etroniumv1.LordServiceClient) {}

// GetLordClient — устарело в Phase 0.
func (s *Server) GetLordClient(lordID string) (etroniumv1.LordServiceClient, error) {
	return nil, fmt.Errorf("GetLordClient: deprecated in Phase 0 (use lordSessions)")
}

// SweepHeartbeats — периодически проверяет heartbeat'ы лордов.
func (s *Server) SweepHeartbeats(ttl time.Duration) []string {
	return s.lords.SweepHeartbeats(ttl)
}

// ============================================================================
// SchedulerService — то что вызывает tenant (арендатор)
// ============================================================================

// Spawn — основная точка входа. Создаёт процесс и запускает на lord'е.
func (s *Server) Spawn(ctx context.Context, req *etroniumv1.SpawnRequest) (*etroniumv1.ProcessInfo, error) {
	if req.TenantId == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id required")
	}
	if req.ExecPath == "" {
		return nil, status.Error(codes.InvalidArgument, "exec_path required")
	}
	if err := validateResources(req.Resources); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid resources: %v", err)
	}

	// 1. Placement
	lord := s.lords.Pick(req.PreferLordId)
	if lord == nil {
		return nil, status.Error(codes.Unavailable, "no healthy lord available")
	}

	// 2. Проверяем что у лорда есть активная session
	if _, err := s.getSession(lord.LordId); err != nil {
		return nil, status.Errorf(codes.Unavailable, "lord %s not connected: %v", lord.LordId, err)
	}

	// 3. Создаём запись в process_table в state READY
	processID := NewID()
	info := &etroniumv1.ProcessInfo{
		Ref: &etroniumv1.ProcessRef{
			ProcessId: processID,
			LordId:    lord.LordId,
		},
		TenantId:        req.TenantId,
		ExecPath:        req.ExecPath,
		Argv:            append([]string{}, req.Argv...),
		Env:             copyMap(req.Env),
		WorkingDir:      req.WorkingDir,
		State:           etroniumv1.ProcessState_PROCESS_STATE_READY,
		StateChangedAt:  nowTimestamp(),
		Resources:       req.Resources,
		StateDumpPath:   req.GetStateDumpPathHint(),
		MaxRestarts:     req.GetMaxRestarts(),
	}
	if info.GetMaxRestarts() == 0 {
		info.MaxRestarts = 10 // sensible default
	}
	entry := s.processes.Create(info)

	// 4. Шлём LordEvent{spawn} лорду через его session.
	//    Лорд в ответ пришлёт ProcessStarted (с local_pid), потом ProcessIo chunks,
	//    потом ProcessExit.
	//
	//    V5 state-dump: если info.StateDumpPath != "", передаём его как
	//    env-переменную ETRONIUM_STATE_DUMP в spawn request. Lord выставит
	//    её в child env. Приложение при старте читает файл, на restore
	//    продолжает с последнего state.
	lordEnv := copyMap(req.Env)
	if info.GetStateDumpPath() != "" {
		if lordEnv == nil {
			lordEnv = make(map[string]string)
		}
		lordEnv["ETRONIUM_STATE_DUMP"] = info.GetStateDumpPath()
	}
	spawnReq := &etroniumv1.SpawnRequest{
		ProcessId:         processID,
		TenantId:          req.TenantId,
		ExecPath:          req.ExecPath,
		Argv:              req.Argv,
		Env:               lordEnv,
		Resources:         req.Resources,
		WorkingDir:        req.WorkingDir,
		StdinInitial:      req.StdinInitial,
		CriuCheckpointable: req.CriuCheckpointable,
	}

	if err := s.SendSpawn(lord.LordId, spawnReq); err != nil {
		entry.UpdateState(etroniumv1.ProcessState_PROCESS_STATE_STOPPED, "", 0)
		entry.UpdateResult(-1, 0)
		return nil, status.Errorf(codes.Internal, "send spawn: %v", err)
	}

	s.logger.Info("process spawn requested",
		"process_id", processID,
		"tenant_id", req.TenantId,
		"lord_id", lord.LordId,
		"exec", req.ExecPath,
	)

	return entry.Snapshot(), nil
}

// Kill — послать сигнал процессу через lord'а.
func (s *Server) Kill(ctx context.Context, req *etroniumv1.KillRequest) (*etroniumv1.KillResponse, error) {
	entry, ok := s.processes.Get(req.ProcessId)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "process %s not found", req.ProcessId)
	}

	// Определяем сигнал
	signal := req.SignalNumber
	if signal == 0 {
		signal = 15 // SIGTERM
	}
	if req.Force {
		signal = 9 // SIGKILL
	}

	lordID := entry.Info.Ref.LordId
	if lordID == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "process not running (no lord)")
	}

	if err := s.SendKill(lordID, &etroniumv1.KillRequest{
		ProcessId:    req.ProcessId,
		SignalNumber: signal,
		Force:        req.Force,
	}); err != nil {
		return nil, status.Errorf(codes.Unavailable, "send kill: %v", err)
	}

	return &etroniumv1.KillResponse{
		Acknowledged: true,
		CurrentState: entry.Info.GetState(),
	}, nil
}

// Wait — блокирующее ожидание exit.
func (s *Server) Wait(ctx context.Context, req *etroniumv1.WaitRequest) (*etroniumv1.ProcessInfo, error) {
	entry, ok := s.processes.Get(req.ProcessId)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "process %s not found", req.ProcessId)
	}

	timeout := time.Duration(req.TimeoutSec) * time.Second
	if timeout == 0 {
		timeout = 600 * time.Second
	}

	var timer *time.Timer
	if req.TimeoutSec > 0 {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
	}

	select {
	case <-entry.ExitedChan():
		return entry.Snapshot(), nil
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	case <-timerOrNever(timer):
		return entry.Snapshot(), nil // timeout, отдаём текущее состояние
	}
}

func timerOrNever(t *time.Timer) <-chan time.Time {
	if t == nil {
		return make(chan time.Time) // никогда не сработает
	}
	return t.C
}

// GetProcess — снимок состояния.
func (s *Server) GetProcess(ctx context.Context, req *etroniumv1.GetProcessRequest) (*etroniumv1.ProcessInfo, error) {
	entry, ok := s.processes.Get(req.ProcessId)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "process %s not found", req.ProcessId)
	}
	return entry.Snapshot(), nil
}

// ListProcesses — список процессов тенанта.
func (s *Server) ListProcesses(ctx context.Context, req *etroniumv1.ListProcessesRequest) (*etroniumv1.ListProcessesResponse, error) {
	if req.TenantId == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id required")
	}
	entries := s.processes.ListByTenant(req.TenantId, req.OnlyRunning)
	out := make([]*etroniumv1.ProcessInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Snapshot())
	}
	limit := int(req.Limit)
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return &etroniumv1.ListProcessesResponse{Processes: out}, nil
}

// Migrate — реализовано в migrate.go (Phase 3).

// ListLords — дамп всех лордов.
func (s *Server) ListLords(ctx context.Context, req *etroniumv1.ListLordsRequest) (*etroniumv1.ListLordsResponse, error) {
	lords := s.lords.ListAll(req.OnlyHealthy)
	return &etroniumv1.ListLordsResponse{Lords: lords}, nil
}

// StreamProcessIO — Phase 0: not implemented (используется для live attach).
func (s *Server) StreamProcessIO(req *etroniumv1.StreamProcessIORequest, stream etroniumv1.SchedulerService_StreamProcessIOServer) error {
	entry, ok := s.processes.Get(req.ProcessId)
	if !ok {
		return status.Errorf(codes.NotFound, "process %s not found", req.ProcessId)
	}
	// Phase 0: просто отдаём ring buffer содержимое и закрываем stream
	data := entry.ioBuf.Bytes()
	if len(data) > 0 {
		if err := stream.Send(&etroniumv1.IOChunk{
			Stream: etroniumv1.IOChunk_STREAM_STDOUT,
			Data:   data,
		}); err != nil {
			return err
		}
	}
	return nil
}

// WatchProcess — Phase 0: упрощение, события не буферизуются.
func (s *Server) WatchProcess(req *etroniumv1.WatchProcessRequest, stream etroniumv1.SchedulerService_WatchProcessServer) error {
	return status.Error(codes.Unimplemented, "watch process not yet implemented")
}

// PullFile / PushFile / InvalidateFileCache — Phase 4.
func (s *Server) PullFile(ctx context.Context, req *etroniumv1.PullFileRequest) (*etroniumv1.PullFileResponse, error) {
	return nil, status.Error(codes.Unimplemented, "pull file not yet implemented (Phase 4)")
}
func (s *Server) PushFile(ctx context.Context, req *etroniumv1.PushFileRequest) (*etroniumv1.PushFileResponse, error) {
	return nil, status.Error(codes.Unimplemented, "push file not yet implemented (Phase 4)")
}
func (s *Server) InvalidateFileCache(ctx context.Context, req *etroniumv1.InvalidateFileCacheRequest) (*etroniumv1.InvalidateFileCacheResponse, error) {
	return nil, status.Error(codes.Unimplemented, "invalidate cache not yet implemented (Phase 4)")
}

// ============================================================================
// helpers
// ============================================================================

func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// validateResources проверяет что ResourceSpec находится в допустимых рамках.
//
// Правила Phase 1:
//   - cpu_shares: 1..10000 (cgroup cpu.weight)
//   - cpu_quota_pct: 0..100 (Phase 2, не используется lord'ом в Phase 1)
//   - mem_limit_bytes: > 0 (если задан); 0 = no limit
//   - io_weight: 1..10000 (cgroup io.weight)
//   - pids_limit: 0..1000000 (cgroup pids.max); 0 = no limit
//
// 0 трактуется как "не задано" и пропускается. Отрицательные / out-of-range
// возвращают ошибку.
func validateResources(r *etroniumv1.ResourceSpec) error {
	if r == nil {
		return nil
	}
	if r.CpuShares < 0 || r.CpuShares > 10000 {
		return fmt.Errorf("cpu_shares out of range [0..10000]: %d", r.CpuShares)
	}
	if r.CpuQuotaPct < 0 || r.CpuQuotaPct > 100 {
		return fmt.Errorf("cpu_quota_pct out of range [0..100]: %d", r.CpuQuotaPct)
	}
	if r.MemLimitBytes < 0 {
		return fmt.Errorf("mem_limit_bytes must be > 0: %d", r.MemLimitBytes)
	}
	if r.IoWeight < 0 || r.IoWeight > 10000 {
		return fmt.Errorf("io_weight out of range [0..10000]: %d", r.IoWeight)
	}
	if r.PidsLimit < 0 || r.PidsLimit > 1000000 {
		return fmt.Errorf("pids_limit out of range [0..1000000]: %d", r.PidsLimit)
	}
	return nil
}
