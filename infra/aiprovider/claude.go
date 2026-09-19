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

const defaultClaudeBaseURL = "https://api.anthropic.com"
const defaultClaudeModel = "claude-3-7-sonnet-20250219"

// ClaudeConfig holds settings for the Anthropic Claude API client.
type ClaudeConfig struct {
	APIKey  string
	Model   string
	BaseURL string
	HTTP    *http.Client
}

// ClaudeProvider implements Provider for Anthropic Claude.
type ClaudeProvider struct {
	apiKey  string
	model   string
	baseURL string
	http    *http.Client
}

// NewClaudeProvider creates a new Claude provider.
func NewClaudeProvider(cfg ClaudeConfig) *ClaudeProvider {
	model := cfg.Model
	if model == "" {
		model = defaultClaudeModel
	}
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultClaudeBaseURL
	}
	httpClient := cfg.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &ClaudeProvider{
		apiKey:  strings.TrimSpace(cfg.APIKey),
		model:   strings.TrimSpace(model),
		baseURL: baseURL,
		http:    httpClient,
	}
}

func (c *ClaudeProvider) Name() string {
	return "Anthropic Claude (" + c.model + ")"
}

type claudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type claudeRequest struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature *float64        `json:"temperature,omitempty"`
	System      string          `json:"system,omitempty"`
	Messages    []claudeMessage `json:"messages"`
}

type claudeResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *ClaudeProvider) endpointURL() string {
	return c.baseURL + "/v1/messages"
}

func (c *ClaudeProvider) RequestClassification(ctx context.Context, system, user string) (ollama.Completion, error) {
	if c.apiKey == "" {
		return ollama.Completion{}, errors.New("Anthropic API key is not configured")
	}

	temp := 0.1
	reqBody := claudeRequest{
		Model:       c.model,
		MaxTokens:   4096,
		Temperature: &temp,
		System:      system,
		Messages: []claudeMessage{
			{Role: "user", Content: user},
		},
	}

	text, _, err := c.post(ctx, reqBody)
	if err != nil {
		return ollama.Completion{}, err
	}
	return ollama.Completion{Response: text}, nil
}

func (c *ClaudeProvider) RequestTextChat(ctx context.Context, system, user string) (ollama.TextCompletion, error) {
	if c.apiKey == "" {
		return ollama.TextCompletion{}, errors.New("Anthropic API key is not configured")
	}

	temp := 0.2
	reqBody := claudeRequest{
		Model:       c.model,
		MaxTokens:   4096,
		Temperature: &temp,
		System:      system,
		Messages: []claudeMessage{
			{Role: "user", Content: user},
		},
	}

	text, stopReason, err := c.post(ctx, reqBody)
	if err != nil {
		return ollama.TextCompletion{}, err
	}
	return ollama.TextCompletion{Response: text, DoneReason: stopReason}, nil
}

func (c *ClaudeProvider) Test(ctx context.Context) (string, error) {
	if c.apiKey == "" {
		return "", errors.New("Anthropic API key is empty")
	}
	reqBody := claudeRequest{
		Model:     c.model,
		MaxTokens: 10,
		Messages: []claudeMessage{
			{Role: "user", Content: "Ping. Reply with 'OK'."},
		},
	}
	text, _, err := c.post(ctx, reqBody)
	return text, err
}

func (c *ClaudeProvider) post(ctx context.Context, payload claudeRequest) (string, string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpointURL(), bytes.NewReader(raw))
	if err != nil {
		return "", "", fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", "", fmt.Errorf("Claude API request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("read response body: %w", err)
	}

	var cResp claudeResponse
	if err := json.Unmarshal(bodyBytes, &cResp); err != nil {
		return "", "", fmt.Errorf("parse Claude API response (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	if cResp.Error != nil {
		return "", "", fmt.Errorf("Claude API error (%s): %s", cResp.Error.Type, cResp.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("Claude API HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var sb strings.Builder
	for _, block := range cResp.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return sb.String(), cResp.StopReason, nil
}
