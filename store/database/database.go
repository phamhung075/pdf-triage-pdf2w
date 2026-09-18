// Package database is a Go port of pdf-triage's src/infrastructure/db/database.ts (645 lines): the
// SQLite schema, idempotent migrations, the documents_fts FTS5 virtual table with drift detection
// and backfill, bm25-ranked search, document / blocked-file CRUD and category statistics.
//
// Driver. database/sql with modernc.org/sqlite (pure Go, no cgo), replacing TS's `sqlite3` native
// binding + `sqlite` wrapper (database.ts:1-2). This is the first external dependency of the
// migration module; see go.mod.
//
// Pool settings, chosen deliberately (requirement 3). TS keeps one cached Database handle for the
// process (database.ts:7-32). database/sql is a pool, so this port pins it to exactly ONE connection
// (SetMaxOpenConns(1)/SetMaxIdleConns(1)) for three reasons:
//
//  1. synchronous/temp_store/busy_timeout are per-connection PRAGMAs. TS sets them once on its one
//     connection (database.ts:20-25); the ports run the same four PRAGMA statements, but a Go pool
//     with more than one connection would hand out later connections that never received them
//     (modernc applies DSN pragmas per connection; the setup Exec runs on one). Pinning to one
//     connection keeps the pragmas in force for the handle's whole lifetime.
//  2. SQLite allows a single writer per database, so a writer pool cannot add write throughput. One
//     in-process connection serialises writers without self-contention SQLITE_BUSY; WAL still lets
//     OTHER processes (the TS app) read concurrently while this store writes.
//  3. It reproduces the TS `getDb()` singleton most closely: the caller opens the Store once and
//     shares it. The deliberate trade-off is that same-process reads are serialised with writes.
//
// Upstream status. `npx vitest run src/infrastructure/db/database.test.ts` -> 20 passed at port
// time; all 20 cases are ported (database_test.go). Added cases cover the four pragmas, migration
// idempotency, FTS5 drift rebuild + backfill, the two new named raw-SQL methods (DeleteDocument,
// PurgeAll) and the once-per-process FTS write-failure log.
//
// Raw SQL that lived OUTSIDE database.ts (inventory section 5) now has named methods here, so no
// caller needs raw SQL:
//
//   - DeleteDocument(id) — `DELETE FROM documents WHERE id = ?` plus the best-effort
//     `DELETE FROM documents_fts WHERE doc_id = ?`; replaces clear-registry.ts:50-52's per-row
//     path, relocalize-document.ts:121-123, :216-218, :377-379 and repair-registry.ts:43-45.
//   - PurgeAll() — `DELETE FROM documents` plus the best-effort `DELETE FROM documents_fts`;
//     replaces clear-registry.ts:50-52.
//
// Semantic gaps, all resolved to MATCH TS:
//
//  1. `SELECT *` column order. TS maps rows to a named object, so physical column order is
//     irrelevant. Go scans positionally, and an old database whose `subcategory` (or a later
//     migration column) was appended by ALTER TABLE has a different physical order than a freshly
//     created one. Every query therefore names its columns explicitly in one canonical order
//     (documentColumnNames) instead of using `*`.
//  2. `updateDocumentRecord(id, ...)` type coercion. TS accepts a dynamic id and does
//     `Number(id)` + `Number.isInteger` + `> 0` (database.ts:408-409) so a string-typed id cannot
//     make the FTS delete no-op into a duplicate row. Go's UpdateDocumentRecord takes `id any` and
//     reproduces that coercion (integers, floats, numeric strings; reject NaN, non-integral, <= 0
//     and values beyond JS's 2^53-1 safe-integer ceiling).
//  3. NULL columns. TS objects expose SQL NULL as null and the code falls back with `|| ”` / `??`.
//     Go scans every text column through sql.NullString and normalises NULL to "".
//  4. Timestamps. `new Date().toISOString()` always renders exactly three fractional digits; Go
//     formats explicitly as "2006-01-02T15:04:05.000Z" in UTC.
//  5. FTS write failures. The INSERT path logs once per process (database.ts:232-242, :370-372); the
//     UPDATE path and the FTS deletes swallow errors exactly as TS does (`catch (e) {}`), so they do
//     not consult the one-shot logger.
//  6. Pragma execution. TS sends the four PRAGMAs in one multi-statement exec; database/sql's Exec is
//     one statement per call, so each is executed separately. The effective connection state is
//     identical, and failures are warned-and-continued as TS does (database.ts:26-28).
//  7. Default search limit. TS defaults `limit = 10`; Go has no optional arguments, so a limit <= 0
//     selects 10.
package database

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/phamhung075/pdf-triage-pdf2w/taxonomy"
)

// Store owns the single SQLite handle. Open it once and share it, mirroring the TS module-level
// `dbInstance` cache (database.ts:7-32).
type Store struct {
	db *sql.DB
}

// documentColumnNames is the canonical documents column order. It matches the CREATE TABLE order in
// database.ts:36-71 with the migration columns in the order database.ts adds them, and is used both
// to build explicit SELECT lists and to scan rows. See semantic gap 1 in the package comment.
var documentColumnNames = []string{
	"id", "checksum", "title", "registre", "date", "category", "subcategory", "summary",
	"tags", "raw_text", "markdown_content", "total_amount", "vat_amount", "siren", "iban",
	"expiry_date", "contact_name", "contact_email", "contact_phone", "contact_address",
	"contact_website", "original_filename", "original_path", "new_path", "file_type",
	"source_image_path", "embedding", "status", "created_at", "updated_at",
}

