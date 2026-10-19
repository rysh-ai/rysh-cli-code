// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// E15: `##upstream connect` and `##upstream status` tell an over-cap account
// about its session limit, with the upgrade link resolved against the server.
// Informational only: connect still succeeds and writes the config, status
// still prints its table, and a server that is within cap, old or down adds
// nothing.

func capServer(t *testing.T, sessionStatus int, sessionBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/server-info":
			_, _ = w.Write([]byte(`{"workspace":"acme","workspace_id":"` + testWorkspaceUUID + `"}`))
		case "/api/resource/check-session-limit":
			w.WriteHeader(sessionStatus)
			_, _ = w.Write([]byte(sessionBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const overCapJSON = `{"error":"session limit reached (3/3). Upgrade your plan to create more sessions — upgrade at /pricing","upgrade_url":"/pricing"}`

func statusActor(url string) *WorkspaceActor {
	w := &WorkspaceActor{}
	w.cfg.Upstream.Enabled = true
	w.cfg.Upstream.URL = url
	w.cfg.Upstream.APIKey = "rysh_sk_test_key"
	return w
}

func TestUpstreamStatus_WarnsWhenOverSessionCap(t *testing.T) {
	srv := capServer(t, http.StatusPaymentRequired, overCapJSON)
	out := upstreamCmd(t, statusActor(srv.URL), "status")

	for _, want := range []string{
		"  shared panes : 0", // the table is still there
		"[rysh] warning: session limit reached (3/3). Upgrade your plan to create more sessions — upgrade at " + srv.URL + "/pricing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}

func TestUpstreamStatus_NoWarningWithinCapOrWhenServerSilent(t *testing.T) {
	for name, status := range map[string]int{"within cap": http.StatusOK, "old server": http.StatusNotFound, "server error": http.StatusInternalServerError} {
		srv := capServer(t, status, `{"allowed":true}`)
		out := upstreamCmd(t, statusActor(srv.URL), "status")
		if strings.Contains(out, "warning") || strings.Contains(out, "session limit") {
			t.Errorf("%s: unexpected warning:\n%s", name, out)
		}
		if !strings.Contains(out, "  shared panes : 0") {
			t.Errorf("%s: status table missing:\n%s", name, out)
		}
	}
}

func TestUpstreamConnect_WarnsButStillConnectsWhenOverSessionCap(t *testing.T) {
	srv := capServer(t, http.StatusPaymentRequired, overCapJSON)
	path := seedConfigFile(t)
	w := connectActor(path)

	out := upstreamCmd(t, w, "connect", srv.URL, "rysh_sk_test_key")

	if err := w.takeRyshFailure(); err != nil {
		t.Fatalf("an over-cap account must still connect; recorded failure: %v", err)
	}
	if !strings.Contains(readFile(t, path), "workspace: "+testWorkspaceUUID) {
		t.Errorf("config not written:\n%s", readFile(t, path))
	}
	for _, want := range []string{
		"upstream connected",
		"[rysh] warning: session limit reached (3/3). Upgrade your plan to create more sessions — upgrade at " + srv.URL + "/pricing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("connect output missing %q:\n%s", want, out)
		}
	}
	// The warning comes after the success block, not instead of it.
	if strings.Index(out, "warning:") < strings.Index(out, "upstream connected") {
		t.Errorf("warning must follow the success block:\n%s", out)
	}
}

func TestUpstreamConnect_NoWarningWithinCap(t *testing.T) {
	srv := capServer(t, http.StatusOK, `{"allowed":true}`)
	w := connectActor(seedConfigFile(t))
	out := upstreamCmd(t, w, "connect", srv.URL, "rysh_sk_test_key")
	if strings.Contains(out, "warning") || strings.Contains(out, "session limit") {
		t.Errorf("unexpected warning within cap:\n%s", out)
	}
}
