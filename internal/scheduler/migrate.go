// Package scheduler — migrate.go
//
// Phase 3.0: process migration via CRIU checkpoint/restore.
//
// Архитектура (ADR 024, 026):
//   1. Tenant вызывает SchedulerService.Migrate(process_id, target_lord_id, reason).
//   2. Scheduler переводит процесс в state=MIGRATING, шлёт LordEvent{Checkpoint}
//      source lord'у. Source lord делает criu dump в shared dir
//      (/var/etronium/cp/<process_id>/).
//   3. Source lord шлёт LordCmd{CheckpointResponse} через stream. Scheduler
//      проверяет ok=true.
//   4. Scheduler шлёт LordEvent{Restore} target lord'у с тем же images_dir.
//      Target lord делает criu restore и шлёт LordCmd{ProcessStarted} с new_local_pid.
//   5. Scheduler обновляет process_table: lord_id=target, local_pid=new.
//
// Требование Phase 3.0 MVP: source и target lord'ы разделяют /var/etronium/cp
// (docker volume или NFS). Phase 3.1+ заменит это на relay через gRPC FilePull/Push.

package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	etroniumv1 "github.com/midas/Etronium-Scdr/internal/gen/etronium/v1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sharedCheckpointDir — общая файловая система между lord'ами.
// В docker compose это shared volume. В e2e_phase3.sh монтируется явно.
// Можно переопределить через SCHEDULER_CHECKPOINT_DIR env var.
var sharedCheckpointDir = getCheckpointDir()

func getCheckpointDir() string {
	if v := os.Getenv("SCHEDULER_CHECKPOINT_DIR"); v != "" {
		return v
	}
	return "/var/etronium/cp"
}

