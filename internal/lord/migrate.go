// Package lord — migrate.go
//
// Phase 3.0: Checkpoint + Restore handlers (ADR 024, 026).
//
// Поток миграции:
//   1. Scheduler шлёт LordEvent{Checkpoint} в bidi stream.
//   2. Lord: cgroup detach pid → criu dump → ответ LordCmd{CheckpointResponse}.
//   3. Scheduler пересылает tar.gz на target lord (через отдельный stream).
//   4. Scheduler шлёт target lord LordEvent{Restore}.
//   5. Target lord: criu restore → cgroup attach → LordCmd{ProcessStarted} с new_local_pid.
//
// Source lord отправляет ProcessExit{reason=MIGRATED} после успешного Checkpoint.
// Target lord становится новым хозяином процесса.

package lord

import (
	"context"
	"fmt"
	"os"

	etroniumv1 "github.com/midas/Etronium-Scdr/internal/gen/etronium/v1"
)

// handleCheckpoint — LordEvent.Checkpoint → CRIU dump.
// Отвечает через outbox: LordCmd{CheckpointResponse}.
func (a *Agent) handleCheckpoint(ctx context.Context, ev *etroniumv1.LordEvent_Checkpoint) error {
	req := ev.Checkpoint
	processID := req.GetProcessId()
	dir := req.GetCheckpointDir()

	if processID == "" {
		return fmt.Errorf("checkpoint: missing process_id")
	}
	if dir == "" {
		return fmt.Errorf("checkpoint: missing checkpoint_dir")
	}
	if !a.criu.Available() {
		a.respondCheckpointError(processID, "criu not available")
		return fmt.Errorf("criu not available")
	}

	a.procsMu.RLock()
	lp, ok := a.procs[processID]
	a.procsMu.RUnlock()
	if !ok {
		a.respondCheckpointError(processID, fmt.Sprintf("process %s not found", processID))
		return fmt.Errorf("checkpoint: process %s not found", processID)
	}

	a.logger.Info("checkpoint requested",
		"process_id", processID,
		"local_pid", lp.LordPID,
		"dir", dir,
		"leave_running", req.GetLeaveRunning(),
	)

	// 1. Detach pid из cgroup (CRIU хочет standalone процесс).
	if err := a.detachPidFromCgroup(lp.LordPID); err != nil {
		a.logger.Warn("cgroup detach failed (proceeding anyway)",
			"err", err, "pid", lp.LordPID)
	}

	// 2. CRIU dump
	if err := os.MkdirAll(dir, 0o755); err != nil {
		a.respondCheckpointError(processID, fmt.Sprintf("mkdir: %v", err))
		return fmt.Errorf("mkdir: %w", err)
	}

	if err := a.criu.Checkpoint(ctx, lp.LordPID, CheckpointOpts{
		ImagesDir:    dir,
		LeaveRunning: req.GetLeaveRunning(),
		Verbose:      false,
	}); err != nil {
		a.respondCheckpointError(processID, err.Error())
		return err
	}

	size, _ := dirSize(dir)

	a.logger.Info("checkpoint complete",
		"process_id", processID,
		"path", dir,
		"size_bytes", size,
	)

	// 3. Отвечаем scheduler'у через outbox
	a.outbox <- &etroniumv1.LordCmd{
		Cmd: &etroniumv1.LordCmd_CheckpointResponse{
			CheckpointResponse: &etroniumv1.CheckpointResponse{
				Ok:             true,
				CheckpointPath: dir,
				SizeBytes:      size,
				ProcessId:      processID,
			},
		},
	}

	// 4. Если leave_running=false — ProcessExit{reason=MIGRATED}
	if !req.GetLeaveRunning() {
		a.procsMu.Lock()
		delete(a.procs, processID)
		a.procsMu.Unlock()

		a.outbox <- &etroniumv1.LordCmd{
			Cmd: &etroniumv1.LordCmd_ProcessExit{
				ProcessExit: &etroniumv1.ProcessExit{
					ProcessId:  processID,
					ExitCode:   0,
					ExitSignal: 0,
					Reason:     etroniumv1.ProcessExitReason_PROCESS_EXIT_REASON_MIGRATED,
				},
			},
		}
		a.logger.Info("process exited due to migration",
			"process_id", processID, "local_pid", lp.LordPID)
	}

	return nil
}

