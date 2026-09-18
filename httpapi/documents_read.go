// Document READ/EXPORT route group (part 2a), ported from pdf-triage's
// src/infrastructure/http/web-server.ts. This file owns the routes that only READ documents and
// export them; the document MUTATION routes (#40 DELETE, #41 PUT, #42 relocalize, #43 clear) are a
// later job and are deliberately not registered here.
//
// Ported routes (inventory 2026-09-18-go-backend-inventory.md §3 row numbers):
//
//	#25 GET    /api/documents                    listDocumentsHandler
//	#26 GET    /api/documents/export/csv         exportCSVHandler
//	#30 GET    /api/mcp/status                   mcpStatusHandler
//	#31 POST   /api/documents/package-zip        packageZipHandler
//	#32 POST   /api/documents/:id/open-folder    openFolderHandler
//	#34 GET    /api/documents/:id/source-image   sourceImageHandler
//	#35 GET    /api/documents/file-by-path       fileByPathHandler
//	#36 GET    /api/documents/:id                getDocumentHandler
//	#37 GET    /api/documents/:id/file           documentFileHandler
//	#38 GET    /api/documents/export/markdown    exportMarkdownHandler
//	#39 GET    /api/documents/:id/markdown       documentMarkdownHandler
//
// The group is built by DocumentReadRoutes(d) and passed to NewServer through Deps.RouteGroups, so
// its collaborators never appear in the package-level Deps. A group that only reads can therefore
// be mounted without the mutation routes' stores.
//
// # TS-vs-Go gaps resolved to match TS
//
//  1. Route order vs ServeMux specificity. Express matches /api/documents/export/markdown BEFORE
//     /api/documents/:id/markdown and /api/documents/export/csv BEFORE /api/documents/:id because
//     it matches in registration order (web-server.ts:1181-1184). Go 1.22's ServeMux matches by
//     specificity instead, and a literal segment is more specific than the {id} wildcard, so both
//     export routes win over the wildcard regardless of registration order. Registering both does
//     not panic (the patterns do not conflict: the literal is a strict subset of the wildcard
//     pattern). TestServeMuxExportVsWildcard pins this.
//  2. app/guards.ResolveManagedPath is the ONE path-boundary implementation (migration design §6).
//     #34 and #35 call it directly; #35's upstream test "returns 404 if file path by path query
//     does not exist" is the known-red 404-vs-403 case, and this port pins the ACTUAL 403 verdict
//     the guard returns for a drive-letter candidate on a non-WSL host (see app/guards/path.go).
//  3. JS String.prototype.substring(0, 800) counts UTF-16 code units, and JS path.extname /
//     String.prototype.trim / encodeURIComponent have no exact stdlib equivalent. truncateRawText
//     and contentDispositionAttachment below reproduce those JS algorithms rather than approximating
//     them.
//  4. #31's docIds entries go through JS parseInt(id, 10) before the DB lookup; jsParseInt reproduces
//     the lenient leading-integer parse (including the NaN-skip the route's `if (isNaN(docId))
//     continue;` performs).
//  5. #34/#35/#37 serve the file through http.ServeFile, which preserves a Content-Type set before
//     the call, exactly as Express's res.setHeader + res.sendFile does. This is the same
//     implementation the existing open* routes use for static files.
//
// # Upstream tests
//
// `npx vitest run src/infrastructure/http/web-server.test.ts` at port time: 53 tests, 51 passed,
// 2 failed. The two red cases:
//
//   - the TOCTOU auto-watcher case (not a document READ route; owned by the later scan job), and
//   - "returns 404 if file path by path query does not exist" (#35), pinned at its actual 403.
//
// Every applicable case for this group's routes is ported in documents_read_test.go; the routes
// with no upstream case (#26, #30, #31, #34) get cases derived from the TS source and pinned as
// goldens where the response is data-only.
package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/app/guards"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
)

// DocumentStore is the narrow SQLite read surface the document READ routes need.
// *database.Store satisfies it.
type DocumentStore interface {
	GetAllDocuments() ([]database.DocumentRecord, error)
	GetDocumentByID(id int64) (*database.DocumentRecord, error)
}

// DocumentOpener is the local launcher seam for #32. infra/osopen is a separate package with its own
// Plan/Runner API, so this group declares the one method it calls and the composition root adapts
// osopen to it; tests inject a fake and never launch a real file manager.
type DocumentOpener interface {
	RevealInFileManager(path string) (Launch, bool)
}