// Migrate — SchedulerService.Migrate handler.
func (s *Server) Migrate(ctx context.Context, req *etroniumv1.MigrateRequest) (*etroniumv1.MigrateResponse, error) {
	processID := req.GetProcessId()
	if processID == "" {
		return nil, status.Error(codes.InvalidArgument, "process_id required")
	}

	// 1. Найти процесс
	entry, ok := s.processes.Get(processID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "process %s not found", processID)
	}

	// Snapshot данных для restore (exec, argv, env, resources)
	entry.mu.Lock()
	sourceLordID := entry.Info.GetRef().GetLordId()
	sourcePID := entry.Info.GetRef().GetLocalPid()
	execPath := entry.Info.GetExecPath()
	argv := append([]string{}, entry.Info.GetArgv()...)
	env := make(map[string]string, len(entry.Info.GetEnv()))
	for k, v := range entry.Info.GetEnv() {
		env[k] = v
	}
	resources := entry.Info.GetResources()
	workingDir := entry.Info.GetWorkingDir()
	entry.mu.Unlock()

	if sourceLordID == "" || sourcePID == 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "process %s not running (state=%s)", processID, entry.StateString())
	}

	// 2. Проверить что source lord поддерживает CRIU
	sourceLord, ok := s.lords.Get(sourceLordID)
	if !ok || !sourceLord.CriuAvailable {
		return nil, status.Errorf(codes.FailedPrecondition, "source lord %s does not support CRIU", sourceLordID)
	}

	// 3. Выбрать target lord
	targetLordID := req.GetTargetLordId()
	if targetLordID == "" {
		// auto-placement: лучший lord кроме source
		targetLordID = s.lords.PickDifferent(sourceLordID)
		if targetLordID == "" {
			return nil, status.Error(codes.Unavailable, "no other healthy lord available")
		}
	}
	if targetLordID == sourceLordID {
		return nil, status.Errorf(codes.InvalidArgument, "target lord equals source (%s)", sourceLordID)
	}
	targetLord, ok := s.lords.Get(targetLordID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "target lord %s not found", targetLordID)
	}
	if !targetLord.CriuAvailable {
		return nil, status.Errorf(codes.FailedPrecondition, "target lord %s does not support CRIU", targetLordID)
	}

	// 4. Проверить target session
	if _, err := s.getSession(targetLordID); err != nil {
		return nil, status.Errorf(codes.Unavailable, "target lord %s not connected: %v", targetLordID, err)
	}

	checkpointDir := filepath.Join(sharedCheckpointDir, processID)

	s.logger.Info("migration started",
		"process_id", processID,
		"from_lord", sourceLordID,
		"to_lord", targetLordID,
		"reason", req.GetReason(),
		"checkpoint_dir", checkpointDir,
	)

	// 5. Переводим в MIGRATING
	entry.UpdateState(etroniumv1.ProcessState_PROCESS_STATE_MIGRATING, "", 0)

	// 6. Шлём Checkpoint source lord'у
	if err := s.SendCheckpoint(sourceLordID, &etroniumv1.CheckpointRequest{
		ProcessId:      processID,
		CheckpointDir:  checkpointDir,
		LeaveRunning:   false,
	}); err != nil {
		// Rollback state
		entry.UpdateState(etroniumv1.ProcessState_PROCESS_STATE_RUNNING, sourceLordID, sourcePID)
		return nil, status.Errorf(codes.Unavailable, "send checkpoint to %s: %v", sourceLordID, err)
	}

	// 7. Ждём CheckpointResponse
	cpResp, err := s.waitCheckpointResponse(ctx, sourceLordID, processID, 60*time.Second)
	if err != nil {
		entry.UpdateState(etroniumv1.ProcessState_PROCESS_STATE_RUNNING, sourceLordID, sourcePID)
		return nil, status.Errorf(codes.DeadlineExceeded, "checkpoint response: %v", err)
	}
	if !cpResp.GetOk() {
		entry.UpdateState(etroniumv1.ProcessState_PROCESS_STATE_RUNNING, sourceLordID, sourcePID)
		return nil, status.Errorf(codes.Internal, "checkpoint failed: %s", cpResp.GetErrorMessage())
	}

	s.logger.Info("checkpoint dumped",
		"process_id", processID,
		"size_bytes", cpResp.GetSizeBytes(),
		"path", cpResp.GetCheckpointPath(),
	)

	// 8. Шлём Restore target lord'у
	if err := s.SendRestore(targetLordID, &etroniumv1.RestoreRequest{
		ProcessId:     processID,
		CheckpointPath: cpResp.GetCheckpointPath(),
		ExecPath:      execPath,
		Argv:          argv,
		Env:           env,
		WorkingDir:    workingDir,
		Resources:     resources,
	}); err != nil {
		// TODO: на этом этапе source lord уже process_exit'нул — процесс потерян.
		// Phase 3.1+: cleanup + retry.
		entry.UpdateState(etroniumv1.ProcessState_PROCESS_STATE_EXITED, "", 0)
		return nil, status.Errorf(codes.Unavailable, "send restore to %s: %v", targetLordID, err)
	}

	// 9. Ждём ProcessStarted от target lord'а (через state change в process_table)
	newPID, err := s.waitRestoreComplete(ctx, processID, 60*time.Second)
	if err != nil {
		entry.UpdateState(etroniumv1.ProcessState_PROCESS_STATE_EXITED, "", 0)
		return nil, status.Errorf(codes.DeadlineExceeded, "restore complete: %v", err)
	}

	// 10. State уже обновлён в handleStarted, просто читаем
	entry.mu.Lock()
	newState := entry.Info.GetState()
	entry.mu.Unlock()

	s.logger.Info("migration complete",
		"process_id", processID,
		"new_lord_id", targetLordID,
		"new_local_pid", newPID,
		"state", newState,
	)

	return &etroniumv1.MigrateResponse{
		Acknowledged: true,
		CurrentState: newState,
		NewLordId:    targetLordID,
		NewLocalPid:  int32(newPID),
	}, nil
}

