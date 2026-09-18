// Ollama routes #4-#6, ported from web-server.ts:183-237.
//
// GET /api/ollama/status builds `{online, model, host, modelsCount, models, modelExists,
// modelCanGenerate, modelError?}` and folds every connection failure into `online:false` (the route
// never returns a non-200 for a down Ollama). GET /api/ollama/models takes an optional `?host=`.
// POST /api/ollama/start spawns `ollama serve` fire-and-forget and responds immediately, exactly as
// the TS exec() callback only logged a warning.
package httpapi

import (
	"net/http"
	"strings"

	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
)

func (s *server) registerOllama(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/ollama/status", s.ollamaStatusHandler)
	mux.HandleFunc("GET /api/ollama/models", s.ollamaModelsHandler)
	mux.HandleFunc("POST /api/ollama/start", s.ollamaStartHandler)
}

func (s *server) ollamaStatusHandler(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Settings.Config()
	model := cfg.OllamaModel
	host := cfg.OllamaHost

	models, err := s.deps.Ollama.ListModels(host)
	if err != nil {
		// TS: modelsCount 0, models [], plus the error message; the model/health keys are absent.
		writeJSON(w, 200, map[string]any{
			"online":      false,
			"model":       model,
			"host":        host,
			"modelsCount": 0,
			"models":      []string{},
			"error":       err.Error(),
		})
		return
	}

	modelExists := false
	for _, name := range models {
		if strings.Contains(name, model) {
			modelExists = true
			break
		}
	}

	health := ollama.ModelHealth{OK: false, Error: "model not found locally"}
	if modelExists {
		health = s.deps.Ollama.CheckModelCanGenerate(model, false)
	}

	resp := map[string]any{
		"online":           true,
		"model":            model,
		"host":             host,
		"modelsCount":      len(models),
		"models":           models,
		"modelExists":      modelExists,
		"modelCanGenerate": health.OK,
	}
	if !health.OK {
		resp["modelError"] = health.Error
	}
	writeJSON(w, 200, resp)
}

func (s *server) ollamaModelsHandler(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Settings.Config()
	host := r.URL.Query().Get("host")
	if host == "" {
		host = cfg.OllamaHost
	}

	models, err := s.deps.Ollama.ListModels(host)
	if err != nil {
		writeJSON(w, 200, map[string]any{"online": false, "models": []string{}, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"online": true, "models": models})
}

func (s *server) ollamaStartHandler(w http.ResponseWriter, r *http.Request) {
	if s.deps.StartOllama != nil {
		// Fire-and-forget: the TS exec() did not await the process, and neither does this.
		go func() { _ = s.deps.StartOllama() }()
	}
	writeJSON(w, 200, map[string]any{"message": "Ollama serve launch initiated"})
}
