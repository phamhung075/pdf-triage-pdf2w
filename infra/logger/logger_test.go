package logger

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These cases are ported from pdf-triage's src/infrastructure/logger.test.ts (5 cases: 1 for the log
// file location and 4 for rotation). The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/logger.test.ts` -> 5 passed), so no upstream case is pinned
// red.
//
// The TS file re-imports the module through vi.resetModules() for every case because logger.ts
// resolves its path and knobs at module load. The Go port has no module-level state: each case
// constructs its own Logger against t.TempDir(), which is the same isolation. Added cases cover the
// behaviours the TS suite never asserted directly (log-line formatting, the 1000-entry ring buffer,
// forDocument, grouped sessions, the subscribe hook and filename extraction).

func newTestLogger(t *testing.T, opts Options) *Logger {
	t.Helper()
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	return New(opts)
}

func TestLogFileLocation(t *testing.T) {
	t.Run("honours PDF_TRIAGE_LOG_DIR so a test run never touches the production log", func(t *testing.T) {
		tempDir := t.TempDir()
		opts := OptionsFromEnv("/base", func(key string) string {
			if key == "PDF_TRIAGE_LOG_DIR" {
				return tempDir
			}
			return ""
		})
		l := newTestLogger(t, opts)

		l.Info("TRIAGE", "hello from a test", nil)

		logFile := l.LogFilePath()
		if filepath.Dir(logFile) != filepath.Clean(tempDir) {
			t.Fatalf("log dir = %q, want %q", filepath.Dir(logFile), filepath.Clean(tempDir))
		}
		raw, err := os.ReadFile(logFile)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !strings.Contains(string(raw), "hello from a test") {
			t.Fatalf("log file = %q, want it to contain the message", raw)
		}
	})
}

func TestLogRotation(t *testing.T) {
	t.Run("rotates once the file would exceed the size cap, preserving the old content as .1", func(t *testing.T) {
		tempDir := t.TempDir()
		l := newTestLogger(t, Options{LogDir: tempDir, MaxBytes: 400})
		logFile := l.LogFilePath()

		l.Info("TRIAGE", "FIRST_GENERATION_MARKER "+strings.Repeat("x", 300), nil)
		if _, err := os.Stat(logFile + ".1"); !os.IsNotExist(err) { // still under the cap
			t.Fatalf(".1 exists prematurely, stat err = %v", err)
		}

		l.Info("TRIAGE", "SECOND_GENERATION_MARKER "+strings.Repeat("y", 300), nil)

		rotated, err := os.ReadFile(logFile + ".1")
		if err != nil {
			t.Fatalf("ReadFile(.1): %v", err)
		}
		if !strings.Contains(string(rotated), "FIRST_GENERATION_MARKER") {
			t.Fatalf(".1 = %q, want FIRST_GENERATION_MARKER", rotated)
		}
		live, err := os.ReadFile(logFile)
		if err != nil {
			t.Fatalf("ReadFile(live): %v", err)
		}
		if !strings.Contains(string(live), "SECOND_GENERATION_MARKER") {
			t.Fatalf("live = %q, want SECOND_GENERATION_MARKER", live)
		}
		// The live file restarted, so it holds only the new line.
		if strings.Contains(string(live), "FIRST_GENERATION_MARKER") {
			t.Fatalf("live = %q, want it not to contain FIRST_GENERATION_MARKER", live)
		}
	})

	t.Run("keeps at most MaxRetain generations and discards the oldest", func(t *testing.T) {
		tempDir := t.TempDir()
		l := newTestLogger(t, Options{LogDir: tempDir, MaxBytes: 300, Retain: 2})
		logFile := l.LogFilePath()

		for i := 1; i <= 5; i++ {
			l.Info("TRIAGE", "GEN_"+string(rune('0'+i))+" "+strings.Repeat("z", 250), nil)
		}

		if _, err := os.Stat(logFile + ".1"); err != nil {
			t.Fatalf(".1 missing: %v", err)
		}
		if _, err := os.Stat(logFile + ".2"); err != nil {
			t.Fatalf(".2 missing: %v", err)
		}
		if _, err := os.Stat(logFile + ".3"); !os.IsNotExist(err) { // capped at 2 generations
			t.Fatalf(".3 exists but retention is 2, stat err = %v", err)
		}
	})

	t.Run("never rotates when the cap is disabled with 0", func(t *testing.T) {
		tempDir := t.TempDir()
		l := newTestLogger(t, Options{LogDir: tempDir, MaxBytes: 0})
		logFile := l.LogFilePath()

		for i := 0; i < 20; i++ {
			l.Info("TRIAGE", "append forever "+strings.Repeat("q", 200), nil)
		}

		if _, err := os.Stat(logFile + ".1"); !os.IsNotExist(err) {
			t.Fatalf(".1 exists with rotation disabled, stat err = %v", err)
		}
		raw, err := os.ReadFile(logFile)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		lines := 0
		for _, line := range strings.Split(string(raw), "\n") {
			if line != "" {
				lines++
			}
		}
		if lines != 20 {
			t.Fatalf("line count = %d, want 20", lines)
		}
	})

	t.Run("keeps writing the line even if rotation itself fails", func(t *testing.T) {
		tempDir := t.TempDir()
		stderr := &bytes.Buffer{}
		l := newTestLogger(t, Options{LogDir: tempDir, MaxBytes: 200, Stderr: stderr})
		logFile := l.LogFilePath()

		l.Info("TRIAGE", "first "+strings.Repeat("a", 200), nil)
		l.renameFile = func(oldpath, newpath string) error { return errors.New("EBUSY") }

		l.Info("TRIAGE", "MUST_STILL_BE_WRITTEN", nil)

		if !strings.Contains(stderr.String(), "Failed to rotate log file:") {
			t.Fatalf("stderr = %q, want the rotation failure notice", stderr.String())
		}
		raw, err := os.ReadFile(logFile)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !strings.Contains(string(raw), "MUST_STILL_BE_WRITTEN") {
			t.Fatalf("live = %q, want MUST_STILL_BE_WRITTEN", raw)
		}
	})
}

