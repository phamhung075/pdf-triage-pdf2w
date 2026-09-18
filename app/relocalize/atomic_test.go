package relocalize

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// The atomic-move primitive is unexported in both implementations, so the TS suite could only
// observe it through relocalizeFileIfNeeded (the "never overwrites an existing file" case, ported
// in relocalize_test.go). These cases exercise the primitive directly: the collision-suffix
// behavior, and the concurrency the link+unlink design exists for — many movers racing one target
// path must every one land on a distinct path with no byte lost and the pre-existing file intact.

func TestRenameAtomicNoOverwrite_UniqueSuffixOnCollision(t *testing.T) {
	root := t.TempDir()
	targetDir := filepath.Join(root, "target")
	target := filepath.Join(targetDir, "facture.pdf")
	writeFile(t, target, "PRE-EXISTING CONTENT — must survive")

	sourceDir := filepath.Join(root, "source")
	source := filepath.Join(sourceDir, "facture.pdf")
	writeFile(t, source, "incoming content")

	got, err := renameAtomicNoOverwrite(source, target, maxRenameAttempts)
	if err != nil {
		t.Fatalf("renameAtomicNoOverwrite: %v", err)
	}
	if got == target {
		t.Fatalf("renameAtomicNoOverwrite returned the pre-existing target %q", target)
	}
	if readFile(t, target) != "PRE-EXISTING CONTENT — must survive" {
		t.Fatalf("pre-existing target was overwritten")
	}
	if readFile(t, got) != "incoming content" {
		t.Fatalf("moved content = %q", readFile(t, got))
	}
	mustNotExist(t, source)
}

func TestRenameAtomicNoOverwrite_ConcurrentMovesNeverLoseAByte(t *testing.T) {
	root := t.TempDir()
	targetDir := filepath.Join(root, "target")
	target := filepath.Join(targetDir, "facture.pdf")
	writeFile(t, target, "PRE-EXISTING")

	const movers = 12
	sources := make([]string, movers)
	for i := 0; i < movers; i++ {
		sources[i] = filepath.Join(root, "src", "m"+strconv.Itoa(i), "facture.pdf")
		writeFile(t, sources[i], "content-"+strconv.Itoa(i))
	}

	var wg sync.WaitGroup
	landed := make([]string, movers)
	errs := make([]error, movers)
	for i := 0; i < movers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			landed[i], errs[i] = renameAtomicNoOverwrite(sources[i], target, maxRenameAttempts)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("mover %d failed: %v", i, err)
		}
	}

	seen := map[string]bool{}
	for i, p := range landed {
		if seen[p] {
			t.Fatalf("mover %d landed on a duplicate path %q", i, p)
		}
		seen[p] = true
		if p == target {
			t.Fatalf("mover %d landed on the pre-existing target %q", i, target)
		}
	}
	if got := readFile(t, target); got != "PRE-EXISTING" {
		t.Fatalf("pre-existing target content = %q", got)
	}

	entries, err := os.ReadDir(targetDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != movers+1 {
		t.Fatalf("target dir has %d files, want %d (pre-existing + every mover)", len(entries), movers+1)
	}

	// Every source is gone and every payload survives exactly once.
	contents := map[string]int{}
	for _, e := range entries {
		contents[readFile(t, filepath.Join(targetDir, e.Name()))]++
	}
	for i := 0; i < movers; i++ {
		mustNotExist(t, sources[i])
		want := "content-" + strconv.Itoa(i)
		if contents[want] != 1 {
			t.Fatalf("payload %q appears %d times under the target, want 1", want, contents[want])
		}
	}
}
