package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Dialect abstracts one provider wire format (design 001 §4.2): the default
// upstream its traffic goes to, how the real API key is attached to a request,
// how token usage is read out of a response, and what its native rate-limit
// error looks like — so the budget 429 the proxy synthesizes is
// indistinguishable in shape from the provider's own.
type Dialect interface {
	// Name is the routing key in /{dialect}/{paneID}/... paths.
	Name() string
	// Upstream is the default provider base URL (overridable via config).
	Upstream() string
	// ApplyAuth attaches key using the provider's convention. key == "" means
	// no key is configured: the caller's own credentials (already copied onto
	// req) must pass through untouched.
	ApplyAuth(req *http.Request, key string)
	// ExtractUsage pulls token usage and model out of a response body
	// (JSON or SSE).
	ExtractUsage(body []byte) Usage
	// WriteBudgetExceeded writes the synthetic budget 429 (design 001 §4.4)
	// in the provider's native error JSON shape, so the wrapped CLI's own
	// error handling recognises it as a rate limit and exits cleanly rather
	// than treating an unfamiliar body as a crash.
	WriteBudgetExceeded(w http.ResponseWriter, message string)
}

// Usage is the token accounting a Dialect extracts from one response.
type Usage struct {
	InTokens   int
	OutTokens  int
	CacheRead  int
	CacheWrite int
	Model      string
}

// dialects is the by-name registry. Registration happens at init; routing
// stays string-keyed (splitPath → upstreams map) exactly as before.
var dialects = map[string]Dialect{}

func registerDialect(d Dialect) { dialects[d.Name()] = d }

func init() {
	registerDialect(anthropicDialect{})
	registerDialect(openaiDialect{})
	registerDialect(geminiDialect{})
}

// lookupDialect returns the dialect registered under name, or a passthrough
// fallback for names that are routed (present in the upstreams map) but not
// modelled: caller credentials pass through untouched, the shared usage scan
// still applies, and the budget error keeps the default shape.
func lookupDialect(name string) Dialect {
	if d, ok := dialects[name]; ok {
		return d
	}
	return fallbackDialect{name: name}
}

// sharedExtractUsage is the one regex scan every dialect delegates to. The
// three providers' usage field names do not collide, so a single tolerant pass
// is unambiguous — and an OpenAI-compatible gateway that reports another
// dialect's field names still meters correctly (see parseUsage).
func sharedExtractUsage(body []byte) Usage {
	in, out, cr, cw, model := parseUsage(string(body))
	return Usage{InTokens: in, OutTokens: out, CacheRead: cr, CacheWrite: cw, Model: model}
}

// writeBudget429 emits the synthetic rate-limit response around a
// dialect-shaped body. Retry-After advertises the retry window the way
// providers do; the ceiling is a daily budget, so "try again later" is honest
// but deliberately coarse.
func writeBudget429(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "3600")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(body)
}

func marshalErrorBody(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"error":{"message":"rysh budget exceeded"}}`)
	}
	return b
}

// ---- anthropic ----

type anthropicDialect struct{}

func (anthropicDialect) Name() string     { return "anthropic" }
func (anthropicDialect) Upstream() string { return "https://api.anthropic.com" }

func (anthropicDialect) ApplyAuth(req *http.Request, key string) {
	if key != "" {
		req.Header.Set("x-api-key", key)
		// A stale bearer would otherwise take precedence upstream.
		req.Header.Del("Authorization")
	}
	if req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
}

func (anthropicDialect) ExtractUsage(body []byte) Usage { return sharedExtractUsage(body) }

func (anthropicDialect) WriteBudgetExceeded(w http.ResponseWriter, message string) {
	writeBudget429(w, marshalErrorBody(map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    "rate_limit_error",
			"message": message,
		},
	}))
}

// ---- openai ----

type openaiDialect struct{}

func (openaiDialect) Name() string     { return "openai" }
func (openaiDialect) Upstream() string { return "https://api.openai.com" }

func (openaiDialect) ApplyAuth(req *http.Request, key string) {
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Del("x-api-key")
	}
}

func (openaiDialect) ExtractUsage(body []byte) Usage { return sharedExtractUsage(body) }

func (openaiDialect) WriteBudgetExceeded(w http.ResponseWriter, message string) {
	writeBudget429(w, marshalErrorBody(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "rate_limit_exceeded",
			"code":    "rate_limit_exceeded",
			"param":   nil,
		},
	}))
}

// ---- gemini ----

type geminiDialect struct{}

func (geminiDialect) Name() string     { return "gemini" }
func (geminiDialect) Upstream() string { return "https://generativelanguage.googleapis.com" }

func (geminiDialect) ApplyAuth(req *http.Request, key string) {
	if key == "" {
		return
	}
	// Gemini's native REST surface authenticates with x-goog-api-key; its
	// OpenAI-compatibility surface (/v1beta/openai/...) expects a bearer
	// instead. Set the right one for the path being called.
	if strings.Contains(req.URL.Path, "/openai/") {
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Del("x-goog-api-key")
	} else {
		req.Header.Set("x-goog-api-key", key)
		req.Header.Del("Authorization")
	}
	// Some clients pass ?key=; a stale value there would win over the header,
	// so replace it rather than leaving both in play.
	if q := req.URL.Query(); q.Get("key") != "" {
		q.Set("key", key)
		req.URL.RawQuery = q.Encode()
	}
}

func (geminiDialect) ExtractUsage(body []byte) Usage { return sharedExtractUsage(body) }

func (geminiDialect) WriteBudgetExceeded(w http.ResponseWriter, message string) {
	writeBudget429(w, marshalErrorBody(map[string]interface{}{
		"error": map[string]interface{}{
			"code":    429,
			"message": message,
			"status":  "RESOURCE_EXHAUSTED",
		},
	}))
}

// ---- fallback (unregistered but routed) ----

// fallbackDialect preserves the pre-interface behaviour for a dialect name that
// appears in the upstreams map without a registered implementation.
type fallbackDialect struct{ name string }

func (d fallbackDialect) Name() string                  { return d.name }
func (fallbackDialect) Upstream() string                { return "" }
func (fallbackDialect) ApplyAuth(*http.Request, string) {}
func (fallbackDialect) ExtractUsage(body []byte) Usage  { return sharedExtractUsage(body) }
func (fallbackDialect) WriteBudgetExceeded(w http.ResponseWriter, message string) {
	// Same default shape the pre-interface writer emitted for unknown names.
	anthropicDialect{}.WriteBudgetExceeded(w, message)
}
