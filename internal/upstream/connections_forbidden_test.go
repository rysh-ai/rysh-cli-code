// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// D-10 / E31-R piece 6: rysh-server answers 403 to the credentials fetch when
// the key belongs to an observer. That refusal is distinguishable from every
// other failure (CredentialsForbiddenError, carrying the server's message), so
// the humanoid can refuse to start instead of starting without its token.

const observerRefusal = "channel credentials need the member role or higher; this key belongs to an observer"

func cfgFor(url string) config.Config {
	var cfg config.Config
	cfg.Upstream.Enabled = true
	cfg.Upstream.URL = url
	cfg.Upstream.APIKey = "rysh_test_observer_key"
	cfg.Upstream.Workspace = "ws-1"
	return cfg
}

func TestFetchConnectionCredentials403IsForbiddenWithServerMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/workspaces/ws-1/connections/by-type/slack/credentials") {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"` + observerRefusal + `"}`))
	}))
	defer srv.Close()

	_, err := FetchConnectionCredentials(cfgFor(srv.URL), "slack")
	var forbidden *CredentialsForbiddenError
	if !errors.As(err, &forbidden) {
		t.Fatalf("403: err = %v (%T), want *CredentialsForbiddenError", err, err)
	}
	if forbidden.Message != observerRefusal {
		t.Errorf("message = %q, want the server's %q", forbidden.Message, observerRefusal)
	}
}

func TestFetchConnectionCredentials500IsNotForbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	_, err := FetchConnectionCredentials(cfgFor(srv.URL), "slack")
	var forbidden *CredentialsForbiddenError
	if err == nil || errors.As(err, &forbidden) {
		t.Fatalf("500: err = %v, want an ordinary error (not CredentialsForbiddenError)", err)
	}
}
