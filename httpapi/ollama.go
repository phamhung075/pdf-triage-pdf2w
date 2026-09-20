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
				cloudModel = "gemini-3.8-flash"
			}
		case "claude":
			cloudModel = cfg.AnthropicModel
			if cloudModel == "" {
				cloudModel = "claude-3-7-sonnet-20250219"
			}
		case "deepseek":
			cloudModel = cfg.DeepSeekModel
			if cloudModel == "" {
				cloudModel = "deepseek-flash"
			}
		case "openai":
			cloudModel = cfg.OpenAIModel
			if cloudModel == "" {
				cloudModel = "gpt-4o-mini"
			}
		}

		health := s.deps.Ollama.CheckModelCanGenerate(cloudModel, r.URL.Query().Get("refresh") == "1")
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
			// model_confirmed is the model id the provider's own response echoed, or "" when the
			// provider omitted it. It is NEVER copied from configuration; model_verified compares the
			// requested model against that echo (a missing echo is never verified).
			"model_confirmed": health.ServedModel,
			"model_verified":  aiprovider.ModelMatches(cloudModel, health.ServedModel),
		}
		if !health.OK {
			resp["modelError"] = health.Error
		}
		// The last-classification keys are omitted until a classification has succeeded since start
		// or since the last config change. The real *aiprovider.Manager implements this optional
		// interface; the OllamaClient deps interface is intentionally not grown (test fakes).
		if lc, ok := s.deps.Ollama.(interface {
			LastClassification() (string, time.Time)
		}); ok {
			if lastModel, at := lc.LastClassification(); lastModel != "" && !at.IsZero() {
				resp["last_classification_model"] = lastModel
				resp["last_classification_at"] = at.UTC().Format(time.RFC3339)
			}
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
		start := time.Now()
		health := s.deps.Ollama.CheckModelCanGenerate(model, true)
		latency := time.Since(start).Milliseconds()
		if !health.OK {
			writeJSON(w, 200, map[string]any{"ok": false, "error": health.Error, "latency_ms": latency})
			return
		}
		writeJSON(w, 200, map[string]any{
			"ok":              true,
			"message":         "Ollama is online and model " + model + " can generate",
			"latency_ms":      latency,
			"model_requested": model,
			"model_confirmed": health.ServedModel,
			"model_verified":  aiprovider.ModelMatches(model, health.ServedModel),
		})
		return
	default:
		writeError(w, 400, "unknown AI provider: "+providerName)
		return
	}

	start := time.Now()
	// Prefer the optional prober so the response carries the model id the provider echoed. Tests and
	// third-party Providers that only implement Provider fall back to Test with an unknown echo.
	var res, servedModel string
	var err error
	if prober, ok := prov.(interface {
		TestWithModel(context.Context) (string, string, error)
	}); ok {
		res, servedModel, err = prober.TestWithModel(ctx)
	} else {
		res, err = prov.Test(ctx)
	}
	latency := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, 200, map[string]any{
			"ok":         false,
			"error":      err.Error(),
			"latency_ms": latency,
		})
		return
	}

	writeJSON(w, 200, map[string]any{
		"ok":              true,
		"message":         fmt.Sprintf("Successfully connected to %s! Test response: %s", prov.Name(), strings.TrimSpace(res)),
		"latency_ms":      latency,
		"model_requested": model,
		"model_confirmed": servedModel,
		"model_verified":  aiprovider.ModelMatches(model, servedModel),
	})
}
