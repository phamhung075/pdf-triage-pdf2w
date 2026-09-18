package httpapi

import (
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/app/repair"
	"github.com/phamhung075/pdf-triage-pdf2w/app/taskstate"
)

// progressRecordingTasks records every UpdateTaskProgress call so a test can assert how many times
// (and with what) the repair progress bridge updated the task state.
type progressRecordingTasks struct {
	updates []taskstate.ProgressUpdate
}

func (r *progressRecordingTasks) StartTask(taskstate.TaskType, int, string) {}
func (r *progressRecordingTasks) UpdateTaskProgress(update taskstate.ProgressUpdate) {
	r.updates = append(r.updates, update)
}
func (r *progressRecordingTasks) FinishTask(any, ...string)            {}
func (r *progressRecordingTasks) FailTask(string)                      {}
func (r *progressRecordingTasks) ResetTaskState()                      {}
func (r *progressRecordingTasks) SetBroadcaster(taskstate.Broadcaster) {}

// TestApplyRepairProgressIgnoresFileFailed pins audit G7: the TS repair progress block
// (web-server.ts:263-270) handles only REPAIR_STARTED and FILE_PROGRESS/FILE_COMPLETED, so a
// FILE_FAILED event must be broadcast without calling UpdateTaskProgress — leaving Stage and
// CurrentFile at their previous values rather than overwriting them with empty pointers.
func TestApplyRepairProgressIgnoresFileFailed(t *testing.T) {
	recorder := &progressRecordingTasks{}
	applyRepairProgress(recorder, repair.RepairStartedEvent{
		Type: repair.EventRepairStarted, TotalFiles: 3, Message: "repairing",
	})
	applyRepairProgress(recorder, repair.FileProgressEvent{
		Type: repair.EventFileProgress, Filename: "a.pdf", ScannedCount: 1, ProcessedCount: 1, Stage: "REPAIRING", Message: "working",
	})
	before := len(recorder.updates)
	if before != 2 {
		t.Fatalf("setup updates = %d, want 2", before)
	}

	applyRepairProgress(recorder, repair.FileFailedEvent{
		Type: repair.EventFileFailed, Filename: "b.pdf", Stage: "REPAIRING", Message: "ollama down",
	})
	if got := len(recorder.updates); got != before {
		t.Fatalf("FILE_FAILED produced %d extra UpdateTaskProgress call(s), want 0", got-before)
	}

	last := recorder.updates[before-1]
	if last.CurrentFile == nil || *last.CurrentFile != "a.pdf" {
		t.Fatalf("CurrentFile = %v, want it untouched at a.pdf", last.CurrentFile)
	}
	if last.Stage == nil || *last.Stage != "REPAIRING" {
		t.Fatalf("Stage = %v, want it untouched at REPAIRING", last.Stage)
	}
}
