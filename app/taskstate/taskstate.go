// Package taskstate is a Go port of pdf-triage's src/infrastructure/http/task-state.ts (109 lines):
// the ActiveTaskState shape and the start/update/finish/fail/reset state machine that broadcasts
// TASK_STARTED / TASK_PROGRESS / TASK_FINISHED / TASK_FAILED.
//
// The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/http/task-state.test.ts` -> 3 passed), so no upstream case is
// pinned red. All three cases are ported, plus cases for the event types, the percent cap, monotonic
// progress and concurrent access.
//
// The broadcaster is an injected hook, exactly as in TS, but Go handlers are concurrent, so every
// manager operation takes a mutex and the broadcaster is invoked after releasing it (a callback that
// re-enters the manager cannot deadlock). A package-level default manager plus free functions keep
// the TS module-singleton call sites working; NewManager gives tests and future wiring an isolated
// instance.
//
// Deviations, all resolved in favor of matching the TS acceptance bar:
//
//  1. Optional arguments. TS `updateTaskProgress(processed, currentFile?, stage?, message?,
//     totalFiles?)` becomes a ProgressUpdate struct whose pointer fields are the "argument was
//     passed" test; TS `finishTask(result?, message = '...')` becomes a variadic message.
//  2. null. TS `startedAt: string | null` and `error: string | null` are *string, so the JSON/SSE
//     form can still emit null; `result?: any` is a plain any.
//  3. Math.round is JavaScript's half-toward-+Infinity; Go's math.Round is half-away-from-zero, so
//     the percentage uses floor(x+0.5) as the extractionqualitygate port does. The result is then
//     clamped to 100, matching `Math.min(100, ...)`.
//  4. `new Date().toISOString()` always renders three fractional digits, so the timestamp uses the
//     explicit "2006-01-02T15:04:05.000Z" UTC layout.
//  5. getTaskState()'s `{...activeState}` is a shallow struct copy.
package taskstate

import (
	"math"
	"sync"
	"time"
)

// TaskType mirrors the TS task-type union.
type TaskType string

const (
	TaskIdle   TaskType = "IDLE"
	TaskScan   TaskType = "SCAN"
	TaskRepair TaskType = "REPAIR"
	TaskClear  TaskType = "CLEAR"
)

const jsISOLayout = "2006-01-02T15:04:05.000Z"

// ActiveTaskState mirrors the TS `ActiveTaskState` interface.
type ActiveTaskState struct {
	IsRunning      bool
	Type           TaskType
	TotalFiles     int
	ProcessedFiles int
	Percent        int
	CurrentFile    string
	Stage          string
	Message        string
	StartedAt      *string
	Result         any
	Error          *string
}

// Event mirrors the TS `{ type, taskState }` broadcast payload.
type Event struct {
	Type      string
	TaskState ActiveTaskState
}

// Broadcaster is the injected event hook.
type Broadcaster func(Event)

// ProgressUpdate is the Go mapping of the TS optional-argument list; a nil pointer means the
// argument was not passed.
type ProgressUpdate struct {
	ProcessedFiles int
	TotalFiles     *int
	CurrentFile    *string
	Stage          *string
	Message        *string
}

// Manager owns one task state machine. It is safe for concurrent use.
type Manager struct {
	mu          sync.Mutex
	state       ActiveTaskState
	broadcaster Broadcaster
}

// NewManager returns a manager in the TS module's initial idle state.
func NewManager() *Manager {
	return &Manager{state: ActiveTaskState{
		IsRunning: false,
		Type:      TaskIdle,
		Message:   "System idle",
	}}
}

// SetBroadcaster installs the event hook (setTaskBroadcaster).
func (m *Manager) SetBroadcaster(broadcaster Broadcaster) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.broadcaster = broadcaster
}

// State returns a shallow copy of the current state (getTaskState).
func (m *Manager) State() ActiveTaskState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// StartTask resets the machine for a new run and emits TASK_STARTED.
func (m *Manager) StartTask(taskType TaskType, totalFiles int, message string) {
	if totalFiles < 0 {
		totalFiles = 0
	}
	startedAt := time.Now().UTC().Format(jsISOLayout)

	m.mu.Lock()
	m.state = ActiveTaskState{
		IsRunning:  true,
		Type:       taskType,
		TotalFiles: totalFiles,
		Stage:      "STARTING",
		Message:    message,
		StartedAt:  &startedAt,
	}
	state, broadcaster := m.state, m.broadcaster
	m.mu.Unlock()

	emit(broadcaster, Event{Type: "TASK_STARTED", TaskState: state})
}

