package repair

// ProgressFunc is the Go form of the TypeScript `(event: any) => void` onProgress callback that
// repairRegistry(onProgress) accepted (repair-registry.ts:19, :53, :71, :245). The event values
// passed to it are the typed structs below, each of which marshals to the exact frozen SSE payload
// the dashboard consumes (inventory §3.1). The callback may be nil.
type ProgressFunc func(event any)

// Event type names. These are the exact TypeScript string literals; the dashboard switches on them.
const (
	EventRepairStarted   = "REPAIR_STARTED"
	EventFileProgress    = "FILE_PROGRESS"
	EventFileFailed      = "FILE_FAILED"
	EventRepairCompleted = "REPAIR_COMPLETED"
)

// Stage values used by the ported events.
const (
	stageRepairing = "REPAIRING"
	stageFailed    = "FAILED"
)

// RepairStartedEvent is the exact TS payload emitted at repair-registry.ts:53:
//
//	{ type: 'REPAIR_STARTED', totalFiles, message }
type RepairStartedEvent struct {
	Type       string `json:"type"`
	TotalFiles int    `json:"totalFiles"`
	Message    string `json:"message"`
}

// FileProgressEvent is the exact TS payload emitted at repair-registry.ts:71:
//
//	{ type: 'FILE_PROGRESS', filename, scannedCount, processedCount, stage: 'REPAIRING', message }
//
// Note the repair stream has no totalFiles field here, unlike CLEAR_STARTED/FILE_PROGRESS.
type FileProgressEvent struct {
	Type           string `json:"type"`
	Filename       string `json:"filename"`
	ScannedCount   int    `json:"scannedCount"`
	ProcessedCount int    `json:"processedCount"`
	Stage          string `json:"stage"`
	Message        string `json:"message"`
}

// FileFailedEvent is the exact TS payload emitted at repair-registry.ts:245 when Ollama is down:
//
//	{ type: 'FILE_FAILED', filename, stage: 'FAILED', message }
type FileFailedEvent struct {
	Type     string `json:"type"`
	Filename string `json:"filename"`
	Stage    string `json:"stage"`
	Message  string `json:"message"`
}

// RepairCompletedEvent is the exact TS payload the HTTP layer broadcast at web-server.ts:272:
//
//	broadcastTriageEvent({ type: 'REPAIR_COMPLETED', ...result })
//
// where result is the object repairRegistry returned, so the field order below matches the TS
// return literal at repair-registry.ts:259-265.
type RepairCompletedEvent struct {
	Type             string `json:"type"`
	ScannedCount     int    `json:"scannedCount"`
	RepairedCount    int    `json:"repairedCount"`
	UpdatedCount     int    `json:"updatedCount"`
	RelocalizedCount int    `json:"relocalizedCount"`
	MovedToRawsCount int    `json:"movedToRawsCount"`
}

// CompletedEvent builds the REPAIR_COMPLETED payload from a repair result. RepairRegistry does NOT
// emit it: TS broadcasts it from the HTTP layer (web-server.ts:272) after repairRegistry returns and
// finishTask ran, so the HTTP adapter calls this constructor at that same point to preserve the SSE
// ordering. It must be emitted exactly once per run. The result is the value RepairRegistry
// returned.
func CompletedEvent(result Result) RepairCompletedEvent {
	return RepairCompletedEvent{
		Type:             EventRepairCompleted,
		ScannedCount:     result.ScannedCount,
		RepairedCount:    result.RepairedCount,
		UpdatedCount:     result.UpdatedCount,
		RelocalizedCount: result.RelocalizedCount,
		MovedToRawsCount: result.MovedToRawsCount,
	}
}
