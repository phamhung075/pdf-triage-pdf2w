package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/app/scanlock"
)

// TestScanLockHelperProcess is the genuinely separate process the cross-process scan-lock case
// needs. It does nothing unless the parent sets PDF_TRIAGE_SCANLOCK_HELPER=1, in which case it
// acquires DATA_DIR/.scan.lock and holds it until killed.
func TestScanLockHelperProcess(t *testing.T) {
	if os.Getenv("PDF_TRIAGE_SCANLOCK_HELPER") != "1" {
		return
	}
	dataDir := os.Getenv("PDF_TRIAGE_SCANLOCK_HELPER_DIR")
	guard := scanlock.New(dataDir)
	release, err := guard.Acquire()
	if err != nil {
		os.Exit(3)
	}
	defer release()
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

// TestScanCommandRefusesWhileAnotherProcessHoldsLock runs the scan subcommand's own code path while
// a second OS process holds app/scanlock, and asserts the command refuses non-zero without scanning.
func TestScanCommandRefusesWhileAnotherProcessHoldsLock(t *testing.T) {
	baseDir := t.TempDir()
	dataDir := t.TempDir()
	inputDir := filepath.Join(dataDir, "input")
	outputDir := filepath.Join(dataDir, "archive")
	if err := os.MkdirAll(inputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PDF_TRIAGE_BASE_DIR", "")
	t.Setenv("PDF_TRIAGE_DATA_DIR", "")
	t.Setenv("PDF_INPUT_DIR", inputDir)
	t.Setenv("PDF_OUTPUT_DIR", outputDir)
	t.Setenv("PDF2W_SERVICE_URL", "http://127.0.0.1:1")
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")

	helper := exec.Command(os.Args[0], "-test.run=TestScanLockHelperProcess")
	helper.Env = append(os.Environ(),
		"PDF_TRIAGE_SCANLOCK_HELPER=1",
		"PDF_TRIAGE_SCANLOCK_HELPER_DIR="+dataDir,
	)
	if err := helper.Start(); err != nil {
		t.Fatalf("start lock helper: %v", err)
	}
	t.Cleanup(func() {
		_ = helper.Process.Kill()
		_ = helper.Wait()
	})

	lockPath := scanlock.LockFilePath(dataDir)
	waitForLockHolder(t, lockPath, helper.Process.Pid)

	app := newTestApplication(t, baseDir, dataDir)

	var stdout, stderr bytes.Buffer
	code := runScanWith(app, context.Background(), &stdout, &stderr)

	if code == 0 {
		t.Fatalf("scan exited 0 while another process held the scan lock; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "already in progress") {
		t.Fatalf("stderr = %q, want a scan-in-progress refusal", stderr.String())
	}

	docs, err := app.db.GetAllDocuments()
	if err != nil {
		t.Fatalf("GetAllDocuments: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("documents after a refused scan = %d, want 0", len(docs))
	}
}

// waitForLockHolder polls until the lock file exists and records wantPID.
func waitForLockHolder(t *testing.T, lockPath string, wantPID int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(lockPath); err == nil {
			if strings.TrimSpace(string(raw)) == strconv.Itoa(wantPID) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("helper process %d did not acquire %s", wantPID, lockPath)
}