var (
	documentColumns  = strings.Join(documentColumnNames, ", ")
	documentColumnsD = qualifyColumns("d")
	blockedColumns   = "original_path, filename, reason, message, mtime_ms, size, blocked_at"
)

func qualifyColumns(prefix string) string {
	parts := strings.Split(documentColumns, ", ")
	for i := range parts {
		parts[i] = prefix + "." + parts[i]
	}
	return strings.Join(parts, ", ")
}

// ftsColumnNames mirrors `FTS_COLUMN_NAMES` (database.ts:173-176).
var ftsColumnNames = []string{
	"doc_id", "title", "original_filename", "original_path", "new_path",
	"registre", "summary", "category", "subcategory", "tags", "raw_text",
}

// BM25_WEIGHTS, in documents_fts schema order (database.ts:493-505). Title and tags carry the
// signal: tags hold the document type (a RIB is tagged 'rib', a statement 'releve_compte') which is
// the dimension the old substring scorer had no way to express.
//
// raw_text is deliberately 0.5. It earns its place on recall — it is what surfaces a document
// whose title was corrupted by OCR — but a word buried in four pages must never outweigh a word
// in the title, which is exactly how account statements crowded out the RIB.
const bm25Weights = "0,10,1,1,1,1,3,2,2,6,0.5"

// CREATE TABLE statements. Each is executed separately: database/sql's Exec runs one statement per
// call, so the TS multi-statement exec (database.ts:35-89) is split. Text and defaults are verbatim.

const createDocuments = `
    CREATE TABLE IF NOT EXISTS documents (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      checksum TEXT UNIQUE NOT NULL,
      title TEXT NOT NULL,
      registre TEXT DEFAULT '',
      date TEXT DEFAULT '',
      category TEXT NOT NULL,
      subcategory TEXT DEFAULT '',
      summary TEXT DEFAULT '',
      tags TEXT DEFAULT '[]',
      raw_text TEXT DEFAULT '',
      markdown_content TEXT DEFAULT '',
      total_amount TEXT DEFAULT '',
      vat_amount TEXT DEFAULT '',
      siren TEXT DEFAULT '',
      iban TEXT DEFAULT '',
      expiry_date TEXT DEFAULT '',
      contact_name TEXT DEFAULT '',
      contact_email TEXT DEFAULT '',
      contact_phone TEXT DEFAULT '',
      contact_address TEXT DEFAULT '',
      contact_website TEXT DEFAULT '',
      original_filename TEXT NOT NULL,
      original_path TEXT NOT NULL,
      new_path TEXT DEFAULT '',
      file_type TEXT DEFAULT 'PDF',
      -- Where the photograph that produced this PDF was parked
      -- (__raws/.delete_files/img_converted/...). Empty for documents that arrived as PDFs, and
      -- for anything converted before sources were retained — those photos were deleted outright,
      -- so there is nothing to point at and no way to backfill.
      source_image_path TEXT DEFAULT '',
      embedding TEXT DEFAULT '[]',
      status TEXT DEFAULT 'PENDING',
      created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
      updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
    );
`

const createCategoriesDB = `
    CREATE TABLE IF NOT EXISTS categories_db (
      id TEXT PRIMARY KEY,
      name TEXT NOT NULL,
      description TEXT DEFAULT '',
      aliases TEXT DEFAULT '[]'
    );
`

const createBlockedFiles = `
    CREATE TABLE IF NOT EXISTS blocked_files (
      original_path TEXT PRIMARY KEY,
      filename TEXT NOT NULL,
      reason TEXT NOT NULL,
      message TEXT NOT NULL,
      mtime_ms REAL NOT NULL,
      size INTEGER NOT NULL,
      blocked_at DATETIME DEFAULT CURRENT_TIMESTAMP
    );
`

// Create manual_decisions table for registering user move / relocalize choices. rule_keywords
// holds the keywords that make the decision teach FUTURE runs (JSON array text); enabled toggles
// whether it is still injected into the AI prompt — see domain/decision-rule.ts.
const createManualDecisions = `
    CREATE TABLE IF NOT EXISTS manual_decisions (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      document_id INTEGER,
      checksum TEXT,
      original_filename TEXT,
      title TEXT,
      old_category TEXT,
      old_subcategory TEXT,
      new_category TEXT,
      new_subcategory TEXT,
      user_feedback_reason TEXT,
      raw_text_snippet TEXT,
      rule_keywords TEXT DEFAULT '[]',
      enabled INTEGER DEFAULT 1,
      created_at TEXT
    );
`

// CREATE_FTS mirrors database.ts:177-191. The IF NOT EXISTS form is what the code executes; the
// plain form is used after a drift DROP.
const createFTSIfNotExists = `
    CREATE VIRTUAL TABLE IF NOT EXISTS documents_fts USING fts5(
      doc_id UNINDEXED,
        title,
        original_filename,
        original_path,
        new_path,
        registre,
        summary,
        category,
        subcategory,
        tags,
        raw_text
    );
`

