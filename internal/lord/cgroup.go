// Package lord — cgroup.go
//
// Cgroup v2 slice management для tenant-процессов.
//
// Иерархия:
//   /sys/fs/cgroup/etronium/<lord_id>/<process_id>/
//
// Контроллеры: cpuset, cpu, io, memory, pids.
// Каждый tenant-процесс пишет свой PID в cgroup.procs.
//
// Phase 1: базовые лимиты (cpu.weight, memory.max, io.weight, pids.max).
// Phase 4: PSI stats, memory.high, memory.events.*
package lord

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// cgroupRoot — путь к v2 cgroup root.
const cgroupRoot = "/sys/fs/cgroup"

// etroniumSlice — общая slice для всех лордов (для systemd-style интеграции).
const etroniumSlice = "etronium"

// CgroupManager управляет slice'ами в cgroup v2 для tenant-процессов.
type CgroupManager struct {
	lordID   string
	basePath string // /sys/fs/cgroup/etronium/<lord_id>/
	logger   *slog.Logger
}

// NewCgroupManager создаёт slice для lord'а (один раз на старте).
func NewCgroupManager(lordID string, logger *slog.Logger) (*CgroupManager, error) {
	if err := checkCgroupV2(); err != nil {
		return nil, fmt.Errorf("cgroup v2 not available: %w", err)
	}

	writableRoot, err := findWritableRoot()
	if err != nil {
		return nil, err
	}

	// Шаг 1: создать /etronium-slice и включить в нём нужные контроллеры.
	// Без этого child slice не сможет создать cpu.* / io.* / memory.* файлы.
	etroniumRoot := filepath.Join(writableRoot, etroniumSlice)
	if err := os.MkdirAll(etroniumRoot, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", etroniumRoot, err)
	}
	if err := enableControllers(etroniumRoot); err != nil {
		return nil, fmt.Errorf("enable controllers in %s: %w", etroniumRoot, err)
	}

	// Шаг 2: создать /etronium/<lord_id> под ним.
	base := filepath.Join(etroniumRoot, lordID)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", base, err)
	}

	logger.Info("cgroup manager ready", "base_path", base)
	return &CgroupManager{
		lordID:   lordID,
		basePath: base,
		logger:   logger,
	}, nil
}

// CreateProcessSlice создаёт slice для процесса и применяет ResourceSpec.
// Возвращает путь к slice (для последующего Attach и Cleanup).
func (m *CgroupManager) CreateProcessSlice(processID string, resources *Resources) (string, error) {
	slice := filepath.Join(m.basePath, processID)
	if err := os.MkdirAll(slice, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", slice, err)
	}

	// Включаем контроллеры в дочернем slice (cpu/io/memory/pids).
	if err := enableControllers(slice); err != nil {
		os.Remove(slice)
		return "", fmt.Errorf("enable controllers in %s: %w", slice, err)
	}

	if resources == nil {
		resources = &Resources{}
	}
	if err := m.applyResources(slice, resources); err != nil {
		os.Remove(slice)
		return "", fmt.Errorf("apply resources to %s: %w", slice, err)
	}

	m.logger.Info("cgroup slice created",
		"process_id", processID,
		"slice", slice,
		"cpu_weight", resources.CPUShares,
		"mem_max_bytes", resources.MemLimitBytes,
		"pids_max", resources.PidsLimit,
	)
	return slice, nil
}

// Attach перемещает процесс (по PID) в slice.
func (m *CgroupManager) Attach(processID string, pid int) error {
	slice := filepath.Join(m.basePath, processID)
	procsFile := filepath.Join(slice, "cgroup.procs")

	// cgroup.procs требует PID в текстовом виде.
	pidStr := strconv.Itoa(pid)
	if err := os.WriteFile(procsFile, []byte(pidStr+"\n"), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", procsFile, err)
	}
	return nil
}