// waitCheckpointResponse ждёт CheckpointResponse для (lord, process).
// Использует канал ожидания в ProcessEntry (Phase 3.0: in-memory chan).
func (s *Server) waitCheckpointResponse(ctx context.Context, lordID, processID string, timeout time.Duration) (*etroniumv1.CheckpointResponse, error) {
	ch := s.registerCheckpointWait(lordID, processID)
	defer s.cancelCheckpointWait(lordID, processID)

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(timeout):
		return nil, errors.New("timeout")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// waitRestoreComplete ждёт пока process перейдёт из MIGRATING в RUNNING на новом lord'е.
func (s *Server) waitRestoreComplete(ctx context.Context, processID string, timeout time.Duration) (int32, error) {
	entry, ok := s.processes.Get(processID)
	if !ok {
		return 0, errors.New("process gone")
	}

	deadline := time.After(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		entry.mu.Lock()
		state := entry.Info.GetState()
		pid := entry.Info.GetRef().GetLocalPid()
		entry.mu.Unlock()

		if state == etroniumv1.ProcessState_PROCESS_STATE_RUNNING && pid > 0 {
			return pid, nil
		}
		if state == etroniumv1.ProcessState_PROCESS_STATE_EXITED || state == etroniumv1.ProcessState_PROCESS_STATE_STOPPED {
			return 0, fmt.Errorf("process exited during restore")
		}

		select {
		case <-deadline:
			return 0, errors.New("timeout")
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticker.C:
		}
	}
}

// SendCheckpoint — послать LordEvent{checkpoint} lord'у через его session.
func (s *Server) SendCheckpoint(lordID string, req *etroniumv1.CheckpointRequest) error {
	sess, err := s.getSession(lordID)
	if err != nil {
		return err
	}
	select {
	case sess.outbox <- &etroniumv1.LordEvent{
		Event: &etroniumv1.LordEvent_Checkpoint{Checkpoint: req},
	}:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("send checkpoint timeout")
	}
}

// SendRestore — послать LordEvent{restore} lord'у через его session.
func (s *Server) SendRestore(lordID string, req *etroniumv1.RestoreRequest) error {
	sess, err := s.getSession(lordID)
	if err != nil {
		return err
	}
	select {
	case sess.outbox <- &etroniumv1.LordEvent{
		Event: &etroniumv1.LordEvent_Restore{Restore: req},
	}:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("send restore timeout")
	}
}

// ============================================================================
// Checkpoint response routing
// ============================================================================

// checkpointWaitKey — ключ для регистрации waiter'а.
type checkpointWaitKey struct {
	lordID    string
	processID string
}

func (s *Server) registerCheckpointWait(lordID, processID string) <-chan *etroniumv1.CheckpointResponse {
	key := checkpointWaitKey{lordID, processID}
	ch := make(chan *etroniumv1.CheckpointResponse, 1)
	s.cpWaitsMu.Lock()
	s.cpWaits[key] = ch
	s.cpWaitsMu.Unlock()
	return ch
}

func (s *Server) cancelCheckpointWait(lordID, processID string) {
	key := checkpointWaitKey{lordID, processID}
	s.cpWaitsMu.Lock()
	if ch, ok := s.cpWaits[key]; ok {
		delete(s.cpWaits, key)
		close(ch)
	}
	s.cpWaitsMu.Unlock()
}

// deliverCheckpointResponse — вызывается из connect.go при получении LordCmd_CheckpointResponse.
func (s *Server) deliverCheckpointResponse(lordID string, resp *etroniumv1.CheckpointResponse) {
	key := checkpointWaitKey{lordID, resp.GetProcessId()}
	s.cpWaitsMu.Lock()
	ch, ok := s.cpWaits[key]
	if ok {
		delete(s.cpWaits, key)
	}
	s.cpWaitsMu.Unlock()
	if ok {
		ch <- resp
		s.logger.Debug("checkpoint response delivered", "lord_id", lordID, "process_id", resp.GetProcessId())
	} else {
		s.logger.Warn("no waiter for checkpoint response", "lord_id", lordID, "process_id", resp.GetProcessId())
	}
}
