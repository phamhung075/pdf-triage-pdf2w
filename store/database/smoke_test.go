package database

import (
	"database/sql"
	"math"
	"os"
	"testing"
)

// TestRealDBSmoke is the Phase 3 FTS5 + bm25 smoke test required by the migration design (section
// 9.2) and inventory section 5. It is skipped unless PDF_TRIAGE_SMOKE_DB points at a COPY of the
// real pdf_triage.db, so `go test ./...` never touches user data. The live database, its -wal/-shm
// files and every other *.db in the repo are never opened by this test.
func TestRealDBSmoke(t *testing.T) {
	path := os.Getenv("PDF_TRIAGE_SMOKE_DB")
	if path == "" {
		t.Skip("set PDF_TRIAGE_SMOKE_DB to a COPY of pdf_triage.db to run the real-DB smoke test")
	}

	// Independently reported count, from a second handle on the copy.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("independent handle: %v", err)
	}
	defer raw.Close()
	var reported int
	if err := raw.QueryRow("SELECT COUNT(*) FROM documents").Scan(&reported); err != nil {
		t.Fatalf("independent count: %v", err)
	}
	if reported == 0 {
		t.Fatalf("copy at %s has no documents", path)
	}

	// Open the COPY with the store: this runs every migration against real data.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(copy) migration error: %v", err)
	}
	defer s.Close()

	all, err := s.GetAllDocuments()
	if err != nil {
		t.Fatalf("GetAllDocuments: %v", err)
	}
	if len(all) != reported {
		t.Errorf("Go documents count = %d, sqlite3-reported = %d", len(all), reported)
	}
	t.Logf("documents rows: Go=%d, independent=%d", len(all), reported)

	// FTS MATCH for a common French word, ranked by bm25 (lower score sorts first).
	const word = "contrat"
	rows, err := s.db.Query(
		`SELECT d.id, bm25(documents_fts, `+bm25Weights+`) AS score
		   FROM documents_fts f
		   JOIN documents d ON d.id = f.doc_id
		  WHERE documents_fts MATCH ?
		  ORDER BY bm25(documents_fts, `+bm25Weights+`)
		  LIMIT 25`, word)
	if err != nil {
		t.Fatalf("FTS MATCH %q: %v", word, err)
	}
	defer rows.Close()
	prev := math.Inf(-1)
	n := 0
	top := math.Inf(1)
	for rows.Next() {
		var id int64
		var score float64
		if err := rows.Scan(&id, &score); err != nil {
			t.Fatalf("scan bm25 row: %v", err)
		}
		if score < prev {
			t.Errorf("bm25 order broken at row %d: %.6f < %.6f", n, score, prev)
		}
		if score < top {
			top = score
		}
		prev = score
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("bm25 rows: %v", err)
	}
	if n == 0 {
		t.Errorf("FTS MATCH %q returned no rows", word)
	}
	t.Logf("FTS MATCH %q: %d rows, best bm25 %.6f", word, n, top)

	res, err := s.SearchDocumentsFts(word, FtsSearchFilters{}, 10)
	if err != nil {
		t.Fatalf("SearchDocumentsFts(%q): %v", word, err)
	}
	if len(res) == 0 {
		t.Errorf("SearchDocumentsFts(%q) returned no rows", word)
	}

	stats, err := s.GetCategorySubcategoryStats()
	if err != nil {
		t.Fatalf("GetCategorySubcategoryStats: %v", err)
	}
	if stats.Total != reported {
		t.Errorf("stats total = %d, want %d", stats.Total, reported)
	}
	if len(stats.CategoryCounts) == 0 {
		t.Error("stats categoryCounts is empty")
	}
	t.Logf("category stats: total=%d categories=%d", stats.Total, len(stats.CategoryCounts))
}

// TestCreateForInterop is the Go->TS half of requirement 1 ("a DB created by the TS app opens in Go
// and vice versa"). It creates a fresh database with this store; PDF_TRIAGE_INTEROP_DB is skipped by
// default. The TS side is a throwaway scratch tsx probe that opens the same file with database.ts
// and asserts the row + FTS are visible.
func TestCreateForInterop(t *testing.T) {
	path := os.Getenv("PDF_TRIAGE_INTEROP_DB")
	if path == "" {
		t.Skip("set PDF_TRIAGE_INTEROP_DB to a fresh path to create a store-written DB for the TS probe")
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	id, err := s.InsertDocumentRecord(sampleDoc())
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Logf("created %s with document id %d", path, id)
}
