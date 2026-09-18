// Document READ/EXPORT route group (part 2a): the export half. Owns GET /api/documents/export/csv
// (#26), POST /api/documents/package-zip (#31), GET /api/documents/export/markdown (#38) and
// GET /api/documents/:id/markdown (#39). The list/detail/file routes live in documents_read.go.
//
// The CSV writer (UTF-8 BOM, exact quoting, CRLF row separator, no trailing newline) and the
// RFC 6266 Content-Disposition encoding are both literal ports of the TypeScript algorithms; see
// documents_read.go for the shared helpers.
package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/phamhung075/pdf-triage-pdf2w/infra/zipbuilder"
)

// csvHeaderRow is the exact header the TS route emits (web-server.ts:790).
var csvHeaderRow = []string{
	"ID", "Checksum", "Title", "Category", "Subcategory", "Date", "Expiry Date",
	"Total Amount", "VAT Amount", "SIREN", "IBAN", "Contact Name", "Contact Email",
	"Contact Phone", "Contact Address", "Contact Website", "Reference", "Status",
	"Summary", "Original Filename", "New Path",
}

// exportCSV ports GET /api/documents/export/csv (web-server.ts:766-828).
func (h *documentReadHandler) exportCSV(w http.ResponseWriter, r *http.Request) {
	docs, err := h.deps.Documents.GetAllDocuments()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	query := parseDocumentQuery(r)
	rows := make([]string, 0, len(docs)+1)
	rows = append(rows, encodeCSVRow(csvHeaderRow))
	for _, doc := range docs {
		if !documentMatches(doc, query, false) {
			continue
		}
		rows = append(rows, encodeCSVRow([]string{
			strconv.FormatInt(doc.ID, 10),
			doc.Checksum,
			doc.Title,
			doc.Category,
			doc.Subcategory,
			doc.Date,
			doc.ExpiryDate,
			doc.TotalAmount,
			doc.VatAmount,
			doc.Siren,
			doc.Iban,
			doc.ContactName,
			doc.ContactEmail,
			doc.ContactPhone,
			doc.ContactAddress,
			doc.ContactWebsite,
			doc.Registre,
			doc.Status,
			replaceNewlines(doc.Summary),
			doc.OriginalFilename,
			firstNonEmpty(doc.NewPath, doc.OriginalPath),
		}))
	}

	// TS: '\uFEFF' + rows.map(escape).join('\r\n'), with no trailing newline.
	content := "\uFEFF" + strings.Join(rows, "\r\n")

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="smart_pdf_triage_export_`+documentExportTimestamp()+`.csv"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(content))
}

// encodeCSVRow is `row.map(v => `"${v.replace(/"/g,'""')}"`).join(',')`.
func encodeCSVRow(cells []string) string {
	escaped := make([]string, 0, len(cells))
	for _, cell := range cells {
		escaped = append(escaped, escapeCSVCell(cell))
	}
	return strings.Join(escaped, ",")
}

// escapeCSVCell is `"${val.replace(/"/g, '""')}"`.
func escapeCSVCell(val string) string {
	return `"` + strings.ReplaceAll(val, `"`, `""`) + `"`
}

// replaceNewlines is `(summary || ”).replace(/\r?\n/g, ' ')`. A lone CR is left untouched, exactly
// as the regex does.
func replaceNewlines(summary string) string {
	summary = strings.ReplaceAll(summary, "\r\n", " ")
	return strings.ReplaceAll(summary, "\n", " ")
}

// packageZip ports POST /api/documents/package-zip (web-server.ts:960-998).
func (h *documentReadHandler) packageZip(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DocIDs  []any   `json:"docIds"`
		ZipName *string `json:"zipName"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.DocIDs) == 0 {
		writeError(w, http.StatusBadRequest, "docIds array is required and cannot be empty.")
		return
	}

	entries := make([]zipbuilder.ZipFileEntry, 0, len(body.DocIDs))
	for _, rawID := range body.DocIDs {
		docID, ok := jsParseInt(rawID)
		if !ok {
			continue
		}
		doc, err := h.deps.Documents.GetDocumentByID(docID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if doc == nil {
			continue
		}
		fileOnDisk := h.findActualFileOnDisk(doc)
		if fileOnDisk == "" {
			continue
		}
		if _, err := os.Stat(fileOnDisk); err != nil {
			continue
		}

		ext := filepath.Ext(fileOnDisk)
		if ext == "" {
			ext = ".pdf"
		}
		baseTitle := firstNonEmpty(doc.Title, doc.OriginalFilename, "doc_"+strconv.FormatInt(doc.ID, 10))
		baseTitle = sanitizeTitle(baseTitle)
		fileNameInZip := baseTitle
		if !strings.HasSuffix(baseTitle, ext) {
			fileNameInZip = baseTitle + ext
		}
		entries = append(entries, zipbuilder.ZipFileEntry{Name: fileNameInZip, Path: fileOnDisk})
	}

	if len(entries) == 0 {
		writeError(w, http.StatusNotFound, "None of the requested document files exist on disk.")
		return
	}

	buffer, err := zipbuilder.CreateZipArchive(entries)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	downloadName := "dossier_documents_package.zip"
	if body.ZipName != nil && *body.ZipName != "" {
		downloadName = *body.ZipName
	}
	downloadName = sanitizeZipName(downloadName)

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+downloadName+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buffer)
}

// exportMarkdown ports GET /api/documents/export/markdown (web-server.ts:1185-1213).
func (h *documentReadHandler) exportMarkdown(w http.ResponseWriter, r *http.Request) {
	docs, err := h.deps.Documents.GetAllDocuments()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	usedNames := map[string]bool{}
	entries := make([]zipbuilder.ZipFileEntry, 0, len(docs))
	for _, doc := range docs {
		content := firstNonEmpty(doc.MarkdownContent, doc.RawText)
		baseTitle := sanitizeTitle(firstNonEmpty(doc.Title, doc.OriginalFilename, "document_"+strconv.FormatInt(doc.ID, 10)))
		fileName := baseTitle + ".md"
		// Multiple documents can share the same title (e.g. two "Accusé de réception" scans) —
		// dedupe by suffixing the doc id rather than silently overwriting one entry in the zip.
		if usedNames[fileName] {
			fileName = baseTitle + "_" + strconv.FormatInt(doc.ID, 10) + ".md"
		}
		usedNames[fileName] = true
		entries = append(entries, zipbuilder.ZipFileEntry{Name: fileName, Content: append([]byte{}, content...)})
	}

	buffer, err := zipbuilder.CreateZipArchive(entries)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	downloadName := "smart_pdf_triage_markdown_export_" + documentExportTimestamp() + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+downloadName+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buffer)
}

// documentMarkdown ports GET /api/documents/:id/markdown (web-server.ts:1216-1233).
func (h *documentReadHandler) documentMarkdown(w http.ResponseWriter, r *http.Request) {
	doc, err := h.deps.Documents.GetDocumentByID(parseDocumentID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if doc == nil {
		writeError(w, http.StatusNotFound, "Document not found")
		return
	}

	content := firstNonEmpty(doc.MarkdownContent, doc.RawText)
	baseTitle := sanitizeTitle(firstNonEmpty(doc.Title, doc.OriginalFilename, "document_"+strconv.FormatInt(doc.ID, 10)))

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", contentDispositionAttachment(baseTitle+".md"))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(content))
}

// firstNonEmpty is the JS `a || b || c` chain for the fields these routes read.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
