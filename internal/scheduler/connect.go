// Package scheduler — connect.go
//
// Реализация LordService.Connect (bidi stream lord↔scheduler).
//
// Архитектура Phase 0:
//
//   1. Lord при старте открывает один долгоживущий stream LordService.Connect
//      к scheduler'у.
//   2. Lord шлёт LordCmd{register} — scheduler создаёт запись о лорде.
//   3. Lord шлёт LordCmd{heartbeat} каждые 10s — scheduler обновляет stats.
//   4. Scheduler хочет запустить процесс → шлёт LordEvent{spawn}.
//   5. Lord запускает процесс локально (fork/exec), шлёт LordCmd{stdio_chunk}
//      для stdout/stderr, в конце LordCmd{process_exit}.
//   6. Scheduler хочет убить процесс → шлёт LordEvent{kill}.
//   7. Lord посылает signal, процесс умирает, приходит LordCmd{process_exit}.
//
// Это позволяет lord'ам быть за NAT без публичного IP.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	etroniumv1 "github.com/midas/Etronium-Scdr/internal/gen/etronium/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// lordSession — состояние одного bidi stream'а от lord'а.
type lordSession struct {
	lordID    string
	logger    *slog.Logger
	stream    etroniumv1.LordService_ConnectServer
	ctx       context.Context
	cancel    context.CancelFunc

	// Канал для отправки событий lord'у. Пишем в него из Spawn/Kill/etc,
	// читает горутина которая крутит stream.Send.
	outbox chan *etroniumv1.LordEvent

	// Map local_pid → process_id для активных процессов на этом lord'е.
	// Нужно чтобы переводить process_exit в правильный entry.
	procsMu sync.RWMutex
	procs   map[int32]string // local_pid → process_id
}

func newLordSession(lordID string, stream etroniumv1.LordService_ConnectServer, logger *slog.Logger) *lordSession {
	ctx, cancel := context.WithCancel(stream.Context())
	return &lordSession{
		lordID:  lordID,
		logger:  logger.With("lord_id", lordID),
		stream:  stream,
		ctx:     ctx,
		cancel:  cancel,
		outbox:  make(chan *etroniumv1.LordEvent, 64),
		procs:   make(map[int32]string),
	}
}

// Connect — точка входа для bidi stream'а.
//
// Этапы:
//   1. Ждём первый message от lord'а — это должен быть Register.
//   2. Создаём lordSession, регистрируем.
//   3. Запускаем две горутины: send (outbox → stream) и recv (stream → handle).
func (s *Server) Connect(stream etroniumv1.LordService_ConnectServer) error {
	// 1. Ждём Register
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "first message must be Register: %v", err)
	}
	regCmd := first.GetRegister()
	if regCmd == nil {
		return status.Error(codes.InvalidArgument, "first message must be Register")
	}

	// 2. Создаём session
	lordID := NewID()
	sess := newLordSession(lordID, stream, s.logger)
	s.lordSessionsMu.Lock()
	s.lordSessions[lordID] = sess
	s.lordSessionsMu.Unlock()

	// Регистрируем лорда в реестре
	info := &etroniumv1.Lord{
		LordId:                lordID,
		Hostname:              regCmd.GetHostname(),
		Os:                    regCmd.GetOs(),
		Arch:                  regCmd.GetArch(),
		CpuCoresPhysical:      regCmd.GetCpuCoresPhysical(),
		MemTotalBytesPhysical: regCmd.GetMemTotalBytesPhysical(),
		AdvertisedCpuShares:   regCmd.GetAdvertisedCpuShares(),
		AdvertisedMemBytes:    regCmd.GetAdvertisedMemBytes(),
		CriuAvailable:         regCmd.GetCriuAvailable(),
		Healthy:               true,
		LastSeen:              nowTimestamp(),
		Reputation:            1.0,
	}
	s.lords.Register(info)

	sess.logger.Info("lord connected", "hostname", info.Hostname, "cores", info.CpuCoresPhysical)

	// 3. Отправляем ack
	sess.outbox <- &etroniumv1.LordEvent{
		Event: &etroniumv1.LordEvent_RegisterAck{
			RegisterAck: &etroniumv1.RegisterResponse{
				LordId:               lordID,
				HeartbeatIntervalSec: 10,
			},
		},
	}

	// 4. Запускаем send/recv горутины
	sendDone := make(chan error, 1)
	go func() {
		sendDone <- sess.sendLoop()
	}()

	recvErr := sess.recvLoop(s)

	// Cleanup
	sess.cancel()
	s.lordSessionsMu.Lock()
	delete(s.lordSessions, lordID)
	s.lordSessionsMu.Unlock()
	s.lords.MarkUnhealthy(lordID)
	sess.logger.Info("lord disconnected")

	sendErr := <-sendDone
	if recvErr != nil && !errors.Is(recvErr, io.EOF) {
		return recvErr
	}
	return sendErr
}

// sendLoop — читает из outbox и шлёт в stream.
func (sess *lordSession) sendLoop() error {
	for {
		select {
		case <-sess.ctx.Done():
			return sess.ctx.Err()
		case ev := <-sess.outbox:
			if err := sess.stream.Send(ev); err != nil {
				return err
			}
		}
	}
}

