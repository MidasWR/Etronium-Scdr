// Package lord — criu.go
//
// CRIU CLI wrapper. ADR 024: Phase 3.0 использует CLI mode (`criu dump` / `criu restore`)
// вместо daemon. Простой os/exec, читаем exit code + stderr для ошибок.
//
// Ограничения нашей интеграции:
//   - vDSO compat требует CRIU 4.x на kernel 6.x
//   - cgroup handling перед dump: вывести pid из slice в /, иначе CRIU не понимает
//     (см. ADR 024 и criu_docs/CGROUP)
//   - После restore процесс получит новый PID; lord должен вернуть его в cgroup
//
// Использование:
//   ops := lord.NewCriuOps(logger)
//   if !ops.Available() { /* fall back */ }
//   pid := ops.Checkpoint(ctx, localPID, "/tmp/img-xyz")
//   newPID := ops.Restore(ctx, "/tmp/img-xyz", &lord.ExecSpec{...})
package lord

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CriuOps — обёртка над criu CLI.
type CriuOps struct {
	criuPath string // путь к бинарю, "" = "criu"
	logger   Logger
}

// NewCriuOps — конструктор. Проверяет что criu в PATH.
func NewCriuOps(logger Logger) *CriuOps {
	path, _ := exec.LookPath("criu")
	if path == "" {
		logger.Warn("criu not found in PATH", "hint", "apt install criu or see test/Dockerfile.phase3")
	}
	return &CriuOps{criuPath: path, logger: logger}
}

// Available — true если criu доступен.
func (c *CriuOps) Available() bool {
	return c.criuPath != ""
}

// Version — возвращает версию CRIU (для Register.criu_available=true).
func (c *CriuOps) Version() string {
	if !c.Available() {
		return ""
	}
	out, err := exec.Command(c.criuPath, "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	// Вывод: "Version: 4.2\n"
	parts := strings.SplitN(string(out), ":", 2)
	if len(parts) == 2 {
		return strings.TrimSpace(parts[1])
	}
	return strings.TrimSpace(string(out))
}

// CheckpointOpts — параметры для dump.
type CheckpointOpts struct {
	ImagesDir    string // куда положить images
	LeaveRunning bool   // freeze и продолжить, или kill процесс
	Verbose      bool   // увеличить verbosity для debug
}

// Checkpoint — делает CRIU dump процесса pid в imagesDir.
// Возвращает error, nil если успешно.
func (c *CriuOps) Checkpoint(ctx context.Context, pid int, opts CheckpointOpts) error {
	if !c.Available() {
		return fmt.Errorf("criu not available")
	}
	if err := os.MkdirAll(opts.ImagesDir, 0o755); err != nil {
		return fmt.Errorf("mkdir images dir: %w", err)
	}

	args := []string{
		"dump",
		"-t", fmt.Sprintf("%d", pid),
		"-D", opts.ImagesDir,
		"-o", filepath.Join(opts.ImagesDir, "criu-dump.log"),
		"--shell-job", // процессы не входят в session leader (criu dump требование)
	}
	if opts.LeaveRunning {
		args = append(args, "--leave-running")
	}
	if opts.Verbose {
		args = append(args, "-v", "4")
	}

	cmd := exec.CommandContext(ctx, c.criuPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr

	c.logger.Info("criu dump starting",
		"pid", pid, "images_dir", opts.ImagesDir, "leave_running", opts.LeaveRunning)

	if err := cmd.Run(); err != nil {
		// Tail stderr для диагностики
		s := stderr.String()
		if len(s) > 1024 {
			s = s[len(s)-1024:]
		}
		c.logger.Error("criu dump failed",
			"pid", pid, "err", err, "stderr_tail", s)
		return fmt.Errorf("criu dump: %w (stderr: %s)", err, s)
	}
	// CRIU пишет log файлы как root 0600 — делаем читаемыми.
	_ = os.Chmod(filepath.Join(opts.ImagesDir, "criu-dump.log"), 0o644)

	// Verify images dir содержит core-<pid>.img
	if _, err := os.Stat(filepath.Join(opts.ImagesDir, fmt.Sprintf("core-%d.img", pid))); err != nil {
		// Fallback: любой файл *.img кроме criu-dump.log
		entries, _ := os.ReadDir(opts.ImagesDir)
		hasImg := false
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".img") {
				hasImg = true
				break
			}
		}
		if !hasImg {
			return fmt.Errorf("criu dump: no image files in %s", opts.ImagesDir)
		}
	}

	c.logger.Info("criu dump complete", "pid", pid, "images_dir", opts.ImagesDir)
	return nil
}

// ExecSpec — что нужно для restore (параметры exec'а).
type ExecSpec struct {
	ExecPath    string
	Argv        []string
	Env         []string
	WorkingDir  string
	CgroupSlice string // cgroup path для нового процесса после restore
}

// RestoreOpts — параметры restore.
type RestoreOpts struct {
	ImagesDir string
	Detached  bool // detach от parent (для systemd-стиля)
	Verbose   bool
}

