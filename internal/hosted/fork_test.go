// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Fork (boat gap #4): POST .../hosted-runs/<id>/fork, the key in the body
// only, an Idempotency-Key, and the forks back with fork_of.
func TestForkPostsCountKeyAndReadsTheForks(t *testing.T) {
	var gotPath, gotMethod, gotAuth, gotIdem string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotAuth, gotIdem = r.URL.Path, r.Method, r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key")
		_ = json.NewDecoder(r.Body).Decode(&body)
		f1, f2 := testRun(), testRun()
		f1.ID, f1.ForkOf, f1.Name = "aaaa000000000001", "423b54f6fd36fa94", "work-f1"
		f2.ID, f2.ForkOf, f2.Name, f2.State = "aaaa000000000002", "423b54f6fd36fa94", "work-f2", "failed"
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"runs": []Run{f1, f2}})
	}))
	defer srv.Close()
	c := &Client{Conn: Conn{URL: srv.URL, APIKey: "k-secret", WorkspaceID: "ws1"}}
	runs, err := c.Fork(context.Background(), "423b54f6fd36fa94", 2, StartOptions{LLMProvider: "openai", LLMKey: "sk-fork"}, "ssh-ed25519 AAAA pub")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/workspaces/ws1/hosted-runs/423b54f6fd36fa94/fork" {
		t.Fatalf("%s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer k-secret" || gotIdem == "" {
		t.Fatalf("auth=%q idem=%q", gotAuth, gotIdem)
	}
	if body["count"] != float64(2) || body["ssh_public_key"] != "ssh-ed25519 AAAA pub" || body["llm_provider"] != "openai" || body["llm_api_key"] != "sk-fork" {
		t.Fatalf("body = %v", body)
	}
	if _, ok := body["name"]; ok {
		t.Fatalf("a fork body has no name/prompt: %v", body)
	}
	if len(runs) != 2 || runs[0].ForkOf != "423b54f6fd36fa94" || runs[1].State != "failed" {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestForkRefusesABadRunIDAndScrubsTheKey(t *testing.T) {
	c := &Client{Conn: Conn{URL: "https://rysh.example", APIKey: "k", WorkspaceID: "ws1"}}
	if _, err := c.Fork(context.Background(), "../x", 1, StartOptions{}, "pub"); err == nil {
		t.Fatal("a bad run id was sent")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"echo sk-fork-secret"}`))
	}))
	defer srv.Close()
	c = &Client{Conn: Conn{URL: srv.URL, APIKey: "k", WorkspaceID: "ws1"}}
	_, err := c.Fork(context.Background(), "423b54f6fd36fa94", 1, StartOptions{LLMProvider: "openai", LLMKey: "sk-fork-secret"}, "pub")
	if err == nil || strings.Contains(err.Error(), "sk-fork-secret") {
		t.Fatalf("err = %v", err)
	}
	// 404: not enabled, unknown, or not your run — said plainly.
	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer srv404.Close()
	c = &Client{Conn: Conn{URL: srv404.URL, APIKey: "k", WorkspaceID: "ws1"}}
	if _, err := c.Fork(context.Background(), "423b54f6fd36fa94", 1, StartOptions{}, "pub"); err == nil || !strings.Contains(err.Error(), "fork") {
		t.Fatalf("404: err = %v", err)
	}
}