// McpToolInfo is one entry of GET /api/mcp/status's tools array.
type McpToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// McpToolLister is the injected seam behind #30. The real mcpserver package (a separate job) will
// satisfy it; tests inject a fake so httpapi never imports mcpserver.
type McpToolLister interface {
	ListTools() ([]McpToolInfo, error)
}

// DocumentReadDeps is the collaborator set for the document READ/EXPORT route group. Settings is the
// same narrow interface NewServer's own routes use; Opener and Spawner are the same local seams as
// the open-location/open-chrome routes (osopen adapts to them at wiring time).
type DocumentReadDeps struct {
	Documents DocumentStore
	Settings  Settings
	Opener    DocumentOpener
	Spawner   Spawner
	Mcp       McpToolLister
}

// DocumentReadRoutes returns the RouteGroup that registers every document READ/EXPORT route. It is
// passed to NewServer through Deps.RouteGroups so this group's collaborators stay out of Deps.
func DocumentReadRoutes(d DocumentReadDeps) RouteGroup {
	h := &documentReadHandler{deps: d}
	return func(s *Server) {
		// #25
		s.mux.HandleFunc("GET /api/documents", h.listDocuments)
		// #26 — literal, resolved before /api/documents/{id} by ServeMux specificity.
		s.mux.HandleFunc("GET /api/documents/export/csv", h.exportCSV)
		// #30
		s.mux.HandleFunc("GET /api/mcp/status", h.mcpStatus)
		// #31
		s.mux.HandleFunc("POST /api/documents/package-zip", h.packageZip)
		// #32
		s.mux.HandleFunc("POST /api/documents/{id}/open-folder", h.openFolder)
		// #34
		s.mux.HandleFunc("GET /api/documents/{id}/source-image", h.sourceImage)
		// #35 — literal, resolved before /api/documents/{id}.
		s.mux.HandleFunc("GET /api/documents/file-by-path", h.fileByPath)
		// #36
		s.mux.HandleFunc("GET /api/documents/{id}", h.getDocument)
		// #37
		s.mux.HandleFunc("GET /api/documents/{id}/file", h.documentFile)
		// #38 — literal, resolved before /api/documents/{id}/markdown. TS registers it before
		// :id/markdown on purpose (web-server.ts:1181-1184); ServeMux's specificity makes the order
		// irrelevant, and TestServeMuxExportVsWildcard proves both still resolve.
		s.mux.HandleFunc("GET /api/documents/export/markdown", h.exportMarkdown)
		// #39
		s.mux.HandleFunc("GET /api/documents/{id}/markdown", h.documentMarkdown)
	}
}

// documentReadHandler carries the group's collaborators. Handlers are methods so the registration
// closure above stays a pure table of method+path -> method.
type documentReadHandler struct {
	deps DocumentReadDeps
}

// resolveManagedPath wraps app/guards.ResolveManagedPath with this server's managed roots.
func (h *documentReadHandler) resolveManagedPath(candidate string) (string, *guards.GuardViolation) {
	cfg := h.deps.Settings.Config()
	return guards.ResolveManagedPath(candidate, cfg.InputDir, cfg.OutputRootDir)
}

