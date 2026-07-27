package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// capturedReq records what the fake provider actually received.
type capturedReq struct {
	authorization string
	xAPIKey       string
	xGoogAPIKey   string
	queryKey      string
	hits          int
}

func newFakeProvider(t *testing.T, got *capturedReq) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.hits++
		got.authorization = r.Header.Get("Authorization")
		got.xAPIKey = r.Header.Get("x-api-key")
		got.xGoogAPIKey = r.Header.Get("x-goog-api-key")
		got.queryKey = r.URL.Query().Get("key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
}

// TestApplyAuth_OpenAINeverReceivesTheAnthropicKey is the regression guard for
// defect 6. The proxy held ONE key and applied it to both the anthropic and
// openai dialects, so enabling governance sent an Anthropic key as the OpenAI
// bearer — every wrapped OpenAI CLI got a 401 the moment `##proxy on` ran.
func TestApplyAuth_OpenAINeverReceivesTheAnthropicKey(t *testing.T) {
	var openaiGot capturedReq
	openai := newFakeProvider(t, &openaiGot)
	defer openai.Close()

	// Only an Anthropic key is configured — the exact setup that broke.
	s := New(nil, nil,
		map[string]string{"anthropic": "sk-ant-real"},
		map[string]string{"openai": openai.URL}, false)

	req := httptest.NewRequest(http.MethodPost, "/openai/pane-1/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer sk-user-own-openai-key")
	s.ServeHTTP(httptest.NewRecorder(), req)

	if openaiGot.hits != 1 {
		t.Fatalf("expected the request to be forwarded, got %d hits", openaiGot.hits)
	}
	if strings.Contains(openaiGot.authorization, "sk-ant-real") {
		t.Fatal("the Anthropic key was sent as the OpenAI bearer — defect 6 has returned")
	}
	if openaiGot.authorization != "Bearer sk-user-own-openai-key" {
		t.Fatalf("with no OpenAI key configured the caller's own credential must pass "+
			"through untouched, got %q", openaiGot.authorization)
	}
}

