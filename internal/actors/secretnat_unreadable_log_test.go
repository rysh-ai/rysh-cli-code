// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package actors

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// security-lead (ledger 220 follow-up, LOW): a secret that the SecretNAT known
// tier cannot read is not redacted, and nothing said so. enumerableSecrets now
// logs every unreadable registered secret and every unreadable secret source
// by NAME (never a value), so a redaction gap is visible.
func TestUnreadableSecretsAreLoggedByNameNeverValue(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a chmod 000 file")
	}
	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	scopeDir := filepath.Join(".rysh", "secrets", "default")
	if err := os.MkdirAll(scopeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	value := "Zt" + strings.Repeat("q7W", 6)
	for name, mode := range map[string]os.FileMode{"GOOD_TOKEN": 0o600, "LOCKED_TOKEN": 0o000} {
		p := filepath.Join(scopeDir, name)
		if err := os.WriteFile(p, []byte(value+name), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = os.Chmod(scopeDir, 0o700)
		_ = os.Chmod(filepath.Join(scopeDir, "LOCKED_TOKEN"), 0o600)
	})
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	w := &WorkspaceActor{secrets: newNamedStore(secretKind, nil, nil)}
	known := w.enumerableSecrets()
	if len(known) != 1 || known[0].Name != "GOOD_TOKEN" {
		t.Fatalf("known tier = %v, want only GOOD_TOKEN", known)
	}
	if !strings.Contains(logs.String(), "LOCKED_TOKEN") {
		t.Fatalf("the unreadable secret is not named in the log:\n%s", logs.String())
	}

	// A whole source that cannot be read.
	logs.Reset()
	if err := os.Chmod(scopeDir, 0o000); err != nil {
		t.Fatal(err)
	}
	if got := w.enumerableSecrets(); len(got) != 0 {
		t.Fatalf("known tier from an unreadable directory = %v", got)
	}
	if !strings.Contains(logs.String(), filepath.Join("secrets", "default")) {
		t.Fatalf("the unreadable secret source is not named in the log:\n%s", logs.String())
	}

	if strings.Contains(logs.String(), value) {
		t.Fatal("a secret value reached the log")
	}
}

// A source under the home directory is logged as $HOME/..., never as an
// absolute home path, so logs copied into evidence carry no user name.
func TestUnreadableHomeSecretSourceIsLoggedUnderHOME(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a chmod 000 directory")
	}
	t.Chdir(t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	scopeDir := filepath.Join(home, ".rysh", "secrets", "default")
	if err := os.MkdirAll(scopeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(scopeDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(scopeDir, 0o700) })
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	w := &WorkspaceActor{secrets: newNamedStore(secretKind, nil, nil)}
	_ = w.enumerableSecrets()
	if !strings.Contains(logs.String(), "$HOME/.rysh/secrets/default") {
		t.Fatalf("the home source is not logged as $HOME/...:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), home) {
		t.Fatalf("the absolute home path reached the log:\n%s", logs.String())
	}
}
