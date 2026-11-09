// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// desktop (CD3 freeze run): the desktop app's connections were recorded as
// vehicle "terminal". `connect --vehicle desktop|terminal` names it; terminal
// stays the default and any other value is refused before anything is sent.
func TestConnectVehicleFlag(t *testing.T) {
	t.Setenv("RYSH_SESSION", "")
	t.Setenv("RYSH_UPSTREAM_API_KEY", "")
	t.Setenv("RYSH_DIR", "")
	var vehicles []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct{ Vehicle string }
		_ = json.Unmarshal(b, &body)
		vehicles = append(vehicles, body.Vehicle)
		w.WriteHeader(http.StatusBadRequest) // only the request matters here
		_, _ = w.Write([]byte(`{"error":"stop"}`))
	}))
	defer srv.Close()
	run := func(extra ...string) error {
		dir := filepath.Join(t.TempDir(), "co")
		args := append([]string{srv.URL, "rysh_ct_good", "--device", "test-box", "--dir", dir, "--no-open"}, extra...)
		return runConnect(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), args)
	}

	_ = run()
	_ = run("--vehicle", "desktop")
	_ = run("--vehicle", "terminal")
	if len(vehicles) != 3 || vehicles[0] != "terminal" || vehicles[1] != "desktop" || vehicles[2] != "terminal" {
		t.Fatalf("vehicles sent = %q, want [terminal desktop terminal]", vehicles)
	}

	vehicles = nil
	for _, bad := range []string{"browser", "", "Desktop"} {
		if err := run("--vehicle", bad); err == nil {
			t.Errorf("--vehicle %q was accepted", bad)
		}
	}
	if len(vehicles) != 0 {
		t.Fatalf("a refused --vehicle still sent %d claim(s)", len(vehicles))
	}
}
