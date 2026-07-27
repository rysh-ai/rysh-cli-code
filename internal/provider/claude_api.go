package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

const (
	anthropicVersion = "2023-06-01"
	defaultModel     = "claude-sonnet-5"
)

// ClaudeAPI calls the Anthropic Messages API directly over HTTP.
type ClaudeAPI struct {
	apiKey       string
	apiURL       string
	model        string
	maxTokens    int
	systemPrompt string
	client       *http.Client
	effort       string // optional output_config.effort override
}

// NewClaudeAPI creates a ClaudeAPI provider from the given config.
func NewClaudeAPI(cfg config.Config) *ClaudeAPI {
	model := cfg.DefaultModel
	if model == "" {
		model = defaultModel
	}
	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	apiURL := cfg.APIURL
	if apiURL == "" {
		apiURL = "https://api.anthropic.com"
	}
	return &ClaudeAPI{
		apiKey:       cfg.APIKey,
		apiURL:       strings.TrimRight(apiURL, "/"),
		model:        model,
		maxTokens:    maxTokens,
		systemPrompt: cfg.SystemPrompt,
		client:       &http.Client{},
	}
}

func (c *ClaudeAPI) Name() string {
	return "claude-api"
}

// messagesRequest is the request body for the Anthropic Messages API.
type messagesRequest struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens"`
	System    string           `json:"system,omitempty"`
	Messages  []messageContent `json:"messages"`
	// OutputConfig carries the effort override (low|medium|high|xhigh|max);
	// omitted entirely when no effort is set.
	OutputConfig *outputConfig `json:"output_config,omitempty"`
}

// outputConfig is the Anthropic output_config request block.
type outputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type messageContent struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// messagesResponse is the response body from the Anthropic Messages API.
type messagesResponse struct {
	ID      string        `json:"id"`
	Type    string        `json:"type"`
	Role    string        `json:"role"`
	Content []contentItem `json:"content"`
	Model   string        `json:"model"`
	Error   *apiError     `json:"error,omitempty"`
}

type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type apiError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// apiErrorResponse is the top-level error envelope returned by the API.
type apiErrorResponse struct {
	Type  string    `json:"type"`
	Error *apiError `json:"error"`
}

func (c *ClaudeAPI) Complete(ctx context.Context, prompt string) (string, error) {
	out, err := c.completeOnce(ctx, prompt)
	// Effort self-heal: some models (e.g. Haiku 4.5) reject
	// output_config.effort with a 400 — strip it and retry once instead of
	// failing every judge/completion that paired effort with such a model.
	if err != nil && c.effort != "" && strings.Contains(err.Error(), "does not support the effort parameter") {
		bare := *c
		bare.effort = ""
		return bare.completeOnce(ctx, prompt)
	}
	return out, err
}

func (c *ClaudeAPI) completeOnce(ctx context.Context, prompt string) (string, error) {
	systemPromptText, err := c.loadSystemPrompt()
	if err != nil {
		return "", fmt.Errorf("claude-api: %w", err)
	}

	reqBody := messagesRequest{
		Model:     c.model,
		MaxTokens: c.maxTokens,
		System:    systemPromptText,
		Messages: []messageContent{
			{Role: "user", Content: prompt},
		},
	}
	if c.effort != "" {
		reqBody.OutputConfig = &outputConfig{Effort: c.effort}
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("claude-api: marshal request: %w", err)
	}

	endpoint := c.apiURL + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("claude-api: create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("claude-api: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("claude-api: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp apiErrorResponse
		if json.Unmarshal(respBytes, &errResp) == nil && errResp.Error != nil {
			return "", fmt.Errorf("claude-api: %s (HTTP %d): %s",
				errResp.Error.Type, resp.StatusCode, errResp.Error.Message)
		}
		return "", fmt.Errorf("claude-api: HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var msgResp messagesResponse
	if err := json.Unmarshal(respBytes, &msgResp); err != nil {
		return "", fmt.Errorf("claude-api: unmarshal response: %w", err)
	}

	// Extract all text content blocks.
	var parts []string
	for _, block := range msgResp.Content {
		if block.Type == "text" {
			parts = append(parts, block.Text)
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("claude-api: response contained no text content")
	}

	return strings.Join(parts, "\n"), nil
}

func (c *ClaudeAPI) loadSystemPrompt() (string, error) {
	if c.systemPrompt == "" {
		return "", nil
	}
	path := c.systemPrompt
	if !filepath.IsAbs(path) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		path = filepath.Join(wd, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read system prompt: %w", err)
	}
	return string(data), nil
}
