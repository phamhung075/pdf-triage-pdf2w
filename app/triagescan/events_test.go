package triagescan

// Pins the TriageProgressEvent JSON contract (triage-scan.ts:35-50): the exact camelCase keys, the
// `omitempty` omission of unset optional fields, and the pointer fields that keep an explicitly
// zero count present (OLLAMA_DOWN emits scannedCount/processedCount/skippedCount/totalFiles = 0).

import (
	"encoding/json"
	"testing"
)

func TestEventJSONShape(t *testing.T) {
	t.Run("SCAN_STARTED serializes byte-for-byte with the TS literal", func(t *testing.T) {
		got, err := json.Marshal(Event{
			Type:       EventScanStarted,
			TotalFiles: intPtr(2),
			Files:      &[]string{"a.pdf", "b.pdf"},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		want := `{"type":"SCAN_STARTED","totalFiles":2,"files":["a.pdf","b.pdf"]}`
		if string(got) != want {
			t.Fatalf("SCAN_STARTED = %s, want %s", got, want)
		}

		empty := []string{}
		gotEmpty, err := json.Marshal(Event{
			Type:       EventScanStarted,
			TotalFiles: intPtr(0),
			Files:      &empty,
		})
		if err != nil {
			t.Fatalf("marshal empty: %v", err)
		}
		wantEmpty := `{"type":"SCAN_STARTED","totalFiles":0,"files":[]}`
		if string(gotEmpty) != wantEmpty {
			t.Fatalf("empty SCAN_STARTED = %s, want %s", gotEmpty, wantEmpty)
		}
	})

	t.Run("plain FILE_FAILED omits every unset optional field", func(t *testing.T) {
		got, err := json.Marshal(Event{
			Type:     EventFileFailed,
			Filename: "blocked.pdf",
			Stage:    StageFailed,
			Message:  "❌ Blocked: No text extracted from PDF. Moved to __raws/blocked_files.",
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		want := `{"type":"FILE_FAILED","filename":"blocked.pdf","stage":"FAILED","message":"❌ Blocked: No text extracted from PDF. Moved to __raws/blocked_files."}`
		if string(got) != want {
			t.Fatalf("FILE_FAILED = %s, want %s", got, want)
		}
	})

	t.Run("OLLAMA_DOWN keeps explicit zero counts", func(t *testing.T) {
		got, err := json.Marshal(Event{
			Type:           EventOllamaDown,
			Message:        OllamaDownReminder("qwen3.5:9b"),
			ScannedCount:   intPtr(0),
			ProcessedCount: intPtr(0),
			SkippedCount:   intPtr(0),
			TotalFiles:     intPtr(0),
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(got, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, key := range []string{"type", "message", "scannedCount", "processedCount", "skippedCount", "totalFiles"} {
			if _, ok := decoded[key]; !ok {
				t.Errorf("OLLAMA_DOWN payload missing key %q: %s", key, got)
			}
		}
		if decoded["scannedCount"].(float64) != 0 || decoded["totalFiles"].(float64) != 0 {
			t.Errorf("OLLAMA_DOWN zero counts changed: %s", got)
		}
		if _, ok := decoded["docId"]; ok {
			t.Errorf("OLLAMA_DOWN unexpectedly carried docId: %s", got)
		}
	})

	t.Run("collision FILE_COMPLETED carries docId and newPath", func(t *testing.T) {
		got, err := json.Marshal(Event{
			Type:        EventFileCompleted,
			Filename:    "race_duplicate.pdf",
			Stage:       StageSkippedDuplicate,
			Message:     "Moved duplicate file to __raws/.duplicates_files (Checksum collided with an existing document)",
			DocID:       int64Ptr(42),
			NewPath:     "/tmp/__raws/.duplicates_files/race_duplicate.pdf",
			Subcategory: "sfr",
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(got, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if decoded["docId"].(float64) != 42 {
			t.Errorf("docId = %v, want 42: %s", decoded["docId"], got)
		}
		if decoded["newPath"] == "" {
			t.Errorf("newPath missing: %s", got)
		}
		if _, ok := decoded["scannedCount"]; ok {
			t.Errorf("counts not set on this event should be omitted: %s", got)
		}
	})
}
