// SPDX-License-Identifier: Apache-2.0

package provider

// Tests for the Gemini provider wiring (design 002, roadmap B6). Gemini rides
// the shared OpenAI-compatible adapter through its /v1beta/openai surface, so
// what needs pinning is (a) SELECTION — RYSH_PROVIDER=gemini must reach a real
// provider, not the Claude default branch or a mock — and (b) the wire
// contract against an httptest fake: endpoint path, Bearer auth header (never
// a ?key= query, which is the native Gemini API's style), defaults, tool
// translation, and response/usage parsing. No live key is ever used.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// TestGeminiSelectable is the reachability guard: before B6, "gemini" fell
// into the Claude default branch of both selectors, so the name existed in
// docs but selected the wrong provider. Reverting the factory wiring makes
// this fail.
func TestGeminiSelectable(t *testing.T) {
	ag := NewAgenticProvider(config.Config{ProviderName: "gemini"})
	if got := ag.Name(); got != "gemini" {
		t.Fatalf("NewAgenticProvider(gemini).Name() = %q, want %q", got, "gemini")
	}
	// Capability table row: the OpenAI-compat dialect calls tools, so no
	// degradation path may fire for Gemini.
	if !SupportsTools(ag) {
		t.Error("gemini provider must report tool support")
	}
	// The simple (judge-seat) selector must stay in lockstep.
	if got := New(config.Config{ProviderName: "gemini"}).Name(); got != "gemini" {
		t.Fatalf("New(gemini).Name() = %q, want %q", got, "gemini")
	}
	if !RequiresAPIKey("gemini") {
		t.Error("gemini is a hosted API — it must require a key (no silent keyless mock)")
	}
	if !IsKnownProviderName("gemini") || !IsKnownProviderName("GEMINI") {
		t.Error("gemini must be a known provider name (case-insensitive)")
	}
}

func TestGeminiDefaultEndpointIsOpenAICompat(t *testing.T) {
	const want = "https://generativelanguage.googleapis.com/v1beta/openai"
	if GeminiAPIURL != want {
		t.Fatalf("GeminiAPIURL = %q, want %q", GeminiAPIURL, want)
	}
}

// TestGeminiRequestShape drives one tool-calling turn against a fake server
// and pins the whole wire contract.
func TestGeminiRequestShape(t *testing.T) {
	var (
		gotPath, gotAuth, gotQuery string
		gotBody                    map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"choices":[{"message":{"content":"","tool_calls":[
				{"id":"call-1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}
			]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":11,"completion_tokens":7}
		}`)
	}))
	defer srv.Close()

	ag := NewAgenticProvider(config.Config{
		ProviderName: "gemini",
		APIURL:       srv.URL,
		APIKey:       "gk-test-key",
	})
	resp, err := ag.CompleteWithTools(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "list files"}},
		[]ToolSpec{{Name: "bash", Description: "run a command", Parameters: json.RawMessage(`{"type":"object"}`)}},
		"system prompt here",
	)
	if err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}

	// Request shape.
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer gk-test-key" {
		t.Errorf("Authorization = %q, want Bearer header", gotAuth)
	}
	if strings.Contains(gotQuery, "key") {
		t.Errorf("API key must never travel in the query string, got %q", gotQuery)
	}
	if got := gotBody["model"]; got != "gemini-2.5-flash" {
		t.Errorf("default model = %v, want gemini-2.5-flash", got)
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want [system, user]", gotBody["messages"])
	}
	if first, _ := msgs[0].(map[string]any); first["role"] != "system" || first["content"] != "system prompt here" {
		t.Errorf("first message = %v, want the system prompt", msgs[0])
	}
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want the one translated spec", gotBody["tools"])
	}
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "bash" {
		t.Errorf("tool function = %v, want bash", fn)
	}

	// Response parsing.
	if resp.StopReason != StopReasonToolUse {
		t.Errorf("StopReason = %v, want tool use", resp.StopReason)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "bash" || resp.ToolCalls[0].ID != "call-1" {
		t.Errorf("ToolCalls = %+v, want the bash call", resp.ToolCalls)
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 {
		t.Errorf("Usage = %+v, want in=11 out=7", resp.Usage)
	}
}

// TestGeminiErrorNeverEchoesKey pins the no-key-leak rule on the failure path:
// an upstream 401 surfaces as an error carrying the server's words, never the
// credential we sent.
func TestGeminiErrorNeverEchoesKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"API key not valid"}}`)
	}))
	defer srv.Close()

	ag := NewAgenticProvider(config.Config{ProviderName: "gemini", APIURL: srv.URL, APIKey: "gk-secret"})
	_, err := ag.CompleteWithTools(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "hi"}}, nil, "")
	if err == nil {
		t.Fatal("expected an error from the 401 response")
	}
	if strings.Contains(err.Error(), "gk-secret") {
		t.Fatalf("error text leaks the API key: %v", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should carry the upstream status, got: %v", err)
	}
}
