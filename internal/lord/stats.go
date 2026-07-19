// Package lord — stats.go
//
// Снятие метрик lord'а для heartbeat.
//
// Phase 0: упрощение, возвращаем приблизительные значения.
// Phase 1: точные значения через gopsutil + cgroups.
package lord

import (
	"runtime"

	etroniumv1 "github.com/midas/Etronium-Scdr/internal/gen/etronium/v1"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
)

// detectHardware — собирает данные о машине для Register.
func detectHardware(hostname string) (*etroniumv1.RegisterRequest, error) {
	cpuCores := int32(runtime.NumCPU())
	memBytes, err := totalMemory()
	if err != nil {
		return nil, err
	}
	return &etroniumv1.RegisterRequest{
		Hostname:              hostname,
		Os:                    "linux",
		Arch:                  runtime.GOARCH,
		CpuCoresPhysical:      cpuCores,
		MemTotalBytesPhysical: memBytes,
		CriuAvailable:         false,
	}, nil
}

func totalMemory() (int64, error) {
	vm, err := mem.VirtualMemory()
	if err != nil {
		return 0, err
	}
	return int64(vm.Total), nil
}

// getCurrentUsage — Phase 0: грубая оценка через gopsutil.
//
// Возвращает cpu_used_pct (0-100) и mem_used_bytes.
func getCurrentUsage() (int32, int64) {
	// Берём мгновенный снимок, без интервала — это даст 0% в первый раз.
	// TODO Phase 1: использовать разностный сэмплинг с 1s интервалом.
	cpuPcts, _ := cpu.Percent(0, false)
	cpuPct := int32(0)
	if len(cpuPcts) > 0 {
		cpuPct = int32(cpuPcts[0])
	}
	vm, err := mem.VirtualMemory()
	memBytes := int64(0)
	if err == nil {
		memBytes = int64(vm.Used)
	}
	return cpuPct, memBytes
}
