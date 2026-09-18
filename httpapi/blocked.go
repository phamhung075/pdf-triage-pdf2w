// GET /api/blocked-files (#18), ported from web-server.ts:533-542: every file held in
// __raws/blocked_files with the reason it was blocked (no extracted text, or no specific
// subcategory could be determined).
package httpapi

import (
	"net/http"

	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
)

func (s *server) registerBlockedFiles(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/blocked-files", s.blockedFilesHandler)
}

func (s *server) blockedFilesHandler(w http.ResponseWriter, r *http.Request) {
	files, err := s.deps.DB.GetAllBlockedFiles()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if files == nil {
		files = []database.BlockedFileRecord{}
	}
	writeJSON(w, 200, map[string]any{"total": len(files), "files": files})
}
