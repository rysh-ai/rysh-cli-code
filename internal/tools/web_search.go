package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type WebSearchTool struct {
	apiKey string
}

type WebSearchParams struct {
	Query string `json:"query"`
	Count int    `json:"count,omitempty"`
}

func NewWebSearchTool(apiKey string) *WebSearchTool {
	return &WebSearchTool{apiKey: apiKey}
}

func (t *WebSearchTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "The search query to look up on the internet"},
    "count": {"type": "integer", "description": "Number of results to return (default 5, max 20)"}
  },
  "required": ["query"]
}`)
	return ToolSpec{
		Name:             "web_search",
		Description:      "Search the internet using Brave Search and return relevant results with titles, URLs, and descriptions.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

func (t *WebSearchTool) RequiresApproval(_ json.RawMessage) bool {
	return false
}

func (t *WebSearchTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p WebSearchParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	if p.Query == "" {
		return ErrOutput(ErrKindValidation, "query is required"), nil
	}
	if t.apiKey == "" {
		return ErrOutput(ErrKindMissing, "web_search requires a Brave Search API key. Set RYSH_BRAVE_API_KEY or configure brave_api_key in rysh.config.yaml."), nil
	}

	count := p.Count
	if count <= 0 {
		count = 5
	}
	if count > 20 {
		count = 20
	}

	// Build request URL
	u, _ := url.Parse("https://api.search.brave.com/res/v1/web/search")
	q := u.Query()
	q.Set("q", p.Query)
	q.Set("count", strconv.Itoa(count))
	u.RawQuery = q.Encode()

	// Create HTTP request with context
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return ErrOutputf(ErrKindInternal, "failed to create request: %v", err), nil
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", t.apiKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ErrOutputf(ErrKindTransient, "search request failed: %v", err), nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024)) // 512KB max
	if err != nil {
		return ErrOutputf(ErrKindTransient, "failed to read response: %v", err), nil
	}

	if resp.StatusCode != http.StatusOK {
		return ErrOutputf(ErrKindTransient, "search API returned status %d: %s", resp.StatusCode, string(body)), nil
	}

	// Parse Brave Search response
	var searchResp struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &searchResp); err != nil {
		return ErrOutputf(ErrKindInternal, "failed to parse search results: %v", err), nil
	}

	results := searchResp.Web.Results
	if len(results) == 0 {
		return &ToolOutput{Content: fmt.Sprintf("No results found for: %s", p.Query)}, nil
	}

	// Format output
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Search results for: %s\n\n", p.Query))
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("[%d] %s\n", i+1, r.Title))
		sb.WriteString(fmt.Sprintf("    URL: %s\n", r.URL))
		if r.Description != "" {
			sb.WriteString(fmt.Sprintf("    %s\n", r.Description))
		}
		if i < len(results)-1 {
			sb.WriteString("\n")
		}
	}

	return &ToolOutput{Content: sb.String()}, nil
}