func TestLogLineFormatting(t *testing.T) {
	tempDir := t.TempDir()
	l := newTestLogger(t, Options{LogDir: tempDir})

	l.Info("TRIAGE", "a message", map[string]any{"filename": "/tmp/facture.pdf"})

	entries := l.RecentLogs(1)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Level != LevelInfo || e.ModuleName != "TRIAGE" || e.Message != "a message" {
		t.Fatalf("entry = %+v", e)
	}
	if e.Filename != "facture.pdf" {
		t.Fatalf("filename = %q, want facture.pdf (extracted from meta and based)", e.Filename)
	}
	if !strings.HasPrefix(e.Line, "[") || !strings.Contains(e.Line, "[INFO] [TRIAGE] [facture.pdf] a message | Meta: ") || !strings.HasSuffix(e.Line, "\n") {
		t.Fatalf("line = %q", e.Line)
	}
	if !strings.Contains(e.Line, `"filename":"/tmp/facture.pdf"`) {
		t.Fatalf("line = %q, want the JSON meta", e.Line)
	}
}

func TestRingBuffer(t *testing.T) {
	tempDir := t.TempDir()
	l := newTestLogger(t, Options{LogDir: tempDir})

	for i := 1; i <= 1005; i++ {
		l.Info("TRIAGE", "line", nil)
	}

	entries := l.RecentLogs(1000)
	if len(entries) != 1000 {
		t.Fatalf("buffer size = %d, want 1000", len(entries))
	}
	if entries[0].ID != 6 || entries[999].ID != 1005 {
		t.Fatalf("ids = [%d..%d], want [6..1005]", entries[0].ID, entries[999].ID)
	}
	if got := len(l.RecentLogs(300)); got != 300 {
		t.Fatalf("RecentLogs(300) = %d, want 300", got)
	}
}

func TestForDocument(t *testing.T) {
	tempDir := t.TempDir()
	l := newTestLogger(t, Options{LogDir: tempDir})

	doc := l.ForDocument("/archive/invoices/Avis de Taxes.pdf")
	doc.Info("TRIAGE", "child logger", nil)

	entries := l.RecentLogs(1)
	if len(entries) != 1 || entries[0].Filename != "Avis de Taxes.pdf" {
		t.Fatalf("entry = %+v, want filename Avis de Taxes.pdf", entries)
	}
}

func TestGroupedSessionLogs(t *testing.T) {
	tempDir := t.TempDir()
	now := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	l := newTestLogger(t, Options{
		LogDir: tempDir,
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			now = now.Add(time.Second)
			return now
		},
	})

	first := l.ForDocument("/a/first.pdf")
	first.Info("TRIAGE", "started", nil)
	first.Info("TRIAGE", "MOVED to archive", map[string]any{"category": "invoices", "subcategory": "sfr", "reason": "match"})

	second := l.ForDocument("/a/second.pdf")
	second.Error("TRIAGE", "FAILED hard", nil)

	sessions := l.GroupedSessionLogs()
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(sessions))
	}
	// Sorted by updatedAt descending: second.pdf logged last.
	if sessions[0].Filename != "second.pdf" {
		t.Fatalf("first session = %q, want second.pdf", sessions[0].Filename)
	}
	if sessions[0].Status != "FAILED" || sessions[0].LogsCount != 1 {
		t.Fatalf("second session = %+v", sessions[0])
	}
	if sessions[1].Filename != "first.pdf" || sessions[1].Status != "COMPLETED" {
		t.Fatalf("first session = %+v", sessions[1])
	}
	if sessions[1].Category != "invoices" || sessions[1].Subcategory != "sfr" || sessions[1].DecisionReason != "match" {
		t.Fatalf("first session meta = %+v", sessions[1])
	}
}

func TestSubscribe(t *testing.T) {
	tempDir := t.TempDir()
	l := newTestLogger(t, Options{LogDir: tempDir})

	var mu sync.Mutex
	var got []LogEntry
	unsubscribe := l.Subscribe(func(entry LogEntry) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, entry)
	})
	l.Info("TRIAGE", "first", nil)
	unsubscribe()
	l.Info("TRIAGE", "second", nil)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Message != "first" {
		t.Fatalf("subscriber saw %+v, want only the first entry", got)
	}
}

func TestExtractFilenameFromLog(t *testing.T) {
	cases := []struct {
		name string
		in   LogEntry
		want string
	}{
		{"explicit filename wins", LogEntry{Filename: "/x/y/doc.pdf", Message: "ignored"}, "doc.pdf"},
		{"meta file path", LogEntry{Message: "m", Meta: map[string]any{"original_path": "/r/Avis.pdf"}}, "Avis.pdf"},
		{"meta key with non-pdf value is skipped", LogEntry{Message: "m", Meta: map[string]any{"filename": "/r/notes.txt"}}, ""},
		{"message with posix path", LogEntry{Message: "Moved file /mnt/c/raws/facture.pdf done"}, "facture.pdf"},
		{"no pdf", LogEntry{Message: "nothing here"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractFilenameFromLog(tc.in); got != tc.want {
				t.Fatalf("ExtractFilenameFromLog = %q, want %q", got, tc.want)
			}
		})
	}
}