// handleRestore — LordEvent.Restore → CRIU restore + cgroup attach.
// Отвечает через outbox: LordCmd{ProcessStarted} с new_local_pid.
func (a *Agent) handleRestore(ctx context.Context, ev *etroniumv1.LordEvent_Restore) error {
	req := ev.Restore
	processID := req.GetProcessId()
	imagesDir := req.GetCheckpointPath()

	if processID == "" {
		return fmt.Errorf("restore: missing process_id")
	}
	if imagesDir == "" {
		return fmt.Errorf("restore: missing checkpoint_path")
	}
	if !a.criu.Available() {
		return fmt.Errorf("criu not available")
	}

	a.logger.Info("restore requested",
		"process_id", processID,
		"images_dir", imagesDir,
		"exec", req.GetExecPath(),
	)

	// ExecSpec из RestoreRequest
	execSpec := ExecSpec{
		ExecPath:   req.GetExecPath(),
		Argv:       req.GetArgv(),
		WorkingDir: req.GetWorkingDir(),
	}
	for k, v := range req.GetEnv() {
		execSpec.Env = append(execSpec.Env, fmt.Sprintf("%s=%s", k, v))
	}

	// Создаём cgroup slice для нового процесса ДО restore.
	a.lordIDMu.RLock()
	lordID := a.lordID
	a.lordIDMu.RUnlock()

	var slicePath string
	if a.cg != nil {
		slicePath, _ = a.cg.CreateProcessSlice(processID, protoToResources(req.GetResources()))
	}

	// CRIU restore с --restore-detached
	newPID, err := a.criu.Restore(ctx, RestoreOpts{
		ImagesDir: imagesDir,
	}, execSpec)
	if err != nil {
		return fmt.Errorf("criu restore: %w", err)
	}

	// Прикрепить процесс в наш slice. Если lord живёт в отдельном PID namespace
	// (unshare -p), newPID это namespace PID — cgroup.procs ожидает global PID,
	// attach может fail. Это best-effort: процесс работает, просто без cgroup лимитов.
	if slicePath != "" && a.cg != nil {
		if err := a.cg.AttachPidToSlice(newPID, slicePath); err != nil {
			a.logger.Warn("attach restored pid to slice failed (process still works, just no cgroup limits)",
				"err", err, "pid", newPID, "slice", slicePath)
		} else {
			a.logger.Info("cgroup attached after restore", "pid", newPID, "slice", slicePath)
		}
	}

	// Регистрируем процесс в local table.
	a.procsMu.Lock()
	a.procs[processID] = &localProcess{
		ProcessID: processID,
		LordPID:   newPID,
		Cmd:       nil,
		started:   nowFunc(),
	}
	a.procsMu.Unlock()

	// Шлём ProcessStarted через stream.
	a.outbox <- &etroniumv1.LordCmd{
		Cmd: &etroniumv1.LordCmd_Started{
			Started: &etroniumv1.ProcessStarted{
				ProcessId: processID,
				LocalPid:  int32(newPID),
			},
		},
	}

	// Также RestoreResponse (если scheduler ждёт).
	a.outbox <- &etroniumv1.LordCmd{
		Cmd: &etroniumv1.LordCmd_RestoreResponse{
			RestoreResponse: &etroniumv1.RestoreResponse{
				Ok:       true,
				LocalPid: int32(newPID),
				ProcessId: processID,
			},
		},
	}

	a.logger.Info("restore complete",
		"process_id", processID,
		"new_local_pid", newPID,
		"lord_id", lordID,
		"slice", slicePath,
	)

	return nil
}

// respondCheckpointError — отправляет error response scheduler'у через outbox.
func (a *Agent) respondCheckpointError(processID, msg string) {
	a.logger.Error("checkpoint failed", "process_id", processID, "msg", msg)
	a.outbox <- &etroniumv1.LordCmd{
		Cmd: &etroniumv1.LordCmd_CheckpointResponse{
			CheckpointResponse: &etroniumv1.CheckpointResponse{
				Ok:           false,
				ErrorMessage: msg,
				ProcessId:    processID,
			},
		},
	}
}