const createFTS = `
    CREATE VIRTUAL TABLE documents_fts USING fts5(
      doc_id UNINDEXED,
        title,
        original_filename,
        original_path,
        new_path,
        registre,
        summary,
        category,
        subcategory,
        tags,
        raw_text
    );
`

// backfillFTS is database.ts:217-224 verbatim, including the "empty subcategory indexes as general"
// COALESCE.
const backfillFTS = `
    INSERT INTO documents_fts (doc_id, title, original_filename, original_path, new_path, registre, summary, category, subcategory, tags, raw_text)
    SELECT id, COALESCE(title, ''), COALESCE(original_filename, ''), COALESCE(original_path, ''),
           COALESCE(new_path, ''), COALESCE(registre, ''), COALESCE(summary, ''),
           COALESCE(category, ''), COALESCE(subcategory, 'general'), COALESCE(tags, '[]'),
           COALESCE(raw_text, '')
    FROM documents;
`

// pragmas mirrors database.ts:20-25.
var pragmas = []string{
	"PRAGMA journal_mode = WAL",
	"PRAGMA synchronous = NORMAL",
	"PRAGMA temp_store = MEMORY",
	"PRAGMA busy_timeout = 10000",
}

// warnf is the port of console.warn; a package variable so tests can capture output.
var warnf = log.Printf

// One-shot so a genuinely FTS5-less SQLite build does not spam the log on every write, while a
// real breakage (schema drift, constraint failure) still surfaces instead of vanishing
// (database.ts:232-242). Guarded because writes can come from several goroutines.
var (
	ftsLogMu              sync.Mutex
	ftsWriteFailureLogged bool
)

func warnFTSWriteFailure(operation string, err error) {
	ftsLogMu.Lock()
	defer ftsLogMu.Unlock()
	if ftsWriteFailureLogged {
		return
	}
	ftsWriteFailureLogged = true
	warnf(
		"FTS5 %s failed — full-text search will be incomplete. "+
			"This message is logged once per process. %v", operation, err,
	)
}

// Open opens (creating if needed) the SQLite database at path, applies the pragmas, runs the
// idempotent schema migrations and returns the one shared handle.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// See the pool-settings note in the package comment.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &Store{db: db}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			warnf("SQLite WAL pragma setup notice: %v", err)
		}
	}

	if err := initSchema(s); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// tableColumns ports `PRAGMA table_info(<table>)` and returns the column names in table order; the
