// Package scheduler — wal.go
//
// Phase 5: Write-Ahead Log для process_table.
//
// Формат: одна строка JSON = одно событие.
//   {"type":"create","info":{...ProcessInfo...}}
//   {"type":"state","process_id":"01H...","state":3,"lord_id":"...","local_pid":123}
//   {"type":"result","process_id":"01H...","exit_code":0,"exit_signal":0}
//
// Replay: открыть файл, проиграть все события в порядке, in-memory state
// восстановлен. Это даёт cold-start resilience — после scheduler crash
// tenant'ы могут продолжать list/get/kill уже запущенные процессы.
//
// Trade-offs:
//   - Нет fsync между событиями (append-only, kernel сам flush'ит page cache).
//     Потеря последних 0-2 секунд при kernel panic допустима для MVP.
//   - Нет compaction. Файл растёт ~150 байт/событие. 1000 spawn'ов = 150 KB.
//     Через неделю можно rotate.
//   - Нет snapshot at start. Cold start = replay full log. Для демо 5 минут
//     это OK; для production — bind в periodic snapshot+tail.
package scheduler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	etroniumv1 "github.com/midas/Etronium-Scdr/internal/gen/etronium/v1"
)

const walHeader = "ETRONIUM_WAL_v1\n"

// WAL — append-only лог.
type WAL struct {
	mu   sync.Mutex
	file *os.File
	w    *bufio.Writer
}

// OpenWAL — открыть WAL на append. Если файл не существует — создать.
// Вызывается из main() scheduler'а.
func OpenWAL(path string) (*WAL, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir wal dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open wal: %w", err)
	}
	return &WAL{file: f, w: bufio.NewWriter(f)}, nil
}

// Close — flush и закрыть файл.
func (w *WAL) Close() error {
	if w == nil || w.file == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.w.Flush()
	return w.file.Close()
}

// AppendCreate — записать создание процесса.
func (w *WAL) AppendCreate(info *etroniumv1.ProcessInfo) error {
	return w.append(map[string]any{
		"type": "create",
		"at":   time.Now().Unix(),
		"info": info,
	})
}

// AppendState — записать смену state.
func (w *WAL) AppendState(processID string, state etroniumv1.ProcessState, lordID string, localPID int32) error {
	return w.append(map[string]any{
		"type":       "state",
		"at":         time.Now().Unix(),
		"process_id": processID,
		"state":      state.String(),
		"lord_id":    lordID,
		"local_pid":  localPID,
	})
}

// AppendResult — записать финальный exit.
func (w *WAL) AppendResult(processID string, exitCode, exitSignal int32) error {
	return w.append(map[string]any{
		"type":        "result",
		"at":          time.Now().Unix(),
		"process_id":  processID,
		"exit_code":   exitCode,
		"exit_signal": exitSignal,
	})
}

func (w *WAL) append(ev map[string]any) error {
	if w == nil {
		return nil // WAL disabled
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := w.w.Write(data); err != nil {
		return err
	}
	if _, err := w.w.WriteString("\n"); err != nil {
		return err
	}
	return w.w.Flush() // fsync isn't called but data is in page cache
}

// ReplayResult — итог replay'а.
type ReplayResult struct {
	Creates int
	States  int
	Results int
}

// ReplayWAL — прочитать файл, восстановить состояние в process_table.
// Вызывается при старте scheduler'а ДО приёма gRPC traffic.
func ReplayWAL(path string, pt *ProcessTable) (ReplayResult, error) {
	res := ReplayResult{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return res, nil
		}
		return res, err
	}
	defer f.Close()

	// Проверяем header.
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024) // 16 MB max line
	first := true
	for scanner.Scan() {
		line := scanner.Bytes()
		if first {
			first = false
			if string(line) == walHeader[:len(walHeader)-1] {
				continue
			}
		}

		var ev struct {
			Type      string                  `json:"type"`
			Info      *etroniumv1.ProcessInfo `json:"info,omitempty"`
			ProcessID string                  `json:"process_id,omitempty"`
			State     string                  `json:"state,omitempty"`
			LordID    string                  `json:"lord_id,omitempty"`
			LocalPID  int32                   `json:"local_pid,omitempty"`
			ExitCode  int32                   `json:"exit_code,omitempty"`
			ExitSig   int32                   `json:"exit_signal,omitempty"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // corrupt line, skip
		}

		switch ev.Type {
		case "create":
			if ev.Info != nil {
				pt.Create(ev.Info)
				res.Creates++
			}
		case "state":
			if entry, ok := pt.Get(ev.ProcessID); ok {
				// Map string back to enum.
				if st, ok := processStateByName(ev.State); ok {
					entry.UpdateState(st, ev.LordID, ev.LocalPID)
					res.States++
				}
			}
		case "result":
			if entry, ok := pt.Get(ev.ProcessID); ok {
				entry.UpdateResult(ev.ExitCode, ev.ExitSig)
				res.Results++
			}
		}
	}
	return res, scanner.Err()
}

// RotateWAL — закрыть и переименовать файл (call at midnight или при размере > N).
func RotateWAL(path string) error {
	if _, err := os.Stat(path); err != nil {
		return nil // nothing to rotate
	}
	rotated := path + "." + time.Now().UTC().Format("20060102-150405")
	if err := os.Rename(path, rotated); err != nil {
		return err
	}
	// Best-effort truncate to 0 the original; new file will be created on next open.
	return nil
}

// WriteHeader — записать header в начале нового файла.
func WriteHeader(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil // file exists, no need
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.WriteString(f, walHeader)
	return err
}

// processStateByName — обратная мапка state.String() → enum.
// State values: PROCESS_STATE_UNSPECIFIED/NEW/READY/RUNNING/PAUSED/MIGRATING/EXITED/STOPPED/RESTARTING.
func processStateByName(s string) (etroniumv1.ProcessState, bool) {
	switch s {
	case "PROCESS_STATE_UNSPECIFIED":
		return etroniumv1.ProcessState_PROCESS_STATE_UNSPECIFIED, true
	case "PROCESS_STATE_NEW":
		return etroniumv1.ProcessState_PROCESS_STATE_NEW, true
	case "PROCESS_STATE_READY":
		return etroniumv1.ProcessState_PROCESS_STATE_READY, true
	case "PROCESS_STATE_RUNNING":
		return etroniumv1.ProcessState_PROCESS_STATE_RUNNING, true
	case "PROCESS_STATE_PAUSED":
		return etroniumv1.ProcessState_PROCESS_STATE_PAUSED, true
	case "PROCESS_STATE_MIGRATING":
		return etroniumv1.ProcessState_PROCESS_STATE_MIGRATING, true
	case "PROCESS_STATE_EXITED":
		return etroniumv1.ProcessState_PROCESS_STATE_EXITED, true
	case "PROCESS_STATE_STOPPED":
		return etroniumv1.ProcessState_PROCESS_STATE_STOPPED, true
	case "PROCESS_STATE_RESTARTING":
		return etroniumv1.ProcessState_PROCESS_STATE_RESTARTING, true
	}
	return 0, false
}
