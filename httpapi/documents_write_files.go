// File-utility and chat routes #27 (image import), #28 (PDF merge), #29 (chat), #33 (PDF split),
// ported from web-server.ts:843-941 and :1028-1071.
//
// #27 writes the uploaded bytes into CONFIG.InputDir with exclusive-create ('wx') semantics, so an
// import can never overwrite a file already waiting in __raws; #28/#33 use github.com/pdfcpu/pdfcpu
// (the module's PDF engine) and are path-guarded through app/guards.ResolveManagedPath before any
// read or write, so a caller cannot turn the server into a file-copy primitive for arbitrary paths.
// #33 writes one file per page into CONFIG.InputDir, where the auto-watcher then triages them.
package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/phamhung075/pdf-triage-pdf2w/app/aichat"
)

// maxImportBytes is express.raw({limit:'64mb'}). It bounds the handler's own read.
const maxImportBytes = 64 * 1024 * 1024

// importableImageExtensions mirrors IMAGE_EXTENSIONS in application/convert-image-document.ts
// (web-server.ts:51). The order is the TS Set's insertion order, which the 400 message prints.
var importableImageExtensions = []string{".png", ".jpg", ".jpeg", ".webp", ".bmp", ".tiff"}

var (
	importNameSanitizer = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	importLeadingTrim   = regexp.MustCompile(`^[._-]+`)
	pdfOutputSanitizer  = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)
)

func init() {
	// Embedded-service policy: use pdfcpu's built-in defaults and never read/write a user
	// config directory or call os.Exit from a config problem. No other module in this binary uses
	// pdfcpu, so the global does not change behavior elsewhere.
	model.ConfigPath = "disable"
}

func pdfConfiguration() *model.Configuration {
	return model.NewDefaultConfiguration()
}

// --- #27 POST /api/images/import -----------------------------------------------------------------

func (s *server) importImageHandler(d DocumentWriteDeps, w http.ResponseWriter, r *http.Request) {
	body, status, message := readImportBody(r)
	if status != 0 {
		writeError(w, status, message)
		return
	}

	// basename() then a strict slug: the name comes from the browser and lands in a path.join.
	safeName := sanitizeImportFilename(r.URL.Query().Get("filename"))
	ext := strings.ToLower(path.Ext(safeName))

	if !isImportableImageExt(ext) {
		showExt := ext
		if showExt == "" {
			showExt = "(none)"
		}
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"Unsupported image type '%s'. Accepted: %s", showExt, strings.Join(importableImageExtensions, ", ")))
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "Empty image body.")
		return
	}

	if err := d.ensureDirectories(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	cfg := d.config()
	base := strings.TrimSuffix(safeName, ext)
	if base == "" {
		base = "image"
	}

	target := filepath.Join(cfg.InputDir, base+ext)
	for attempt := 1; ; attempt++ {
		file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			_, writeErr := file.Write(body)
			closeErr := file.Close()
			if writeErr != nil {
				writeError(w, http.StatusInternalServerError, writeErr.Error())
				return
			}
			if closeErr != nil {
				writeError(w, http.StatusInternalServerError, closeErr.Error())
				return
			}
			break
		}
		if !os.IsExist(err) {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if attempt > 50 {
			writeError(w, http.StatusConflict, "Could not find a free filename in the incoming folder.")
			return
		}
		target = filepath.Join(cfg.InputDir, fmt.Sprintf("%s_%d%s", base, attempt, ext))
	}

	d.logInfo("IMPORT", "Imported image into the incoming folder", map[string]any{"target": target, "bytes": len(body)})
	writeJSON(w, http.StatusOK, map[string]any{
		"message":  "Image imported",
		"path":     target,
		"filename": path.Base(target),
	})
}

// readImportBody returns the raw upload body. The composed handler already read it (≤100 kB) into
// the body context, so that value is preferred when present; otherwise the request body is read
// directly, which is how a direct handler call (and the 64 MB unit test) reaches it. See package
// comment gap 1: the composed 100 kB middleware cap is the reported limitation.
func readImportBody(r *http.Request) (data []byte, status int, message string) {
	if raw, ok := r.Context().Value(bodyCtxKey{}).([]byte); ok {
		if int64(len(raw)) > maxImportBytes {
			return nil, http.StatusRequestEntityTooLarge, "request entity too large"
		}
		return raw, 0, ""
	}
	if r.Body == nil {
		return nil, 0, ""
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxImportBytes+1))
	if err != nil {
		return nil, http.StatusInternalServerError, err.Error()
	}
	if int64(len(data)) > maxImportBytes {
		return nil, http.StatusRequestEntityTooLarge, "request entity too large"
	}
	return data, 0, ""
}

