// Package jsonregistry is a Go port of pdf-triage's src/infrastructure/json-registry.ts (73 lines):
// the JSONRegistryEntry mirror shape and syncJSONRegistry.
//
// The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/json-registry.test.ts` -> 4 passed), so no upstream case is
// pinned red. All four cases are ported, plus the EPERM/EBUSY fallback and its classifier, which the
// TS suite never covered.
//
// The SQLite layer (getAllDocuments / DocumentRecord) is a later migration phase, so the document
// source is taken as an injected function parameter instead of importing the database. The settings
// port is also later, so the registry path is an explicit parameter rather than CONFIG.
//
// Deviations, all resolved in favor of matching the TS acceptance bar:
//
//  1. EPERM/EBUSY detection. TS checks `e.code === 'EPERM' || e.code === 'EBUSY'`; Go matches the
//     wrapped os error with errors.Is against syscall.EPERM / syscall.EBUSY (plus fs.ErrPermission),
//     and on Windows additionally the raw Win32 codes ERROR_ACCESS_DENIED (5),
//     ERROR_SHARING_VIOLATION (32) and ERROR_LOCK_VIOLATION (33) that libuv/Node translates to
//     EPERM/EBUSY. See atomic_unix.go / atomic_windows.go.
//  2. `updated_at`. JS Date.toISOString() always renders exactly three fractional digits; Go's
//     time.RFC3339Nano trims trailing zeros, so the timestamp is formatted explicitly as
//     "2006-01-02T15:04:05.000Z" in UTC.
//  3. `subcategory: doc.subcategory || 'general'` maps the empty string (and any JS-falsy value) to
//     "general"; Go's zero value "" is the equivalent input.
//  4. safeParseJSON mirrors JSON.parse-or-fallback: the parsed value is kept as any (so a valid JSON
//     scalar/object survives round-trip, exactly as in TS) and any parse error yields a non-nil
//     empty []any, so the emitted tags field is [] and never null.
//  5. JSON.stringify(data, null, 2) does not escape HTML; Go's encoding/json escapes <, > and & by
//     default, so the writer disables HTML escaping and strips the Encoder's trailing newline.
//  6. On a non-EPERM/EBUSY rename failure TS throws and leaves the .tmp file behind; this port does
//     the same (it returns the error without unlinking). On the fallback path the copy failure is
//     returned and the .tmp file is likewise left behind, matching the TS ordering.
package jsonregistry

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"time"
)

// DocumentRecord mirrors the subset of pdf-triage's `DocumentRecord` that this module reads. Tags is
// the stored JSON text column, not a decoded array, exactly as getAllDocuments() returns it.
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
	SourceImagePath  string `json:"source_image_path"`
	Embedding        string `json:"embedding"`
	Status           string `json:"status"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

// JSONRegistryEntry mirrors the TS `JSONRegistryEntry` interface. Field order matches the TS map
// order so the emitted JSON keys come out in the same order.
type JSONRegistryEntry struct {
	ID               int64  `json:"id"`
	Checksum         string `json:"checksum"`
	Title            string `json:"title"`
	Registre         string `json:"registre"`
	Date             string `json:"date"`
	Category         string `json:"category"`
	Subcategory      string `json:"subcategory"`
	Summary          string `json:"summary"`
	Tags             any    `json:"tags"`
	OriginalFilename string `json:"original_filename"`
	OriginalPath     string `json:"original_path"`
	NewPath          string `json:"new_path"`
	Status           string `json:"status"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

// registryData mirrors the anonymous object syncJSONRegistry serializes.
type registryData struct {
	UpdatedAt  string              `json:"updated_at"`
	TotalCount int                 `json:"total_count"`
	Documents  []JSONRegistryEntry `json:"documents"`
}

// DocumentSource is the injected replacement for TS's `getAllDocuments()`. The SQLite port will pass
// its own query function.
type DocumentSource func() ([]DocumentRecord, error)

// renameFile is a seam for the fallback test; production uses os.Rename.
var renameFile = os.Rename

// SyncJSONRegistry is `syncJSONRegistry()`. The registry path replaces CONFIG.JSON_REGISTRY_PATH.
func SyncJSONRegistry(registryPath string, source DocumentSource) error {
	docs, err := source()
	if err != nil {
		return err
	}

	entries := make([]JSONRegistryEntry, 0, len(docs))
	for _, doc := range docs {
		subcategory := doc.Subcategory
		if subcategory == "" {
			subcategory = "general"
		}
		entries = append(entries, JSONRegistryEntry{
			ID:               doc.ID,
			Checksum:         doc.Checksum,
			Title:            doc.Title,
			Registre:         doc.Registre,
			Date:             doc.Date,
			Category:         doc.Category,
			Subcategory:      subcategory,
			Summary:          doc.Summary,
			Tags:             safeParseJSON(doc.Tags),
			OriginalFilename: doc.OriginalFilename,
			OriginalPath:     doc.OriginalPath,
			NewPath:          doc.NewPath,
			Status:           doc.Status,
			CreatedAt:        doc.CreatedAt,
			UpdatedAt:        doc.UpdatedAt,
		})
	}

	data := registryData{
		UpdatedAt:  time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		TotalCount: len(entries),
		Documents:  entries,
	}

	payload, err := marshalIndentNoHTMLEscape(data)
	if err != nil {
		return err
	}

	// Write to a temp file then rename over the target - a plain write can be interrupted mid-write
	// (process kill, crash), leaving a truncated/corrupted registry.json; rename is atomic on the
	// same filesystem. The comment is preserved from the TS source.
	tmpPath := registryPath + ".tmp"
	if err := os.WriteFile(tmpPath, payload, 0o644); err != nil {
		return err
	}
	if err := renameFile(tmpPath, registryPath); err != nil {
		if isPermOrBusy(err) {
			if copyErr := copyFileContents(tmpPath, registryPath); copyErr != nil {
				return copyErr
			}
			_ = os.Remove(tmpPath)
		} else {
			return err
		}
	}
	return nil
}

// safeParseJSON is `JSON.parse(str)` with the `[]` fallback. The parsed value is any; a parse error
// returns a non-nil empty array so the JSON output is [] rather than null.
func safeParseJSON(s string) any {
	var parsed any
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return []any{}
	}
	return parsed
}

// marshalIndentNoHTMLEscape is `JSON.stringify(data, null, 2)`: two-space indent and no HTML
// escaping (Go escapes <, > and & by default). The Encoder appends a newline that stringify does
// not, so it is trimmed.
func marshalIndentNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// copyFileContents is the fallback replacement for fs.copyFileSync.
func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
