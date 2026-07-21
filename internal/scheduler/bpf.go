// BPF map updates for lord placement — uses cilium/ebpf to update
// pinned BPF hash maps directly (no bpftool subprocess needed).
//
// Phase 3.3: scheduler writes CPU mask and DSQ ID into the BPF maps
// (pinned at /sys/fs/bpf/etronium/maps) when lord registers.
//
// Maps (managed by etronium BPF scheduler):
//   etr_task_lord   u32 pid -> u32 lord_id
//   etr_lord_cpus   u32 lord_id -> u64 cpu_mask
//   etr_lord_dsq    u32 lord_id -> u64 dsq_id
package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/cilium/ebpf"
)

// BPFMaps — paths to pinned maps.
var BPFMaps = struct {
	TaskLord string
	LordCpus string
	LordDSQ  string
}{
	TaskLord: "/sys/fs/bpf/etronium/maps/etr_task_lord",
	LordCpus: "/sys/fs/bpf/etronium/maps/etr_lord_cpus",
	LordDSQ:  "/sys/fs/bpf/etronium/maps/etr_lord_dsq",
}

// pinnedHandles cache of opened pinned maps (lazy).
var (
	pinnedMu sync.Mutex
	cache    = map[string]*ebpf.Map{}
)

// openPin — open pinned map (cached).
func openPin(path string) (*ebpf.Map, error) {
	pinnedMu.Lock()
	defer pinnedMu.Unlock()
	if m, ok := cache[path]; ok {
		return m, nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("pinned map not found at %s: %w", path, err)
	}
	m, err := ebpf.LoadPinnedMap(path, nil)
	if err != nil {
		return nil, fmt.Errorf("load pinned map %s: %w", path, err)
	}
	cache[path] = m
	return m, nil
}

// LordIDFromHostname — stable u32 hash for BPF map key.
// Same hostname → same lord_id (consistent across reconnects).
func LordIDFromHostname(hostname string) uint32 {
	h := sha256.Sum256([]byte(hostname))
	return binary.LittleEndian.Uint32(h[:4])
}

// LordDSQID — derive DSQ ID from lord_id. Bit 63 = 0 (non-builtin).
// Pattern: 0xE77000000000xx where xx = lord_id low byte.
func LordDSQID(lordID uint32) uint64 {
	return 0xE7700000000000 | uint64(lordID&0xFF)
}

// LordCPUMaskFromShares — CPU mask = low N bits set, where N = advertised shares.
// If shares=0 or shares>16, returns 0x1 (CPU 0 default).
func LordCPUMaskFromShares(shares uint32) uint64 {
	if shares == 0 || shares > 16 {
		return 0x1
	}
	return (uint64(1) << shares) - 1
}

// RegisterLordBPF — register lord in BPF maps: lord_id → cpu_mask, dsq_id.
func RegisterLordBPF(ctx context.Context, hostname string, cpuShares uint32, logger *slog.Logger) error {
	lordID := LordIDFromHostname(hostname)
	cpuMask := LordCPUMaskFromShares(cpuShares)
	dsqID := LordDSQID(lordID)

	// Update etr_lord_cpus (lord_id -> u64 cpu_mask)
	cpusMap, err := openPin(BPFMaps.LordCpus)
	if err != nil {
		return fmt.Errorf("open etr_lord_cpus: %w", err)
	}
	if err := cpusMap.Put(lordID, cpuMask); err != nil {
		return fmt.Errorf("put etr_lord_cpus: %w", err)
	}

	// Update etr_lord_dsq (lord_id -> u64 dsq_id)
	dsqMap, err := openPin(BPFMaps.LordDSQ)
	if err != nil {
		return fmt.Errorf("open etr_lord_dsq: %w", err)
	}
	if err := dsqMap.Put(lordID, dsqID); err != nil {
		return fmt.Errorf("put etr_lord_dsq: %w", err)
	}

	logger.Info("lord registered in BPF maps",
		"hostname", hostname, "lord_id", lordID,
		"cpu_mask", fmt.Sprintf("0x%x", cpuMask), "dsq_id", fmt.Sprintf("0x%x", dsqID))
	return nil
}

// AssignTaskBPF — register task → lord mapping.
func AssignTaskBPF(ctx context.Context, taskPID uint32, lordID uint32, logger *slog.Logger) error {
	taskMap, err := openPin(BPFMaps.TaskLord)
	if err != nil {
		return fmt.Errorf("open etr_task_lord: %w", err)
	}
	if err := taskMap.Put(taskPID, lordID); err != nil {
		return fmt.Errorf("put etr_task_lord: %w", err)
	}
	return nil
}

// UpdateLordCPUMask — Phase 3.5 live migration: change lord's CPU mask
// in BPF map. BPF select_cpu re-evaluates first_cpu_in_mask on next wakeup
// → tasks migrate to new CPU without explicit lock.
//
// Returns the new mask for caller logging.
func UpdateLordCPUMask(ctx context.Context, hostname string, newShares uint32, logger *slog.Logger) (uint64, error) {
	lordID := LordIDFromHostname(hostname)
	newMask := LordCPUMaskFromShares(newShares)

	cpusMap, err := openPin(BPFMaps.LordCpus)
	if err != nil {
		return 0, fmt.Errorf("open etr_lord_cpus: %w", err)
	}
	if err := cpusMap.Put(lordID, newMask); err != nil {
		return 0, fmt.Errorf("put etr_lord_cpus: %w", err)
	}
	logger.Info("lord CPU mask updated (live migration trigger)",
		"hostname", hostname, "lord_id", lordID,
		"new_cpu_mask", fmt.Sprintf("0x%x", newMask), "new_shares", newShares)
	return newMask, nil
}

// StatsMap — pinned stats map for live BPF counters.
var StatsMap = "/sys/fs/bpf/etronium/maps/etr_lord_stats"