// sanitizeImportFilename is `path.basename(raw).replace(/[^A-Za-z0-9._-]+/g, '_').replace(/^[._-]+/, ”)`.
func sanitizeImportFilename(raw string) string {
	name := importNameSanitizer.ReplaceAllString(path.Base(raw), "_")
	return importLeadingTrim.ReplaceAllString(name, "")
}

func isImportableImageExt(ext string) bool {
	for _, candidate := range importableImageExtensions {
		if candidate == ext {
			return true
		}
	}
	return false
}

// --- #28 POST /api/pdf/merge ---------------------------------------------------------------------

func (s *server) mergePDFHandler(d DocumentWriteDeps, w http.ResponseWriter, r *http.Request) {
	body := struct {
		Filepaths      []string `json:"filepaths"`
		OutputFilename string   `json:"outputFilename"`
	}{}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.Filepaths) < 2 {
		writeError(w, http.StatusBadRequest, "At least 2 PDF filepaths are required for merging.")
		return
	}

	absPaths := make([]string, 0, len(body.Filepaths))
	for _, candidate := range body.Filepaths {
		abs, violation := s.resolveManagedPath(candidate)
		if violation != nil {
			writeError(w, http.StatusForbidden, "Path is outside the managed input/output directories — not allowed.")
			return
		}
		if !fileExistsOnDisk(abs) {
			writeError(w, http.StatusNotFound, "PDF file not found on disk: "+candidate)
			return
		}
		absPaths = append(absPaths, abs)
	}

	name := pdfOutputSanitizer.ReplaceAllString(body.OutputFilename, "_")
	if name == "" {
		name = fmt.Sprintf("merged_%d.pdf", time.Now().UnixMilli())
	}
	if !strings.HasSuffix(name, ".pdf") {
		name += ".pdf"
	}
	cfg := d.config()
	targetPath := filepath.Join(cfg.InputDir, name)

	if err := api.MergeCreateFile(absPaths, targetPath, false, pdfConfiguration()); err != nil {
		writeError(w, http.StatusInternalServerError, "PDF merge failed: "+err.Error())
		return
	}

	d.logInfo("PDF_UTIL", fmt.Sprintf("Merged %d PDFs into '%s'", len(body.Filepaths), targetPath), nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"message":    fmt.Sprintf("Successfully merged %d PDF files into %s", len(body.Filepaths), path.Base(targetPath)),
		"targetPath": targetPath,
	})
}

// --- #33 POST /api/pdf/split ---------------------------------------------------------------------

func (s *server) splitPDFHandler(d DocumentWriteDeps, w http.ResponseWriter, r *http.Request) {
	body := struct {
		Filepath string `json:"filepath"`
	}{}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	abs, violation := s.resolveManagedPath(body.Filepath)
	if body.Filepath != "" && violation != nil {
		writeError(w, http.StatusForbidden, "Path is outside the managed input/output directories — not allowed.")
		return
	}
	if violation != nil || abs == "" || !fileExistsOnDisk(abs) {
		writeError(w, http.StatusBadRequest, "Valid PDF filepath is required.")
		return
	}

	pageCount, err := api.PageCountFile(abs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "PDF split failed: "+err.Error())
		return
	}
	if pageCount <= 1 {
		writeError(w, http.StatusBadRequest, "PDF only has 1 page; splitting requires a multi-page PDF.")
		return
	}

	stem := strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
	cfg := d.config()
	createdFiles := make([]string, 0, pageCount)
	conf := pdfConfiguration()
	for i := 0; i < pageCount; i++ {
		singleName := fmt.Sprintf("%s_page_%d.pdf", stem, i+1)
		singlePath := filepath.Join(cfg.InputDir, singleName)
		if err := api.TrimFile(abs, singlePath, []string{strconv.Itoa(i + 1)}, conf); err != nil {
			writeError(w, http.StatusInternalServerError, "PDF split failed: "+err.Error())
			return
		}
		createdFiles = append(createdFiles, singleName)
	}

	d.logInfo("PDF_UTIL", fmt.Sprintf("Split '%s' (%d pages) into %d files in __raws", body.Filepath, pageCount, len(createdFiles)), nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"message":      fmt.Sprintf("Successfully split PDF into %d single-page PDF files in __raws", len(createdFiles)),
		"createdFiles": createdFiles,
	})
}

// --- #29 POST /api/chat --------------------------------------------------------------------------

func (s *server) chatHandler(d DocumentWriteDeps, w http.ResponseWriter, r *http.Request) {
	body := struct {
		Message string               `json:"message"`
		History []aichat.ChatMessage `json:"history"`
	}{}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Message text is required.")
		return
	}
	if strings.TrimSpace(body.Message) == "" {
		writeError(w, http.StatusBadRequest, "Message text is required.")
		return
	}
	history := body.History
	if history == nil {
		history = []aichat.ChatMessage{}
	}
	result, err := aichat.ProcessChatQuery(d.AIChat, body.Message, history, d.nowOr())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}
