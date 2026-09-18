// Command server exposes the canonicalpath package over HTTP for pdf-triage's TypeScript app
// (src/infrastructure/canonical-path-remote.ts) to call. See
// https://github.com/phamhung075/pdf-triage-pdf2w and pdf-triage's
// docs/superpowers/specs/2026-09-17-pdf2w-extraction-swap-design.md for the design.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"

	"github.com/phamhung075/pdf-triage-pdf2w/canonicalpath"
	"github.com/phamhung075/pdf-triage-pdf2w/cleantext"
)

type canonicalPathRequest struct {
	OriginalPath  string  `json:"originalPath"`
	Category      string  `json:"category"`
	OutputRootDir string  `json:"outputRootDir"`
	Subcategory   *string `json:"subcategory"`
	DateStr       *string `json:"dateStr"`
	Title         *string `json:"title"`
}

func canonicalPathHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req canonicalPathRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.OriginalPath == "" || req.OutputRootDir == "" {
		http.Error(w, "originalPath and outputRootDir are required", http.StatusBadRequest)
		return
	}
	result := canonicalpath.ComputeCanonicalPath(req.OriginalPath, req.Category, req.OutputRootDir, req.Subcategory, req.DateStr, req.Title)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"canonicalPath": result})
}

type cleanTextRequest struct {
	Text string `json:"text"`
}

func cleanTextHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req cleanTextRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	result := cleantext.CleanExtractedText(req.Text)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"text": result})
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "pdf-triage-pdf2w"})
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3985"
	}
	http.HandleFunc("/canonical-path", canonicalPathHandler)
	http.HandleFunc("/clean-text", cleanTextHandler)
	http.HandleFunc("/health", healthHandler)
	log.Printf("pdf-triage-pdf2w listening on :%s", port)
	log.Fatal(http.ListenAndServe("0.0.0.0:"+port, nil))
}
