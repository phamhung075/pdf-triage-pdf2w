package triagescan

// This file ports the TriageProgressEvent union from src/application/triage-scan.ts:35-50.
//
// The TypeScript interface declares only optional fields besides `type`:
//
//	export interface TriageProgressEvent {
//	  type: 'SCAN_STARTED' | 'FILE_PROGRESS' | 'FILE_COMPLETED' | 'FILE_FAILED' | 'SCAN_COMPLETED' | 'OLLAMA_DOWN';
//	  totalFiles?: number;
//	  files?: string[];
//	  filename?: string;
//	  stage?: 'EXTRACTING_TEXT' | 'AI_CLASSIFYING' | 'RELOCALIZING' | 'COMPLETED' | 'SKIPPED_DUPLICATE' | 'FAILED';
//	  message?: string;
//	  docId?: number;
//	  title?: string;
//	  category?: string;
//	  subcategory?: string;
//	  newPath?: string;
//	  scannedCount?: number;
//	  processedCount?: number;
//	  skippedCount?: number;
//	}
//
// Event keeps the exact JSON field names (camelCase, not Go names) and the "absent when the
// TypeScript call site passed `undefined`" behavior through `omitempty` plus pointer types for
// the numeric fields. Pointers matter: TS distinguishes `scannedCount: 0` (present, OLLAMA_DOWN
// and SCAN_COMPLETED emit it) from an absent key, and a plain `int` with `omitempty` would drop
// every zero. The declaration order below is the interface's order, which is the closest a single
// Go struct can get to JSON.stringify's key order (which in TS follows each call site's object
// literal, not the interface).
//
// The payload the HTTP/SSE layer marshals is therefore byte-identical for the events whose
// call-site literal matches the interface order (SCAN_STARTED, the plain FILE_PROGRESS and
// FILE_FAILED forms, SCAN_COMPLETED); for the events built with a different literal order the
// keys, values and omissions are identical and only the key order differs, which no JSON consumer
// observes.

// EventType is the TriageProgressEvent `type` discriminant.
type EventType string

// The six event types triage-scan.ts emits.
const (
	EventScanStarted   EventType = "SCAN_STARTED"
	EventFileProgress  EventType = "FILE_PROGRESS"
	EventFileCompleted EventType = "FILE_COMPLETED"
	EventFileFailed    EventType = "FILE_FAILED"
	EventScanCompleted EventType = "SCAN_COMPLETED"
	EventOllamaDown    EventType = "OLLAMA_DOWN"
)

// Stage is the TriageProgressEvent `stage` discriminant. It is a fixed union: the WHY comment at
// triage-scan.ts:112-113 is preserved verbatim below.
//
//	// No `stage` — the union is a fixed set the UI switches on, and inventing a value here
//	// would fall through every branch of the consumer.
type Stage string

// The six stage values the UI switches on.
const (
	StageExtractingText   Stage = "EXTRACTING_TEXT"
	StageAIClassifying    Stage = "AI_CLASSIFYING"
	StageRelocalizing     Stage = "RELOCALIZING"
	StageCompleted        Stage = "COMPLETED"
	StageSkippedDuplicate Stage = "SKIPPED_DUPLICATE"
	StageFailed           Stage = "FAILED"
)

// Event is the Go form of the TS `TriageProgressEvent`. See the file comment for the exact
// json-tag/omitempty contract. `Files` is a pointer for the same reason the counts are: SCAN_STARTED
// always emits `files` (an empty array on an empty scan), while the other events omit the key.
type Event struct {
	Type           EventType `json:"type"`
	TotalFiles     *int      `json:"totalFiles,omitempty"`
	Files          *[]string `json:"files,omitempty"`
	Filename       string    `json:"filename,omitempty"`
	Stage          Stage     `json:"stage,omitempty"`
	Message        string    `json:"message,omitempty"`
	DocID          *int64    `json:"docId,omitempty"`
	Title          string    `json:"title,omitempty"`
	Category       string    `json:"category,omitempty"`
	Subcategory    string    `json:"subcategory,omitempty"`
	NewPath        string    `json:"newPath,omitempty"`
	ScannedCount   *int      `json:"scannedCount,omitempty"`
	ProcessedCount *int      `json:"processedCount,omitempty"`
	SkippedCount   *int      `json:"skippedCount,omitempty"`
}

// ResultItem mirrors the TS `TriageResultItem` (triage-scan.ts:25-33). The field names match the
// TS property names; the json tags make the eventual HTTP shape explicit.
type ResultItem struct {
	Filename    string `json:"filename"`
	DocID       int64  `json:"docId"`
	Title       string `json:"title"`
	Category    string `json:"category"`
	Subcategory string `json:"subcategory"`
	NewPath     string `json:"newPath"`
	Status      string `json:"status"`
}

// Result is the runTriageScan return object. The TS optional fields are the zero value when
// absent; `Items` is always a non-nil slice because the TS initializes `const items = []`.
type Result struct {
	ScannedCount   int
	ProcessedCount int
	SkippedCount   int
	Items          []ResultItem
	OllamaDown     bool
	Message        string
}

// intPtr / int64Ptr make the "explicitly present, possibly zero" event fields expressible.
func intPtr(v int) *int       { return &v }
func int64Ptr(v int64) *int64 { return &v }

// strPtr is the address-of helper for the optional string arguments of the relocalize and update
// seams.
func strPtr(v string) *string { return &v }
