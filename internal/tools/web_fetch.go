// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	webFetchMaxBody    = 2 * 1024 * 1024 // 2MB
	webFetchDefaultMax = 50000           // chars
	webFetchTimeout    = 30 * time.Second
)

// WebFetchTool fetches and parses content from a specific URL.
type WebFetchTool struct{}

// WebFetchParams holds the parameters for a web fetch request.
type WebFetchParams struct {
	URL         string `json:"url"`
	MaxLength   int    `json:"max_length,omitempty"`
	ExtractMode string `json:"extract_mode,omitempty"` // "text", "html", "raw"
}

// NewWebFetchTool creates a new WebFetchTool.
func NewWebFetchTool() *WebFetchTool {
	return &WebFetchTool{}
}

// Spec returns the tool specification for the LLM.
func (t *WebFetchTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "The URL to fetch content from"},
    "max_length": {"type": "integer", "description": "Maximum number of characters to return (default 50000)"},
    "extract_mode": {"type": "string", "enum": ["text", "html", "raw"], "description": "Content extraction mode: text (strip HTML tags), html (raw HTML truncated), raw (raw bytes). Default: text"}
  },
  "required": ["url"]
}`)
	return ToolSpec{
		Name:             "web_fetch",
		Description:      "Fetch and parse content from a specific URL. Supports text extraction (HTML tag stripping), raw HTML, and raw byte modes.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval always returns false for web_fetch.
func (t *WebFetchTool) RequiresApproval(_ json.RawMessage) bool {
	return false
}

// Execute fetches content from the specified URL.
func (t *WebFetchTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p WebFetchParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}

	if p.URL == "" {
		return ErrOutput(ErrKindValidation, "url is required"), nil
	}

	maxLength := p.MaxLength
	if maxLength <= 0 {
		maxLength = webFetchDefaultMax
	}

	extractMode := p.ExtractMode
	if extractMode == "" {
		extractMode = "text"
	}
	if extractMode != "text" && extractMode != "html" && extractMode != "raw" {
		return ErrOutputf(ErrKindValidation, "invalid extract_mode: %s (must be text, html, or raw)", extractMode), nil
	}

	// Create HTTP request with context and timeout
	reqCtx, cancel := context.WithTimeout(ctx, webFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, p.URL, nil)
	if err != nil {
		return ErrOutputf(ErrKindValidation, "failed to create request: %v", err), nil
	}
	req.Header.Set("User-Agent", "rysh/1.0")

	client := &http.Client{Timeout: webFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return ErrOutputf(ErrKindTransient, "fetch failed: %v", err), nil
	}
	defer resp.Body.Close()

	// Read body with size limit
	body, err := io.ReadAll(io.LimitReader(resp.Body, webFetchMaxBody))
	if err != nil {
		return ErrOutputf(ErrKindTransient, "failed to read response body: %v", err), nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		// Classify by status: 5xx + 408/429 are transient (retriable);
		// the rest are validation (model should re-call with a different URL
		// or accept the failure).
		kind := ErrKindValidation
		if resp.StatusCode >= 500 || resp.StatusCode == 408 || resp.StatusCode == 429 {
			kind = ErrKindTransient
		}
		return ErrOutputf(kind, "HTTP %d: %s", resp.StatusCode, truncateStr(string(body), 500)), nil
	}

	content := string(body)

	switch extractMode {
	case "text":
		content = stripHTMLTags(content)
	case "html":
		// Return raw HTML, just truncate
	case "raw":
		// Return raw bytes as string
	}

	// Truncate to max_length
	if len(content) > maxLength {
		content = content[:maxLength] + "\n\n[Content truncated at " + fmt.Sprintf("%d", maxLength) + " characters]"
	}

	return &ToolOutput{
		Content: content,
		Metadata: map[string]string{
			"url":            p.URL,
			"status_code":    fmt.Sprintf("%d", resp.StatusCode),
			"content_type":   resp.Header.Get("Content-Type"),
			"extract_mode":   extractMode,
			"content_length": fmt.Sprintf("%d", len(content)),
		},
	}, nil
}

// Pre-compiled regexes for stripHTMLTags. Compiled once at package init
// rather than on every call (stripHTMLTags runs on every web_fetch).
var (
	reHTMLScript       = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reHTMLStyle        = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reHTMLComments     = regexp.MustCompile(`(?s)<!--.*?-->`)
	reHTMLBlock        = regexp.MustCompile(`(?i)</(p|div|h[1-6]|li|tr|br|hr)[^>]*>`)
	reHTMLBr           = regexp.MustCompile(`(?i)<br\s*/?>`)
	reHTMLTags         = regexp.MustCompile(`<[^>]+>`)
	reHTMLMultiNewline = regexp.MustCompile(`\n{3,}`)
	reHTMLMultiSpace   = regexp.MustCompile(`[ \t]+`)
)

// stripHTMLTags removes HTML tags and returns readable text content.
func stripHTMLTags(s string) string {
	// Remove script and style blocks entirely
	s = reHTMLScript.ReplaceAllString(s, "")
	s = reHTMLStyle.ReplaceAllString(s, "")

	// Remove HTML comments
	s = reHTMLComments.ReplaceAllString(s, "")

	// Replace block-level elements with newlines
	s = reHTMLBlock.ReplaceAllString(s, "\n")
	s = reHTMLBr.ReplaceAllString(s, "\n")

	// Remove all remaining HTML tags
	s = reHTMLTags.ReplaceAllString(s, "")

	// Decode common HTML entities
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", "\"")
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = strings.ReplaceAll(s, "&nbsp;", " ")

	// Collapse multiple newlines
	s = reHTMLMultiNewline.ReplaceAllString(s, "\n\n")

	// Collapse multiple spaces
	s = reHTMLMultiSpace.ReplaceAllString(s, " ")

	return strings.TrimSpace(s)
}

// truncateStr truncates a string to maxLen characters.
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