// Destroy удаляет slice процесса. Сначала убивает процессы внутри (если есть).
func (m *CgroupManager) Destroy(processID string) error {
	slice := filepath.Join(m.basePath, processID)

	// Если есть процессы внутри, cgroup не даст удалить директорию.
	// Стратегия: записываем "1" в cgroup.kill (cgroup v2 поддерживает начиная с ядра 5.14)
	killFile := filepath.Join(slice, "cgroup.kill")
	if data, err := os.ReadFile(killFile); err == nil && len(data) > 0 {
		// Контроллер доступен, убиваем
		_ = os.WriteFile(killFile, []byte("1"), 0o644)
	}

	// Попытка рекурсивного удаления. Если не вышло (процессы ещё внутри),
	// оставляем slice — следующий Destroy попробует ещё раз.
	if err := os.Remove(slice); err != nil {
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EBUSY) {
			m.logger.Debug("cgroup slice busy, will retry", "slice", slice)
			return nil
		}
		return fmt.Errorf("rmdir %s: %w", slice, err)
	}
	m.logger.Info("cgroup slice destroyed", "slice", slice)
	return nil
}

// ReadStats читает cpu.usage_usec и memory.current (для heartbeat).
type Stats struct {
	CPUUsageUSec  uint64
	MemoryCurrent uint64
	MemoryPeak    uint64
	ProcsCurrent  uint32
}

// ReadStats возвращает метрики slice'а.
func (m *CgroupManager) ReadStats(processID string) (Stats, error) {
	slice := filepath.Join(m.basePath, processID)
	var s Stats

	if v, err := readUint(filepath.Join(slice, "cpu.usage.usec")); err == nil {
		s.CPUUsageUSec = v
	}
	if v, err := readUint(filepath.Join(slice, "memory.current")); err == nil {
		s.MemoryCurrent = v
	}
	if v, err := readUint(filepath.Join(slice, "memory.peak")); err == nil {
		s.MemoryPeak = v
	}
	if v, err := readUint(filepath.Join(slice, "pids.current")); err == nil {
		s.ProcsCurrent = uint32(v)
	}
	return s, nil
}

// applyResources — пишет лимиты в cgroup файлы.
//
// Толерантно к отсутствию интерфейсных файлов: если parent не делегировал
// нужный контроллер, child slice просто не будет иметь нужного файла
// (например memory.max). В этом случае мы пропускаем лимит и логируем warning.
func (m *CgroupManager) applyResources(slice string, r *Resources) error {
	// cpu.weight: 1..10000, default 100. Ноль = "оставить default".
	if r.CPUShares > 0 {
		if r.CPUShares > 10000 {
			r.CPUShares = 10000
		}
		path := filepath.Join(slice, "cpu.weight")
		if _, err := os.Stat(path); err != nil {
			m.logger.Warn("cpu.weight not available (parent cgroup does not delegate cpu controller); skipping cpu limit",
				"slice", slice)
		} else if err := writeFile(path, strconv.Itoa(int(r.CPUShares))); err != nil {
			m.logger.Warn("cpu.weight write failed", "slice", slice, "err", err)
		}
	}

	// memory.max: bytes, "max" = unlimited. Ноль = no limit.
	if r.MemLimitBytes > 0 {
		path := filepath.Join(slice, "memory.max")
		if _, err := os.Stat(path); err != nil {
			m.logger.Warn("memory.max not available (parent cgroup does not delegate memory controller); skipping mem limit",
				"slice", slice)
		} else if err := writeFile(path, strconv.FormatInt(r.MemLimitBytes, 10)); err != nil {
			m.logger.Warn("memory.max write failed", "slice", slice, "err", err)
		}
	}

	// io.weight: 1..10000, default 100.
	if r.IOWeight > 0 {
		if r.IOWeight > 10000 {
			r.IOWeight = 10000
		}
		path := filepath.Join(slice, "io.weight")
		if _, err := os.Stat(path); err != nil {
			m.logger.Warn("io.weight not available (parent cgroup does not delegate io controller); skipping io limit",
				"slice", slice)
		} else if err := writeFile(path, strconv.Itoa(int(r.IOWeight))); err != nil {
			m.logger.Warn("io.weight write failed", "slice", slice, "err", err)
		}
	}

	// pids.max: число или "max".
	if r.PidsLimit > 0 {
		path := filepath.Join(slice, "pids.max")
		if _, err := os.Stat(path); err != nil {
			m.logger.Warn("pids.max not available (parent cgroup does not delegate pids controller); skipping pids limit",
				"slice", slice)
		} else if err := writeFile(path, strconv.FormatInt(int64(r.PidsLimit), 10)); err != nil {
			m.logger.Warn("pids.max write failed", "slice", slice, "err", err)
		}
	}
	return nil
}

