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

func TestApplyJQ(t *testing.T) {
	doc := []byte(`{"data":[{"id":"a","name":"Alice"},{"id":"b","name":"Bob"}],"total":2}`)
	cases := []struct {
		filter, want string
		wantErr      bool
	}{
		{".", "", false},
		{".total", "2", false},
		{".data[0].id", "\"a\"", false},
		{".data[].id", "[\n  \"a\",\n  \"b\"\n]", false},
		{".missing", "", true},
		{".data[9]", "", true},
	}
	for _, c := range cases {
		got, err := ApplyJQ(doc, c.filter)
		if c.wantErr {
			if err == nil {
				t.Errorf("ApplyJQ(%q) expected error, got %q", c.filter, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ApplyJQ(%q) error: %v", c.filter, err)
			continue
		}
		if c.want != "" && got != c.want {
			t.Errorf("ApplyJQ(%q) = %q, want %q", c.filter, got, c.want)
		}
	}
}

func TestHTTPExecutorEndToEnd(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotAuth = r.Header.Get("X-Api-Key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"cus_1","email":"x@y.z","secret":"shh"}`))
	}))
	defer srv.Close()

	auth := []ir.AuthScheme{{Name: "k", Type: "apiKey", In: "header", KeyName: "X-Api-Key"}}
	exec := NewHTTPExecutor(srv.URL, auth, Credential{APIKey: "sk_test"}, Options{
		Redact: func(s string) string { return strings.ReplaceAll(s, "shh", "[redacted]") },
	})

	// GET with path + query params.
	get := &ir.Operation{ID: "get_customer", Method: "GET", Path: "/v1/customers/{id}", Params: []ir.Param{
		{Name: "id", In: "path", Required: true},
		{Name: "expand", In: "query"},
	}}
	out, err := exec.Call(context.Background(), get, map[string]any{"id": "cus_1", "expand": "card"}, "")
	if err != nil {
		t.Fatalf("GET Call: %v", err)
	}
	if gotMethod != "GET" || gotPath != "/v1/customers/cus_1?expand=card" {
		t.Fatalf("bad request: %s %s", gotMethod, gotPath)
	}
	if gotAuth != "sk_test" {
		t.Fatalf("auth header not injected: %q", gotAuth)
	}
	if !strings.Contains(out, "[redacted]") || strings.Contains(out, "shh") {
		t.Fatalf("redaction not applied: %s", out)
	}

	// POST with body args + jq_filter.
	post := &ir.Operation{ID: "create_customer", Method: "POST", Path: "/v1/customers", Mutating: true}
	out, err = exec.Call(context.Background(), post, map[string]any{"email": "a@b.c", "name": "A"}, ".id")
	if err != nil {
		t.Fatalf("POST Call: %v", err)
	}
	if gotMethod != "POST" {
		t.Fatalf("expected POST, got %s", gotMethod)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("body not JSON: %q", gotBody)
	}
	if body["email"] != "a@b.c" || body["name"] != "A" {
		t.Fatalf("body args wrong: %v", body)
	}
	if strings.TrimSpace(out) != `"cus_1"` {
		t.Fatalf("jq_filter .id = %q, want \"cus_1\"", out)
	}
}

func TestHTTPExecutorErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()
	exec := NewHTTPExecutor(srv.URL, nil, Credential{}, Options{})
	op := &ir.Operation{ID: "x", Method: "GET", Path: "/x"}
	_, err := exec.Call(context.Background(), op, nil, "")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected 404 error, got %v", err)
	}
}
