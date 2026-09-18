package database

import (
	"path/filepath"
	"strconv"
	"testing"
)

// These cases are ported from pdf-triage's src/infrastructure/db/database.test.ts (20 cases). The
// upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/db/database.test.ts` -> 20 passed), so no upstream case is
// pinned red.
//
// TS-specific scaffolding (vi.resetModules() + a module-level singleton + process.env.PDF_DB_PATH)
// has no Go equivalent: each case here opens its own *Store against a t.TempDir() file, which is the
// same isolation the TS afterEach/beforeEach pair produced. Added cases cover the pragmas, the
// migrations, FTS drift rebuild/backfill and the two new raw-SQL methods (DeleteDocument, PurgeAll)
// that had no TS test.

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func strPtr(s string) *string { return &s }

func sampleDoc() NewDocument {
	return NewDocument{
		Checksum:         "abc123",
		Title:            "Facture SFR Janvier",
		Registre:         "REF-001",
		Date:             "2026-01-15",
		Category:         "invoices",
		Subcategory:      "sfr",
		Summary:          "Facture mensuelle SFR pour janvier",
		Tags:             []string{"facture", "sfr"},
		RawText:          "Contenu complet de la facture SFR de janvier 2026",
		MarkdownContent:  "# Facture SFR",
		OriginalFilename: "facture.pdf",
		OriginalPath:     "C:/raws/facture.pdf",
		NewPath:          "C:/archive/invoices/sfr/2026/facture.pdf",
		Embedding:        []float64{0.1, 0.2, 0.3},
		Status:           "COMPLETED",
	}
}

func tableNames(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	rows, err := s.db.Query("SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		t.Fatalf("sqlite_master: %v", err)
	}
	defer rows.Close()
	names := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names[n] = true
	}
	return names
}

