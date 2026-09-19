// Ollama & Cloud AI routes #4-#6, ported from web-server.ts:183-237 and extended with
// multi-provider Cloud AI status and connection testing.
//
// GET /api/ollama/status builds `{online, model, host, modelsCount, models, modelExists,
// modelCanGenerate, modelError?}` and folds every connection failure into `online:false` (the route
// never returns a non-200 for a down Ollama). GET /api/ollama/models takes an optional `?host=`.
// POST /api/ollama/start spawns `ollama serve` fire-and-forget and responds immediately.
// POST /api/ai/test verifies live connectivity for any AI provider (local or cloud).
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/infra/aiprovider"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
)

func (s *server) registerOllama(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/ollama/status", s.ollamaStatusHandler)
	mux.HandleFunc("GET /api/ollama/models", s.ollamaModelsHandler)
	mux.HandleFunc("POST /api/ollama/start", s.ollamaStartHandler)
	mux.HandleFunc("POST /api/ai/test", s.aiTestHandler)
}

func (s *server) ollamaStatusHandler(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Settings.Config()

	// If cloud AI is active, report the cloud provider status
	if strings.EqualFold(cfg.AIProvider, "cloud") {
		cloudProv := strings.TrimSpace(cfg.CloudProvider)
		canonical, ok := aiprovider.NormalizeCloud(cloudProv)
		if !ok {
			if cloudProv != "" {
				// A non-empty unknown provider is a config error. Report the same error the Manager
				// would produce instead of pretending Google answered.
				writeJSON(w, 200, map[string]any{
					"online":         false,
					"provider":       "cloud",
					"cloud_provider": cloudProv,
					"error":          fmt.Sprintf("unknown cloud provider %q", cloudProv),
				})
				return
			}
			// Empty keeps the historical Google default (settings.Config leaves it empty).
			canonical = "google"
		}
		cloudModel := ""
		switch canonical {
		case "google":
			cloudModel = cfg.GoogleModel
			if cloudModel == "" {
				cloudModel = "gemini-2.5-flash"
			}
		case "claude":
			cloudModel = cfg.AnthropicModel
			if cloudModel == "" {
				cloudModel = "claude-3-7-sonnet-20250219"
			}
		case "deepseek":
			cloudModel = cfg.DeepSeekModel
			if cloudModel == "" {
				cloudModel = "deepseek-chat"
			}
		case "openai":
			cloudModel = cfg.OpenAIModel
			if cloudModel == "" {
				cloudModel = "gpt-4o-mini"
			}
		}

		health := s.deps.Ollama.CheckModelCanGenerate(cloudModel, false)
		resp := map[string]any{
			"online":           health.OK,
			"provider":         "cloud",
			"cloud_provider":   cloudProv,
			"model":            cloudModel,
			"host":             "Cloud API (" + cloudProv + ")",
			"modelsCount":      1,
			"models":           []string{cloudModel},
			"modelExists":      true,
			"modelCanGenerate": health.OK,
		}
		if !health.OK {
			resp["modelError"] = health.Error
		}
		writeJSON(w, 200, resp)
		return
	}

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

func (s *server) aiTestHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
		APIKey   string `json:"api_key"`
		Model    string `json:"model"`
		BaseURL  string `json:"base_url"`
	}
	if err := json.Unmarshal(bodyBytes(r), &req); err != nil {
		writeError(w, 400, "invalid json body")
		return
	}

	cfg := s.deps.Settings.Config()
	providerName := strings.ToLower(strings.TrimSpace(req.Provider))
	apiKey := strings.TrimSpace(req.APIKey)
	model := strings.TrimSpace(req.Model)
	baseURL := strings.TrimSpace(req.BaseURL)

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	var prov aiprovider.Provider
	switch providerName {
	case "google", "gemini":
		if apiKey == "" {
			apiKey = cfg.GoogleAPIKey
		}
		if model == "" {
			model = cfg.GoogleModel
		}
		if baseURL == "" {
			baseURL = cfg.GoogleBaseURL
		}
		prov = aiprovider.NewGoogleProvider(aiprovider.GoogleConfig{
			APIKey:  apiKey,
			Model:   model,
			BaseURL: baseURL,
		})
	case "claude", "anthropic":
		if apiKey == "" {
			apiKey = cfg.AnthropicAPIKey
		}
		if model == "" {
			model = cfg.AnthropicModel
		}
		if baseURL == "" {
			baseURL = cfg.AnthropicBaseURL
		}
		prov = aiprovider.NewClaudeProvider(aiprovider.ClaudeConfig{
			APIKey:  apiKey,
			Model:   model,
			BaseURL: baseURL,
		})
	case "deepseek":
		if apiKey == "" {
			apiKey = cfg.DeepSeekAPIKey
		}
		if model == "" {
			model = cfg.DeepSeekModel
		}
		if baseURL == "" {
			baseURL = cfg.DeepSeekBaseURL
		}
		prov = aiprovider.NewDeepSeekProvider(aiprovider.DeepSeekConfig{
			APIKey:  apiKey,
			Model:   model,
			BaseURL: baseURL,
		})
	case "openai":
		if apiKey == "" {
			apiKey = cfg.OpenAIAPIKey
		}
		if model == "" {
			model = cfg.OpenAIModel
		}
		if baseURL == "" {
			baseURL = cfg.OpenAIBaseURL
		}
		prov = aiprovider.NewOpenAIProvider(aiprovider.OpenAIConfig{
			APIKey:  apiKey,
			Model:   model,
			BaseURL: baseURL,
		})
	case "local", "ollama":
		host := baseURL
		if host == "" {
			host = cfg.OllamaHost
		}
		if model == "" {
			model = cfg.OllamaModel
		}
		health := s.deps.Ollama.CheckModelCanGenerate(model, true)
		if !health.OK {
			writeJSON(w, 200, map[string]any{"ok": false, "error": health.Error})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "message": "Ollama is online and model " + model + " can generate"})
		return
	default:
		writeError(w, 400, "unknown AI provider: "+providerName)
		return
	}

	res, err := prov.Test(ctx)
	if err != nil {
		writeJSON(w, 200, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}

	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"message": fmt.Sprintf("Successfully connected to %s! Test response: %s", prov.Name(), strings.TrimSpace(res)),
	})
}
