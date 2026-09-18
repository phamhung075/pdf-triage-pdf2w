package taskstate

import (
	"sync"
	"testing"
)

// These cases are ported from pdf-triage's src/infrastructure/http/task-state.test.ts (3 cases). The
// upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/http/task-state.test.ts` -> 3 passed), so no upstream case is
// pinned red. Added cases cover the reset/monotonic/cap behaviours the TS suite left unasserted.

func resetState(t *testing.T) {
	t.Helper()
	FinishTask(nil, "Reset")
	// The TS suite leaves the broadcaster from a previous case installed (harmless single-threaded);
	// clearing it keeps the added concurrency case race-free.
	SetTaskBroadcaster(nil)
}

func TestTaskState(t *testing.T) {
	t.Run("starts with an idle state", func(t *testing.T) {
		resetState(t)
		state := GetTaskState()
		if state.IsRunning {
			t.Fatal("isRunning = true, want false")
		}
		if state.Type != TaskIdle {
			t.Fatalf("type = %q, want IDLE", state.Type)
		}
	})

	t.Run("updates state when startTask, updateTaskProgress, and finishTask are called", func(t *testing.T) {
		resetState(t)
		var events []Event
		SetTaskBroadcaster(func(evt Event) { events = append(events, evt) })

		StartTask(TaskRepair, 50, "Starting repair...")
		state := GetTaskState()
		if !state.IsRunning || state.Type != TaskRepair || state.TotalFiles != 50 || state.Percent != 0 {
			t.Fatalf("after startTask: %+v", state)
		}

		currentFile := "invoice_123.pdf"
		stage := "REPAIRING"
		message := "Repairing file 25/50"
		UpdateTaskProgress(ProgressUpdate{
			ProcessedFiles: 25,
			CurrentFile:    &currentFile,
			Stage:          &stage,
			Message:        &message,
		})
		state = GetTaskState()
		if state.ProcessedFiles != 25 || state.Percent != 50 || state.CurrentFile != "invoice_123.pdf" {
			t.Fatalf("after updateTaskProgress: %+v", state)
		}

		FinishTask(map[string]any{"repairedCount": 25}, "Repair finished")
		state = GetTaskState()
		if state.IsRunning || state.Percent != 100 || state.Stage != "COMPLETED" {
			t.Fatalf("after finishTask: %+v", state)
		}
		if len(events) < 3 {
			t.Fatalf("events = %d, want >= 3", len(events))
		}
	})

	t.Run("handles failTask properly", func(t *testing.T) {
		resetState(t)
		StartTask(TaskScan, 10, "Scanning...")
		FailTask("Disk error")
		state := GetTaskState()
		if state.IsRunning {
			t.Fatal("isRunning = true, want false")
		}
		if state.Stage != "FAILED" {
			t.Fatalf("stage = %q, want FAILED", state.Stage)
		}
		if state.Error == nil || *state.Error != "Disk error" {
			t.Fatalf("error = %v, want Disk error", state.Error)
		}
	})
}

func TestTaskStateExtras(t *testing.T) {
	t.Run("emits the expected event types", func(t *testing.T) {
		resetState(t)
		var events []Event
		SetTaskBroadcaster(func(evt Event) { events = append(events, evt) })

		StartTask(TaskClear, 0, "Clearing...")
		FailTask("boom")
		ResetTaskState()

		want := []string{"TASK_STARTED", "TASK_FAILED", "TASK_FINISHED"}
		if len(events) != len(want) {
			t.Fatalf("events = %d, want %d", len(events), len(want))
		}
		for i, typ := range want {
			if events[i].Type != typ {
				t.Fatalf("event[%d].Type = %q, want %q", i, events[i].Type, typ)
			}
		}
	})

	t.Run("clamps negative totals and keeps processedFiles monotonic", func(t *testing.T) {
		resetState(t)
		StartTask(TaskScan, -5, "Scanning...")
		if got := GetTaskState().TotalFiles; got != 0 {
			t.Fatalf("totalFiles = %d, want 0", got)
		}

		UpdateTaskProgress(ProgressUpdate{ProcessedFiles: 30})
		UpdateTaskProgress(ProgressUpdate{ProcessedFiles: 10}) // must not regress
		state := GetTaskState()
		if state.ProcessedFiles != 30 {
			t.Fatalf("processedFiles = %d, want 30", state.ProcessedFiles)
		}
		if state.Percent != 0 { // totalFiles is 0 -> percent stays 0
			t.Fatalf("percent = %d, want 0", state.Percent)
		}
	})

	t.Run("caps percent at 100 and honours an updated total", func(t *testing.T) {
		resetState(t)
		StartTask(TaskScan, 10, "Scanning...")
		total := 200
		UpdateTaskProgress(ProgressUpdate{ProcessedFiles: 250, TotalFiles: &total})
		state := GetTaskState()
		if state.TotalFiles != 200 || state.ProcessedFiles != 250 || state.Percent != 100 {
			t.Fatalf("state = %+v, want total 200, processed 250, percent 100", state)
		}
	})

	t.Run("state copies are safe for concurrent readers and writers", func(t *testing.T) {
		resetState(t)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(2)
			go func(n int) {
				defer wg.Done()
				StartTask(TaskScan, 100, "Scanning...")
				UpdateTaskProgress(ProgressUpdate{ProcessedFiles: n})
			}(i)
			go func() {
				defer wg.Done()
				_ = GetTaskState()
			}()
		}
		wg.Wait()
	})
}
