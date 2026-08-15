// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
)

func TestGraphQLExecutorQuery(t *testing.T) {
	var gotQuery string
	var gotVars map[string]any
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(b, &body)
		gotQuery = body.Query
		gotVars = body.Variables
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"user":{"id":"u1","name":"Ada","token":"secret"}}}`))
	}))
	defer srv.Close()

	auth := []ir.AuthScheme{{Name: "k", Type: "http", Scheme: "bearer"}}
	exec := NewGraphQLExecutor(srv.URL, auth, Credential{APIKey: "tok"}, Options{
		Redact: func(s string) string { return strings.ReplaceAll(s, "secret", "[redacted]") },
	})

	out, err := exec.Query(context.Background(),
		"query($id:ID!){ user(id:$id){ id name } }",
		map[string]any{"id": "u1"}, ".data.user.id")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !strings.Contains(gotQuery, "user(id:$id)") {
		t.Fatalf("query not forwarded: %q", gotQuery)
	}
	if gotVars["id"] != "u1" {
		t.Fatalf("variables not forwarded: %v", gotVars)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("bearer auth not injected: %q", gotAuth)
	}
	if strings.TrimSpace(out) != `"u1"` {
		t.Fatalf("jq_filter .data.user.id = %q, want \"u1\"", out)
	}
}

func TestGraphQLExecutorSurfacesErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 200 OK but with a GraphQL errors array.
		w.Write([]byte(`{"errors":[{"message":"field 'x' not found"}],"data":null}`))
	}))
	defer srv.Close()
	exec := NewGraphQLExecutor(srv.URL, nil, Credential{}, Options{})
	_, err := exec.Query(context.Background(), "query { x }", nil, "")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected graphql error surfaced, got %v", err)
	}
}

func TestGraphQLExecutorRequiresQuery(t *testing.T) {
	exec := NewGraphQLExecutor("http://x", nil, Credential{}, Options{})
	if _, err := exec.Query(context.Background(), "  ", nil, ""); err == nil {
		t.Fatalf("expected error for empty query")
	}
}
