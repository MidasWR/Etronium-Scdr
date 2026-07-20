// Package lord — stats.go
//
// Снятие метрик lord'а для heartbeat.
//
// Phase 0: упрощение, возвращаем приблизительные значения.
// Phase 1: точные значения из cgroup агрегата + delta sampling для CPU.
package lord

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	etroniumv1 "github.com/midas/Etronium-Scdr/internal/gen/etronium/v1"
	"github.com/shirou/gopsutil/v3/mem"
)

// detectHardware — собирает данные о машине для Register.
func detectHardware(cfg *Config) (*etroniumv1.RegisterRequest, error) {
	cpuCores := int32(runtime.NumCPU())
	memBytes, err := totalMemory()
	if err != nil {
		return nil, err
	}
	return &etroniumv1.RegisterRequest{
		Hostname:              cfg.Hostname,
		Os:                    "linux",
		Arch:                  runtime.GOARCH,
		CpuCoresPhysical:      cpuCores,
		MemTotalBytesPhysical: memBytes,
		AdvertisedCpuShares:   cfg.AdvertisedCpuShares,
		AdvertisedMemBytes:    cfg.AdvertisedMemBytes,
		CriuAvailable:         cfg.CriuAvailable,
	}, nil
}

func totalMemory() (int64, error) {
	vm, err := mem.VirtualMemory()
	if err != nil {
		return 0, err
	}
	return int64(vm.Total), nil
}

// readCpuUsec читает cpu.usage.usec из cgroup.
func readCpuUsec(cgroupPath string) uint64 {
	data, err := os.ReadFile(filepath.Join(cgroupPath, "cpu.usage.usec"))
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	return v
}

// readMemTotal суммирует memory.current всех дочерних slices.
func readMemTotal(cgroupPath string) uint64 {
	var total uint64
	entries, err := os.ReadDir(cgroupPath)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// memory.current
		data, err := os.ReadFile(filepath.Join(cgroupPath, e.Name(), "memory.current"))
		if err == nil {
			v, _ := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
			total += v
		}
	}
	return total
}

// getCurrentUsage — Phase 1: читаем cgroup-агрегаты текущего процесса агента.
//
// Агент сам живёт в cgroup user.slice (мы не перемещаем агента).
// Но мы хотим знать сколько ресурсов жрут TENANT процессы, а не вся машина.
// Поэтому cpu/mem берём через cgroup-агрегатор по slice'у /etronium/<lord_id>/.
//
// Возвращает:
//   cpuUsedPct — 0-100 (от одного ядра), считается по delta cpu.usage
//   memUsedBytes — RSS текущих tenant-процессов
func (a *Agent) getCurrentUsage() (int32, int64) {
	if a.cg == nil {
		return 0, 0
	}
	// CPU delta
	var cpuUsec uint64
	a.cpuStatsMu.Lock()
	nowUsec := readCpuUsec(a.cg.basePath)
	if a.lastCpuUsec > 0 && nowUsec > a.lastCpuUsec && !a.lastSampleAt.IsZero() {
		elapsed := time.Since(a.lastSampleAt).Seconds()
		if elapsed > 0 {
			cores := float64(runtime.NumCPU())
			deltaUsec := nowUsec - a.lastCpuUsec
			// Использовано % от одного ядра * cores = % от всей машины
			cpuPct := float64(deltaUsec) / 1e6 / elapsed * 100.0 / cores
			if cpuPct < 0 {
				cpuPct = 0
			}
			if cpuPct > 100*float64(runtime.NumCPU()) {
				cpuPct = 100 * float64(runtime.NumCPU())
			}
			cpuUsec = uint64(cpuPct)
		}
	}
	a.lastCpuUsec = nowUsec
	a.lastSampleAt = time.Now()
	a.cpuStatsMu.Unlock()

	// Mem: суммируем memory.current всех slices
	memBytes := readMemTotal(a.cg.basePath)

	return int32(cpuUsec), int64(memBytes)
}