// Resources — упрощённый view ResourceSpec proto, чтобы не тянуть protobuf в lord.
// Преобразуется из etroniumv1.ResourceSpec на стороне вызова.
type Resources struct {
	CPUShares     uint32
	MemLimitBytes int64
	IOWeight      uint32
	PidsLimit     uint32
}

// --- helpers ---

// checkCgroupV2 проверяет что cgroup v2 смонтирован и доступен.
func checkCgroupV2() error {
	// /sys/fs/cgroup/cgroup.controllers появляется только в v2.
	_, err := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		return err
	}
	return nil
}

// findWritableRoot возвращает путь к cgroup-директории, в которую мы можем писать.
//
// cgroup v2 запрещает создание произвольных файлов в cgroup-директориях
// (только интерфейсные файлы от ядра). Поэтому probe делается через попытку
// mkdir дочерней директории.
//
// Алгоритм:
//  1. Берём /proc/self/cgroup — находим нашу "текущую" slice (path от cgroup root).
//  2. Пробуем mkdir в этой slice → если OK, возвращаем этот путь.
//  3. Иначе пробуем /sys/fs/cgroup/ напрямую (нужен CAP_SYS_ADMIN, root или delegate).
//  4. Иначе ошибка.
func findWritableRoot() (string, error) {
	candidates := []string{}

	// Candidate 1: наша текущая slice (v2, 0::)
	data, err := os.ReadFile("/proc/self/cgroup")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			// v2: "0::/path"
			if strings.HasPrefix(line, "0::") {
				p := strings.TrimPrefix(line, "0::")
				if p != "" && p != "/" {
					candidates = append(candidates, filepath.Join(cgroupRoot, p))
				}
				break
			}
		}
	}
	// Candidate 2: cgroup root
	candidates = append(candidates, cgroupRoot)

	var lastErr error
	for _, c := range candidates {
		probe := filepath.Join(c, ".etronium-probe-dir")
		if err := os.Mkdir(probe, 0o755); err != nil {
			lastErr = err
			continue
		}
		_ = os.Remove(probe)
		return c, nil
	}
	return "", fmt.Errorf("no writable cgroup root (need CAP_SYS_ADMIN or proper slice delegation); last error: %w", lastErr)
}

// enableControllers включает все необходимые контроллеры в slice.
// В v2 нужно писать в cgroup.subtree_control формата "+cpu +memory +io +pids".
// Если родитель не делегировал нужные контроллеры — мы НЕ сможем их включить.
// Возвращаем список успешно включённых (для логов).
func enableControllers(slice string) error {
	subtreeCtrl := filepath.Join(slice, "cgroup.subtree_control")
	current, _ := os.ReadFile(subtreeCtrl)

	currentStr := string(current)
	want := []string{"cpu", "memory", "io", "pids"}
	// Пробуем включать каждый по одному (бывает что набор меняется в зависимости от ядра).
	for _, c := range want {
		if strings.Contains(currentStr, "+"+c) {
			continue
		}
		// Пытаемся включить; если родитель не делегировал — фейлимся на этом.
		if err := writeFile(subtreeCtrl, "+"+c); err != nil {
			// Не критично, продолжаем — следующий контроллер может сработать.
			continue
		}
		// Обновим currentStr для следующей итерации.
		newRead, _ := os.ReadFile(subtreeCtrl)
		currentStr = string(newRead)
	}
	return nil
}

// writeFile — обёртка для лучшего error'а.
func writeFile(path, value string) error {
	if err := os.WriteFile(path, []byte(value+"\n"), 0o644); err != nil {
		return err
	}
	return nil
}

// readUint читает uint64 из файла (cgroup value-файлы).
func readUint(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}