func ftsDocIDs(t *testing.T, s *Store, match string) []int64 {
	t.Helper()
	rows, err := s.db.Query("SELECT doc_id FROM documents_fts WHERE documents_fts MATCH ?", match)
	if err != nil {
		t.Fatalf("fts match %q: %v", match, err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

func containsID(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// --- getDb / initSchema ---

func TestOpenCreatesTables(t *testing.T) {
	s := newTestStore(t)
	names := tableNames(t, s)
	for _, want := range []string{"documents", "categories_db", "blocked_files"} {
		if !names[want] {
			t.Errorf("expected table %q to exist; got %v", want, names)
		}
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	// TS asserts `getDb() === getDb()` (one module-level handle). Go has no singleton: the case here
	// is that opening an already-initialized database a second time re-runs every
	// CREATE TABLE IF NOT EXISTS / PRAGMA migration without error, which is the property the TS
	// singleton guaranteed implicitly.
	path := filepath.Join(t.TempDir(), "test.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	defer s1.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()
	if len(tableNames(t, s2)) == 0 {
		t.Fatal("second Open left no tables")
	}
}

// --- insertDocumentRecord / retrieval ---

func TestInsertAndRetrieveByIDAndChecksum(t *testing.T) {
	s := newTestStore(t)
	id, err := s.InsertDocumentRecord(sampleDoc())
	if err != nil {
		t.Fatalf("InsertDocumentRecord: %v", err)
	}

	byID, err := s.GetDocumentByID(id)
	if err != nil || byID == nil {
		t.Fatalf("GetDocumentByID: %v / %v", byID, err)
	}
	if byID.Title != "Facture SFR Janvier" {
		t.Errorf("title = %q", byID.Title)
	}
	if byID.Category != "invoices" {
		t.Errorf("category = %q", byID.Category)
	}
	if byID.Subcategory != "sfr" {
		t.Errorf("subcategory = %q", byID.Subcategory)
	}
	if byID.Tags != `["facture","sfr"]` {
		t.Errorf("tags = %q", byID.Tags)
	}

	byChecksum, err := s.GetDocumentByChecksum("abc123")
	if err != nil || byChecksum == nil {
		t.Fatalf("GetDocumentByChecksum: %v / %v", byChecksum, err)
	}
	if byChecksum.ID != id {
		t.Errorf("checksum id = %d, want %d", byChecksum.ID, id)
	}
}

func TestInsertDefaultsSubcategoryToGeneral(t *testing.T) {
	s := newTestStore(t)
	doc := sampleDoc()
	doc.Subcategory = ""
	id, err := s.InsertDocumentRecord(doc)
	if err != nil {
		t.Fatalf("InsertDocumentRecord: %v", err)
	}
	got, _ := s.GetDocumentByID(id)
	if got.Subcategory != "general" {
		t.Errorf("subcategory = %q, want general", got.Subcategory)
	}
}

func TestInsertRejectsDuplicateChecksum(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.InsertDocumentRecord(sampleDoc()); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	dup := sampleDoc()
	dup.OriginalFilename = "other.pdf"
	if _, err := s.InsertDocumentRecord(dup); err == nil {
		t.Fatal("expected duplicate checksum insert to fail")
	}
}

func TestGetAllDocumentsNewestFirst(t *testing.T) {
	s := newTestStore(t)
	d1 := sampleDoc()
	d1.Checksum = "c1"
	id1, _ := s.InsertDocumentRecord(d1)
	d2 := sampleDoc()
	d2.Checksum = "c2"
	id2, _ := s.InsertDocumentRecord(d2)

	all, err := s.GetAllDocuments()
	if err != nil {
		t.Fatalf("GetAllDocuments: %v", err)
	}
	if len(all) != 2 || all[0].ID != id2 || all[1].ID != id1 {
		t.Fatalf("order = %v, want [%d %d]", all, id2, id1)
	}
}

func TestInsertIndexesFTS(t *testing.T) {
	s := newTestStore(t)
	id, _ := s.InsertDocumentRecord(sampleDoc())
	ids := ftsDocIDs(t, s, "SFR")
	if !containsID(ids, id) {
		t.Errorf("FTS MATCH 'SFR' = %v, want to contain %d", ids, id)
	}
}

// --- updateDocumentRecord ---

func TestUpdateReturnsFalseForMissingID(t *testing.T) {
	s := newTestStore(t)
	ok, err := s.UpdateDocumentRecord(999, DocumentUpdates{Title: strPtr("Nope")})
	if err != nil {
		t.Fatalf("UpdateDocumentRecord: %v", err)
	}
	if ok {
		t.Fatal("expected false for missing id")
	}
}

func TestUpdateOnlyProvidedFields(t *testing.T) {
	s := newTestStore(t)
	id, _ := s.InsertDocumentRecord(sampleDoc())
	ok, err := s.UpdateDocumentRecord(id, DocumentUpdates{Summary: strPtr("Updated summary only")})
	if err != nil || !ok {
		t.Fatalf("update: ok=%v err=%v", ok, err)
	}
	doc, _ := s.GetDocumentByID(id)
	if doc.Summary != "Updated summary only" {
		t.Errorf("summary = %q", doc.Summary)
	}
	if doc.Title != "Facture SFR Janvier" {
		t.Errorf("title = %q (must be untouched)", doc.Title)
	}
	if doc.Category != "invoices" {
		t.Errorf("category = %q (must be untouched)", doc.Category)
	}
}

func TestUpdateFrenchAliases(t *testing.T) {
	s := newTestStore(t)
	id, _ := s.InsertDocumentRecord(sampleDoc())
	_, err := s.UpdateDocumentRecord(id, DocumentUpdates{
		Titre:        strPtr("Nouveau Titre"),
		Categorie:    strPtr("utilities"),
		Subcategorie: strPtr("edf"),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	doc, _ := s.GetDocumentByID(id)
	if doc.Title != "Nouveau Titre" {
		t.Errorf("title = %q", doc.Title)
	}
	if doc.Category != "utilities" {
		t.Errorf("category = %q", doc.Category)
	}
	if doc.Subcategory != "edf" {
		t.Errorf("subcategory = %q", doc.Subcategory)
	}
}

func TestUpdateEnglishKeyWins(t *testing.T) {
	s := newTestStore(t)
	id, _ := s.InsertDocumentRecord(sampleDoc())
	_, _ = s.UpdateDocumentRecord(id, DocumentUpdates{
		Category:  strPtr("english-wins"),
		Categorie: strPtr("french-loses"),
	})
	doc, _ := s.GetDocumentByID(id)
	if doc.Category != "english-wins" {
		t.Errorf("category = %q, want english-wins", doc.Category)
	}
}

func TestUpdateReindexesFTS(t *testing.T) {
	s := newTestStore(t)
	doc := sampleDoc()
	doc.Summary = "Facture mensuelle pour janvier"
	id, _ := s.InsertDocumentRecord(doc)
	_, err := s.UpdateDocumentRecord(id, DocumentUpdates{Summary: strPtr("Completely different renamed content xyzzy")})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if containsID(ftsDocIDs(t, s, "mensuelle"), id) {
		t.Error("stale FTS row for 'mensuelle' survived the update")
	}
	if !containsID(ftsDocIDs(t, s, "xyzzy"), id) {
		t.Error("updated FTS row for 'xyzzy' not found")
	}
}

func TestUpdateCoercesStringID(t *testing.T) {
	// FTS5's doc_id is INTEGER; a TEXT-bound id would make `DELETE ... WHERE doc_id = ?` delete
	// nothing while the re-insert adds a second row. The coercion must make a string id behave like
	// the numeric one.
	s := newTestStore(t)
	id, _ := s.InsertDocumentRecord(sampleDoc())
	_, err := s.UpdateDocumentRecord(strconv.FormatInt(id, 10), DocumentUpdates{Summary: strPtr("nouveau contenu xyzzy")})
	if err != nil {
		t.Fatalf("update with string id: %v", err)
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM documents_fts WHERE doc_id = ?", id).Scan(&n); err != nil {
		t.Fatalf("count fts: %v", err)
	}
	if n != 1 {
		t.Errorf("FTS rows for doc %d = %d, want 1", id, n)
	}
	if !containsID(ftsDocIDs(t, s, "xyzzy"), id) {
		t.Error("updated content not searchable")
	}
	if containsID(ftsDocIDs(t, s, "ancien"), id) {
		t.Error("stale content still searchable")
	}
}

// --- blocked files CRUD ---

func blockedEntry() BlockedFileEntry {
	return BlockedFileEntry{
		OriginalPath: "C:/raws/bad.pdf",
		Filename:     "bad.pdf",
		Reason:       "no_text",
		Message:      "Blocked: No text extracted from PDF.",
		MtimeMs:      123456,
		Size:         42,
	}
}

func TestBlockedUpsertThenRead(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertBlockedFile(blockedEntry()); err != nil {
		t.Fatalf("UpsertBlockedFile: %v", err)
	}
	found, err := s.GetBlockedFile("C:/raws/bad.pdf")
	if err != nil || found == nil {
		t.Fatalf("GetBlockedFile: %v / %v", found, err)
	}
	if found.Filename != "bad.pdf" || found.Reason != "no_text" {
		t.Errorf("found = %+v", found)
	}
}

func TestBlockedUpsertUpdatesNotDuplicates(t *testing.T) {
	s := newTestStore(t)
	_ = s.UpsertBlockedFile(blockedEntry())
	e := blockedEntry()
	e.Reason = "ocr_failed"
	e.Message = "Blocked: OCR failed."
	if err := s.UpsertBlockedFile(e); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM blocked_files WHERE original_path = ?", e.OriginalPath).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
	found, _ := s.GetBlockedFile(e.OriginalPath)
	if found.Reason != "ocr_failed" {
		t.Errorf("reason = %q", found.Reason)
	}
}

func TestBlockedDelete(t *testing.T) {
	s := newTestStore(t)
	_ = s.UpsertBlockedFile(blockedEntry())
	if err := s.DeleteBlockedFile(blockedEntry().OriginalPath); err != nil {
		t.Fatalf("DeleteBlockedFile: %v", err)
	}
	found, _ := s.GetBlockedFile(blockedEntry().OriginalPath)
	if found != nil {
		t.Errorf("expected nil, got %+v", found)
	}
}

func TestPruneBlockedFiles(t *testing.T) {
	s := newTestStore(t)
	_ = s.UpsertBlockedFile(blockedEntry())
	keep := blockedEntry()
	keep.OriginalPath = "C:/raws/still-there.pdf"
	keep.Filename = "still-there.pdf"
	_ = s.UpsertBlockedFile(keep)

	if err := s.PruneBlockedFiles([]string{"C:/raws/still-there.pdf"}); err != nil {
		t.Fatalf("PruneBlockedFiles: %v", err)
	}
	if got, _ := s.GetBlockedFile(blockedEntry().OriginalPath); got != nil {
		t.Errorf("stale entry not pruned: %+v", got)
	}
	if got, _ := s.GetBlockedFile("C:/raws/still-there.pdf"); got == nil {
		t.Error("kept entry was pruned")
	}
}

func TestGetAllBlockedFiles(t *testing.T) {
	s := newTestStore(t)
	_ = s.UpsertBlockedFile(blockedEntry())
	second := blockedEntry()
	second.OriginalPath = "C:/raws/second.pdf"
	second.Filename = "second.pdf"
	_ = s.UpsertBlockedFile(second)

	all, err := s.GetAllBlockedFiles()
	if err != nil {
		t.Fatalf("GetAllBlockedFiles: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len = %d, want 2", len(all))
	}
	names := map[string]bool{all[0].Filename: true, all[1].Filename: true}
	if !names["bad.pdf"] || !names["second.pdf"] {
		t.Errorf("filenames = %v", names)
	}
}

// --- getCategorySubcategoryStats ---

func TestStatsCaseInsensitive(t *testing.T) {
	s := newTestStore(t)
	a := sampleDoc()
	a.Checksum, a.Category, a.Subcategory = "a", "invoices", "sfr"
	b := sampleDoc()
	b.Checksum, b.Category, b.Subcategory = "b", "Invoices", "SFR"
	c := sampleDoc()
	c.Checksum, c.Category, c.Subcategory = "c", "invoices", "edf"
	for _, d := range []NewDocument{a, b, c} {
		if _, err := s.InsertDocumentRecord(d); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	stats, err := s.GetCategorySubcategoryStats()
	if err != nil {
		t.Fatalf("GetCategorySubcategoryStats: %v", err)
	}
	if stats.Total != 3 {
		t.Errorf("total = %d", stats.Total)
	}
	if stats.CategoryCounts["invoices"] != 3 {
		t.Errorf("categoryCounts[invoices] = %d", stats.CategoryCounts["invoices"])
	}
	if stats.SubcategoryCounts["invoices"]["sfr"] != 2 {
		t.Errorf("subcategoryCounts[invoices][sfr] = %d", stats.SubcategoryCounts["invoices"]["sfr"])
	}
	if stats.SubcategoryCounts["invoices"]["edf"] != 1 {
		t.Errorf("subcategoryCounts[invoices][edf] = %d", stats.SubcategoryCounts["invoices"]["edf"])
	}
}

func TestStatsEmptySubcategoryGeneral(t *testing.T) {
	s := newTestStore(t)
	doc := sampleDoc()
	doc.Checksum, doc.Category, doc.Subcategory = "a", "misc", ""
	if _, err := s.InsertDocumentRecord(doc); err != nil {
		t.Fatalf("insert: %v", err)
	}
	stats, err := s.GetCategorySubcategoryStats()
	if err != nil {
		t.Fatalf("GetCategorySubcategoryStats: %v", err)
	}
	if stats.SubcategoryCounts["misc"]["general"] != 1 {
		t.Errorf("subcategoryCounts[misc][general] = %d", stats.SubcategoryCounts["misc"]["general"])
	}
}

// --- added: pragmas, migrations, drift/backfill, named raw-SQL methods ---

func TestPragmasMatchTS(t *testing.T) {
	s := newTestStore(t)
	cases := []struct {
		pragma string
		want   string
	}{
		{"journal_mode", "wal"},
		{"synchronous", "1"}, // 1 = NORMAL
		{"temp_store", "2"},  // 2 = MEMORY
		{"busy_timeout", "10000"},
	}
	for _, c := range cases {
		var got string
		if err := s.db.QueryRow("PRAGMA " + c.pragma).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", c.pragma, err)
		}
		if got != c.want {
			t.Errorf("PRAGMA %s = %q, want %q", c.pragma, got, c.want)
		}
	}
}

func TestDeleteDocumentRemovesRowAndFTS(t *testing.T) {
	s := newTestStore(t)
	id, _ := s.InsertDocumentRecord(sampleDoc())
	if err := s.DeleteDocument(id); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	if got, _ := s.GetDocumentByID(id); got != nil {
		t.Errorf("document %d still present", id)
	}
	if containsID(ftsDocIDs(t, s, "SFR"), id) {
		t.Errorf("FTS row for %d survived DeleteDocument", id)
	}
}

func TestPurgeAllRemovesDocumentsAndFTS(t *testing.T) {
	s := newTestStore(t)
	d1 := sampleDoc()
	d1.Checksum = "c1"
	d2 := sampleDoc()
	d2.Checksum = "c2"
	_, _ = s.InsertDocumentRecord(d1)
	_, _ = s.InsertDocumentRecord(d2)

	if err := s.PurgeAll(); err != nil {
		t.Fatalf("PurgeAll: %v", err)
	}
	all, _ := s.GetAllDocuments()
	if len(all) != 0 {
		t.Errorf("documents remaining = %d", len(all))
	}
	if ids := ftsDocIDs(t, s, "SFR"); len(ids) != 0 {
		t.Errorf("FTS rows remaining = %v", ids)
	}
}

func TestFTSDriftRebuildAndBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drift.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Simulate the historical drift: documents_fts exists with the original 7 columns while the
	// code writes 11. Reproduce the pre-drift table and a document whose insert cannot index it.
	if _, err := s.db.Exec("DROP TABLE documents_fts"); err != nil {
		t.Fatalf("drop fts: %v", err)
	}
	if _, err := s.db.Exec(`CREATE VIRTUAL TABLE documents_fts USING fts5(
		doc_id UNINDEXED, title, original_filename, original_path, new_path, registre, summary)`); err != nil {
		t.Fatalf("create drifted fts: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO documents (
		checksum, title, category, original_filename, original_path
	) VALUES ('k1', 'Facture SFR Janvier', 'invoices', 'facture.pdf', 'C:/raws/facture.pdf')`); err != nil {
		t.Fatalf("insert raw document: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	cols, err := tableColumns(reopened, "documents_fts")
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	if len(cols) != len(ftsColumnNames) {
		t.Fatalf("rebuilt fts columns = %v", cols)
	}
	for i, name := range ftsColumnNames {
		if cols[i] != name {
			t.Fatalf("rebuilt fts column %d = %q, want %q", i, cols[i], name)
		}
	}
	ids := ftsDocIDs(t, reopened, "SFR")
	if len(ids) != 1 {
		t.Errorf("backfilled FTS ids = %v, want 1 row", ids)
	}
}

func TestBackfillWhenFTSIndexEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backfill.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// The backfill SELECT is COALESCE(subcategory, 'general') (database.ts:221): a SQL NULL becomes
	// "general", but an empty-string subcategory (the column default) stays "" — unlike the insert
	// path's `subcategory || 'general'`. Both are pinned here because the real corpus has empty
	// strings, not NULLs.
	if _, err := s.db.Exec(`INSERT INTO documents (
		checksum, title, category, subcategory, original_filename, original_path
	) VALUES ('k1', 'Facture EDF', 'utilities', NULL, 'edf.pdf', 'C:/raws/edf.pdf')`); err != nil {
		t.Fatalf("insert raw document: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO documents (
		checksum, title, category, subcategory, original_filename, original_path
	) VALUES ('k2', 'Facture Orange', 'utilities', '', 'orange.pdf', 'C:/raws/orange.pdf')`); err != nil {
		t.Fatalf("insert raw document: %v", err)
	}
	if _, err := s.db.Exec("DELETE FROM documents_fts"); err != nil {
		t.Fatalf("clear fts: %v", err)
	}
	_ = s.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if ids := ftsDocIDs(t, reopened, "EDF"); len(ids) != 1 {
		t.Fatalf("backfilled FTS ids for EDF = %v, want 1 row", ids)
	}
	if got := ftsSubcategory(t, reopened, "EDF"); got != "general" {
		t.Errorf("NULL subcategory backfilled as %q, want general", got)
	}
	if got := ftsSubcategory(t, reopened, "Orange"); got != "" {
		t.Errorf("empty subcategory backfilled as %q, want \"\" (COALESCE quirk)", got)
	}
}

func ftsSubcategory(t *testing.T, s *Store, match string) string {
	t.Helper()
	rows, err := s.db.Query("SELECT subcategory FROM documents_fts WHERE documents_fts MATCH ?", match)
	if err != nil {
		t.Fatalf("fts query: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		var sub string
		if err := rows.Scan(&sub); err != nil {
			t.Fatalf("scan: %v", err)
		}
		return sub
	}
	return ""
}

func TestWarnFTSWriteFailureLoggedOncePerProcess(t *testing.T) {
	oldWarnf := warnf
	var calls int
	warnf = func(format string, args ...any) { calls++ }
	defer func() {
		warnf = oldWarnf
		ftsLogMu.Lock()
		ftsWriteFailureLogged = false
		ftsLogMu.Unlock()
	}()
	ftsLogMu.Lock()
	ftsWriteFailureLogged = false
	ftsLogMu.Unlock()

	warnFTSWriteFailure("insert", nil)
	warnFTSWriteFailure("insert", nil)
	if calls != 1 {
		t.Errorf("warned %d times, want exactly 1 per process", calls)
	}
}

// --- added: manual_decisions CRUD (raw SQL moved here from store/manualdecisions) ---
//
// These methods are the store/database half of the manual_decisions statements that lived in
// src/infrastructure/manual-decisions-store.ts (:119-124 insert, :197 list, :236 select-by-id,
// :247-252 update, :288 delete, :312 clear). store/manualdecisions still owns the TS-matching
// normalization and the JSON mirror; this layer owns the SQL and the row shape.

func sampleDecision() ManualDecisionInsert {
	return ManualDecisionInsert{
		DocumentID:         42,
		Checksum:           "abc123checksum",
		OriginalFilename:   "RLV_CHQ_001.pdf",
		Title:              "Relevé de chèques BNP",
		OldCategory:        "housing",
		OldSubcategory:     "northwind_realty",
		NewCategory:        "administrative",
		NewSubcategory:     "bnp_paribas",
		UserFeedbackReason: "This is a BNP check statement, not Northwind Realty rent",
		RawTextSnippet:     "BNP PARIBAS RELEVE DE CHEQUES",
		RuleKeywords:       []string{"rlv", "chq", "bnp"},
		Enabled:            1,
		CreatedAt:          "2026-01-15T00:00:00.000Z",
	}
}

func TestInsertAndGetManualDecision(t *testing.T) {
	s := newTestStore(t)
	id, err := s.InsertManualDecision(sampleDecision())
	if err != nil {
		t.Fatalf("InsertManualDecision: %v", err)
	}
	if id == 0 {
		t.Fatal("InsertManualDecision returned id 0")
	}

	got, err := s.GetManualDecision(id)
	if err != nil || got == nil {
		t.Fatalf("GetManualDecision: %v / %v", got, err)
	}
	if got.ID != id {
		t.Errorf("id = %d, want %d", got.ID, id)
	}
	if got.DocumentID != 42 {
		t.Errorf("document_id = %d, want 42", got.DocumentID)
	}
	if got.Checksum != "abc123checksum" || got.OriginalFilename != "RLV_CHQ_001.pdf" {
		t.Errorf("identity fields = %+v", got)
	}
	if got.Title != "Relevé de chèques BNP" {
		t.Errorf("title = %q", got.Title)
	}
	if got.OldCategory != "housing" || got.OldSubcategory != "northwind_realty" {
		t.Errorf("old = %q/%q", got.OldCategory, got.OldSubcategory)
	}
	if got.NewCategory != "administrative" || got.NewSubcategory != "bnp_paribas" {
		t.Errorf("new = %q/%q", got.NewCategory, got.NewSubcategory)
	}
	if got.UserFeedbackReason != "This is a BNP check statement, not Northwind Realty rent" {
		t.Errorf("reason = %q", got.UserFeedbackReason)
	}
	if got.RawTextSnippet != "BNP PARIBAS RELEVE DE CHEQUES" {
		t.Errorf("snippet = %q", got.RawTextSnippet)
	}
	// rule_keywords is stored as JSON array text, exactly as the TS JSON.stringify wrote it.
	if got.RuleKeywords != `["rlv","chq","bnp"]` {
		t.Errorf("rule_keywords = %q", got.RuleKeywords)
	}
	if !got.Enabled.Valid || got.Enabled.Int64 != 1 {
		t.Errorf("enabled = %+v, want 1", got.Enabled)
	}
	if got.CreatedAt != "2026-01-15T00:00:00.000Z" {
		t.Errorf("created_at = %q", got.CreatedAt)
	}
}

func TestInsertManualDecisionDefaultsRuleKeywordsToEmptyArray(t *testing.T) {
	// The column default is '[]' (database.ts:134-151); a nil keyword list must be stored as []
	// rather than JSON null so a later read normalizes to an empty list, not a string "null".
	s := newTestStore(t)
	rec := sampleDecision()
	rec.RuleKeywords = nil
	rec.Enabled = 0
	id, err := s.InsertManualDecision(rec)
	if err != nil {
		t.Fatalf("InsertManualDecision: %v", err)
	}
	got, _ := s.GetManualDecision(id)
	if got.RuleKeywords != "[]" {
		t.Errorf("rule_keywords = %q, want []", got.RuleKeywords)
	}
	if !got.Enabled.Valid || got.Enabled.Int64 != 0 {
		t.Errorf("enabled = %+v, want 0", got.Enabled)
	}
}

func TestGetManualDecisionMissingReturnsNil(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetManualDecision(9999)
	if err != nil {
		t.Fatalf("GetManualDecision: %v", err)
	}
	if got != nil {
		t.Fatalf("got %+v, want nil", got)
	}
}

func TestListManualDecisionsNewestFirst(t *testing.T) {
	s := newTestStore(t)
	empty, err := s.ListManualDecisions()
	if err != nil {
		t.Fatalf("ListManualDecisions: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty list = %v, want non-nil empty", empty)
	}

	first := sampleDecision()
	id1, _ := s.InsertManualDecision(first)
	second := sampleDecision()
	second.Checksum = "second"
	second.OriginalFilename = "second.pdf"
	id2, _ := s.InsertManualDecision(second)

	all, err := s.ListManualDecisions()
	if err != nil {
		t.Fatalf("ListManualDecisions: %v", err)
	}
	if len(all) != 2 || all[0].ID != id2 || all[1].ID != id1 {
		t.Fatalf("order = %+v, want ids [%d %d]", all, id2, id1)
	}
}

func TestUpdateManualDecisionRow(t *testing.T) {
	s := newTestStore(t)
	id, _ := s.InsertManualDecision(sampleDecision())

	err := s.UpdateManualDecisionRow(id, ManualDecisionUpdate{
		NewCategory:        "bank",
		NewSubcategory:     "societe_generale",
		UserFeedbackReason: "Actually Société Générale",
		RuleKeywords:       []string{"SG_CODE"},
		Enabled:            0,
	})
	if err != nil {
		t.Fatalf("UpdateManualDecisionRow: %v", err)
	}

	got, _ := s.GetManualDecision(id)
	if got.NewCategory != "bank" || got.NewSubcategory != "societe_generale" {
		t.Errorf("new = %q/%q", got.NewCategory, got.NewSubcategory)
	}
	if got.UserFeedbackReason != "Actually Société Générale" {
		t.Errorf("reason = %q", got.UserFeedbackReason)
	}
	if got.RuleKeywords != `["SG_CODE"]` {
		t.Errorf("rule_keywords = %q", got.RuleKeywords)
	}
	if !got.Enabled.Valid || got.Enabled.Int64 != 0 {
		t.Errorf("enabled = %+v, want 0", got.Enabled)
	}
	// The untouched identity columns survive the update.
	if got.DocumentID != 42 || got.Checksum != "abc123checksum" || got.Title != "Relevé de chèques BNP" {
		t.Errorf("identity columns changed: %+v", got)
	}
}

func TestUpdateManualDecisionRowMissingIDIsNoOp(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpdateManualDecisionRow(9999, ManualDecisionUpdate{NewCategory: "x"}); err != nil {
		t.Fatalf("UpdateManualDecisionRow(missing): %v", err)
	}
}

func TestDeleteManualDecisionRow(t *testing.T) {
	s := newTestStore(t)
	id, _ := s.InsertManualDecision(sampleDecision())
	if err := s.DeleteManualDecisionRow(id); err != nil {
		t.Fatalf("DeleteManualDecisionRow: %v", err)
	}
	if got, _ := s.GetManualDecision(id); got != nil {
		t.Errorf("row %d still present", id)
	}
	// Deleting a missing id is a silent no-op, the same as the TS db.run.
	if err := s.DeleteManualDecisionRow(id); err != nil {
		t.Fatalf("DeleteManualDecisionRow(missing): %v", err)
	}
}

func TestClearManualDecisions(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.InsertManualDecision(sampleDecision())
	second := sampleDecision()
	second.Checksum = "second"
	_, _ = s.InsertManualDecision(second)

	if err := s.ClearManualDecisions(); err != nil {
		t.Fatalf("ClearManualDecisions: %v", err)
	}
	all, _ := s.ListManualDecisions()
	if len(all) != 0 {
		t.Errorf("rows after clear = %d, want 0", len(all))
	}
}

func TestManualDecisionNullColumnsPreserved(t *testing.T) {
	// The table's manual_decisions columns are all nullable except the autoincrement id. A legacy
	// row can hold SQL NULL in document_id / rule_keywords / enabled; the row decoder must surface
	// zero values for the text/number columns and keep enabled NULL distinguishable (Valid == false)
	// so store/manualdecisions can normalise it to active, matching TS.
	s := newTestStore(t)
	if _, err := s.db.Exec(
		`INSERT INTO manual_decisions (document_id, rule_keywords, enabled, created_at)
         VALUES (NULL, NULL, NULL, 'legacy')`,
	); err != nil {
		t.Fatalf("insert NULL row: %v", err)
	}
	got, err := s.GetManualDecision(1)
	if err != nil || got == nil {
		t.Fatalf("GetManualDecision: %v / %v", got, err)
	}
	if got.DocumentID != 0 {
		t.Errorf("document_id = %d, want 0", got.DocumentID)
	}
	if got.Checksum != "" || got.RuleKeywords != "" {
		t.Errorf("NULL text columns = %q/%q, want empty", got.Checksum, got.RuleKeywords)
	}
	if got.Enabled.Valid {
		t.Errorf("enabled = %+v, want invalid (NULL)", got.Enabled)
	}
}
