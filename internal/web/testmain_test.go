// SPDX-License-Identifier: Apache-2.0

package web

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestMain keeps every test in this package away from the user's real config
// dir (a control-mode server handed no secret writes one there), and gives
// control-mode servers a per-run handover secret that the shared helpers
// present, as the desktop app does. Tests of the secret itself reset it.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "rysh-web-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	controlSecretDir = func() (string, error) { return filepath.Join(dir, "web-control"), nil }
	secret, err := NewControlSecret()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	handover.secret = secret
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// controlSecretHeaders carries the run's handover secret, as the app does.
func controlSecretHeaders() http.Header {
	h := http.Header{}
	if s := handoverSecret(); s != "" {
		h.Set(ControlSecretHeader, s)
	}
	return h
}