// UpdateTaskProgress advances the run and emits TASK_PROGRESS.
func (m *Manager) UpdateTaskProgress(update ProgressUpdate) {
	m.mu.Lock()
	if update.TotalFiles != nil && *update.TotalFiles > 0 {
		m.state.TotalFiles = *update.TotalFiles
	}
	if update.ProcessedFiles > m.state.ProcessedFiles {
		m.state.ProcessedFiles = update.ProcessedFiles
	}
	if m.state.TotalFiles > 0 {
		percent := int(math.Floor(float64(m.state.ProcessedFiles)/float64(m.state.TotalFiles)*100 + 0.5))
		if percent > 100 {
			percent = 100
		}
		m.state.Percent = percent
	} else {
		m.state.Percent = 0
	}
	if update.CurrentFile != nil {
		m.state.CurrentFile = *update.CurrentFile
	}
	if update.Stage != nil {
		m.state.Stage = *update.Stage
	}
	if update.Message != nil {
		m.state.Message = *update.Message
	}
	state, broadcaster := m.state, m.broadcaster
	m.mu.Unlock()

	emit(broadcaster, Event{Type: "TASK_PROGRESS", TaskState: state})
}

// FinishTask marks the run completed and emits TASK_FINISHED. With no message it uses the TS
// default.
func (m *Manager) FinishTask(result any, message ...string) {
	finalMessage := "Operation completed successfully"
	if len(message) > 0 {
		finalMessage = message[0]
	}

	m.mu.Lock()
	m.state.IsRunning = false
	m.state.Percent = 100
	m.state.Stage = "COMPLETED"
	m.state.Message = finalMessage
	m.state.Result = result
	state, broadcaster := m.state, m.broadcaster
	m.mu.Unlock()

	emit(broadcaster, Event{Type: "TASK_FINISHED", TaskState: state})
}

// FailTask marks the run failed and emits TASK_FAILED.
func (m *Manager) FailTask(errorMessage string) {
	m.mu.Lock()
	m.state.IsRunning = false
	m.state.Stage = "FAILED"
	m.state.Error = &errorMessage
	m.state.Message = "Operation failed: " + errorMessage
	state, broadcaster := m.state, m.broadcaster
	m.mu.Unlock()

	emit(broadcaster, Event{Type: "TASK_FAILED", TaskState: state})
}

// ResetTaskState returns the machine to idle and emits TASK_FINISHED.
func (m *Manager) ResetTaskState() {
	m.mu.Lock()
	m.state = ActiveTaskState{
		IsRunning: false,
		Type:      TaskIdle,
		Stage:     "IDLE",
		Message:   "System unlocked",
	}
	state, broadcaster := m.state, m.broadcaster
	m.mu.Unlock()

	emit(broadcaster, Event{Type: "TASK_FINISHED", TaskState: state})
}

func emit(broadcaster Broadcaster, event Event) {
	if broadcaster != nil {
		broadcaster(event)
	}
}

// defaultManager preserves the TS module-singleton used by the web routes.
var defaultManager = NewManager()

// SetTaskBroadcaster is the package-level `setTaskBroadcaster`.
func SetTaskBroadcaster(broadcaster Broadcaster) { defaultManager.SetBroadcaster(broadcaster) }

// GetTaskState is the package-level `getTaskState`.
func GetTaskState() ActiveTaskState { return defaultManager.State() }

// StartTask is the package-level `startTask`.
func StartTask(taskType TaskType, totalFiles int, message string) {
	defaultManager.StartTask(taskType, totalFiles, message)
}

// UpdateTaskProgress is the package-level `updateTaskProgress`.
func UpdateTaskProgress(update ProgressUpdate) { defaultManager.UpdateTaskProgress(update) }

// FinishTask is the package-level `finishTask`.
func FinishTask(result any, message ...string) { defaultManager.FinishTask(result, message...) }

// FailTask is the package-level `failTask`.
func FailTask(errorMessage string) { defaultManager.FailTask(errorMessage) }

// ResetTaskState is the package-level `resetTaskState`.
func ResetTaskState() { defaultManager.ResetTaskState() }
