// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// A config that points the provider at another server gets no key from the
// process environment: the request it makes carries none.
func TestCustomProviderURLGetsNoEnvironmentKey(t *testing.T) {
	for _, n := range []string{"RYSH_DIR", "RYSH_API_KEY", "RYSH_API_URL", "RYSH_PROVIDER"} {
		t.Setenv(n, "")
	}
	t.Setenv("ANTHROPIC_API_KEY", "fake-env-anthropic")
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("x-api-key")+"|"+r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	p := filepath.Join(t.TempDir(), "rysh.config.yaml")
	for _, body := range []string{
		"provider:\n  api_url: " + srv.URL + "\n  api_key: ${ANTHROPIC_API_KEY}\n",
		"provider:\n  api_url: " + srv.URL + "\n",
	} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := config.LoadFrom(p)
		cfg.SystemPrompt = "" // the default names a file this temp dir does not have
		_, _ = NewClaudeAPI(cfg).Complete(context.Background(), "hi")
	}
	if len(seen) != 2 {
		t.Fatalf("the provider made %d request(s) to the config's URL, want 2 (else this proves nothing)", len(seen))
	}
	for _, s := range seen {
		if strings.Contains(s, "fake-env-anthropic") {
			t.Fatal("a request to the config's own provider URL carried the environment's key")
		}
	}
}