// listDocuments ports GET /api/documents (web-server.ts:703-763).
func (h *documentReadHandler) listDocuments(w http.ResponseWriter, r *http.Request) {
	docs, err := h.deps.Documents.GetAllDocuments()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	query := parseDocumentQuery(r)
	formatted := make([]map[string]any, 0, len(docs))
	for _, doc := range docs {
		if !documentMatches(doc, query, true) {
			continue
		}
		formatted = append(formatted, map[string]any{
			"id":                doc.ID,
			"checksum":          doc.Checksum,
			"title":             doc.Title,
			"registre":          doc.Registre,
			"date":              doc.Date,
			"category":          doc.Category,
			"subcategory":       subcategoryOrDefault(doc.Subcategory),
			"summary":           doc.Summary,
			"tags":              safeParseJSONArray(doc.Tags),
			"raw_text":          truncateRawText(doc.RawText),
			"markdown_content":  doc.MarkdownContent,
			"total_amount":      doc.TotalAmount,
			"vat_amount":        doc.VatAmount,
			"siren":             doc.Siren,
			"iban":              doc.Iban,
			"expiry_date":       doc.ExpiryDate,
			"contact_name":      doc.ContactName,
			"contact_email":     doc.ContactEmail,
			"contact_phone":     doc.ContactPhone,
			"contact_address":   doc.ContactAddress,
			"contact_website":   doc.ContactWebsite,
			"original_filename": doc.OriginalFilename,
			"original_path":     doc.OriginalPath,
			"new_path":          doc.NewPath,
			"status":            doc.Status,
			"created_at":        doc.CreatedAt,
			"updated_at":        doc.UpdatedAt,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total":     len(formatted),
		"documents": formatted,
	})
}

// documentQuery is the lowercased ?q&category&subcategory triple both list routes read.
type documentQuery struct {
	search      string
	category    string
	subcategory string
}

func parseDocumentQuery(r *http.Request) documentQuery {
	q := r.URL.Query()
	return documentQuery{
		search:      strings.ToLower(q.Get("q")),
		category:    strings.ToLower(q.Get("category")),
		subcategory: strings.ToLower(q.Get("subcategory")),
	}
}

// documentMatches is the shared JS filter. extended selects #25's larger search surface
// (original_path, new_path, raw_text) over #26's narrower one.
func documentMatches(doc database.DocumentRecord, q documentQuery, extended bool) bool {
	if q.category != "" && strings.ToLower(doc.Category) != q.category {
		return false
	}
	if q.subcategory != "" && (doc.Subcategory == "" || strings.ToLower(doc.Subcategory) != q.subcategory) {
		return false
	}
	if q.search == "" {
		return true
	}
	fields := make([]string, 0, 11)
	if extended {
		fields = append(fields, doc.OriginalPath, doc.NewPath)
	}
	fields = append(fields,
		doc.Title,
		doc.OriginalFilename,
		doc.Summary,
		doc.Registre,
		doc.Subcategory,
		doc.ContactName,
		doc.ContactEmail,
		doc.Tags,
	)
	if extended {
		fields = append(fields, doc.RawText)
	}
	for _, field := range fields {
		if strings.Contains(strings.ToLower(field), q.search) {
			return true
		}
	}
	return false
}

// subcategoryOrDefault is `doc.subcategory || 'general'`.
func subcategoryOrDefault(subcategory string) string {
	if subcategory == "" {
		return "general"
	}
	return subcategory
}

// safeParseJSONArray is `safeParseJSON(doc.tags, [])`: JSON.parse with an empty-array fallback.
func safeParseJSONArray(raw string) any {
	if raw == "" {
		return []any{}
	}
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return []any{}
	}
	return parsed
}

// truncateRawText is `(doc.raw_text || ”).substring(0, 800)` with JS UTF-16 code-unit counting.
func truncateRawText(raw string) string {
	if utf16Len(raw) <= 800 {
		return raw
	}
	return utf16Slice(raw, 800)
}

// utf16Len is JavaScript's `str.length`: the number of UTF-16 code units, not runes.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// utf16Slice reproduces `str.substring(0, n)`: the first n UTF-16 code units. A rune whose code
// units would straddle the cut is dropped (JS would keep a lone surrogate, which a Go UTF-8 string
// cannot represent).
func utf16Slice(s string, n int) string {
	if n <= 0 {
		return ""
	}
	units := 0
	for i, r := range s {
		width := 1
		if r > 0xFFFF {
			width = 2
		}
		if units+width > n {
			return s[:i]
		}
		units += width
	}
	return s
}

