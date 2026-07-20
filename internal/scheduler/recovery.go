// Package scheduler — recovery.go
//
// Phase 3.4: lord died → respawn processes on healthy lord.
//
// State machine:
//   1. Lord dies (stream closed for any reason: SIGTERM, OOM, network).
//   2. We mark the lord unhealthy. Heartbeat sweeper eventually retires it.
//   3. Each PROCESS_STATE_RUNNING process whose LordId == deadLord is
//      candidate for respawn.
//   4. For each candidate we:
//        a. Mark state PROCESS_STATE_RESTARTING (new state, transient).
//        b. Pick a fresh lord via Lords.Pick("").
//        c. Update entry.LordId = newLord, with same exec/argv/env/etc.
//        d. Issue LordEvent{spawn} to the new lord (same code path as
//           the original Spawn RPC, minus placement).
//        e. Update Result to {RestartCount: old + 1, ExitCode: -2,
//           LastError: "lord-disconnected"} (state preserved as info).
//
// This is **not** migration — the process is re-launched from scratch.
// For processes with V5 state dump opt-in (CheckpointEverySec > 0), the
// application itself has been writing state to /tmp/etronium/state/ and
// can read it back on startup to continue from where it was.
package scheduler

import (
	"context"
	"fmt"
	"time"

	etroniumv1 "github.com/midas/Etronium-Scdr/internal/gen/etronium/v1"
)

// onLordDisconnect — вызывается из Connect() после lord disconnect.
//
//   - Marks the lord unhealthy (already done in connect.go, but we redo it
//     for safety in case the call ordering changes).
//   - Iterates process_table, finds RUNNING processes on this lord.
//   - Schedules a respawn on a healthy lord via PlaceAndSendSpawn.
//
// We deliberately do not block Connect on respawn — lord disconnects should
// fail fast. Recovery happens in a background goroutine.
func (s *Server) onLordDisconnect(deadLordID string) {
	s.logger.Info("recovery: lord disconnected, will respawn its processes",
		"dead_lord_id", deadLordID,
	)
	go s.recoverFromLordDisconnect(deadLordID)
}

// recoverFromLordDisconnect is a separate goroutine so the sender end of
// the stream is not blocked on recovery time.
func (s *Server) recoverFromLordDisconnect(deadLordID string) {
	candidates := s.processes.ListByLord(deadLordID, func(e *ProcessEntry) bool {
		state := e.Info.GetState()
		return state == etroniumv1.ProcessState_PROCESS_STATE_RUNNING ||
			state == etroniumv1.ProcessState_PROCESS_STATE_READY ||
			state == etroniumv1.ProcessState_PROCESS_STATE_PAUSED ||
			state == etroniumv1.ProcessState_PROCESS_STATE_MIGRATING
	})

	if len(candidates) == 0 {
		s.logger.Debug("recovery: no processes to respawn",
			"dead_lord_id", deadLordID,
		)
		return
	}

	s.logger.Info("recovery: candidates",
		"dead_lord_id", deadLordID,
		"count", len(candidates),
	)

	// Retry loop: pick a lord and respawn. If placement fails (no lord),
	// wait briefly and retry. We don't crash on the first failure to
	// avoid losing recovery when bursts of lord deaths happen.
	const maxAttempts = 10
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}

		targetLord := s.lords.Pick("")
		if targetLord == nil {
			s.logger.Warn("recovery: no healthy lord available yet",
				"dead_lord_id", deadLordID,
				"attempt", attempt,
			)
			continue
		}

		succeeded := 0
		for _, entry := range candidates {
			if err := s.respawnProcessOnLord(entry, targetLord.LordId); err != nil {
				s.logger.Warn("recovery: respawn failed",
					"process_id", entry.Info.GetRef().GetProcessId(),
					"old_lord", deadLordID,
					"new_lord", targetLord.LordId,
					"err", err,
				)
				continue
			}
			succeeded++
		}
		s.logger.Info("recovery: respawn pass done",
			"dead_lord_id", deadLordID,
			"target_lord", targetLord.LordId,
			"succeeded", succeeded,
			"of", len(candidates),
			"attempt", attempt,
		)
		// We stop after first successful attempt; subsequent ones are
		// no-op because process_table entries are no longer RUNNING on
		// deadLord once they've been respawned.
		_ = succeeded
		break
	}

	// Any candidates still in RESTARTING after retry exhaustion get STOPPED.
	for _, entry := range candidates {
		if entry.Info.GetState() == etroniumv1.ProcessState_PROCESS_STATE_RESTARTING {
			entry.mu.Lock()
			entry.Info.LastError = "no lord available for respawn"
			entry.mu.Unlock()
			entry.UpdateState(etroniumv1.ProcessState_PROCESS_STATE_STOPPED, "", 0)
			entry.UpdateResult(-5, 0) // -5 = "no lord available"
		}
	}

	// Final: log summary.
	s.logger.Info("recovery: complete",
		"dead_lord_id", deadLordID,
		"candidates_seen", len(candidates),
	)
}