// Restore — делает CRIU restore. Возвращает новый PID.
func (c *CriuOps) Restore(ctx context.Context, opts RestoreOpts, execSpec ExecSpec) (int, error) {
	if !c.Available() {
		return 0, fmt.Errorf("criu not available")
	}

	// Создаём pidfile чтобы узнать новый PID после restore.
	pidfile := filepath.Join(opts.ImagesDir, "criu-restored.pid")
	_ = os.Remove(pidfile)

	// CRIU в root пишет log файлы с 0600 — сделаем 0644 чтобы lord мог прочитать.
	// Делаем до запуска, чтобы даже при failed restore прочитать.

	args := []string{
		"restore",
		"-D", opts.ImagesDir,
		"-o", filepath.Join(opts.ImagesDir, "criu-restore.log"),
		"--shell-job", // non-detached: target PPID=criu, but setsid+session keeps target alive after criu exit
		"--pidfile", pidfile,
	}
	if opts.Verbose {
		args = append(args, "-v", "4")
	}

	// exec параметры из image (не из CLI: CRIU не принимает --argv0/--exec/--env
	// при restore — процесс восстанавливается с argv/env, которые были в checkpoint).
	// exec параметры сохранены в images, restore их использует автоматически.

	// НЕ используем unshare: процесс остаётся в lord namespace (PID global видим),
	// pre-fork dummy процессов в lord startup поднимает PID counter выше thread'ов.
	// В новом namespace criu = PID 1 и любой target PID из images свободен.
	// После restore процесс существует внутри namespace — мы получим его
	// namespace-local PID из pidfile.
	// NB: attach к cgroup делается через namespace — kernel cgroup понимает
	// namespace-local PID потому что lord работает в parent namespace.
	cmd := exec.CommandContext(ctx, c.criuPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr

	c.logger.Info("criu restore starting", "images_dir", opts.ImagesDir)

	if err := cmd.Run(); err != nil {
		s := stderr.String()
		if len(s) > 1024 {
			s = s[len(s)-1024:]
		}
		c.logger.Error("criu restore failed", "err", err, "stderr_tail", s)
		return 0, fmt.Errorf("criu restore: %w (stderr: %s)", err, s)
	}
	// CRIU пишет log файлы как root 0600 — делаем читаемыми.
	_ = os.Chmod(filepath.Join(opts.ImagesDir, "criu-restore.log"), 0o644)

	// Читаем PID из pidfile.
	data, err := os.ReadFile(pidfile)
	if err != nil {
		c.logger.Error("cannot read pidfile after restore", "err", err, "pidfile", pidfile)
		// Fallback: scan ps
		newPID, err := scanForRestoredPID(ctx, execSpec.ExecPath)
		if err != nil {
			return 0, fmt.Errorf("find restored pid: %w", err)
		}
		return newPID, nil
	}
	newPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || newPID <= 0 {
		c.logger.Error("invalid pidfile content", "content", string(data))
		return 0, fmt.Errorf("invalid pidfile content: %s", string(data))
	}

	c.logger.Info("criu restore complete", "new_pid", newPID)
	return newPID, nil
}

// readRestoredRootPID — парсит pstree.img чтобы найти root task PID.
// NB: CRIU pstree.img — protobuf binary, полный парс сложен; для MVP берём
// имя процесса из inventory и ищем ps.
func readRestoredRootPID(imagesDir string) (int, error) {
	// inventory.img содержит root task info. Без proto парсера это сложно.
	// Fallback: scan ps для execPath после restore (см. caller).
	return 0, fmt.Errorf("pstree.img parsing not implemented; use scanForRestoredPID")
}

// scanForRestoredPID — после restore делаем `ps -o pid,comm` и ищем наш exec.
// Простой подход для MVP. Если restore успел зарегистрировать несколько
// процессов (fork'нутых), берём root.
func scanForRestoredPID(ctx context.Context, execPath string) (int, error) {
	// basename execPath
	base := filepath.Base(execPath)

	// ps может занять время, добавим timeout
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(pctx, "ps", "-e", "-o", "pid,comm,args")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return 0, err
	}

	// Ищем строки с base в args
	var candidates []int
	for _, line := range strings.Split(out.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		var pid int
		fmt.Sscanf(fields[0], "%d", &pid)
		if pid == 0 {
			continue
		}
		// comm или args содержит base
		if strings.Contains(fields[1], base) || strings.Contains(strings.Join(fields[2:], " "), base) {
			candidates = append(candidates, pid)
		}
	}
	if len(candidates) == 0 {
		return 0, fmt.Errorf("no process found for %s", execPath)
	}
	// Берем минимальный PID (скорее всего root)
	min := candidates[0]
	for _, p := range candidates {
		if p < min {
			min = p
		}
	}
	return min, nil
}

// Logger — интерфейс, который criu_ops использует для structured logging.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}
