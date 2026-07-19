// Package lord — exec.go
//
// Fork/exec процессов с I/O capture.
//
// Phase 0: os/exec без cgroups, без namespaces, без лимитов.
// Phase 1: cgroup v2 slice per process + cpu.weight/memory.max/io.weight/pids.max limits.
package lord

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	etroniumv1 "github.com/midas/Etronium-Scdr/internal/gen/etronium/v1"
)

// handleSpawn — обрабатывает LordEvent{spawn} от scheduler'а.
//
// Алгоритм:
//   1. Берём process_id из req.ProcessId (scheduler его уже проставил)
//   2. Создаём localCmd (os/exec.Cmd с pipes)
//   3. Запускаем
//   4. Шлём ProcessStarted с local_pid
//   5. Горутины pumpStream читают stdout/stderr → ProcessIo chunks
//   6. Ждём завершения, шлём ProcessExit
func (a *Agent) handleSpawn(ctx context.Context, req *etroniumv1.SpawnRequest) error {
	processID := req.GetProcessId()
	if processID == "" {
		return fmt.Errorf("spawn: missing process_id")
	}

	a.logger.Info("handling spawn",
		"process_id", processID,
		"exec", req.ExecPath,
		"argv", req.Argv,
	)

	// Создаём cmd
	cmd := exec.Command(req.ExecPath, req.Argv...)
	if req.WorkingDir != "" {
		cmd.Dir = req.WorkingDir
	}
	cmd.Env = buildEnv(req.Env)
	// Новая process group (для удобного kill целого дерева)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		a.sendProcessExit(processID, -1, 0, 0, 0)
		return fmt.Errorf("start: %w", err)
	}

	// cgroup attach (Phase 1) — перемещаем PID в slice, применяем ResourceSpec.
	a.cgMu.Lock()
	cg := a.cg
	a.cgMu.Unlock()
	if cg != nil {
		resources := protoResourcesToLord(req.GetResources())
		slice, err := cg.CreateProcessSlice(processID, resources)
		if err != nil {
			a.logger.Warn("cgroup create failed, continuing without limits",
				"process_id", processID, "err", err,
			)
		} else if err := cg.Attach(processID, cmd.Process.Pid); err != nil {
			a.logger.Warn("cgroup attach failed",
				"process_id", processID, "pid", cmd.Process.Pid, "err", err,
			)
		} else {
			a.logger.Info("cgroup attached",
				"process_id", processID, "pid", cmd.Process.Pid, "slice", slice,
			)
		}
	}

	// Регистрируем процесс
	lp := &localProcess{
		ProcessID: processID,
		LordPID:   cmd.Process.Pid,
		Cmd: &localCmd{
			execPath: req.ExecPath,
			argv:     req.Argv,
			env:      cmd.Env,
			stdin:    stdin,
			stdout:   stdout,
			stderr:   stderr,
			done:     make(chan struct{}),
		},
		started: time.Now(),
	}
	a.procsMu.Lock()
	a.procs[processID] = lp
	a.procsMu.Unlock()

	a.logger.Info("process started", "process_id", processID, "local_pid", lp.LordPID)

	// Пишем initial stdin если есть
	if len(req.StdinInitial) > 0 {
		_, _ = stdin.Write(req.StdinInitial)
		_ = stdin.Close()
	}

	// Горутины чтения stdout/stderr
	go a.pumpStream(processID, etroniumv1.IOChunk_STREAM_STDOUT, stdout)
	go a.pumpStream(processID, etroniumv1.IOChunk_STREAM_STDERR, stderr)

	// Шлём ProcessStarted с local_pid
	a.outbox <- &etroniumv1.LordCmd{
		Cmd: &etroniumv1.LordCmd_Started{
			Started: &etroniumv1.ProcessStarted{
				ProcessId: processID,
				LocalPid:  int32(lp.LordPID),
				At:        nil,
			},
		},
	}

	// Горутина ожидания завершения
	go func() {
		err := cmd.Wait()
		close(lp.Cmd.done)
		duration := time.Since(lp.started)
		var exitCode, exitSignal int32 = 0, 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				exitCode = int32(ee.ExitCode())
				// Если был killed сигналом — ee.ExitCode() == -1
				if exitCode == -1 {
					if status, ok := ee.Sys().(syscall.WaitStatus); ok {
						exitSignal = int32(status.Signal())
					}
				}
			} else {
				exitCode = -1
				a.logger.Warn("wait error", "process_id", processID, "err", err)
			}
		}
		a.logger.Info("process exited",
			"process_id", processID,
			"local_pid", lp.LordPID,
			"exit_code", exitCode,
			"exit_signal", exitSignal,
			"duration_ms", duration.Milliseconds(),
		)

		// Читаем cgroup stats перед Destroy (Phase 1).
		var cpuUsec, memPeak int64
		if cg != nil {
			stats, err := cg.ReadStats(processID)
			if err == nil {
				cpuUsec = int64(stats.CPUUsageUSec)
				memPeak = int64(stats.MemoryPeak)
			}
			if err := cg.Destroy(processID); err != nil {
				a.logger.Warn("cgroup destroy failed", "process_id", processID, "err", err)
			}
		}

		a.sendProcessExit(processID, exitCode, exitSignal, cpuUsec, memPeak)
		a.procsMu.Lock()
		delete(a.procs, processID)
		a.procsMu.Unlock()
	}()

	return nil
}

