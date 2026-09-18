package manualdecisions

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
)

// newTestStore builds a Store over a temp DB whose schema is created by store/database.Open, as the
// migration design requires. Tests never open the live pdf_triage.db; every path is under t.TempDir.
func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pdf_triage.db")

	// database.Open creates the schema (documents, blocked_files, manual_decisions) and runs the
	// idempotent migrations. It is closed once the schema exists; the store gets its own handle on
	// the same temp file because database.Store exposes no handle and no manual-decision SQL (GAP).
	seed, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("database.Open(%s): %v", dbPath, err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open(%s): %v", dbPath, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return New(db, Options{DecisionsFile: filepath.Join(dir, "manual_decisions.json")}),
		filepath.Join(dir, "manual_decisions.json")
}

func readMirror(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read mirror %s: %v", path, err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse mirror %s: %v", path, err)
	}
	return out
}

func baseRecord() Record {
	return Record{
		DocumentID:       1,
		Checksum:         "a",
		OriginalFilename: "A.pdf",
		Title:            "A",
		OldCategory:      "x",
		OldSubcategory:   "y",
		NewCategory:      "c",
		NewSubcategory:   "d",
	}
}

// --- Ported from src/infrastructure/manual-decisions-store.test.ts (9 cases) ---

func TestRecordManualDecisionInDBAndMirror(t *testing.T) {
	s, mirror := newTestStore(t)

	s.RecordManualDecision(Record{
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
	})

	decisions := s.GetManualDecisions()
	if len(decisions) != 1 {
		t.Fatalf("decisions length = %d, want 1", len(decisions))
	}
	d := decisions[0]
	if d.DocumentID != 42 {
		t.Errorf("document_id = %d, want 42", d.DocumentID)
	}
	if d.OldCategory != "housing" {
		t.Errorf("old_category = %q, want housing", d.OldCategory)
	}
	if d.OldSubcategory != "northwind_realty" {
		t.Errorf("old_subcategory = %q, want northwind_realty", d.OldSubcategory)
	}
	if d.NewCategory != "administrative" {
		t.Errorf("new_category = %q, want administrative", d.NewCategory)
	}
	if d.NewSubcategory != "bnp_paribas" {
		t.Errorf("new_subcategory = %q, want bnp_paribas", d.NewSubcategory)
	}
	if d.UserFeedbackReason != "This is a BNP check statement, not Northwind Realty rent" {
		t.Errorf("user_feedback_reason = %q", d.UserFeedbackReason)
	}

	if _, err := os.Stat(mirror); err != nil {
		t.Fatalf("mirror file missing: %v", err)
	}
	j := readMirror(t, mirror)
	if len(j) != 1 {
		t.Fatalf("mirror length = %d, want 1", len(j))
	}
	if j[0]["new_subcategory"] != "bnp_paribas" {
		t.Errorf("mirror new_subcategory = %v, want bnp_paribas", j[0]["new_subcategory"])
	}
}

