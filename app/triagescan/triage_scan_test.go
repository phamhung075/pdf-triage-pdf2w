package triagescan

// Ported from pdf-triage's src/application/triage-scan.test.ts (3 cases) and extended with the
// moveBlockedFileToBlockedFolder `_blockedN` collision case the objective names.
//
// The TS suite pointed CONFIG.INPUT_DIR at the operator's real __raws and cleaned up after itself.
// The Go port fits each case inside a t.TempDir(), so no real project directory is ever touched;
// the filesystem behavior asserted is the same.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMoveDuplicateFileToDuplicatesFolder(t *testing.T) {
	t.Run("moves duplicate file into input/.duplicates_files directory", func(t *testing.T) {
		inputDir := t.TempDir()
		testInputDir := filepath.Join(inputDir, "test_tmp_scan")
		if err := os.MkdirAll(testInputDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		testFile := filepath.Join(testInputDir, "sample_duplicate.pdf")
		if err := os.WriteFile(testFile, []byte("dummy pdf content"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		resultPath, err := MoveDuplicateFileToDuplicatesFolder(inputDir, testFile)
		if err != nil {
			t.Fatalf("MoveDuplicateFileToDuplicatesFolder: %v", err)
		}

		if fileExists(testFile) {
			t.Errorf("source file still exists at %s", testFile)
		}
		if !fileExists(resultPath) {
			t.Errorf("moved file does not exist at %s", resultPath)
		}
		if !strings.Contains(resultPath, ".duplicates_files") {
			t.Errorf("result path %q does not contain .duplicates_files", resultPath)
		}
		if got := filepath.Base(resultPath); got != "sample_duplicate.pdf" {
			t.Errorf("basename = %q, want sample_duplicate.pdf", got)
		}
	})

	t.Run("handles filename collision by appending _dup counter", func(t *testing.T) {
		inputDir := t.TempDir()
		testInputDir := filepath.Join(inputDir, "test_tmp_scan")
		dupDir := filepath.Join(inputDir, ".duplicates_files")
		if err := os.MkdirAll(testInputDir, 0o755); err != nil {
			t.Fatalf("mkdir input: %v", err)
		}
		if err := os.MkdirAll(dupDir, 0o755); err != nil {
			t.Fatalf("mkdir dup: %v", err)
		}

		testFile := filepath.Join(testInputDir, "collision.pdf")
		if err := os.WriteFile(testFile, []byte("dummy pdf content"), 0o644); err != nil {
			t.Fatalf("write source: %v", err)
		}
		existingInDup := filepath.Join(dupDir, "collision.pdf")
		if err := os.WriteFile(existingInDup, []byte("already existing duplicate"), 0o644); err != nil {
			t.Fatalf("write existing: %v", err)
		}

		resultPath, err := MoveDuplicateFileToDuplicatesFolder(inputDir, testFile)
		if err != nil {
			t.Fatalf("MoveDuplicateFileToDuplicatesFolder: %v", err)
		}

		if fileExists(testFile) {
			t.Errorf("source file still exists at %s", testFile)
		}
		if !fileExists(resultPath) {
			t.Errorf("moved file does not exist at %s", resultPath)
		}
		if got := filepath.Base(resultPath); got != "collision_dup1.pdf" {
			t.Errorf("basename = %q, want collision_dup1.pdf", got)
		}
	})
}

func TestMoveBlockedFileToBlockedFolder(t *testing.T) {
	t.Run("moves blocked file into input/.blocked_files directory", func(t *testing.T) {
		inputDir := t.TempDir()
		testInputDir := filepath.Join(inputDir, "test_tmp_block")
		if err := os.MkdirAll(testInputDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		testFile := filepath.Join(testInputDir, "blocked.pdf")
		if err := os.WriteFile(testFile, []byte("dummy pdf content"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		resultPath, err := MoveBlockedFileToBlockedFolder(inputDir, testFile)
		if err != nil {
			t.Fatalf("MoveBlockedFileToBlockedFolder: %v", err)
		}

		if fileExists(testFile) {
			t.Errorf("source file still exists at %s", testFile)
		}
		if !strings.Contains(resultPath, ".blocked_files") {
			t.Errorf("result path %q does not contain .blocked_files", resultPath)
		}
		if got := filepath.Base(resultPath); got != "blocked.pdf" {
			t.Errorf("basename = %q, want blocked.pdf", got)
		}
	})

	t.Run("handles filename collision by appending _blocked counter", func(t *testing.T) {
		inputDir := t.TempDir()
		testInputDir := filepath.Join(inputDir, "test_tmp_block")
		blockDir := filepath.Join(inputDir, ".blocked_files")
		if err := os.MkdirAll(testInputDir, 0o755); err != nil {
			t.Fatalf("mkdir input: %v", err)
		}
		if err := os.MkdirAll(blockDir, 0o755); err != nil {
			t.Fatalf("mkdir blocked: %v", err)
		}

		testFile := filepath.Join(testInputDir, "collision.pdf")
		if err := os.WriteFile(testFile, []byte("dummy pdf content"), 0o644); err != nil {
			t.Fatalf("write source: %v", err)
		}
		if err := os.WriteFile(filepath.Join(blockDir, "collision.pdf"), []byte("already blocked"), 0o644); err != nil {
			t.Fatalf("write existing: %v", err)
		}

		resultPath, err := MoveBlockedFileToBlockedFolder(inputDir, testFile)
		if err != nil {
			t.Fatalf("MoveBlockedFileToBlockedFolder: %v", err)
		}

		if got := filepath.Base(resultPath); got != "collision_blocked1.pdf" {
			t.Errorf("basename = %q, want collision_blocked1.pdf", got)
		}
	})
}

func TestCleanEmptyDirectories(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "test_clean_dir")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}

	emptySub1 := filepath.Join(baseDir, "sub1", "nested_empty")
	if err := os.MkdirAll(emptySub1, 0o755); err != nil {
		t.Fatalf("mkdir empty sub: %v", err)
	}

	nonEmptySub := filepath.Join(baseDir, "sub2")
	if err := os.MkdirAll(nonEmptySub, 0o755); err != nil {
		t.Fatalf("mkdir non-empty sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nonEmptySub, "keep.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write keep: %v", err)
	}

	dupSub := filepath.Join(baseDir, ".duplicates_files")
	if err := os.MkdirAll(dupSub, 0o755); err != nil {
		t.Fatalf("mkdir dup: %v", err)
	}

	CleanEmptyDirectories(baseDir, baseDir, nil)

	if fileExists(emptySub1) {
		t.Errorf("nested empty directory %s still exists", emptySub1)
	}
	if fileExists(filepath.Join(baseDir, "sub1")) {
		t.Errorf("empty parent directory %s still exists", filepath.Join(baseDir, "sub1"))
	}
	if !fileExists(nonEmptySub) {
		t.Errorf("non-empty directory %s was removed", nonEmptySub)
	}
	if !fileExists(dupSub) {
		t.Errorf(".duplicates_files %s was removed", dupSub)
	}
}
