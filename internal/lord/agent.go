// Package lord — agent.go
//
// Lord — машина-донор. Держит долгоживущий bidi stream к scheduler'у.
//
// В stream шлёт:
//   • Register (при открытии)
//   • Heartbeat (каждые N сек)
//   • StdioChunk (stdout/stderr запущенных процессов)
//   • ProcessExit (когда процесс завершился)
//
// Из stream читает:
//   • Spawn (запустить процесс)
//   • Kill (послать сигнал)
//   • LazyDeathAck (graceful shutdown)
//   • FilePush (Phase 4)
package lord

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	etroniumv1 "github.com/midas/Etronium-Scdr/internal/gen/etronium/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Agent — long-lived client к scheduler'у.
type Agent struct {
	cfg    *Config
	logger *slog.Logger

	conn   *grpc.ClientConn
	client etroniumv1.LordServiceClient

	// cgroup manager — lazy init после Register (нужен lordID)
	cg      *CgroupManager
	cgMu    sync.Mutex

	// Active local processes keyed by process_id
	procsMu sync.RWMutex
	procs   map[string]*localProcess

	// LordID, присвоенный scheduler'ом при Register
	lordID   string
	lordIDMu sync.RWMutex

	// streamsMu защищает stream reference
	streamsMu sync.RWMutex
	outbox    chan *etroniumv1.LordCmd

	// shutdownCtx — отменяется из main loop при ошибке stream'а
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc

	// CPU delta sampling для heartbeat
	cpuStatsMu  sync.Mutex
	lastCpuUsec uint64
	lastSampleAt time.Time
}

// localProcess — запись о процессе который lord запустил.
type localProcess struct {
	ProcessID string
	LordPID   int
	Cmd       *localCmd
	started   time.Time
}

// localCmd — обёртка над *exec.Cmd с I/O capture.
type localCmd struct {
	execPath string
	argv     []string
	env      []string
	workdir  string

	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	done chan struct{}
}

// NewAgent — конструктор.
func NewAgent(cfg *Config, logger *slog.Logger) *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	return &Agent{
		cfg:            cfg,
		logger:         logger,
		procs:          make(map[string]*localProcess),
		outbox:         make(chan *etroniumv1.LordCmd, 128),
		shutdownCtx:    ctx,
		shutdownCancel: cancel,
	}
}

