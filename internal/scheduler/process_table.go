// Package scheduler — process_table.go
//
// Глобальная таблица процессов scheduler'а. Thread-safe in-memory.
//
// Это аналог NUMA scheduler'а который видит все нити на всех ядрах.
// В Phase 0 — RWMutex + map. В Phase 5 добавим WAL для crash recovery.
package scheduler

import (
	"sync"

	etroniumv1 "github.com/midas/Etronium-Scdr/internal/gen/etronium/v1"

	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ProcessEntry — внутреннее представление процесса в scheduler'е.
//
// Содержит runtime-поля которых нет в proto (каналы, callbacks), плюс
// последний известный ProcessInfo для GetProcess/ListProcesses.
type ProcessEntry struct {
	Info *etroniumv1.ProcessInfo

	// Мутекс для атомарного обновления Info из разных горутин.
	mu sync.Mutex

	// Канал для Wait: закрывается когда state in {EXITED, STOPPED}.
	exited chan struct{}

	// Ring buffer последних IOChunk (для StreamProcessIO с follow=false).
	// Phase 0: не используется, упрощение.
	ioBuf *ringBuffer

	// Подписчики WatchProcess.
	watchersMu sync.Mutex
	watchers   []watcher
}

type watcher struct {
	events chan *etroniumv1.ProcessEvent
	cancel chan struct{}
}

// ProcessTable — глобальная таблица.
type ProcessTable struct {
	mu       sync.RWMutex
	byID     map[string]*ProcessEntry // process_id → entry
	byTenant map[string]map[string]*ProcessEntry // tenant_id → process_id → entry
}

// NewProcessTable — конструктор.
func NewProcessTable() *ProcessTable {
	return &ProcessTable{
		byID:     make(map[string]*ProcessEntry),
		byTenant: make(map[string]map[string]*ProcessEntry),
	}
}

// NewID — ULID для process_id.
func NewID() string {
	return ulid.Make().String()
}

// Create — создаёт новую запись в state NEW.
func (t *ProcessTable) Create(info *etroniumv1.ProcessInfo) *ProcessEntry {
	t.mu.Lock()
	defer t.mu.Unlock()

	entry := &ProcessEntry{
		Info:   info,
		exited: make(chan struct{}),
		ioBuf:  newRingBuffer(64 * 1024), // 64KB ring buffer
	}
	t.byID[info.Ref.ProcessId] = entry
	if _, ok := t.byTenant[info.TenantId]; !ok {
		t.byTenant[info.TenantId] = make(map[string]*ProcessEntry)
	}
	t.byTenant[info.TenantId][info.Ref.ProcessId] = entry
	return entry
}

// Get — по process_id.
func (t *ProcessTable) Get(processID string) (*ProcessEntry, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.byID[processID]
	return e, ok
}

// ListByTenant — список процессов тенанта.
func (t *ProcessTable) ListByTenant(tenantID string, onlyRunning bool) []*ProcessEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	tenantMap, ok := t.byTenant[tenantID]
	if !ok {
		return nil
	}
	out := make([]*ProcessEntry, 0, len(tenantMap))
	for _, e := range tenantMap {
		if onlyRunning {
			state := e.Info.GetState()
			if state != etroniumv1.ProcessState_PROCESS_STATE_RUNNING &&
				state != etroniumv1.ProcessState_PROCESS_STATE_PAUSED {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

// ListByLord — все процессы, у которых lord_id == lordID. filter — optional
// predicate; nil = no filter. Used by recovery.go to find candidates
// for respawn when a lord disconnects.
func (t *ProcessTable) ListByLord(lordID string, filter func(*ProcessEntry) bool) []*ProcessEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]*ProcessEntry, 0)
	for _, e := range t.byID {
		if e.Info.GetRef().GetLordId() != lordID {
			continue
		}
		if filter != nil && !filter(e) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// UpdateState — атомарно меняет state и будит Wait'еров.
func (e *ProcessEntry) UpdateState(newState etroniumv1.ProcessState, lordID string, localPID int32) {
	e.mu.Lock()
	oldState := e.Info.State
	e.Info.State = newState
	e.Info.StateChangedAt = timestampNow()
	if lordID != "" {
		e.Info.Ref.LordId = lordID
	}
	if localPID != 0 {
		e.Info.Ref.LocalPid = localPID
	}
	e.mu.Unlock()

	// Если финальное состояние — закрываем exited channel.
	if newState == etroniumv1.ProcessState_PROCESS_STATE_EXITED ||
		newState == etroniumv1.ProcessState_PROCESS_STATE_STOPPED {
		select {
		case <-e.exited: // already closed
		default:
			close(e.exited)
		}
	}

	// Уведомляем подписчиков WatchProcess.
	e.watchersMu.Lock()
	ws := append([]watcher(nil), e.watchers...)
	e.watchersMu.Unlock()
	ev := &etroniumv1.ProcessEvent{
		Ref:      e.Info.Ref,
		Type:     etroniumv1.ProcessEvent_EVENT_TYPE_STATE_CHANGED,
		At:       timestampNow(),
		Payload:  &etroniumv1.ProcessEvent_NewState{NewState: newState},
	}
	for _, w := range ws {
		select {
		case w.events <- ev:
		case <-w.cancel:
		default: // не блокируем если подписчик не успевает
		}
	}

	_ = oldState
}

// UpdateResult — записывает exit info (exit_code, exit_signal, exited_at).
func (e *ProcessEntry) UpdateResult(exitCode, exitSignal int32) {
	e.mu.Lock()
	e.Info.ExitCode = exitCode
	e.Info.ExitSignal = exitSignal
	e.Info.ExitedAt = timestampNow()
	e.mu.Unlock()
}

// StateString — для логов.
func (e *ProcessEntry) StateString() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Info == nil {
		return "?"
	}
	return e.Info.GetState().String()
}

// Snapshot — возвращает копию Info для внешних читателей.
func (e *ProcessEntry) Snapshot() *etroniumv1.ProcessInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	// proto.Clone чтобы не отдавать указатель на внутреннюю структуру.
	return cloneProcessInfo(e.Info)
}

// ExitedChan — канал который закрывается при exit/stop.
func (e *ProcessEntry) ExitedChan() <-chan struct{} {
	return e.exited
}

// SubscribeWatch — добавляет подписчика WatchProcess.
func (e *ProcessEntry) SubscribeWatch() (events <-chan *etroniumv1.ProcessEvent, cancel func()) {
	ch := make(chan *etroniumv1.ProcessEvent, 16)
	cancelCh := make(chan struct{})
	w := watcher{events: ch, cancel: cancelCh}
	e.watchersMu.Lock()
	e.watchers = append(e.watchers, w)
	e.watchersMu.Unlock()
	cancel = func() {
		close(cancelCh)
		e.watchersMu.Lock()
		defer e.watchersMu.Unlock()
		for i, ww := range e.watchers {
			if ww.cancel == cancelCh {
				e.watchers = append(e.watchers[:i], e.watchers[i+1:]...)
				break
			}
		}
	}
	return ch, cancel
}

// --- helpers ---

func timestampNow() *timestamppb.Timestamp {
	return nowTimestamp()
}

// --- ring buffer для I/O (Phase 0: упрощение, не используется) ---

type ringBuffer struct {
	mu   sync.Mutex
	data []byte
	off  int
	full bool
	cap  int
}

func newRingBuffer(capBytes int) *ringBuffer {
	return &ringBuffer{data: make([]byte, capBytes), cap: capBytes}
}

func (r *ringBuffer) Write(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(p) >= r.cap {
		// последний cap байт
		copy(r.data, p[len(p)-r.cap:])
		r.full = true
		r.off = 0
		return
	}
	n := copy(r.data[r.off:], p)
	if r.off+n >= r.cap {
		r.full = true
		r.off = (r.off + n) % r.cap
	} else {
		r.off += n
	}
}

func (r *ringBuffer) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return append([]byte(nil), r.data[:r.off]...)
	}
	out := make([]byte, r.cap)
	copy(out, r.data[r.off:])
	copy(out[r.cap-r.off:], r.data[:r.off])
	return out
}