func TestWritesNowhereWhenDecisionsPathNotConfigured(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pdf_triage.db")
	seed, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	relTarget := filepath.Join(cwd, "manual_decisions.json")
	_ = os.Remove(relTarget)

	before := dirNames(t, cwd)

	// Empty path (an incomplete CONFIG) and a relative path (the old fallback that resolved against
	// process.cwd()) must both be refused without crashing: both TS call sites catch and log.
	for _, bad := range []string{"", "manual_decisions.json"} {
		s := New(db, Options{DecisionsFile: bad})
		s.RecordManualDecision(baseRecord())
	}

	after := dirNames(t, cwd)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("cwd changed: before=%v after=%v", before, after)
	}
	if _, err := os.Stat(relTarget); err == nil {
		t.Fatalf("a relative write landed in process.cwd(): %s", relTarget)
	}

	s := New(db, Options{DecisionsFile: filepath.Join(dir, "m.json")})
	if got := len(s.GetManualDecisions()); got != 2 {
		t.Errorf("DB rows = %d, want 2 (a bad mirror path must not block the DB insert)", got)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestAutoDerivesKeywordsAndStampsID(t *testing.T) {
	s, mirror := newTestStore(t)

	s.RecordManualDecision(Record{
		DocumentID:         42,
		Checksum:           "abc123checksum",
		OriginalFilename:   "RLV_CHQ_001.pdf",
		Title:              "Relevé de chèques BNP",
		OldCategory:        "housing",
		OldSubcategory:     "northwind_realty",
		NewCategory:        "bank",
		NewSubcategory:     "bnp_paribas",
		UserFeedbackReason: "This is a BNP check statement, not Northwind Realty rent",
		RawTextSnippet:     "BNP PARIBAS RELEVE DE CHEQUES",
	})

	decisions := s.GetManualDecisions()
	if len(decisions) != 1 {
		t.Fatalf("decisions length = %d, want 1", len(decisions))
	}
	d := decisions[0]
	for _, want := range []string{"rlv", "chq", "bnp"} {
		if !containsString(d.RuleKeywords, want) {
			t.Errorf("rule_keywords = %v, missing %q", d.RuleKeywords, want)
		}
	}
	if d.Enabled == nil || *d.Enabled != 1 {
		t.Errorf("enabled = %v, want 1", d.Enabled)
	}
	if d.ID == 0 {
		t.Errorf("id was not stamped")
	}

	j := readMirror(t, mirror)
	if got := int64(j[0]["id"].(float64)); got != d.ID {
		t.Errorf("mirror id = %d, want %d", got, d.ID)
	}
	if kw := j[0]["rule_keywords"].([]any); len(kw) == 0 {
		t.Errorf("mirror rule_keywords is empty")
	}
	if got := int(j[0]["enabled"].(float64)); got != 1 {
		t.Errorf("mirror enabled = %d, want 1", got)
	}
}

func TestKeepsCallerSuppliedKeywordsAndDisabledFlag(t *testing.T) {
	s, _ := newTestStore(t)

	s.RecordManualDecision(Record{
		DocumentID:       9,
		Checksum:         "x",
		OriginalFilename: "MY_FILE.pdf",
		Title:            "My file",
		OldCategory:      "a",
		OldSubcategory:   "b",
		NewCategory:      "c",
		NewSubcategory:   "d",
		RuleKeywords:     []string{"MY_CODE"},
		Enabled:          intPtr(0),
	})

	decisions := s.GetManualDecisions()
	if !reflect.DeepEqual(decisions[0].RuleKeywords, []string{"MY_CODE"}) {
		t.Errorf("rule_keywords = %v, want [MY_CODE]", decisions[0].RuleKeywords)
	}
	if decisions[0].Enabled == nil || *decisions[0].Enabled != 0 {
		t.Errorf("enabled = %v, want 0", decisions[0].Enabled)
	}
}

func TestUpdatesDecisionInBothStores(t *testing.T) {
	s, mirror := newTestStore(t)

	s.RecordManualDecision(Record{
		DocumentID:         5,
		Checksum:           "cc",
		OriginalFilename:   "SG_RELEVE.pdf",
		Title:              "Relevé Société Générale",
		OldCategory:        "housing",
		OldSubcategory:     "northwind_realty",
		NewCategory:        "bank",
		NewSubcategory:     "bnp_paribas",
		UserFeedbackReason: "Wrong bank",
	})

	before := s.GetManualDecisions()
	id := before[0].ID

	updated, err := s.UpdateManualDecision(id, Patch{
		NewSubcategory:     strPtr("societe_generale"),
		UserFeedbackReason: strPtr("Actually Société Générale"),
		RuleKeywords:       &[]string{"SG_CODE"},
		Enabled:            intPtr(0),
	})
	if err != nil {
		t.Fatalf("UpdateManualDecision: %v", err)
	}
	if updated == nil {
		t.Fatal("UpdateManualDecision returned nil")
	}
	if updated.NewSubcategory != "societe_generale" {
		t.Errorf("new_subcategory = %q", updated.NewSubcategory)
	}
	if updated.UserFeedbackReason != "Actually Société Générale" {
		t.Errorf("user_feedback_reason = %q", updated.UserFeedbackReason)
	}
	if !reflect.DeepEqual(updated.RuleKeywords, []string{"SG_CODE"}) {
		t.Errorf("rule_keywords = %v", updated.RuleKeywords)
	}
	if updated.Enabled == nil || *updated.Enabled != 0 {
		t.Errorf("enabled = %v, want 0", updated.Enabled)
	}

	decisions := s.GetManualDecisions()
	if decisions[0].NewSubcategory != "societe_generale" {
		t.Errorf("DB new_subcategory = %q", decisions[0].NewSubcategory)
	}
	if decisions[0].Enabled == nil || *decisions[0].Enabled != 0 {
		t.Errorf("DB enabled = %v, want 0", decisions[0].Enabled)
	}

	j := readMirror(t, mirror)
	if j[0]["new_subcategory"] != "societe_generale" {
		t.Errorf("mirror new_subcategory = %v", j[0]["new_subcategory"])
	}
	if got := j[0]["rule_keywords"].([]any); !reflect.DeepEqual(got, []any{"SG_CODE"}) {
		t.Errorf("mirror rule_keywords = %v", got)
	}
	if got := int(j[0]["enabled"].(float64)); got != 0 {
		t.Errorf("mirror enabled = %d, want 0", got)
	}
}

func TestUpdateNonExistentReturnsNil(t *testing.T) {
	s, _ := newTestStore(t)

	updated, err := s.UpdateManualDecision(9999, Patch{Enabled: intPtr(0)})
	if err != nil {
		t.Fatalf("UpdateManualDecision: %v", err)
	}
	if updated != nil {
		t.Fatalf("updated = %+v, want nil", updated)
	}
}

func TestDeletesSingleFromBothStores(t *testing.T) {
	s, mirror := newTestStore(t)

	s.RecordManualDecision(Record{
		DocumentID: 1, Checksum: "a", OriginalFilename: "A.pdf", Title: "A",
		OldCategory: "x", OldSubcategory: "y", NewCategory: "c", NewSubcategory: "d",
	})
	s.RecordManualDecision(Record{
		DocumentID: 2, Checksum: "b", OriginalFilename: "B.pdf", Title: "B",
		OldCategory: "x", OldSubcategory: "y", NewCategory: "c", NewSubcategory: "e",
	})

	decisions := s.GetManualDecisions()
	var idA int64
	for _, d := range decisions {
		if d.OriginalFilename == "A.pdf" {
			idA = d.ID
		}
	}

	ok, err := s.DeleteManualDecision(idA)
	if err != nil {
		t.Fatalf("DeleteManualDecision(A): %v", err)
	}
	if !ok {
		t.Error("DeleteManualDecision(A) = false, want true")
	}
	ok, err = s.DeleteManualDecision(9999)
	if err != nil {
		t.Fatalf("DeleteManualDecision(9999): %v", err)
	}
	if ok {
		t.Error("DeleteManualDecision(9999) = true, want false")
	}

	remaining := s.GetManualDecisions()
	if len(remaining) != 1 {
		t.Fatalf("remaining length = %d, want 1", len(remaining))
	}
	if remaining[0].OriginalFilename != "B.pdf" {
		t.Errorf("remaining[0] = %q, want B.pdf", remaining[0].OriginalFilename)
	}

	j := readMirror(t, mirror)
	if len(j) != 1 {
		t.Fatalf("mirror length = %d, want 1", len(j))
	}
	if j[0]["original_filename"] != "B.pdf" {
		t.Errorf("mirror[0] = %v, want B.pdf", j[0]["original_filename"])
	}
}

func TestClearsEveryDecisionFromBothStores(t *testing.T) {
	s, mirror := newTestStore(t)

	s.RecordManualDecision(baseRecord())
	if err := s.ClearManualDecisions(); err != nil {
		t.Fatalf("ClearManualDecisions: %v", err)
	}

	if got := s.GetManualDecisions(); len(got) != 0 {
		t.Errorf("decisions after clear = %v, want empty", got)
	}
	if _, err := os.Stat(mirror); err != nil {
		t.Fatalf("mirror missing after clear: %v", err)
	}
	if j := readMirror(t, mirror); len(j) != 0 {
		t.Errorf("mirror after clear = %v, want []", j)
	}
}

func TestReadManualDecisionsSyncNewestFirstNormalized(t *testing.T) {
	s, _ := newTestStore(t)

	s.RecordManualDecision(Record{
		DocumentID: 1, Checksum: "a", OriginalFilename: "A.pdf", Title: "A",
		OldCategory: "x", OldSubcategory: "y", NewCategory: "c", NewSubcategory: "d",
	})
	s.RecordManualDecision(Record{
		DocumentID: 2, Checksum: "b", OriginalFilename: "B.pdf", Title: "B",
		OldCategory: "x", OldSubcategory: "y", NewCategory: "c", NewSubcategory: "e",
	})

	sync := s.ReadManualDecisionsSync()
	if len(sync) != 2 {
		t.Fatalf("sync length = %d, want 2", len(sync))
	}
	if sync[0].OriginalFilename != "B.pdf" {
		t.Errorf("sync[0] = %q, want B.pdf (newest first)", sync[0].OriginalFilename)
	}
	if sync[0].Enabled == nil || *sync[0].Enabled != 1 {
		t.Errorf("sync[0].enabled = %v, want 1", sync[0].Enabled)
	}
	if sync[0].RuleKeywords == nil {
		t.Errorf("sync[0].rule_keywords is nil, want a (possibly empty) array")
	}
}

// --- Added cases for behaviour the TS suite left unasserted ---

func strPtr(v string) *string { return &v }

func TestDecisionsFilePathGuard(t *testing.T) {
	s, mirror := newTestStore(t)

	if _, err := s.decisionsFilePath(); err != nil {
		t.Fatalf("absolute path rejected: %v", err)
	}

	// A relative path is exactly the process.cwd() failure mode the guard removes.
	s2 := New(s.db, Options{DecisionsFile: "manual_decisions.json"})
	if _, err := s2.decisionsFilePath(); err == nil {
		t.Fatal("relative path accepted, want error")
	}
	if got := s2.ReadManualDecisionsSync(); len(got) != 0 {
		t.Errorf("ReadManualDecisionsSync with a bad path = %v, want empty", got)
	}

	// Absolute mirror path still round-trips.
	s.RecordManualDecision(baseRecord())
	if len(s.ReadManualDecisionsSync()) != 1 {
		t.Errorf("absolute mirror read = %d, want 1", len(s.ReadManualDecisionsSync()))
	}
	if _, err := os.Stat(mirror); err != nil {
		t.Fatalf("mirror missing: %v", err)
	}
}

func TestNormalizeRuleKeywords(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  []string
	}{
		{"nil", nil, []string{}},
		{"json array string", `["a","b"]`, []string{"a", "b"}},
		{"csv fallback", "a, b ,", []string{"a", "b"}},
		{"empty string", "   ", []string{}},
		{"json non-array", `{"a":1}`, []string{}},
		{"already a slice", []string{"x"}, []string{"x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeRuleKeywords(tc.value); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("normalizeRuleKeywords(%v) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestNormalizeEnabled(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  int
	}{
		{"nil defaults active", nil, 1},
		{"number 0", int64(0), 0},
		{"float 0", float64(0), 0},
		{"string 0", "0", 0},
		{"string 1", "1", 1},
		{"number 1", float64(1), 1},
		{"padded string 0", " 0", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeEnabled(tc.value); got != tc.want {
				t.Errorf("normalizeEnabled(%v) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

func TestUpdateTrimsLowercasesAndDedups(t *testing.T) {
	s, _ := newTestStore(t)

	s.RecordManualDecision(baseRecord())
	id := s.GetManualDecisions()[0].ID

	updated, err := s.UpdateManualDecision(id, Patch{
		NewCategory:    strPtr("  BANK "),
		NewSubcategory: strPtr(" BNP "),
		RuleKeywords:   &[]string{" a ", "b", "a", ""},
	})
	if err != nil {
		t.Fatalf("UpdateManualDecision: %v", err)
	}
	if updated.NewCategory != "bank" {
		t.Errorf("new_category = %q, want bank", updated.NewCategory)
	}
	if updated.NewSubcategory != "bnp" {
		t.Errorf("new_subcategory = %q, want bnp", updated.NewSubcategory)
	}
	if !reflect.DeepEqual(updated.RuleKeywords, []string{"a", "b"}) {
		t.Errorf("rule_keywords = %v, want [a b]", updated.RuleKeywords)
	}
	// Untouched fields survive.
	if updated.OriginalFilename != "A.pdf" || updated.OldCategory != "x" {
		t.Errorf("unrelated fields changed: %+v", updated)
	}
	// They also survive in the row reloaded from SQLite.
	reloaded := s.GetManualDecisions()[0]
	if reloaded.NewCategory != "bank" || !reflect.DeepEqual(reloaded.RuleKeywords, []string{"a", "b"}) {
		t.Errorf("reloaded = %+v", reloaded)
	}
}

func TestRecordTruncatesSnippetTo500(t *testing.T) {
	s, mirror := newTestStore(t)

	long := strings.Repeat("x", 600)
	s.RecordManualDecision(Record{
		DocumentID: 1, Checksum: "a", OriginalFilename: "A.pdf", Title: "A",
		OldCategory: "x", OldSubcategory: "y", NewCategory: "c", NewSubcategory: "d",
		RawTextSnippet: long,
	})

	j := readMirror(t, mirror)
	if got := j[0]["raw_text_snippet"].(string); len(got) != 500 {
		t.Errorf("mirror snippet length = %d, want 500", len(got))
	}
	if got := s.GetManualDecisions()[0].RawTextSnippet; len(got) != 500 {
		t.Errorf("DB snippet length = %d, want 500", len(got))
	}
}

func TestUpdateCreatesMirrorWhenMissing(t *testing.T) {
	s, mirror := newTestStore(t)

	s.RecordManualDecision(baseRecord())
	id := s.GetManualDecisions()[0].ID

	// A user editing a DB-only record (mirror absent) must not fail; TS writes the (unchanged) list.
	if err := os.Remove(mirror); err != nil {
		t.Fatalf("remove mirror: %v", err)
	}
	if _, err := s.UpdateManualDecision(id, Patch{NewSubcategory: strPtr("zz")}); err != nil {
		t.Fatalf("UpdateManualDecision: %v", err)
	}
	if _, err := os.Stat(mirror); err != nil {
		t.Fatalf("mirror not recreated: %v", err)
	}
	if j := readMirror(t, mirror); len(j) != 0 {
		t.Errorf("mirror = %v, want [] (the id was not present in the absent mirror)", j)
	}
}

func TestGetManualDecisionsFallsBackToMirror(t *testing.T) {
	dir := t.TempDir()
	mirror := filepath.Join(dir, "manual_decisions.json")

	// Mirror written newest-last (append order); the sync reader reverses to newest-first.
	payload := `[
	  {"id":1,"document_id":1,"original_filename":"A.pdf","rule_keywords":["a"],"enabled":1,"created_at":"t1"},
	  {"id":2,"document_id":2,"original_filename":"B.pdf","rule_keywords":"[\"b\"]","enabled":"0","created_at":"t2"}
	]`
	if err := os.WriteFile(mirror, []byte(payload), 0o644); err != nil {
		t.Fatalf("write mirror: %v", err)
	}

	s := New(failingDB{}, Options{DecisionsFile: mirror})
	got := s.GetManualDecisions()
	if len(got) != 2 {
		t.Fatalf("fallback length = %d, want 2", len(got))
	}
	if got[0].OriginalFilename != "B.pdf" {
		t.Errorf("fallback[0] = %q, want B.pdf", got[0].OriginalFilename)
	}
	if got[1].RuleKeywords == nil || got[1].RuleKeywords[0] != "a" {
		t.Errorf("fallback[1].rule_keywords = %v, want [a]", got[1].RuleKeywords)
	}
	if got[0].Enabled == nil || *got[0].Enabled != 0 {
		t.Errorf("fallback[0].enabled = %v, want 0", got[0].Enabled)
	}
}

func TestRecordStillMirrorsWhenDBInsertFails(t *testing.T) {
	dir := t.TempDir()
	mirror := filepath.Join(dir, "manual_decisions.json")

	s := New(failingDB{}, Options{DecisionsFile: mirror})
	rec := baseRecord()
	rec.ID = 77
	s.RecordManualDecision(rec)

	j := readMirror(t, mirror)
	if len(j) != 1 {
		t.Fatalf("mirror length = %d, want 1", len(j))
	}
	if got := int64(j[0]["id"].(float64)); got != 77 {
		t.Errorf("mirror id = %d, want the input fallback 77", got)
	}
}

func TestReadManualDecisionsSyncMtimeCache(t *testing.T) {
	dir := t.TempDir()
	mirror := filepath.Join(dir, "manual_decisions.json")
	s := New(failingDB{}, Options{DecisionsFile: mirror})

	one := `[{"id":1,"original_filename":"A.pdf","rule_keywords":[],"enabled":1}]`
	two := one[:len(one)-1] + `,{"id":2,"original_filename":"B.pdf","rule_keywords":[],"enabled":1}]`
	if err := os.WriteFile(mirror, []byte(one), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(mirror)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	original := info.ModTime()

	if got := s.ReadManualDecisionsSync(); len(got) != 1 {
		t.Fatalf("first read = %d, want 1", len(got))
	}

	// Same mtime (content changed underneath) → the cache wins, exactly as TS's mtime check does.
	if err := os.WriteFile(mirror, []byte(two), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(mirror, original, original); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if got := s.ReadManualDecisionsSync(); len(got) != 1 {
		t.Errorf("cached read = %d, want 1", len(got))
	}

	// A newer mtime invalidates the cache.
	newer := original.Add(2 * time.Second)
	if err := os.Chtimes(mirror, newer, newer); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	got := s.ReadManualDecisionsSync()
	if len(got) != 2 {
		t.Fatalf("refreshed read = %d, want 2", len(got))
	}
	if got[0].OriginalFilename != "B.pdf" {
		t.Errorf("refreshed[0] = %q, want B.pdf", got[0].OriginalFilename)
	}
}

func TestClearWithBadMirrorPathStillClearsDB(t *testing.T) {
	s, _ := newTestStore(t)
	s.RecordManualDecision(baseRecord())

	bad := New(s.db, Options{DecisionsFile: "relative.json"})
	if err := bad.ClearManualDecisions(); err != nil {
		t.Fatalf("ClearManualDecisions: %v", err)
	}
	if got := bad.GetManualDecisions(); len(got) != 0 {
		t.Errorf("DB rows after clear = %d, want 0", len(got))
	}
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// failingDB is the "DB unavailable" double: every query fails so the fallback paths run.
type failingDB struct{}

func (failingDB) Exec(string, ...any) (sql.Result, error) { return nil, errors.New("db down") }
func (failingDB) Query(string, ...any) (*sql.Rows, error) { return nil, errors.New("db down") }
func (failingDB) QueryRow(string, ...any) *sql.Row        { return nil }