func TestApplyAuth_PerDialectKeys(t *testing.T) {
	var anthropicGot, openaiGot, geminiGot capturedReq
	anthropic := newFakeProvider(t, &anthropicGot)
	defer anthropic.Close()
	openai := newFakeProvider(t, &openaiGot)
	defer openai.Close()
	gemini := newFakeProvider(t, &geminiGot)
	defer gemini.Close()

	s := New(nil, nil,
		map[string]string{
			"anthropic": "sk-ant-real",
			"openai":    "sk-oai-real",
			"gemini":    "gem-real",
		},
		map[string]string{
			"anthropic": anthropic.URL,
			"openai":    openai.URL,
			"gemini":    gemini.URL,
		}, false)

	// Each pane sends the dummy placeholder; the proxy swaps in the real key.
	for _, tc := range []struct{ path, header, value string }{
		{"/anthropic/p/v1/messages", "x-api-key", config.ProxyDummyKey},
		{"/openai/p/v1/chat/completions", "Authorization", "Bearer " + config.ProxyDummyKey},
		{"/gemini/p/v1beta/models/gemini-2.0:generateContent", "x-goog-api-key", config.ProxyDummyKey},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{}`))
		req.Header.Set(tc.header, tc.value)
		s.ServeHTTP(httptest.NewRecorder(), req)
	}

	if anthropicGot.xAPIKey != "sk-ant-real" {
		t.Errorf("anthropic must use x-api-key with its own key, got %q", anthropicGot.xAPIKey)
	}
	if anthropicGot.authorization != "" {
		t.Errorf("a stale bearer must be cleared for anthropic, got %q", anthropicGot.authorization)
	}
	if openaiGot.authorization != "Bearer sk-oai-real" {
		t.Errorf("openai must use its own bearer, got %q", openaiGot.authorization)
	}
	if geminiGot.xGoogAPIKey != "gem-real" {
		t.Errorf("gemini native REST must use x-goog-api-key, got %q", geminiGot.xGoogAPIKey)
	}

	// No dialect may ever forward the placeholder.
	for name, v := range map[string]string{
		"anthropic": anthropicGot.xAPIKey,
		"openai":    openaiGot.authorization,
		"gemini":    geminiGot.xGoogAPIKey,
	} {
		if strings.Contains(v, config.ProxyDummyKey) {
			t.Errorf("%s forwarded the dummy placeholder upstream: %q", name, v)
		}
	}
}

// TestApplyAuth_GeminiOpenAICompatSurfaceUsesBearer covers Gemini's dual
// authentication surfaces.
func TestApplyAuth_GeminiOpenAICompatSurfaceUsesBearer(t *testing.T) {
	var got capturedReq
	gemini := newFakeProvider(t, &got)
	defer gemini.Close()

	s := New(nil, nil,
		map[string]string{"gemini": "gem-real"},
		map[string]string{"gemini": gemini.URL}, false)

	req := httptest.NewRequest(http.MethodPost,
		"/gemini/p/v1beta/openai/chat/completions", strings.NewReader(`{}`))
	s.ServeHTTP(httptest.NewRecorder(), req)

	if got.authorization != "Bearer gem-real" {
		t.Fatalf("gemini's OpenAI-compat surface expects a bearer, got %q", got.authorization)
	}
	if got.xGoogAPIKey != "" {
		t.Fatalf("x-goog-api-key must not be set on the compat surface, got %q", got.xGoogAPIKey)
	}
}

// TestApplyAuth_GeminiQueryKeyIsReplaced ensures a stale ?key= cannot win over
// the header the proxy just set.
func TestApplyAuth_GeminiQueryKeyIsReplaced(t *testing.T) {
	var got capturedReq
	gemini := newFakeProvider(t, &got)
	defer gemini.Close()

	s := New(nil, nil,
		map[string]string{"gemini": "gem-real"},
		map[string]string{"gemini": gemini.URL}, false)

	req := httptest.NewRequest(http.MethodPost,
		"/gemini/p/v1beta/models/x:generateContent?key="+config.ProxyDummyKey,
		strings.NewReader(`{}`))
	s.ServeHTTP(httptest.NewRecorder(), req)

	if got.queryKey == config.ProxyDummyKey {
		t.Fatal("the dummy placeholder leaked upstream in the ?key= parameter")
	}
	if got.queryKey != "gem-real" {
		t.Fatalf("?key= should be rewritten to the real key, got %q", got.queryKey)
	}
}

// TestParseUsage_AllDialects is the metering half of defect 6: before this,
// every wrapped non-Anthropic CLI metered as ZERO, so `##cost` silently
// under-reported and budget ceilings could never fire for those panes.
func TestParseUsage_AllDialects(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantIn        int
		wantOut       int
		wantModelPart string
	}{
		{
			name:          "anthropic",
			body:          `{"model":"claude-sonnet-5","usage":{"input_tokens":100,"output_tokens":50}}`,
			wantIn:        100,
			wantOut:       50,
			wantModelPart: "claude",
		},
		{
			name:          "openai chat completions",
			body:          `{"model":"gpt-4o","usage":{"prompt_tokens":120,"completion_tokens":30}}`,
			wantIn:        120,
			wantOut:       30,
			wantModelPart: "gpt-4o",
		},
		{
			name:          "gemini native",
			body:          `{"modelVersion":"gemini-2.0-flash","usageMetadata":{"promptTokenCount":77,"candidatesTokenCount":22}}`,
			wantIn:        77,
			wantOut:       22,
			wantModelPart: "gemini",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in, out, _, _, model := parseUsage(tc.body)
			if in != tc.wantIn || out != tc.wantOut {
				t.Fatalf("tokens = %d in / %d out, want %d/%d", in, out, tc.wantIn, tc.wantOut)
			}
			if !strings.Contains(model, tc.wantModelPart) {
				t.Fatalf("model = %q, want something containing %q", model, tc.wantModelPart)
			}
		})
	}
}