// Run — блокирующий main loop. Открывает stream, шлёт команды, обрабатывает события.
func (a *Agent) Run(ctx context.Context) error {
	// 1. Подключаемся к scheduler'у
	conn, err := grpc.NewClient(a.cfg.SchedulerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial scheduler %s: %w", a.cfg.SchedulerAddr, err)
	}
	a.conn = conn
	a.client = etroniumv1.NewLordServiceClient(conn)
	defer conn.Close()

	// 2. Открываем bidi stream
	stream, err := a.client.Connect(a.shutdownCtx)
	if err != nil {
		return fmt.Errorf("open connect stream: %w", err)
	}
	defer a.shutdownCancel()

	// 3. Получаем аппаратную инфу для Register
	hw, err := detectHardware(a.cfg)
	if err != nil {
		return fmt.Errorf("detect hardware: %w", err)
	}

	// 4. Запускаем две горутины: send и recv
	sendErr := make(chan error, 1)
	go func() {
		sendErr <- a.sendLoop(stream)
	}()

	recvErr := make(chan error, 1)
	go func() {
		recvErr <- a.recvLoop(ctx, stream, hw)
	}()

	select {
	case err := <-sendErr:
		a.logger.Warn("send loop exited", "err", err)
		return err
	case err := <-recvErr:
		a.logger.Warn("recv loop exited", "err", err)
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sendLoop — читает из outbox, шлёт в stream.
func (a *Agent) sendLoop(stream etroniumv1.LordService_ConnectClient) error {
	for {
		select {
		case <-a.shutdownCtx.Done():
			return a.shutdownCtx.Err()
		case cmd := <-a.outbox:
			if err := stream.Send(cmd); err != nil {
				return err
			}
		}
	}
}

// recvLoop — читает команды от scheduler'а.
func (a *Agent) recvLoop(ctx context.Context, stream etroniumv1.LordService_ConnectClient, hw *etroniumv1.RegisterRequest) error {
	// 1. Первый message от lord'а — Register
	if err := stream.Send(&etroniumv1.LordCmd{
		Cmd: &etroniumv1.LordCmd_Register{Register: hw},
	}); err != nil {
		return fmt.Errorf("send register: %w", err)
	}

	// 2. Тикер heartbeat
	hbTicker := time.NewTicker(time.Duration(a.cfg.HeartbeatSec) * time.Second)
	defer hbTicker.Stop()

	// 3. Первый message от scheduler'а — RegisterAck
	firstEv, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("recv register ack: %w", err)
	}
	ackEv := firstEv.GetRegisterAck()
	if ackEv == nil {
		return fmt.Errorf("expected RegisterAck, got %T", firstEv.Event)
	}
	a.lordIDMu.Lock()
	a.lordID = ackEv.LordId
	a.lordIDMu.Unlock()
	a.logger.Info("registered with scheduler", "lord_id", a.lordID)

	// 3.5 Lazy-init cgroup manager (нужен lordID для slice path).
	cgm, err := NewCgroupManager(a.lordID, a.logger)
	if err != nil {
		// Не фатально — lord стартует без cgroup, но все ресурсы будут no-op.
		a.logger.Warn("cgroup manager init failed, resource limits disabled",
			"err", err,
		)
	} else {
		a.cgMu.Lock()
		a.cg = cgm
		a.cgMu.Unlock()
	}

	// 4. Цикл обработки событий
	for {
		// Heartbeat (non-blocking) + Recv (blocking)
		select {
		case <-a.shutdownCtx.Done():
			return a.shutdownCtx.Err()
		case <-hbTicker.C:
			if err := a.sendHeartbeat(); err != nil {
				return err
			}
		default:
			// Неблокирующий тик heartbeat сделан, теперь блокирующее чтение.
		}

		// Читаем событие от scheduler'а (blocking, с возможностью отмены)
		type recvResult struct {
			ev *etroniumv1.LordEvent
			err error
		}
		resCh := make(chan recvResult, 1)
		go func() {
			ev, err := stream.Recv()
			resCh <- recvResult{ev: ev, err: err}
		}()
		select {
		case <-a.shutdownCtx.Done():
			return a.shutdownCtx.Err()
		case r := <-resCh:
			if r.err == io.EOF {
				return nil
			}
			if r.err != nil {
				return r.err
			}
			if err := a.handleEvent(ctx, r.ev); err != nil {
				a.logger.Warn("handle event error", "err", err)
			}
		}
	}
}

func (a *Agent) sendHeartbeat() error {
	active := int32(len(a.procs))
	// Берём CPU/RAM текущие из cgroup агрегата
	cpuPct, memBytes := a.getCurrentUsage()
	a.outbox <- &etroniumv1.LordCmd{
		Cmd: &etroniumv1.LordCmd_Heartbeat{
			Heartbeat: &etroniumv1.HeartbeatRequest{
				LordId:          a.lordID,
				CpuUsedPct:      cpuPct,
				MemUsedBytes:    memBytes,
				ActiveProcesses: active,
			},
		},
	}
	return nil
}

// handleEvent — обрабатывает LordEvent от scheduler'а.
func (a *Agent) handleEvent(ctx context.Context, ev *etroniumv1.LordEvent) error {
	switch e := ev.Event.(type) {
	case *etroniumv1.LordEvent_RegisterAck:
		// уже обработали в recvLoop
	case *etroniumv1.LordEvent_HeartbeatAck:
		// ничего, ack
	case *etroniumv1.LordEvent_Spawn:
		return a.handleSpawn(ctx, e.Spawn)
	case *etroniumv1.LordEvent_Kill:
		return a.handleKill(e.Kill)
	case *etroniumv1.LordEvent_LazyDeathAck:
		a.logger.Info("lazy death ack received")
		return errors.New("lazy death requested")
	case *etroniumv1.LordEvent_SetDrain:
		return a.handleSetDrain(ctx, e.SetDrain)
	case *etroniumv1.LordEvent_FilePush:
		a.logger.Warn("file push not implemented in Phase 0")
	default:
		a.logger.Warn("unknown event", "type", fmt.Sprintf("%T", e))
	}
	return nil
}