// pumpStream — читает из pipe и шлёт chunks в scheduler stream через ProcessIo envelope.
func (a *Agent) pumpStream(processID string, stream etroniumv1.IOChunk_Stream, r io.Reader) {
	buf := make([]byte, 4*1024)
	offset := int64(0)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			a.outbox <- &etroniumv1.LordCmd{
				Cmd: &etroniumv1.LordCmd_Io{
					Io: &etroniumv1.ProcessIo{
						ProcessId: processID,
						Chunk: &etroniumv1.IOChunk{
							Stream: stream,
							Data:   append([]byte(nil), buf[:n]...),
							Offset: offset,
						},
					},
				},
			}
			offset += int64(n)
		}
		if err != nil {
			if err != io.EOF {
				a.logger.Warn("pump read error", "process_id", processID, "stream", stream, "err", err)
			}
			return
		}
	}
}

// handleKill — послать сигнал процессу.
func (a *Agent) handleKill(req *etroniumv1.KillRequest) error {
	a.procsMu.RLock()
	lp, ok := a.procs[req.ProcessId]
	a.procsMu.RUnlock()
	if !ok {
		a.logger.Warn("kill: process not found", "process_id", req.ProcessId)
		return nil
	}
	signal := req.SignalNumber
	if signal == 0 {
		signal = 15 // SIGTERM
	}
	if req.Force {
		signal = 9 // SIGKILL
	}
	// Посылаем сигнал всей process group
	err := syscall.Kill(-lp.LordPID, syscall.Signal(signal))
	if err != nil {
		a.logger.Warn("kill syscall error", "process_id", req.ProcessId, "err", err)
		return err
	}
	a.logger.Info("signal sent",
		"process_id", req.ProcessId,
		"local_pid", lp.LordPID,
		"signal", signal,
	)
	return nil
}

// sendProcessExit — послать ProcessExit в stream.
func (a *Agent) sendProcessExit(processID string, exitCode, exitSignal int32, cpuUsec, memPeak int64) {
	a.outbox <- &etroniumv1.LordCmd{
		Cmd: &etroniumv1.LordCmd_ProcessExit{
			ProcessExit: &etroniumv1.ProcessExit{
				ProcessId:     processID,
				ExitCode:      exitCode,
				ExitSignal:    exitSignal,
				CpuUsageUsec:  cpuUsec,
				MemPeakBytes:  memPeak,
			},
		},
	}
}

// --- helpers ---

// buildEnv — конвертирует map в []string формата KEY=VALUE, добавляя os.Environ().
func buildEnv(env map[string]string) []string {
	out := make([]string, 0, len(os.Environ())+len(env))
	out = append(out, os.Environ()...)
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// protoResourcesToLord — конвертация etroniumv1.ResourceSpec → lord.Resources.
func protoResourcesToLord(r *etroniumv1.ResourceSpec) *Resources {
	if r == nil {
		return nil
	}
	return &Resources{
		CPUShares:     uint32(max32(r.CpuShares, 0)),
		MemLimitBytes: r.MemLimitBytes,
		IOWeight:      uint32(max32(r.IoWeight, 0)),
		PidsLimit:     uint32(max32(r.PidsLimit, 0)),
	}
}

func max32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}
