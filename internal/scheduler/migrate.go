// Package scheduler — migrate.go
//
// Phase 3 was CRIU-based live migration. After session experience
// (kernel 6.17 + Docker + CRIU 4.2 = namespace death kills target),
// we abandoned live migration in favour of Phase 3.4 fault tolerance
// (respawn-on-disconnect + V5 opt-in state serialization, see
// internal/scheduler/recovery.go).
//
// This file is kept as a stub so future contributors know the path
// was considered. If/when a host-kernel with stable CRIU namespace
// support appears, this is where the live-migration orchestrator
// will live.
//
// The `Migrate` RPC still exists in proto as an alias for fault-tolerant
// respawn: scheduler picks a different lord and re-issues spawn. This
// is the same code path the disconnect-recovery path uses.
//
// See docs/DECISIONS.md ADR 028-029 for context.
package scheduler

import (
	"context"
	"fmt"

	etroniumv1 "github.com/midas/Etronium-Scdr/internal/gen/etronium/v1"
)

// Migrate — fault-tolerant respawn of a process on a different lord.
//
// NOT a CRIU migration. Just: pick a fresh lord, send LordEvent{spawn}
// with the original exec/argv/env (same code path as respawnProcessOnLord
// in recovery.go).
//
// Returns Acknowledge=true if a fresh lord was selected and spawn sent.
// Returns error if no healthy alternative lord exists or send failed.
func (s *Server) Migrate(ctx context.Context, req *etroniumv1.MigrateRequest) (*etroniumv1.MigrateResponse, error) {
	processID := req.GetProcessId()

	entry, ok := s.processes.Get(processID)
	if !ok {
		return nil, fmt.Errorf("process %s not found", processID)
	}

	// Honour "auto" semantics: pick any healthy lord different from current.
	targetLordID := req.GetTargetLordId()
	if req.GetAuto() || targetLordID == "" {
		targetLordID = s.lords.PickDifferent(entry.Info.GetRef().GetLordId())
		if targetLordID == "" {
			return nil, fmt.Errorf("no alternative healthy lord available")
		}
	}

	// Bail if target is same as current (no migration).
	if targetLordID == entry.Info.GetRef().GetLordId() {
		return &etroniumv1.MigrateResponse{
			Acknowledged: false,
			CurrentState: entry.Info.GetState(),
			NewLordId:    targetLordID,
			NewLocalPid:  entry.Info.GetRef().GetLocalPid(),
		}, nil
	}

	if err := s.respawnProcessOnLord(entry, targetLordID); err != nil {
		return nil, fmt.Errorf("respawn: %w", err)
	}

	return &etroniumv1.MigrateResponse{
		Acknowledged: true,
		CurrentState: entry.Info.GetState(),
		NewLordId:    targetLordID,
		NewLocalPid:  0, // not yet known, ack from lord will update
	}, nil
}
