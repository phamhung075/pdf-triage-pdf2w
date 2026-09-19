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

const defaultOpenAIBaseURL = "https://api.openai.com/v1"
const defaultOpenAIModel = "gpt-4o-mini"

// OpenAIConfig holds settings for the OpenAI API client.
type OpenAIConfig struct {
	APIKey  string
	Model   string
	BaseURL string
	HTTP    *http.Client
}

// OpenAIProvider implements Provider for OpenAI.
type OpenAIProvider struct {
	apiKey  string
	model   string
	baseURL string
	http    *http.Client
}

// NewOpenAIProvider creates a new OpenAI provider.
func NewOpenAIProvider(cfg OpenAIConfig) *OpenAIProvider {
	model := cfg.Model
	if model == "" {
		model = defaultOpenAIModel
	}
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultOpenAIBaseURL
	}
	httpClient := cfg.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &OpenAIProvider{
		apiKey:  strings.TrimSpace(cfg.APIKey),
		model:   strings.TrimSpace(model),
		baseURL: baseURL,
		http:    httpClient,
	}
}

func (o *OpenAIProvider) Name() string {
	return "OpenAI (" + o.model + ")"
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIResponseFormat struct {
	Type string `json:"type"`
}

type openAIRequest struct {
	Model               string                `json:"model"`
	Messages            []openAIChatMessage   `json:"messages"`
	Temperature         *float64              `json:"temperature,omitempty"`
	ResponseFormat      *openAIResponseFormat `json:"response_format,omitempty"`
	MaxTokens           *int                  `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                  `json:"max_completion_tokens,omitempty"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error,omitempty"`
}

func (o *OpenAIProvider) endpointURL() string {
	return o.baseURL + "/chat/completions"
}

// isReasoningModel reports whether model is an OpenAI reasoning model that
// rejects a non-default temperature and only accepts max_completion_tokens
// (the o-series such as o1/o3-mini/o4-mini, and the gpt-5 family).
// Matching is case-insensitive: a name starting with "gpt-5", or with "o"
// followed by a digit. Any other name (including OpenAI-compatible servers'
// models) keeps the legacy request shape.
func isReasoningModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if strings.HasPrefix(m, "gpt-5") {
		return true
	}
	return len(m) >= 2 && m[0] == 'o' && m[1] >= '0' && m[1] <= '9'
}

func (o *OpenAIProvider) RequestClassification(ctx context.Context, system, user string) (ollama.Completion, error) {
	if o.apiKey == "" {
		return ollama.Completion{}, errors.New("OpenAI API key is not configured")
	}

	temp := 0.1
	reqBody := openAIRequest{
		Model: o.model,
		Messages: []openAIChatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		ResponseFormat: &openAIResponseFormat{Type: "json_object"},
	}
	if !isReasoningModel(o.model) {
		reqBody.Temperature = &temp
	}

	text, _, err := o.post(ctx, reqBody)
	if err != nil {
		return ollama.Completion{}, err
	}
	return ollama.Completion{Response: text}, nil
}

func (o *OpenAIProvider) RequestTextChat(ctx context.Context, system, user string) (ollama.TextCompletion, error) {
	if o.apiKey == "" {
		return ollama.TextCompletion{}, errors.New("OpenAI API key is not configured")
	}

	temp := 0.2
	reqBody := openAIRequest{
		Model: o.model,
		Messages: []openAIChatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	}
	if !isReasoningModel(o.model) {
		reqBody.Temperature = &temp
	}

	text, finishReason, err := o.post(ctx, reqBody)
	if err != nil {
		return ollama.TextCompletion{}, err
	}
	return ollama.TextCompletion{Response: text, DoneReason: finishReason}, nil
}

func (o *OpenAIProvider) Test(ctx context.Context) (string, error) {
	if o.apiKey == "" {
		return "", errors.New("OpenAI API key is empty")
	}
	maxTokens := 10
	reqBody := openAIRequest{
		Model: o.model,
		Messages: []openAIChatMessage{
			{Role: "user", Content: "Ping. Reply with 'OK'."},
		},
	}
	if isReasoningModel(o.model) {
		reqBody.MaxCompletionTokens = &maxTokens
	} else {
		reqBody.MaxTokens = &maxTokens
	}
	text, _, err := o.post(ctx, reqBody)
	return text, err
}

func (o *OpenAIProvider) post(ctx context.Context, payload openAIRequest) (string, string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpointURL(), bytes.NewReader(raw))
	if err != nil {
		return "", "", fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.http.Do(httpReq)
	if err != nil {
		return "", "", fmt.Errorf("OpenAI API request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("read response body: %w", err)
	}

	var oResp openAIResponse
	if err := json.Unmarshal(bodyBytes, &oResp); err != nil {
		return "", "", fmt.Errorf("parse OpenAI API response (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	if oResp.Error != nil {
		return "", "", fmt.Errorf("OpenAI API error (%s): %s", oResp.Error.Type, oResp.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("OpenAI API HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	if len(oResp.Choices) == 0 {
		return "", "", errors.New("OpenAI API returned no choices")
	}

	return oResp.Choices[0].Message.Content, oResp.Choices[0].FinishReason, nil
}