// recvLoop — читает из stream и обрабатывает команды от lord'а.
func (sess *lordSession) recvLoop(s *Server) error {
	for {
		cmd, err := sess.stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch c := cmd.Cmd.(type) {
		case *etroniumv1.LordCmd_Register:
			// Повторный register — игнорируем
			sess.logger.Warn("duplicate register in stream")
		case *etroniumv1.LordCmd_Heartbeat:
			s.lords.UpdateStats(sess.lordID, c.Heartbeat.GetCpuUsedPct(),
				c.Heartbeat.GetMemUsedBytes(), c.Heartbeat.GetActiveProcesses())
			sess.outbox <- &etroniumv1.LordEvent{
				Event: &etroniumv1.LordEvent_HeartbeatAck{
					HeartbeatAck: &etroniumv1.HeartbeatResponse{
						Ack:               true,
						NextHeartbeatSec:  10,
					},
				},
			}
		case *etroniumv1.LordCmd_Started:
			s.handleStarted(sess, c.Started)
		case *etroniumv1.LordCmd_Io:
			s.handleIo(sess, c.Io)
		case *etroniumv1.LordCmd_ProcessExit:
			s.handleProcessExit(sess, c.ProcessExit)
		case *etroniumv1.LordCmd_CheckpointResponse:
			s.deliverCheckpointResponse(sess.lordID, c.CheckpointResponse)
		case *etroniumv1.LordCmd_RestoreResponse:
			s.logger.Info("restore response received",
				"lord_id", sess.lordID,
				"process_id", c.RestoreResponse.GetProcessId(),
				"local_pid", c.RestoreResponse.GetLocalPid(),
				"ok", c.RestoreResponse.GetOk(),
			)
		case *etroniumv1.LordCmd_LazyDeath:
			s.lords.SetDrain(sess.lordID)
			sess.outbox <- &etroniumv1.LordEvent{
				Event: &etroniumv1.LordEvent_LazyDeathAck{
					LazyDeathAck: &etroniumv1.AcknowledgeLazyDeathResponse{
						Ack:               true,
						DrainTimeoutSec:   c.LazyDeath.GetGracePeriodSec(),
					},
				},
			}
		default:
			sess.logger.Warn("unknown cmd", "cmd", fmt.Sprintf("%T", c))
		}
	}
}

// handleStarted — лорд сообщил что процесс стартовал с local_pid.
func (s *Server) handleStarted(sess *lordSession, started *etroniumv1.ProcessStarted) {
	entry, ok := s.processes.Get(started.ProcessId)
	if !ok {
		sess.logger.Warn("started for unknown process", "process_id", started.ProcessId)
		return
	}
	entry.UpdateState(etroniumv1.ProcessState_PROCESS_STATE_RUNNING, sess.lordID, started.LocalPid)
	sess.procsMu.Lock()
	sess.procs[started.LocalPid] = started.ProcessId
	sess.procsMu.Unlock()
	sess.logger.Info("process running on lord",
		"process_id", started.ProcessId,
		"local_pid", started.LocalPid,
	)
}

// handleIo — IO chunk от lord'а, с явным process_id.
func (s *Server) handleIo(sess *lordSession, io *etroniumv1.ProcessIo) {
	entry, ok := s.processes.Get(io.ProcessId)
	if !ok {
		sess.logger.Warn("io for unknown process", "process_id", io.ProcessId)
		return
	}
	entry.ioBuf.Write(io.Chunk.GetData())
}

// handleProcessExit — обрабатывает завершение процесса.
func (s *Server) handleProcessExit(sess *lordSession, exit *etroniumv1.ProcessExit) {
	entry, ok := s.processes.Get(exit.ProcessId)
	if !ok {
		sess.logger.Warn("exit for unknown process", "process_id", exit.ProcessId)
		return
	}
	entry.mu.Lock()
	entry.Info.CpuUsageUsecTotal = exit.CpuUsageUsec
	entry.Info.MemPeakBytes = exit.MemPeakBytes
	entry.mu.Unlock()

	if exit.ExitSignal != 0 {
		entry.UpdateState(etroniumv1.ProcessState_PROCESS_STATE_STOPPED, "", 0)
	} else {
		entry.UpdateState(etroniumv1.ProcessState_PROCESS_STATE_EXITED, "", 0)
	}
	entry.UpdateResult(exit.ExitCode, exit.ExitSignal)

	sess.procsMu.Lock()
	for pid, pid2 := range sess.procs {
		if pid2 == exit.ProcessId {
			delete(sess.procs, pid)
			break
		}
	}
	sess.procsMu.Unlock()
}

// ============================================================================
// Server helpers для пуша команд лорду
// ============================================================================

// SendSpawn — послать LordEvent{spawn} lord'у через его session.
//
// Вызывается из Spawn RPC после placement.
func (s *Server) SendSpawn(lordID string, req *etroniumv1.SpawnRequest) error {
	sess, err := s.getSession(lordID)
	if err != nil {
		return err
	}
	select {
	case sess.outbox <- &etroniumv1.LordEvent{
		Event: &etroniumv1.LordEvent_Spawn{Spawn: req},
	}:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("send spawn timeout")
	}
}

// SendKill — послать LordEvent{kill} lord'у.
func (s *Server) SendKill(lordID string, req *etroniumv1.KillRequest) error {
	sess, err := s.getSession(lordID)
	if err != nil {
		return err
	}
	select {
	case sess.outbox <- &etroniumv1.LordEvent{
		Event: &etroniumv1.LordEvent_Kill{Kill: req},
	}:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("send kill timeout")
	}
}

func (s *Server) getSession(lordID string) (*lordSession, error) {
	s.lordSessionsMu.RLock()
	defer s.lordSessionsMu.RUnlock()
	sess, ok := s.lordSessions[lordID]
	if !ok {
		return nil, fmt.Errorf("no active session for lord %s", lordID)
	}
	return sess, nil
}

// ============================================================================
// Server fields (добавляем lordSessions к Server)
// ============================================================================

// lordSessions — добавляется в Server отдельным файлом через init pattern.
// Реализовано в server.go через изменение Server struct.
// (См. server.go для определения.)
