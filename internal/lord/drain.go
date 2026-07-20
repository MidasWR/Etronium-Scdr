// Package lord — drain.go
//
// Phase 5 graceful shutdown. When scheduler is shutting down it asks
// each lord to drain:
//   - New spawns: refused (we'll log + send AcknowledgeLazyDeath and
//     scheduler can re-route those).
//   - Existing processes: SIGTERM, wait up to grace_period_sec, then SIGKILL.
//   - Then: agent.Run() returns, main() exits, container stops.
//
// Tenant получает:
//   - Existing processes killed → state=STOPPED with exit_signal=15
//   - On restart, scheduler doesn't have them anymore (Phase 5 has no
//     WAL yet) — tenants have to re-spawn. Acceptable for 5-minute demo.
package lord

import (
	"context"
	"errors"
	"os"
	"sync"
	"syscall"
	"time"

	etroniumv1 "github.com/midas/Etronium-Scdr/internal/gen/etronium/v1"
)

// drainingMu — protects drain state. Once draining=true, lord refuses
// new spawns.
var (
	drainingMu sync.RWMutex
	draining   bool
)

func isDraining() bool {
	drainingMu.RLock()
	defer drainingMu.RUnlock()
	return draining
}

func setDraining(v bool) {
	drainingMu.Lock()
	draining = v
	drainingMu.Unlock()
}

// handleSetDrain — scheduler graceful shutdown request.
//
// 1. Mark lord as draining (refuse future spawns).
// 2. SIGTERM all local processes.
// 3. Wait grace_period_sec; remaining processes get SIGKILL.
// 4. Send AcknowledgeLazyDeath so scheduler can stop waiting.
// 5. Return error so agent.Run() exits → main() exits.
func (a *Agent) handleSetDrain(ctx context.Context, req *etroniumv1.SetDrainRequest) error {
	if isDraining() {
		// Idempotent — already draining.
		return nil
	}
	setDraining(true)

	grace := time.Duration(req.GetGracePeriodSec()) * time.Second
	if grace == 0 {
		grace = 30 * time.Second
	}
	a.logger.Warn("drain requested by scheduler, will refuse new spawns and kill active procs",
		"reason", req.GetReason(),
		"grace_period_sec", int(grace.Seconds()),
	)

	// 1. SIGTERM all local processes.
	a.procsMu.RLock()
	pids := make([]int, 0, len(a.procs))
	for _, lp := range a.procs {
		pids = append(pids, lp.LordPID)
	}
	a.procsMu.RUnlock()

	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			a.logger.Warn("drain SIGTERM failed", "pid", pid, "err", err)
		}
	}

	// 2. Wait grace period, polling for proc table empty.
	deadline := time.Now().Add(grace)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		a.procsMu.RLock()
		alive := len(a.procs)
		a.procsMu.RUnlock()
		if alive == 0 {
			a.logger.Info("drain: all processes exited within grace period")
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
		}
	}

	// 3. SIGKILL anyone left.
	a.procsMu.RLock()
	stillAlive := make([]int, 0)
	for _, lp := range a.procs {
		stillAlive = append(stillAlive, lp.LordPID)
	}
	a.procsMu.RUnlock()
	for _, pid := range stillAlive {
		a.logger.Warn("drain SIGKILL after grace period", "pid", pid)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}

	// 4. Notify scheduler (best-effort — it might already be shutting down).
	select {
	case a.outbox <- &etroniumv1.LordCmd{
		Cmd: &etroniumv1.LordCmd_LazyDeath{
			LazyDeath: &etroniumv1.AcknowledgeLazyDeathRequest{
				LordId:         a.lordID,
				GracePeriodSec: int32(grace.Seconds()),
			},
		},
	}:
	default:
	}

	a.logger.Info("drain complete, exiting")

	// 5. Force-exit so container / process supervisor sees clean stop.
	// Using os.Exit(0) bypasses normal cleanup, but for 5-min demo this
	// is fine — graceful would still let the recv loop spin on closed
	// stream and wait 30+ sec for TCP timeout.
	go func() {
		time.Sleep(100 * time.Millisecond)
		os.Exit(0)
	}()

	return errors.New("drain complete")
}
