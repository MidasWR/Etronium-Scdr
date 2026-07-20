// Package lord — helpers.go
//
// Внутренние утилиты, общие для всех handler'ов.
package lord

import (
	"os"
	"path/filepath"
	"time"

	etroniumv1 "github.com/midas/Etronium-Scdr/internal/gen/etronium/v1"
)

// nowFunc — перехватываемый для тестов таймстемп.
var nowFunc = time.Now

// detachPidFromCgroup — выводит pid из нашего cgroup slice в /.
// Используется перед CRIU dump, чтобы dump видел процесс без slice-dependency.
func (a *Agent) detachPidFromCgroup(pid int) error {
	a.cgMu.Lock()
	cg := a.cg
	a.cgMu.Unlock()
	if cg == nil {
		return nil
	}
	return cg.MovePidToRoot(pid)
}

// dirSize — суммарный размер всех файлов в директории (для метрик).
func dirSize(dir string) (int64, error) {
	var size int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info == nil || info.IsDir() {
			return nil
		}
		size += info.Size()
		return nil
	})
	return size, err
}

// protoToResources — конвертирует etroniumv1.ResourceSpec в локальный Resources.
func protoToResources(spec *etroniumv1.ResourceSpec) *Resources {
	if spec == nil {
		return &Resources{}
	}
	return &Resources{
		CPUShares:     uint32(spec.GetCpuShares()),
		MemLimitBytes: spec.GetMemLimitBytes(),
		IOWeight:      uint32(spec.GetIoWeight()),
		PidsLimit:     uint32(spec.GetPidsLimit()),
	}
}

// localCmdEnvToProto — обратное преобразование не нужно для Phase 3.
// Оставлено для будущей миграции env через stream.

// fmtLordID — helper для логов (lordID с защитой от race).
func (a *Agent) fmtLordID() string {
	a.lordIDMu.RLock()
	defer a.lordIDMu.RUnlock()
	if a.lordID == "" {
		return "?"
	}
	return a.lordID[:8]
}
