// SPDX-License-Identifier: Apache-2.0

package embed

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/memory/vecindex"
)

// stubOllama is a loopback stand-in for Ollama's POST /api/embed. Each input's
// vector is [len(input), 1, 0, …] so order mistakes show. No model, no network
// beyond 127.0.0.1.
func stubOllama(t *testing.T, dim int) (*httptest.Server, *[]embedRequest) {
	t.Helper()
	var mu sync.Mutex
	var got []embedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/embed" {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		mu.Lock()
		got = append(got, req)
		mu.Unlock()
		out := embedResponse{Embeddings: make([][]float32, len(req.Input))}
		for i, s := range req.Input {
			v := make([]float32, dim)
			v[0], v[1] = float32(len(s)), 1
			out.Embeddings[i] = v
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestOllamaEmbedsInOrderAndBatches(t *testing.T) {
	srv, got := stubOllama(t, 8)
	o, err := NewOllama(srv.URL, "nomic-embed-text", 8)
	if err != nil {
		t.Fatalf("NewOllama: %v", err)
	}
	var _ vecindex.Embedder = o
	o.batch = 2
	texts := []string{"a", "bb", "ccc", "dddd", "eeeee"}
	vecs, err := o.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != len(texts) {
		t.Fatalf("got %d vectors, want %d", len(vecs), len(texts))
	}
	for i, v := range vecs {
		if len(v) != 8 || v[0] != float32(len(texts[i])) {
			t.Errorf("vector %d out of order or wrong shape", i)
		}
	}
	if len(*got) != 3 {
		t.Errorf("requests = %d, want 3 (batches of 2)", len(*got))
	}
	for _, r := range *got {
		if r.Model != "nomic-embed-text" {
			t.Errorf("model = %q", r.Model)
		}
	}
	if o.Dim() != 8 || o.ID() != "ollama:nomic-embed-text:8" {
		t.Errorf("Dim/ID = %d/%q", o.Dim(), o.ID())
	}
	if v, err := o.Embed(context.Background(), nil); err != nil || v != nil {
		t.Errorf("empty input: %v, %v", v, err)
	}
}

func TestOllamaRejectsWrongDimension(t *testing.T) {
	srv, _ := stubOllama(t, 4)
	o, err := NewOllama(srv.URL, "m", 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Embed(context.Background(), []string{"x"}); !errors.Is(err, vecindex.ErrDimension) {
		t.Errorf("err = %v, want ErrDimension", err)
	}
}

// TestOllamaIsLoopbackOnly: air-gapped by construction. A non-loopback base
// URL is refused before any request, and the dialer refuses any non-loopback
// address it is ever handed (checked by calling the guard, never by dialing).
func TestOllamaIsLoopbackOnly(t *testing.T) {
	for _, u := range []string{
		"http://example.com:11434", "http://10.1.2.3:11434", "http://[2001:db8::1]:11434",
		"http://0.0.0.0:11434", "ftp://127.0.0.1/", "http://user:pw@127.0.0.1:11434",
	} {
		if _, err := NewOllama(u, "m", 8); !errors.Is(err, ErrNotLoopback) {
			t.Errorf("%s: err = %v, want ErrNotLoopback", u, err)
		}
	}
	for _, u := range []string{"", "http://127.0.0.1:11434", "http://localhost:11434", "http://[::1]:11434"} {
		if _, err := NewOllama(u, "m", 8); err != nil {
			t.Errorf("%q: %v", u, err)
		}
	}
	for _, a := range []string{"10.1.2.3:11434", "93.184.216.34:80", "[2001:db8::1]:443", "0.0.0.0:11434"} {
		if err := loopbackOnly("tcp", a, nil); !errors.Is(err, ErrNotLoopback) {
			t.Errorf("dial guard let %s through", a)
		}
	}
	for _, a := range []string{"127.0.0.1:11434", "[::1]:11434"} {
		if err := loopbackOnly("tcp", a, nil); err != nil {
			t.Errorf("dial guard refused %s: %v", a, err)
		}
	}
	o, _ := NewOllama("", "m", 8)
	tr, ok := o.client.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Errorf("transport must not use a proxy (HTTP_PROXY would leave the machine)")
	}
}

func TestOllamaRefusesRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.1.2.3:11434/api/embed", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	o, _ := NewOllama(srv.URL, "m", 8)
	if _, err := o.Embed(context.Background(), []string{"x"}); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("redirect followed or unreported: %v", err)
	}
}

// TestOllamaRemoteHostNeedsExplicitOptIn: a remote embedder is never a
// default. Only AllowRemoteHost() lifts the loopback rule, and then only for
// https without credentials; proxy and redirects stay off.
func TestOllamaRemoteHostNeedsExplicitOptIn(t *testing.T) {
	if _, err := NewOllama("https://embed.example.com", "m", 8); !errors.Is(err, ErrNotLoopback) {
		t.Errorf("remote without opt-in: err = %v, want ErrNotLoopback", err)
	}
	o, err := NewOllama("https://embed.example.com", "m", 8, AllowRemoteHost())
	if err != nil {
		t.Fatalf("remote with opt-in: %v", err)
	}
	if tr := o.client.Transport.(*http.Transport); tr.Proxy != nil {
		t.Errorf("opt-in must not enable a proxy")
	}
	for _, u := range []string{"http://embed.example.com", "https://u:p@embed.example.com"} {
		if _, err := NewOllama(u, "m", 8, AllowRemoteHost()); err == nil {
			t.Errorf("%s: accepted with opt-in, want refused (https only, no credentials)", u)
		}
	}
	// The opt-in does not change the default for anyone else.
	if _, err := NewOllama("https://embed.example.com", "m", 8); !errors.Is(err, ErrNotLoopback) {
		t.Errorf("default changed after an opt-in")
	}
}
