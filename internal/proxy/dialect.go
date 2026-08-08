package proxy

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
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
	// WriteRateLimited writes a synthetic 429 in the provider's native error
	// JSON shape, so the wrapped CLI's own error handling recognises it as a
	// rate limit and exits cleanly rather than treating an unfamiliar body as
	// a crash.
	//
	// It serves both refusals rysh can issue — the token ceiling (design 001
	// §4.4) and the request-rate limiter (design 022 §4.2) — because on the
	// wire they are the same thing: a rate limit. They differ only in the
	// message and in how long the caller should wait, which is why retryAfter
	// is the caller's to supply. A ceiling is a daily figure and answers
	// coarsely; a token bucket knows its delay exactly.
	WriteRateLimited(w http.ResponseWriter, message string, retryAfter time.Duration)
	// ErrorBody renders message as this provider's native error JSON for an
	// arbitrary status. Same reasoning as WriteRateLimited: every refusal rysh
	// issues — an unknown pane in the path, a model the allowlist forbids —
	// lands in a wrapped CLI's error handling, and a body it cannot parse turns
	// a clean refusal into a crash.
	ErrorBody(status int, message string) []byte
	// AdaptRequestBody may rewrite the (already redacted) request body before it
	// is forwarded, for the provider-specific adjustment metering requires —
	// today only OpenAI's `stream_options.include_usage` (design 001 §4.2).
	// path is the upstream path being called, so an adapter can scope itself to
	// the endpoints it understands. Returning body unchanged is the norm.
	AdaptRequestBody(path string, body []byte) []byte
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

// write429 emits the synthetic rate-limit response around a dialect-shaped
// body. Retry-After advertises the retry window the way providers do, so a
// wrapped CLI's own backoff has something real to work from.
func write429(w http.ResponseWriter, body []byte, retryAfter time.Duration) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", retryAfterSeconds(retryAfter))
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(body)
}

// retryAfterSeconds renders a duration as the integer seconds Retry-After
// requires. It rounds UP, so a client that waits exactly the advertised time is
// never refused again for the same reason — rounding down would hand out a
// number that is guaranteed to fail. Sub-second and non-positive values floor
// at 1: "0" reads as "retry immediately", which is how a limiter turns into a
// retry storm.
func retryAfterSeconds(d time.Duration) string {
	secs := int64(math.Ceil(d.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return strconv.FormatInt(secs, 10)
}

// writeDialectError writes a provider-shaped error with an arbitrary status.
// Every non-429 refusal the proxy issues goes through here, so a wrapped CLI
// always gets a body its own error path understands.
func writeDialectError(w http.ResponseWriter, d Dialect, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(d.ErrorBody(status, message))
}

// ---- request adaptation ----

var (
	reStreamTrue    = regexp.MustCompile(`"stream"\s*:\s*true`)
	reStreamOptions = regexp.MustCompile(`"stream_options"\s*:`)
)

// injectStreamUsageOptions adds `"stream_options":{"include_usage":true}` to a
// streaming OpenAI chat/completions request.
//
// Design 001 §4.2 requires it and it was never built, so EVERY streaming
// OpenAI-dialect CLI — which is to say every one of them in normal use —
// metered 0 in / 0 out: OpenAI omits the usage block from a stream unless it is
// asked for. `##cost` under-reported instead of failing visibly, and no token
// ceiling could ever bind on those panes.
//
// The insertion is surgical rather than a decode/re-encode round trip: the body
// is already SNAT-redacted and is replayed byte-for-byte by failover, and
// re-marshalling would rewrite number formatting and key order for every
// request in order to add one field to some.
//
// Scoped to the chat/completions surface on purpose. The Responses API reports
// usage on its own and does not accept stream_options; sending it there would
// turn a metering fix into a 400 on every request.
func injectStreamUsageOptions(path string, body []byte) []byte {
	if len(body) == 0 || !strings.Contains(path, "chat/completions") {
		return body
	}
	if !reStreamTrue.Match(body) || reStreamOptions.Match(body) {
		return body
	}
	end := bytes.LastIndexByte(body, '}')
	if end < 0 {
		return body // not a JSON object; leave it exactly as it came
	}
	out := make([]byte, 0, len(body)+40)
	out = append(out, body[:end]...)
	out = append(out, `,"stream_options":{"include_usage":true}`...)
	out = append(out, body[end:]...)
	if !json.Valid(out) {
		return body
	}
	return out
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

// Anthropic reports usage on message_start/message_delta without being asked.
func (anthropicDialect) AdaptRequestBody(_ string, body []byte) []byte { return body }

// anthropicErrorType maps an HTTP status onto Anthropic's own error `type`
// vocabulary, which its SDKs switch on.
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}

func (anthropicDialect) ErrorBody(status int, message string) []byte {
	return marshalErrorBody(map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    anthropicErrorType(status),
			"message": message,
		},
	})
}

