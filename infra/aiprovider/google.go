package aiprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
)

const defaultGoogleBaseURL = "https://generativelanguage.googleapis.com"
const defaultGoogleModel = "gemini-2.5-flash"

// GoogleConfig holds settings for the Google Gemini API client.
type GoogleConfig struct {
	APIKey  string
	Model   string
	BaseURL string
	HTTP    *http.Client
}

// GoogleProvider implements Provider for Google Gemini.
type GoogleProvider struct {
	apiKey  string
	model   string
	baseURL string
	http    *http.Client
}

// NewGoogleProvider creates a new Google Gemini provider.
func NewGoogleProvider(cfg GoogleConfig) *GoogleProvider {
	model := cfg.Model
	if model == "" {
		model = defaultGoogleModel
	}
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultGoogleBaseURL
	}
	httpClient := cfg.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &GoogleProvider{
		apiKey:  strings.TrimSpace(cfg.APIKey),
		model:   strings.TrimSpace(model),
		baseURL: baseURL,
		http:    httpClient,
	}
}

func (g *GoogleProvider) Name() string {
	return "Google Gemini (" + g.model + ")"
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiGenConfig struct {
	Temperature      *float64 `json:"temperature,omitempty"`
	ResponseMimeType string   `json:"responseMimeType,omitempty"`
	MaxOutputTokens  *int     `json:"maxOutputTokens,omitempty"`
}

type geminiRequest struct {
	Contents          []geminiContent  `json:"contents"`
	SystemInstruction *geminiContent   `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenConfig `json:"generationConfig,omitempty"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error,omitempty"`
}

func (g *GoogleProvider) endpointURL() string {
	return fmt.Sprintf("%s/v1beta/models/%s:generateContent", g.baseURL, g.model)
}

// redact removes the configured API key from a string before it can reach logs
// or user-visible error messages.
func (g *GoogleProvider) redact(s string) string {
	if g.apiKey == "" {
		return s
	}
	return strings.ReplaceAll(s, g.apiKey, "[REDACTED]")
}

// redactedTransportError wraps a transport error and strips the API key from
// its message while preserving error unwrapping.
type redactedTransportError struct {
	err    error
	secret string
}

func (e redactedTransportError) Error() string {
	msg := e.err.Error()
	if e.secret == "" {
		return msg
	}
	return strings.ReplaceAll(msg, e.secret, "[REDACTED]")
}

func (e redactedTransportError) Unwrap() error { return e.err }

func (g *GoogleProvider) RequestClassification(ctx context.Context, system, user string) (ollama.Completion, error) {
	if g.apiKey == "" {
		return ollama.Completion{}, errors.New("Google API key is not configured")
	}

	temp := 0.1
	reqBody := geminiRequest{
		Contents: []geminiContent{
			{Role: "user", Parts: []geminiPart{{Text: user}}},
		},
		GenerationConfig: &geminiGenConfig{
			Temperature:      &temp,
			ResponseMimeType: "application/json",
		},
	}
	if system != "" {
		reqBody.SystemInstruction = &geminiContent{
			Parts: []geminiPart{{Text: system}},
		}
	}

	text, err := g.post(ctx, reqBody)
	if err != nil {
		return ollama.Completion{}, err
	}
	return ollama.Completion{Response: text}, nil
}

func (g *GoogleProvider) RequestTextChat(ctx context.Context, system, user string) (ollama.TextCompletion, error) {
	if g.apiKey == "" {
		return ollama.TextCompletion{}, errors.New("Google API key is not configured")
	}

	temp := 0.2
	reqBody := geminiRequest{
		Contents: []geminiContent{
			{Role: "user", Parts: []geminiPart{{Text: user}}},
		},
		GenerationConfig: &geminiGenConfig{
			Temperature: &temp,
		},
	}
	if system != "" {
		reqBody.SystemInstruction = &geminiContent{
			Parts: []geminiPart{{Text: system}},
		}
	}

	text, err := g.post(ctx, reqBody)
	if err != nil {
		return ollama.TextCompletion{}, err
	}
	return ollama.TextCompletion{Response: text, DoneReason: "stop"}, nil
}

func (g *GoogleProvider) Test(ctx context.Context) (string, error) {
	if g.apiKey == "" {
		return "", errors.New("Google API key is empty")
	}
	maxTokens := 10
	reqBody := geminiRequest{
		Contents: []geminiContent{
			{Role: "user", Parts: []geminiPart{{Text: "Ping. Reply with 'OK'."}}},
		},
		GenerationConfig: &geminiGenConfig{
			MaxOutputTokens: &maxTokens,
		},
	}
	return g.post(ctx, reqBody)
}

func (g *GoogleProvider) post(ctx context.Context, payload geminiRequest) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpointURL(), bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", g.apiKey)

	resp, err := g.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("Google API request failed: %w", redactedTransportError{err: err, secret: g.apiKey})
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response body: %w", err)
	}

	var gResp geminiResponse
	if err := json.Unmarshal(bodyBytes, &gResp); err != nil {
		return "", fmt.Errorf("parse Google API response (status %d): %s", resp.StatusCode, g.redact(string(bodyBytes)))
	}

	if gResp.Error != nil {
		return "", fmt.Errorf("Google API error (%d %s): %s", gResp.Error.Code, gResp.Error.Status, g.redact(gResp.Error.Message))
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Google API HTTP %d: %s", resp.StatusCode, g.redact(string(bodyBytes)))
	}

	if len(gResp.Candidates) == 0 || len(gResp.Candidates[0].Content.Parts) == 0 {
		return "", errors.New("Google API returned empty candidates")
	}

	var sb strings.Builder
	for _, p := range gResp.Candidates[0].Content.Parts {
		sb.WriteString(p.Text)
	}
	return sb.String(), nil
}