// respawnProcessOnLord sends a new spawn event for an existing entry on a
// different lord. We do not create a new process_id — we reuse the existing
// entry and just update Ref.LordId.
//
// Note: this is **kill-old-then-start-new** semantics. If the original
// process was somehow still alive (very rare race), we'd send kill + spawn.
// We don't issue kill here — the old lord is gone.
func (s *Server) respawnProcessOnLord(entry *ProcessEntry, newLordID string) error {
	processID := entry.Info.GetRef().GetProcessId()

	// Re-check state: a delayed ProcessExit / Kill / manual finalize may
	// have transitioned the entry between recovery sweep and now.
	entry.mu.Lock()
	curState := entry.Info.GetState()
	if curState == etroniumv1.ProcessState_PROCESS_STATE_EXITED ||
		curState == etroniumv1.ProcessState_PROCESS_STATE_STOPPED {
		entry.mu.Unlock()
		s.logger.Debug("recovery: entry already finalized, skipping respawn",
			"process_id", processID,
			"state", curState.String(),
		)
		return nil
	}
	entry.mu.Unlock()

	// Honour max_restarts. If exceeded, mark STOPPED with error.
	maxR := entry.Info.GetMaxRestarts()
	if maxR >= 0 && int(entry.Info.GetRestartCount()) >= int(maxR) {
		s.logger.Warn("recovery: max_restarts reached, NOT respawning",
			"process_id", processID,
			"restart_count", entry.Info.GetRestartCount(),
			"max_restarts", maxR,
		)
		entry.mu.Lock()
		entry.Info.LastError = fmt.Sprintf("max_restarts=%d reached", maxR)
		entry.mu.Unlock()
		entry.UpdateState(etroniumv1.ProcessState_PROCESS_STATE_STOPPED, "", 0)
		entry.UpdateResult(-4, 0) // -4 = "max_restarts exceeded"
		return fmt.Errorf("max_restarts=%d reached", maxR)
	}

	// Mark RESTARTING transient state.
	entry.UpdateState(
		etroniumv1.ProcessState_PROCESS_STATE_RESTARTING,
		"", 0, // clear ref until new lord acks
	)
	entry.mu.Lock()
	entry.Info.Ref.LordId = newLordID
	entry.Info.RestartCount++
	entry.mu.Unlock()

	// Build spawn request from existing entry.
	// NB: ProcessInfo does not carry StdinInitial / CriuCheckpointable,
	// we leave those as defaults. If a process truly relies on stdin
	// pipe on initial start, it must declare it via the original
	// SpawnRequest; we cannot reproduce it here.
	req := &etroniumv1.SpawnRequest{
		ProcessId:          processID,
		TenantId:           entry.Info.GetTenantId(),
		ExecPath:           entry.Info.GetExecPath(),
		Argv:               entry.Info.GetArgv(),
		Env:                entry.Info.GetEnv(),
		WorkingDir:         entry.Info.GetWorkingDir(),
		Resources:          entry.Info.GetResources(),
		StateDumpPathHint:  entry.Info.GetStateDumpPath(),
		MaxRestarts:        entry.Info.GetMaxRestarts(),
	}

	if err := s.SendSpawn(newLordID, req); err != nil {
		entry.mu.Lock()
		entry.Info.LastError = fmt.Sprintf("respawn on %s: %v", newLordID, err)
		entry.mu.Unlock()
		entry.UpdateState(etroniumv1.ProcessState_PROCESS_STATE_STOPPED, "", 0)
		entry.UpdateResult(-2, 0) // -2 = "lord disconnected, respawn failed"
		return err
	}

	// Schedule a timeout — if lord doesn't ack within 30s, mark as stopped.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		<-ctx.Done()
		// If still RESTARTING (no ProcessStarted arrived), give up.
		if state := entry.Info.GetState(); state == etroniumv1.ProcessState_PROCESS_STATE_RESTARTING {
			s.logger.Warn("recovery: respawn ack timeout",
				"process_id", processID,
			)
			entry.mu.Lock()
			entry.Info.LastError = "respawn ack timeout (>30s)"
			entry.mu.Unlock()
			entry.UpdateState(etroniumv1.ProcessState_PROCESS_STATE_STOPPED, "", 0)
			entry.UpdateResult(-3, 0) // -3 = "respawn ack timeout"
		}
	}()

	return nil
}