func (anthropicDialect) WriteRateLimited(w http.ResponseWriter, message string, retryAfter time.Duration) {
	write429(w, marshalErrorBody(map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    "rate_limit_error",
			"message": message,
		},
	}), retryAfter)
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

func (openaiDialect) AdaptRequestBody(path string, body []byte) []byte {
	return injectStreamUsageOptions(path, body)
}

func (openaiDialect) ErrorBody(status int, message string) []byte {
	code := "invalid_request_error"
	switch status {
	case http.StatusForbidden:
		code = "permission_denied"
	case http.StatusUnauthorized:
		code = "invalid_api_key"
	case http.StatusTooManyRequests:
		code = "rate_limit_exceeded"
	}
	return marshalErrorBody(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "invalid_request_error",
			"code":    code,
			"param":   nil,
		},
	})
}

func (openaiDialect) WriteRateLimited(w http.ResponseWriter, message string, retryAfter time.Duration) {
	write429(w, marshalErrorBody(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "rate_limit_exceeded",
			"code":    "rate_limit_exceeded",
			"param":   nil,
		},
	}), retryAfter)
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

// Gemini reports usageMetadata on the final chunk unprompted; its
// OpenAI-compatibility surface is served by the openai dialect.
func (geminiDialect) AdaptRequestBody(_ string, body []byte) []byte { return body }

// geminiStatus maps an HTTP status onto the google.rpc.Code name Google's own
// errors carry alongside it.
func geminiStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	default:
		return "INTERNAL"
	}
}

func (geminiDialect) ErrorBody(status int, message string) []byte {
	return marshalErrorBody(map[string]interface{}{
		"error": map[string]interface{}{
			"code":    status,
			"message": message,
			"status":  geminiStatus(status),
		},
	})
}

func (geminiDialect) WriteRateLimited(w http.ResponseWriter, message string, retryAfter time.Duration) {
	write429(w, marshalErrorBody(map[string]interface{}{
		"error": map[string]interface{}{
			"code":    429,
			"message": message,
			"status":  "RESOURCE_EXHAUSTED",
		},
	}), retryAfter)
}

// ---- fallback (unregistered but routed) ----

// fallbackDialect preserves the pre-interface behaviour for a dialect name that
// appears in the upstreams map without a registered implementation.
type fallbackDialect struct{ name string }

func (d fallbackDialect) Name() string                  { return d.name }
func (fallbackDialect) Upstream() string                { return "" }
func (fallbackDialect) ApplyAuth(*http.Request, string) {}
func (fallbackDialect) ExtractUsage(body []byte) Usage  { return sharedExtractUsage(body) }
func (fallbackDialect) WriteRateLimited(w http.ResponseWriter, message string, retryAfter time.Duration) {
	// Same default shape the pre-interface writer emitted for unknown names.
	anthropicDialect{}.WriteRateLimited(w, message, retryAfter)
}

func (fallbackDialect) AdaptRequestBody(_ string, body []byte) []byte { return body }

func (fallbackDialect) ErrorBody(status int, message string) []byte {
	return anthropicDialect{}.ErrorBody(status, message)
}
