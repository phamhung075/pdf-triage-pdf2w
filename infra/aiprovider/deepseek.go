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

const defaultDeepSeekBaseURL = "https://api.deepseek.com"
const defaultDeepSeekModel = "deepseek-flash"

// DeepSeekConfig holds settings for the DeepSeek API client.
type DeepSeekConfig struct {
	APIKey  string
	Model   string
	BaseURL string
	HTTP    *http.Client
}

// DeepSeekProvider implements Provider for DeepSeek.
type DeepSeekProvider struct {
	apiKey  string
	model   string
	baseURL string
	http    *http.Client
}

// NewDeepSeekProvider creates a new DeepSeek provider.
func NewDeepSeekProvider(cfg DeepSeekConfig) *DeepSeekProvider {
	model := cfg.Model
	if model == "" {
		model = defaultDeepSeekModel
	}
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultDeepSeekBaseURL
	}
	httpClient := cfg.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &DeepSeekProvider{
		apiKey:  strings.TrimSpace(cfg.APIKey),
		model:   strings.TrimSpace(model),
		baseURL: baseURL,
		http:    httpClient,
	}
}

func (d *DeepSeekProvider) Name() string {
	return "DeepSeek (" + d.model + ")"
}

type deepSeekChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type deepSeekResponseFormat struct {
	Type string `json:"type"`
}

type deepSeekRequest struct {
	Model          string                  `json:"model"`
	Messages       []deepSeekChatMessage   `json:"messages"`
	Temperature    *float64                `json:"temperature,omitempty"`
	ResponseFormat *deepSeekResponseFormat `json:"response_format,omitempty"`
	MaxTokens      *int                    `json:"max_tokens,omitempty"`
}

type deepSeekResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error,omitempty"`
}

func (d *DeepSeekProvider) endpointURL() string {
	if strings.HasSuffix(d.baseURL, "/v1") {
		return d.baseURL + "/chat/completions"
	}
	return d.baseURL + "/chat/completions"
}

func (d *DeepSeekProvider) RequestClassification(ctx context.Context, system, user string) (ollama.Completion, error) {
	if d.apiKey == "" {
		return ollama.Completion{}, errors.New("DeepSeek API key is not configured")
	}

	temp := 0.1
	reqBody := deepSeekRequest{
		Model: d.model,
		Messages: []deepSeekChatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature:    &temp,
		ResponseFormat: &deepSeekResponseFormat{Type: "json_object"},
	}

	content, thinking, _, servedModel, err := d.post(ctx, reqBody)
	if err != nil {
		return ollama.Completion{}, err
	}
	return ollama.Completion{Response: content, Thinking: thinking, Model: servedModel}, nil
}

func (d *DeepSeekProvider) RequestTextChat(ctx context.Context, system, user string) (ollama.TextCompletion, error) {
	if d.apiKey == "" {
		return ollama.TextCompletion{}, errors.New("DeepSeek API key is not configured")
	}

	temp := 0.2
	reqBody := deepSeekRequest{
		Model: d.model,
		Messages: []deepSeekChatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: &temp,
	}

	content, thinking, finishReason, servedModel, err := d.post(ctx, reqBody)
	if err != nil {
		return ollama.TextCompletion{}, err
	}
	return ollama.TextCompletion{Response: content, Thinking: thinking, DoneReason: finishReason, Model: servedModel}, nil
}

// TestWithModel runs the connection test and returns the model id the upstream response echoed, or
// "" when the provider omitted it. It never copies the configured model into the result.
func (d *DeepSeekProvider) TestWithModel(ctx context.Context) (string, string, error) {
	if d.apiKey == "" {
		return "", "", errors.New("DeepSeek API key is empty")
	}
	maxTokens := 10
	reqBody := deepSeekRequest{
		Model: d.model,
		Messages: []deepSeekChatMessage{
			{Role: "user", Content: "Ping. Reply with 'OK'."},
		},
		MaxTokens: &maxTokens,
	}
	content, _, _, servedModel, err := d.post(ctx, reqBody)
	return content, servedModel, err
}

func (d *DeepSeekProvider) Test(ctx context.Context) (string, error) {
	reply, _, err := d.TestWithModel(ctx)
	return reply, err
}

func (d *DeepSeekProvider) post(ctx context.Context, payload deepSeekRequest) (string, string, string, string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", "", "", "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpointURL(), bytes.NewReader(raw))
	if err != nil {
		return "", "", "", "", fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+d.apiKey)

	resp, err := d.http.Do(httpReq)
	if err != nil {
		return "", "", "", "", fmt.Errorf("DeepSeek API request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", "", fmt.Errorf("read response body: %w", err)
	}

	var dResp deepSeekResponse
	if err := json.Unmarshal(bodyBytes, &dResp); err != nil {
		return "", "", "", "", fmt.Errorf("parse DeepSeek API response (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	if dResp.Error != nil {
		return "", "", "", "", fmt.Errorf("DeepSeek API error: %s", dResp.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", "", "", fmt.Errorf("DeepSeek API HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	if len(dResp.Choices) == 0 {
		return "", "", "", "", errors.New("DeepSeek API returned no choices")
	}

	choice := dResp.Choices[0]
	return choice.Message.Content, choice.Message.ReasoningContent, choice.FinishReason, dResp.Model, nil
}