// getDocument ports GET /api/documents/:id (web-server.ts:1141-1156): the full stored record with
// `tags` replaced by its parsed JSON value.
func (h *documentReadHandler) getDocument(w http.ResponseWriter, r *http.Request) {
	doc, err := h.deps.Documents.GetDocumentByID(parseDocumentID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if doc == nil {
		writeError(w, http.StatusNotFound, "Document not found")
		return
	}
	writeJSON(w, http.StatusOK, documentReadWithTags{DocumentRecord: *doc, Tags: safeParseJSONArray(doc.Tags)})
}

// documentReadWithTags is `{ ...doc, tags: safeParseJSON(doc.tags, []) }`. The outer Tags field is
// shallower than DocumentRecord's embedded Tags, so encoding/json emits the parsed value. (Named
// distinctly from documents_write.go's documentWithTags map builder so the two route groups can
// coexist in one package.)
type documentReadWithTags struct {
	database.DocumentRecord
	Tags any `json:"tags"`
}

// fileByPath ports GET /api/documents/file-by-path (web-server.ts:1112-1138).
func (h *documentReadHandler) fileByPath(w http.ResponseWriter, r *http.Request) {
	targetPath := r.URL.Query().Get("path")
	if targetPath == "" {
		writeError(w, http.StatusNotFound, "PDF file missing on disk")
		return
	}

	absPath, violation := h.resolveManagedPath(targetPath)
	if violation != nil {
		writeError(w, violation.HTTPStatus, violation.Message)
		return
	}
	if _, err := os.Stat(absPath); err != nil {
		writeError(w, http.StatusNotFound, "PDF file missing on disk")
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", "inline")
	http.ServeFile(w, r, absPath)
}

// documentFile ports GET /api/documents/:id/file (web-server.ts:1159-1179). The path is taken from
// new_path || original_path exactly as TS does; this route intentionally has no managed-path guard in
// the TS source.
func (h *documentReadHandler) documentFile(w http.ResponseWriter, r *http.Request) {
	doc, err := h.deps.Documents.GetDocumentByID(parseDocumentID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if doc == nil {
		writeError(w, http.StatusNotFound, "Document not found")
		return
	}

	targetPath := doc.NewPath
	if targetPath == "" {
		targetPath = doc.OriginalPath
	}
	if targetPath == "" {
		writeError(w, http.StatusNotFound, "PDF file missing on disk")
		return
	}
	if _, err := os.Stat(targetPath); err != nil {
		writeError(w, http.StatusNotFound, "PDF file missing on disk")
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", "inline")
	http.ServeFile(w, r, filepath.Clean(targetPath))
}

// sourceImage ports GET /api/documents/:id/source-image (web-server.ts:1080-1109).
func (h *documentReadHandler) sourceImage(w http.ResponseWriter, r *http.Request) {
	doc, err := h.deps.Documents.GetDocumentByID(parseDocumentID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if doc == nil {
		writeError(w, http.StatusNotFound, "Document not found")
		return
	}
	if doc.SourceImagePath == "" {
		writeError(w, http.StatusNotFound, "This document has no retained source image.")
		return
	}

	absPath, violation := h.resolveManagedPath(doc.SourceImagePath)
	if violation != nil {
		writeError(w, http.StatusForbidden, "Source image is outside the managed directories.")
		return
	}
	if _, err := os.Stat(absPath); err != nil {
		writeError(w, http.StatusNotFound, "Source image is no longer on disk.")
		return
	}

	w.Header().Set("Content-Type", sourceImageMIME(absPath))
	http.ServeFile(w, r, absPath)
}

// sourceImageMIME is the TS extension table (web-server.ts:1098-1104).
func sourceImageMIME(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".tiff", ".tif":
		return "image/tiff"
	default:
		return "image/jpeg"
	}
}

// openFolder ports POST /api/documents/:id/open-folder (web-server.ts:1001-1026).
func (h *documentReadHandler) openFolder(w http.ResponseWriter, r *http.Request) {
	doc, err := h.deps.Documents.GetDocumentByID(parseDocumentID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if doc == nil {
		writeError(w, http.StatusNotFound, "Document not found")
		return
	}

	fileOnDisk := h.findActualFileOnDisk(doc)
	if fileOnDisk == "" {
		writeError(w, http.StatusNotFound, "Document file not found on disk")
		return
	}
	if _, err := os.Stat(fileOnDisk); err != nil {
		writeError(w, http.StatusNotFound, "Document file not found on disk")
		return
	}

	if h.deps.Opener != nil {
		if launch, ok := h.deps.Opener.RevealInFileManager(fileOnDisk); ok {
			if h.deps.Spawner != nil {
				h.deps.Spawner.Spawn(launch.Cmd, launch.Args)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"message":  "Opened folder in OS file manager",
		"filePath": fileOnDisk,
	})
}

// findActualFileOnDisk delegates to the shared guard (migration design §6).
func (h *documentReadHandler) findActualFileOnDisk(doc *database.DocumentRecord) string {
	cfg := h.deps.Settings.Config()
	return guards.FindActualFileOnDisk(guards.DocumentLocation{
		NewPath:          doc.NewPath,
		OriginalPath:     doc.OriginalPath,
		OriginalFilename: doc.OriginalFilename,
	}, cfg.InputDir, cfg.OutputRootDir)
}

// mcpStatus ports GET /api/mcp/status (web-server.ts:944-957).
func (h *documentReadHandler) mcpStatus(w http.ResponseWriter, r *http.Request) {
	if h.deps.Mcp == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"status":    "error",
			"connected": false,
			"error":     "MCP tool lister is not configured",
		})
		return
	}
	tools, err := h.deps.Mcp.ListTools()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"status":    "error",
			"connected": false,
			"error":     err.Error(),
		})
		return
	}
	views := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		views = append(views, map[string]any{"name": tool.Name, "description": tool.Description})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "active",
		"connected":  true,
		"toolsCount": len(tools),
		"tools":      views,
	})
}