// caller only ever passes the two literal table names below.
func tableColumns(s *Store, table string) ([]string, error) {
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var (
			cid       int
			name      string
			colType   string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func initSchema(s *Store) error {
	for _, stmt := range []string{createDocuments, createCategoriesDB, createBlockedFiles} {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}

	// Migration: add subcategory, markdown_content, total_amount, vat_amount, siren, iban,
	// expiry_date, contact_name, contact_email, contact_phone, contact_address, and
	// contact_website columns if missing (database.ts:91-129).
	if err := migrateDocuments(s); err != nil {
		warnf("Table info pragma migration check notice: %v", err)
	}

	if _, err := s.db.Exec(createManualDecisions); err != nil {
		return err
	}

	// Migration: databases created before the feedback-teaches-AI loop learned from the audit log
	// lack rule_keywords/enabled. Legacy rows stay ACTIVE (enabled defaults to 1) and derive their
	// keywords lazily from the stored filename/title (database.ts:153-166).
	if err := migrateManualDecisions(s); err != nil {
		warnf("Manual decisions migration notice: %v", err)
	}

	// FTS5 index. `CREATE VIRTUAL TABLE IF NOT EXISTS` is a no-op against an EXISTING table, so
	// it silently does NOT migrate one whose columns have drifted — which is what happened here:
	// the on-disk table still had the original 7 columns while the INSERTs below had grown to 11.
	// Every insert then failed at prepare, straight into an empty catch, so the index sat at 0 rows
	// against a full corpus of documents and nobody noticed. Detect the drift and rebuild
	// (database.ts:168-226).
	if err := ensureFTS(s); err != nil {
		warnf("FTS5 virtual table setup skipped or not supported: %v", err)
	}
	return nil
}

func migrateDocuments(s *Store) error {
	cols, err := tableColumns(s, "documents")
	if err != nil {
		return err
	}
	has := func(name string) bool {
		for _, c := range cols {
			if c == name {
				return true
			}
		}
		return false
	}

	if !has("subcategory") {
		if _, err := s.db.Exec("ALTER TABLE documents ADD COLUMN subcategory TEXT DEFAULT '';"); err != nil {
			return err
		}
	}
	if !has("markdown_content") {
		if _, err := s.db.Exec("ALTER TABLE documents ADD COLUMN markdown_content TEXT DEFAULT '';"); err != nil {
			return err
		}
	}
	if !has("total_amount") {
		for _, stmt := range []string{
			"ALTER TABLE documents ADD COLUMN total_amount TEXT DEFAULT '';",
			"ALTER TABLE documents ADD COLUMN vat_amount TEXT DEFAULT '';",
			"ALTER TABLE documents ADD COLUMN siren TEXT DEFAULT '';",
			"ALTER TABLE documents ADD COLUMN iban TEXT DEFAULT '';",
			"ALTER TABLE documents ADD COLUMN expiry_date TEXT DEFAULT '';",
		} {
			if _, err := s.db.Exec(stmt); err != nil {
				return err
			}
		}
	}
	if !has("contact_name") {
		for _, stmt := range []string{
			"ALTER TABLE documents ADD COLUMN contact_name TEXT DEFAULT '';",
			"ALTER TABLE documents ADD COLUMN contact_email TEXT DEFAULT '';",
			"ALTER TABLE documents ADD COLUMN contact_phone TEXT DEFAULT '';",
			"ALTER TABLE documents ADD COLUMN contact_address TEXT DEFAULT '';",
			"ALTER TABLE documents ADD COLUMN contact_website TEXT DEFAULT '';",
		} {
			if _, err := s.db.Exec(stmt); err != nil {
				return err
			}
		}
	}
	if !has("file_type") {
		if _, err := s.db.Exec("ALTER TABLE documents ADD COLUMN file_type TEXT DEFAULT 'PDF';"); err != nil {
			return err
		}
	}
	if !has("source_image_path") {
		if _, err := s.db.Exec("ALTER TABLE documents ADD COLUMN source_image_path TEXT DEFAULT '';"); err != nil {
			return err
		}
	}
	_, err = s.db.Exec("UPDATE documents SET contact_name='', contact_email='', contact_phone='', contact_address='', contact_website='' WHERE contact_name LIKE '%Description%' OR contact_name LIKE '%Qty%' OR contact_name LIKE '%Subtotal%' OR contact_name LIKE '%Unit price%';")
	return err
}

func migrateManualDecisions(s *Store) error {
	cols, err := tableColumns(s, "manual_decisions")
	if err != nil {
		return err
	}
	has := func(name string) bool {
		for _, c := range cols {
			if c == name {
				return true
			}
		}
		return false
	}
	if !has("rule_keywords") {
		if _, err := s.db.Exec("ALTER TABLE manual_decisions ADD COLUMN rule_keywords TEXT DEFAULT '[]';"); err != nil {
			return err
		}
	}
	if !has("enabled") {
		if _, err := s.db.Exec("ALTER TABLE manual_decisions ADD COLUMN enabled INTEGER DEFAULT 1;"); err != nil {
			return err
		}
	}
	return nil
}

func ensureFTS(s *Store) error {
	if _, err := s.db.Exec(createFTSIfNotExists); err != nil {
		return err
	}

	actual, err := tableColumns(s, "documents_fts")
	if err != nil {
		return err
	}
	drifted := len(actual) != len(ftsColumnNames)
	if !drifted {
		for i, name := range ftsColumnNames {
			if actual[i] != name {
				drifted = true
				break
			}
		}
	}
	if drifted {
		warnf(
			"FTS5 schema drift: documents_fts has [%s] but the code writes "+
				"[%s]. Rebuilding and backfilling the index.",
			strings.Join(actual, ", "), strings.Join(ftsColumnNames, ", "),
		)
		if _, err := s.db.Exec("DROP TABLE IF EXISTS documents_fts;"); err != nil {
			return err
		}
		if _, err := s.db.Exec(createFTS); err != nil {
			return err
		}
	}

	// Backfill whenever the index is empty but documents exist — covers both the rebuild above
	// and any database whose index was never populated because of the drift.
	var ftsCount, docCount int
	if err := s.db.QueryRow("SELECT COUNT(*) AS n FROM documents_fts").Scan(&ftsCount); err != nil {
		return err
	}
	if err := s.db.QueryRow("SELECT COUNT(*) AS n FROM documents").Scan(&docCount); err != nil {
		return err
	}
	if ftsCount == 0 && docCount > 0 {
		if _, err := s.db.Exec(backfillFTS); err != nil {
			return err
		}
		warnf("FTS5 index backfilled from %d existing document(s).", docCount)
	}
	return nil
}

// DocumentRecord mirrors the TS `DocumentRecord` interface (database.ts:244-276). Tags and embedding
// are the stored JSON text, exactly as the TS row carries them.
type DocumentRecord struct {
	ID               int64  `json:"id"`
	Checksum         string `json:"checksum"`
	Title            string `json:"title"`
	Registre         string `json:"registre"`
	Date             string `json:"date"`
	Category         string `json:"category"`
	Subcategory      string `json:"subcategory"`
	Summary          string `json:"summary"`
	Tags             string `json:"tags"` // JSON string
	RawText          string `json:"raw_text"`
	MarkdownContent  string `json:"markdown_content"`
	TotalAmount      string `json:"total_amount"`
	VatAmount        string `json:"vat_amount"`
	Siren            string `json:"siren"`
	Iban             string `json:"iban"`
	ExpiryDate       string `json:"expiry_date"`
	ContactName      string `json:"contact_name"`
	ContactEmail     string `json:"contact_email"`
	ContactPhone     string `json:"contact_phone"`
	ContactAddress   string `json:"contact_address"`
	ContactWebsite   string `json:"contact_website"`
	OriginalFilename string `json:"original_filename"`
	OriginalPath     string `json:"original_path"`
	NewPath          string `json:"new_path"`
	FileType         string `json:"file_type"`
	// Photo this PDF was made from, parked under .delete_files/img_converted. '' when unknown.
	SourceImagePath string `json:"source_image_path"`
	Embedding       string `json:"embedding"` // JSON string
	Status          string `json:"status"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

// NewDocument is the insert input (database.ts:278-306). Optional TS fields map to the Go zero
// value; `||` fallbacks are applied at insert time.
type NewDocument struct {
	Checksum         string
	Title            string
	Registre         string
	Date             string
	Category         string
	Subcategory      string
	Summary          string
	Tags             []string
	RawText          string
	MarkdownContent  string
	TotalAmount      string
	VatAmount        string
	Siren            string
	Iban             string
	ExpiryDate       string
	ContactName      string
	ContactEmail     string
	ContactPhone     string
	ContactAddress   string
	ContactWebsite   string
	OriginalFilename string
	OriginalPath     string
	NewPath          string
	FileType         string
	SourceImagePath  string
	Embedding        []float64
	Status           string
}

// DocumentUpdates is the update input (database.ts:377-402). A nil pointer means "not provided";
// a non-nil pointer replaces the column even with "". Titre/Categorie/Subcategorie are the French
// aliases of Golden Rule #19, and the English name wins when both are set.
type DocumentUpdates struct {
	Title           *string
	Titre           *string
	Registre        *string
	Date            *string
	Category        *string
	Categorie       *string
	Subcategory     *string
	Subcategorie    *string
	Summary         *string
	Tags            *[]string
	RawText         *string
	MarkdownContent *string
	TotalAmount     *string
	VatAmount       *string
	Siren           *string
	Iban            *string
	ExpiryDate      *string
	ContactName     *string
	ContactEmail    *string
	ContactPhone    *string
	ContactAddress  *string
	ContactWebsite  *string
	NewPath         *string
	Status          *string
}

// FtsSearchFilters mirrors the TS interface (database.ts:476-482). Empty strings mean "no filter".
type FtsSearchFilters struct {
	Category    string
	Subcategory string
	// Inclusive ISO YYYY-MM-DD bound compared against documents.date, which is stored ISO.
	DateFrom string
	DateTo   string
}

// BlockedFileRecord mirrors the blocked_files row (database.ts:556-564).
type BlockedFileRecord struct {
	OriginalPath string  `json:"original_path"`
	Filename     string  `json:"filename"`
	Reason       string  `json:"reason"`
	Message      string  `json:"message"`
	MtimeMs      float64 `json:"mtime_ms"`
	Size         int64   `json:"size"`
	BlockedAt    string  `json:"blocked_at"`
}

// BlockedFileEntry is the upsert input (database.ts:576-583).
type BlockedFileEntry struct {
	OriginalPath string
	Filename     string
	Reason       string
	Message      string
	MtimeMs      float64
	Size         int64
}

// CategoryStats is the return of GetCategorySubcategoryStats (database.ts:614-618).
type CategoryStats struct {
	Total             int
	CategoryCounts    map[string]int
	SubcategoryCounts map[string]map[string]int
}

func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func scanDocument(scan func(dest ...any) error) (DocumentRecord, error) {
	var d DocumentRecord
	var (
		checksum, title, registre, date, category, subcategory, summary, tags, rawText        sql.NullString
		markdownContent, totalAmount, vatAmount, siren, iban, expiryDate                      sql.NullString
		contactName, contactEmail, contactPhone, contactAddress, contactWebsite               sql.NullString
		originalFilename, originalPath, newPath, fileType, sourceImagePath, embedding, status sql.NullString
		createdAt, updatedAt                                                                  sql.NullString
	)
	if err := scan(
		&d.ID, &checksum, &title, &registre, &date, &category, &subcategory, &summary, &tags,
		&rawText, &markdownContent, &totalAmount, &vatAmount, &siren, &iban, &expiryDate,
		&contactName, &contactEmail, &contactPhone, &contactAddress, &contactWebsite,
		&originalFilename, &originalPath, &newPath, &fileType, &sourceImagePath, &embedding,
		&status, &createdAt, &updatedAt,
	); err != nil {
		return DocumentRecord{}, err
	}
	d.Checksum = checksum.String
	d.Title = title.String
	d.Registre = registre.String
	d.Date = date.String
	d.Category = category.String
	d.Subcategory = subcategory.String
	d.Summary = summary.String
	d.Tags = tags.String
	d.RawText = rawText.String
	d.MarkdownContent = markdownContent.String
	d.TotalAmount = totalAmount.String
	d.VatAmount = vatAmount.String
	d.Siren = siren.String
	d.Iban = iban.String
	d.ExpiryDate = expiryDate.String
	d.ContactName = contactName.String
	d.ContactEmail = contactEmail.String
	d.ContactPhone = contactPhone.String
	d.ContactAddress = contactAddress.String
	d.ContactWebsite = contactWebsite.String
	d.OriginalFilename = originalFilename.String
	d.OriginalPath = originalPath.String
	d.NewPath = newPath.String
	d.FileType = fileType.String
	d.SourceImagePath = sourceImagePath.String
	d.Embedding = embedding.String
	d.Status = status.String
	d.CreatedAt = createdAt.String
	d.UpdatedAt = updatedAt.String
	return d, nil
}

// InsertDocumentRecord ports insertDocumentRecord (database.ts:278-375) and returns the new row id.
func (s *Store) InsertDocumentRecord(doc NewDocument) (int64, error) {
	now := nowISO()

	subcategory := doc.Subcategory
	if subcategory == "" {
		subcategory = "general"
	}
	fileType := doc.FileType
	if fileType == "" {
		fileType = string(taxonomy.DetectFileType(doc.OriginalFilename))
	}
	status := doc.Status
	if status == "" {
		status = "PENDING"
	}
	tags := doc.Tags
	if tags == nil {
		tags = []string{}
	}
	embedding := doc.Embedding
	if embedding == nil {
		embedding = []float64{}
	}
	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return 0, err
	}
	embeddingJSON, err := json.Marshal(embedding)
	if err != nil {
		return 0, err
	}

	res, err := s.db.Exec(
		`INSERT INTO documents (
      checksum, title, registre, date, category, subcategory, summary, tags, raw_text, markdown_content,
      total_amount, vat_amount, siren, iban, expiry_date, contact_name, contact_email, contact_phone, contact_address, contact_website,
      original_filename, original_path, new_path, file_type, source_image_path, embedding, status, created_at, updated_at
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		doc.Checksum,
		doc.Title,
		doc.Registre,
		doc.Date,
		doc.Category,
		subcategory,
		doc.Summary,
		string(tagsJSON),
		doc.RawText,
		doc.MarkdownContent,
		doc.TotalAmount,
		doc.VatAmount,
		doc.Siren,
		doc.Iban,
		doc.ExpiryDate,
		doc.ContactName,
		doc.ContactEmail,
		doc.ContactPhone,
		doc.ContactAddress,
		doc.ContactWebsite,
		doc.OriginalFilename,
		doc.OriginalPath,
		doc.NewPath,
		fileType,
		doc.SourceImagePath,
		string(embeddingJSON),
		status,
		now,
		now,
	)
	if err != nil {
		return 0, err
	}
	docID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	// Index in FTS if available.
	if _, err := s.db.Exec(
		`INSERT INTO documents_fts (doc_id, title, original_filename, original_path, new_path, registre, summary, category, subcategory, tags, raw_text)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		docID,
		doc.Title,
		doc.OriginalFilename,
		doc.OriginalPath,
		doc.NewPath,
		doc.Registre,
		doc.Summary,
		doc.Category,
		subcategory,
		string(tagsJSON),
		doc.RawText,
	); err != nil {
		warnFTSWriteFailure("insert", err)
	}

	return docID, nil
}

// UpdateDocumentRecord ports updateDocumentRecord (database.ts:377-459). id is `any` so a numeric
// string is coerced exactly as TS does; see semantic gap 2.
func (s *Store) UpdateDocumentRecord(id any, updates DocumentUpdates) (bool, error) {
	// The FTS delete below is strictly typed on the INTEGER doc_id: a string-typed id binds as
	// TEXT, FTS5's strict comparison silently deletes nothing (changes: 0), and the re-insert
	// then leaves a stale row PLUS a duplicate — silent FTS corruption. Coerce once here so no
	// caller (HTTP route, MCP tool, future code) can trip it (database.ts:404-409).
	numericID, ok := coerceDocumentID(id)
	if !ok {
		return false, nil
	}
	existing, err := s.GetDocumentByID(numericID)
	if err != nil {
		return false, err
	}
	if existing == nil {
		return false, nil
	}

	now := nowISO()
	title := pick(updates.Title, updates.Titre, existing.Title)
	registre := pick(updates.Registre, nil, existing.Registre)
	date := pick(updates.Date, nil, existing.Date)
	category := pick(updates.Category, updates.Categorie, existing.Category)
	subcategory := pick(updates.Subcategory, updates.Subcategorie, existing.Subcategory)
	summary := pick(updates.Summary, nil, existing.Summary)
	tagsStr := existing.Tags
	if updates.Tags != nil {
		t := *updates.Tags
		if t == nil {
			t = []string{}
		}
		b, err := json.Marshal(t)
		if err != nil {
			return false, err
		}
		tagsStr = string(b)
	}
	rawText := pick(updates.RawText, nil, existing.RawText)
	markdownContent := pick(updates.MarkdownContent, nil, existing.MarkdownContent)
	totalAmount := pick(updates.TotalAmount, nil, existing.TotalAmount)
	vatAmount := pick(updates.VatAmount, nil, existing.VatAmount)
	siren := pick(updates.Siren, nil, existing.Siren)
	iban := pick(updates.Iban, nil, existing.Iban)
	expiryDate := pick(updates.ExpiryDate, nil, existing.ExpiryDate)
	contactName := pick(updates.ContactName, nil, existing.ContactName)
	contactEmail := pick(updates.ContactEmail, nil, existing.ContactEmail)
	contactPhone := pick(updates.ContactPhone, nil, existing.ContactPhone)
	contactAddress := pick(updates.ContactAddress, nil, existing.ContactAddress)
	contactWebsite := pick(updates.ContactWebsite, nil, existing.ContactWebsite)
	newPath := pick(updates.NewPath, nil, existing.NewPath)
	status := pick(updates.Status, nil, existing.Status)

	if _, err := s.db.Exec(
		`UPDATE documents SET
      title = ?, registre = ?, date = ?, category = ?, subcategory = ?, summary = ?,
      tags = ?, raw_text = ?, markdown_content = ?, total_amount = ?, vat_amount = ?,
      siren = ?, iban = ?, expiry_date = ?, contact_name = ?, contact_email = ?, contact_phone = ?,
      contact_address = ?, contact_website = ?, new_path = ?, status = ?, updated_at = ?
     WHERE id = ?`,
		title, registre, date, category, subcategory, summary, tagsStr, rawText, markdownContent,
		totalAmount, vatAmount, siren, iban, expiryDate, contactName, contactEmail, contactPhone,
		contactAddress, contactWebsite, newPath, status, now, numericID,
	); err != nil {
		return false, err
	}

	// Update FTS. TS wraps the delete+insert in `try {} catch (err) { // Ignore FTS errors }`
	// (database.ts:446-456), so both failures are swallowed and the one-shot logger is not used.
	if _, err := s.db.Exec("DELETE FROM documents_fts WHERE doc_id = ?", numericID); err == nil {
		_, _ = s.db.Exec(
			`INSERT INTO documents_fts (doc_id, title, original_filename, original_path, new_path, registre, summary, category, subcategory, tags, raw_text)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			numericID, title, existing.OriginalFilename, existing.OriginalPath, newPath,
			registre, summary, category, subcategory, tagsStr, rawText,
		)
	}

	return true, nil
}

// pick is `a ?? b ?? existing` for the string columns. The French alias is only consulted when the
// English key is absent, matching database.ts:414-434.
func pick(primary, alias *string, existing string) string {
	if primary != nil {
		return *primary
	}
	if alias != nil {
		return *alias
	}
	return existing
}

// coerceDocumentID mirrors `Number(id)` + `Number.isInteger(numericId) && numericId > 0`
// (database.ts:408-409). It accepts the integer and float types Go callers use plus numeric strings.
func coerceDocumentID(id any) (int64, bool) {
	var f float64
	switch v := id.(type) {
	case int:
		f = float64(v)
	case int8:
		f = float64(v)
	case int16:
		f = float64(v)
	case int32:
		f = float64(v)
	case int64:
		f = float64(v)
	case uint:
		f = float64(v)
	case uint8:
		f = float64(v)
	case uint16:
		f = float64(v)
	case uint32:
		f = float64(v)
	case uint64:
		f = float64(v)
	case float32:
		f = float64(v)
	case float64:
		f = v
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, false
		}
		f = n
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return 0, false
	}
	// JS integers are only exact up to 2^53-1; beyond that Number.isInteger stays true but the id
	// can never match a real AUTOINCREMENT row.
	if f < 1 || f > 9007199254740991 {
		return 0, false
	}
	return int64(f), true
}

// GetAllDocuments is `SELECT * FROM documents ORDER BY id DESC` (database.ts:461-464).
func (s *Store) GetAllDocuments() ([]DocumentRecord, error) {
	return s.queryDocuments("SELECT " + documentColumns + " FROM documents ORDER BY id DESC")
}

// GetDocumentByID is getDocumentById (database.ts:466-469); nil means no row.
func (s *Store) GetDocumentByID(id int64) (*DocumentRecord, error) {
	row := s.db.QueryRow("SELECT "+documentColumns+" FROM documents WHERE id = ?", id)
	d, err := scanDocument(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// GetDocumentByChecksum is getDocumentByChecksum (database.ts:471-474); nil means no row.
func (s *Store) GetDocumentByChecksum(checksum string) (*DocumentRecord, error) {
	row := s.db.QueryRow("SELECT "+documentColumns+" FROM documents WHERE checksum = ?", checksum)
	d, err := scanDocument(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *Store) queryDocuments(query string, args ...any) ([]DocumentRecord, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	docs := []DocumentRecord{}
	for rows.Next() {
		d, err := scanDocument(rows.Scan)
		if err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

// SearchDocumentsFts ports searchDocumentsFts (database.ts:516-554).
//
// Ranked full-text search over documents_fts.
//
// Filters are applied in SQL, not in JavaScript afterwards: a filter applied after LIMIT would
// silently return fewer rows than asked for.
//
// Throws if the expression is malformed or SQLite was built without FTS5. Callers must catch and
// degrade — the chat must never surface a search error to the user.
func (s *Store) SearchDocumentsFts(matchExpr string, filters FtsSearchFilters, limit int) ([]DocumentRecord, error) {
	if limit <= 0 {
		limit = 10
	}
	conditions := []string{"documents_fts MATCH ?"}
	args := []any{matchExpr}

	if filters.Category != "" {
		conditions = append(conditions, "d.category = ?")
		args = append(args, filters.Category)
	}
	if filters.Subcategory != "" {
		conditions = append(conditions, "d.subcategory = ?")
		args = append(args, filters.Subcategory)
	}
	// Documents with an empty date are excluded from a bounded search rather than sorting as '' —
	// 72 of 861 have no date and would otherwise all pass a >= filter (database.ts:533-542).
	if filters.DateFrom != "" {
		conditions = append(conditions, "d.date <> '' AND d.date >= ?")
		args = append(args, filters.DateFrom)
	}
	if filters.DateTo != "" {
		conditions = append(conditions, "d.date <> '' AND d.date <= ?")
		args = append(args, filters.DateTo)
	}
	args = append(args, limit)

	query := "SELECT " + documentColumnsD + `
       FROM documents_fts f
       JOIN documents d ON d.id = f.doc_id
      WHERE ` + strings.Join(conditions, " AND ") + `
      ORDER BY bm25(documents_fts, ` + bm25Weights + `)
      LIMIT ?`
	return s.queryDocuments(query, args...)
}

// DeleteDocument removes one document row and its FTS entry. It replaces the raw SQL in
// clear-registry.ts:50-52, relocalize-document.ts:121-123, :216-218, :377-379 and
// repair-registry.ts:43-45. The FTS delete is best-effort, as it is at every TS call site
// (`try {} catch (e) {}`).
func (s *Store) DeleteDocument(id int64) error {
	if _, err := s.db.Exec("DELETE FROM documents WHERE id = ?", id); err != nil {
		return err
	}
	_, _ = s.db.Exec("DELETE FROM documents_fts WHERE doc_id = ?", id)
	return nil
}

// PurgeAll removes every document and every FTS entry. It replaces clear-registry.ts:50-52. The
// documents delete is authoritative; the FTS delete is best-effort exactly as in TS.
func (s *Store) PurgeAll() error {
	if _, err := s.db.Exec("DELETE FROM documents"); err != nil {
		return err
	}
	_, _ = s.db.Exec("DELETE FROM documents_fts")
	return nil
}

// GetBlockedFile is getBlockedFile (database.ts:566-569); nil means no row.
func (s *Store) GetBlockedFile(originalPath string) (*BlockedFileRecord, error) {
	row := s.db.QueryRow("SELECT "+blockedColumns+" FROM blocked_files WHERE original_path = ?", originalPath)
	var (
		rec       BlockedFileRecord
		blockedAt sql.NullString
	)
	if err := row.Scan(&rec.OriginalPath, &rec.Filename, &rec.Reason, &rec.Message, &rec.MtimeMs, &rec.Size, &blockedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	rec.BlockedAt = blockedAt.String
	return &rec, nil
}

// GetAllBlockedFiles is getAllBlockedFiles (database.ts:571-574).
func (s *Store) GetAllBlockedFiles() ([]BlockedFileRecord, error) {
	rows, err := s.db.Query("SELECT " + blockedColumns + " FROM blocked_files ORDER BY blocked_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	recs := []BlockedFileRecord{}
	for rows.Next() {
		var (
			rec       BlockedFileRecord
			blockedAt sql.NullString
		)
		if err := rows.Scan(&rec.OriginalPath, &rec.Filename, &rec.Reason, &rec.Message, &rec.MtimeMs, &rec.Size, &blockedAt); err != nil {
			return nil, err
		}
		rec.BlockedAt = blockedAt.String
		recs = append(recs, rec)
	}
	return recs, rows.Err()
}

// UpsertBlockedFile ports upsertBlockedFile (database.ts:576-597), including the
// ON CONFLICT(original_path) DO UPDATE upsert.
func (s *Store) UpsertBlockedFile(entry BlockedFileEntry) error {
	_, err := s.db.Exec(
		`INSERT INTO blocked_files (original_path, filename, reason, message, mtime_ms, size, blocked_at)
     VALUES (?, ?, ?, ?, ?, ?, ?)
     ON CONFLICT(original_path) DO UPDATE SET
       filename = excluded.filename,
       reason = excluded.reason,
       message = excluded.message,
       mtime_ms = excluded.mtime_ms,
       size = excluded.size,
       blocked_at = excluded.blocked_at`,
		entry.OriginalPath, entry.Filename, entry.Reason, entry.Message, entry.MtimeMs, entry.Size, nowISO(),
	)
	return err
}

// DeleteBlockedFile is deleteBlockedFile (database.ts:599-602).
func (s *Store) DeleteBlockedFile(originalPath string) error {
	_, err := s.db.Exec("DELETE FROM blocked_files WHERE original_path = ?", originalPath)
	return err
}

// PruneBlockedFiles is pruneBlockedFiles (database.ts:604-612).
func (s *Store) PruneBlockedFiles(existingPaths []string) error {
	rows, err := s.db.Query("SELECT original_path FROM blocked_files")
	if err != nil {
		return err
	}
	var all []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return err
		}
		all = append(all, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	existing := make(map[string]bool, len(existingPaths))
	for _, p := range existingPaths {
		existing[p] = true
	}
	for _, p := range all {
		if existing[p] {
			continue
		}
		if _, err := s.db.Exec("DELETE FROM blocked_files WHERE original_path = ?", p); err != nil {
			return err
		}
	}
	return nil
}

// GetCategorySubcategoryStats ports getCategorySubcategoryStats (database.ts:614-645).
func (s *Store) GetCategorySubcategoryStats() (CategoryStats, error) {
	rows, err := s.db.Query(
		`SELECT LOWER(category) as category, LOWER(COALESCE(NULLIF(subcategory, ''), 'general')) as subcategory, COUNT(*) as count 
     FROM documents 
     GROUP BY LOWER(category), LOWER(COALESCE(NULLIF(subcategory, ''), 'general'))`,
	)
	if err != nil {
		return CategoryStats{}, err
	}
	defer rows.Close()

	stats := CategoryStats{
		CategoryCounts:    map[string]int{},
		SubcategoryCounts: map[string]map[string]int{},
	}
	for rows.Next() {
		var category, subcategory string
		var count int
		if err := rows.Scan(&category, &subcategory, &count); err != nil {
			return CategoryStats{}, err
		}
		stats.Total += count
		stats.CategoryCounts[category] += count
		if stats.SubcategoryCounts[category] == nil {
			stats.SubcategoryCounts[category] = map[string]int{}
		}
		stats.SubcategoryCounts[category][subcategory] += count
	}
	return stats, rows.Err()
}
