package jsonregistry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"
)

// These cases are ported from pdf-triage's src/infrastructure/json-registry.test.ts (4 cases). The
// upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/json-registry.test.ts` -> 4 passed), so no upstream case is
// pinned red.
//
// The TS suite drives a real SQLite database through getAllDocuments(); the SQLite layer is a later
// migration phase, so here the document source is an injected stub returning the same rows the test
// inserted. Added cases cover the EPERM/EBUSY copy fallback and its error classifier, which the TS
// suite never exercised.

type registryFile struct {
	UpdatedAt  string           `json:"updated_at"`
	TotalCount int              `json:"total_count"`
	Documents  []map[string]any `json:"documents"`
}

func readRegistry(t *testing.T, path string) registryFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var got registryFile
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal(%s): %v", path, err)
	}
	return got
}

func sampleDoc() DocumentRecord {
	return DocumentRecord{
		ID:               1,
		Checksum:         "chk-abc",
		Title:            "Facture SFR",
		Registre:         "REF-1",
		Date:             "2026-01-15",
		Category:         "invoices",
		Subcategory:      "sfr",
		Summary:          "resume",
		Tags:             `["a","b"]`,
		RawText:          "contenu",
		OriginalFilename: "facture.pdf",
		OriginalPath:     "C:/raws/facture.pdf",
		NewPath:          "C:/archive/invoices/sfr/2026/facture.pdf",
		Status:           "MOVED",
		CreatedAt:        "2026-01-15 10:00:00",
		UpdatedAt:        "2026-01-15 10:00:00",
	}
}

func TestSyncJSONRegistry(t *testing.T) {
	t.Run("writes an empty registry when there are no documents", func(t *testing.T) {
		registryPath := filepath.Join(t.TempDir(), "registry.json")

		err := SyncJSONRegistry(registryPath, func() ([]DocumentRecord, error) {
			return []DocumentRecord{}, nil
		})
		if err != nil {
			t.Fatalf("SyncJSONRegistry: %v", err)
		}

		written := readRegistry(t, registryPath)
		if written.TotalCount != 0 {
			t.Fatalf("total_count = %d, want 0", written.TotalCount)
		}
		if len(written.Documents) != 0 {
			t.Fatalf("documents = %v, want []", written.Documents)
		}
		iso := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)
		if !iso.MatchString(written.UpdatedAt) {
			t.Fatalf("updated_at = %q, want a JavaScript toISOString() timestamp", written.UpdatedAt)
		}
	})

	t.Run("maps DB documents into registry entries, parsing tags and defaulting empty subcategory to \"general\"", func(t *testing.T) {
		registryPath := filepath.Join(t.TempDir(), "registry.json")
		doc := sampleDoc()
		doc.Subcategory = "" // TS sampleDoc({ subcategory: undefined }) stores the DB default ''
		doc.Tags = `["facture","sfr"]`

		err := SyncJSONRegistry(registryPath, func() ([]DocumentRecord, error) {
			return []DocumentRecord{doc}, nil
		})
		if err != nil {
			t.Fatalf("SyncJSONRegistry: %v", err)
		}

		written := readRegistry(t, registryPath)
		if written.TotalCount != 1 {
			t.Fatalf("total_count = %d, want 1", written.TotalCount)
		}
		entry := written.Documents[0]
		if entry["title"] != "Facture SFR" {
			t.Fatalf("title = %v, want Facture SFR", entry["title"])
		}
		if entry["category"] != "invoices" {
			t.Fatalf("category = %v, want invoices", entry["category"])
		}
		if entry["subcategory"] != "general" {
			t.Fatalf("subcategory = %v, want general", entry["subcategory"])
		}
		tags, ok := entry["tags"].([]any)
		if !ok {
			t.Fatalf("tags type = %T, want []any", entry["tags"])
		}
		if len(tags) != 2 || tags[0] != "facture" || tags[1] != "sfr" {
			t.Fatalf("tags = %v, want [facture sfr]", tags)
		}
	})

	t.Run("falls back to an empty array when a document's stored tags are not valid JSON", func(t *testing.T) {
		registryPath := filepath.Join(t.TempDir(), "registry.json")
		doc := sampleDoc()
		doc.Tags = "not valid json{{"

		err := SyncJSONRegistry(registryPath, func() ([]DocumentRecord, error) {
			return []DocumentRecord{doc}, nil
		})
		if err != nil {
			t.Fatalf("SyncJSONRegistry: %v", err)
		}

		written := readRegistry(t, registryPath)
		tags, ok := written.Documents[0]["tags"].([]any)
		if !ok {
			t.Fatalf("tags type = %T, want []any", written.Documents[0]["tags"])
		}
		if len(tags) != 0 {
			t.Fatalf("tags = %v, want []", tags)
		}
	})

	t.Run("does not leave a .tmp file behind after the atomic write", func(t *testing.T) {
		registryPath := filepath.Join(t.TempDir(), "registry.json")

		if err := SyncJSONRegistry(registryPath, func() ([]DocumentRecord, error) {
			return []DocumentRecord{}, nil
		}); err != nil {
			t.Fatalf("SyncJSONRegistry: %v", err)
		}

		if _, err := os.Stat(registryPath + ".tmp"); !os.IsNotExist(err) {
			t.Fatalf(".tmp exists after write, stat err = %v", err)
		}
		if _, err := os.Stat(registryPath); err != nil {
			t.Fatalf("registry does not exist after write: %v", err)
		}
	})
}

func TestSyncJSONRegistryFallsBackToCopyOnEpermOrBusy(t *testing.T) {
	// The TS fallback branch is unreachable in a test, so it is exercised through the renameFile
	// seam with a synthetic EPERM.
	registryPath := filepath.Join(t.TempDir(), "registry.json")
	original := renameFile
	renameFile = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EPERM}
	}
	defer func() { renameFile = original }()

	if err := SyncJSONRegistry(registryPath, func() ([]DocumentRecord, error) {
		return []DocumentRecord{sampleDoc()}, nil
	}); err != nil {
		t.Fatalf("SyncJSONRegistry: %v", err)
	}

	written := readRegistry(t, registryPath)
	if written.TotalCount != 1 {
		t.Fatalf("total_count = %d, want 1", written.TotalCount)
	}
	if _, err := os.Stat(registryPath + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp exists after fallback, stat err = %v", err)
	}
}

func TestIsPermOrBusy(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"EPERM", &os.LinkError{Op: "rename", Err: syscall.EPERM}, true},
		{"EBUSY", &os.LinkError{Op: "rename", Err: syscall.EBUSY}, true},
		{"ENOENT", &os.LinkError{Op: "rename", Err: syscall.ENOENT}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPermOrBusy(tc.err); got != tc.want {
				t.Fatalf("isPermOrBusy(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