// parseDocumentID is `parseInt(req.params.id, 10)` for a Go path value. A non-numeric id yields 0,
// for which GetDocumentByID returns no row, so the route answers 404 exactly as JS did for NaN.
func parseDocumentID(r *http.Request) int64 {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// jsParseInt reproduces `parseInt(value, 10)` for the JSON shapes #31 can receive: a float64 or a
// string. Strings are parsed leniently (leading whitespace, optional sign, leading digits), and a
// value with no leading digits is missing (NaN -> the caller skips it).
func jsParseInt(value any) (int64, bool) {
	var text string
	switch v := value.(type) {
	case float64:
		text = strconv.FormatFloat(v, 'f', -1, 64)
	case string:
		text = strings.TrimSpace(v)
	default:
		return 0, false
	}
	text = strings.TrimSpace(text)
	sign := int64(1)
	if strings.HasPrefix(text, "-") {
		sign = -1
		text = text[1:]
	} else if strings.HasPrefix(text, "+") {
		text = text[1:]
	}
	end := 0
	for end < len(text) && text[end] >= '0' && text[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(text[:end], 10, 64)
	if err != nil {
		// Overflowing digit run: JS parseInt yields an imprecise float; the ID can never match a
		// row, so treat it as missing rather than wrapping.
		return 0, false
	}
	return sign * n, true
}

// documentExportTimestamp is `new Date().toISOString().slice(0, 10)`.
func documentExportTimestamp() string {
	return time.Now().UTC().Format("2006-01-02")
}

// forbiddenFilenameChars is the TS `[/\\?%*:|"<>]` character class.
var forbiddenFilenameChars = regexp.MustCompile(`[/\\?%*:|"<>]`)

// sanitizeTitle is `.replace(/[/\\?%*:|"<>]/g, '_')`.
func sanitizeTitle(title string) string {
	return forbiddenFilenameChars.ReplaceAllString(title, "_")
}

// zipNameSafeChars is the TS `[^a-zA-Z0-9_.-]` character class.
var zipNameSafeChars = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

// sanitizeZipName is `.replace(/[^a-zA-Z0-9_.-]/g, '_')`.
func sanitizeZipName(name string) string {
	return zipNameSafeChars.ReplaceAllString(name, "_")
}

// encodeURIComponent is a literal port of JavaScript's encodeURIComponent, used for the RFC 6266
// filename* parameter. Go's url.QueryEscape encodes space as '+' and url.PathEscape leaves
// delimiters such as '+' unescaped, so neither matches the browser contract.
func encodeURIComponent(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z',
			c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '!', c == '~', c == '*', c == '\'', c == '(', c == ')':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

// contentDispositionAttachment is the TS contentDispositionAttachment (web-server.ts:1532-1535):
// an ASCII-safe fallback plus an RFC 6266 filename* carrying the real (possibly accented) name.
func contentDispositionAttachment(filename string) string {
	var fallback strings.Builder
	for _, r := range filename {
		switch {
		case r >= 0x20 && r <= 0x7E && r == '"':
			fallback.WriteByte('\'')
		case r >= 0x20 && r <= 0x7E:
			fallback.WriteRune(r)
		case r > 0xFFFF:
			// The JS regex replaces per UTF-16 code unit, so an astral rune yields two underscores.
			fallback.WriteString("__")
		default:
			fallback.WriteByte('_')
		}
	}
	return `attachment; filename="` + fallback.String() + `"; filename*=UTF-8''` + encodeURIComponent(filename)
}
