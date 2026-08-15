// SPDX-License-Identifier: Apache-2.0

package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A session with no login of its own inherits the project's — that is what
// keeps every install that predates per-session logins working.
func TestSessionCredentialsFallBackToProject(t *testing.T) {
	dir := t.TempDir()
	if _, err := SaveCredentials(dir, "project-user", "pw"); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	ref := SessionCredentials(dir, "macmini-rysh")

	got, err := LoadCredentialsFor(ref)
	if err != nil || got == nil {
		t.Fatalf("load = (%+v, %v)", got, err)
	}
	if got.Username != "project-user" {
		t.Fatalf("username = %q, want the project login", got.Username)
	}
	if ref.Path() != CredentialsPath(dir) {
		t.Fatalf("read path = %s, want the project file", ref.Path())
	}
}

// Once a session sets its own, it stops following the project's — and the other
// session in the same project is untouched. This is the bug that prompted the
// split: two sessions, two ports, two URLs, and one password file, so the
// second one to set a login silently changed the first one's.
func TestSessionCredentialsAreIndependent(t *testing.T) {
	dir := t.TempDir()
	if _, err := SaveCredentials(dir, "halilagin", "cli-password"); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	cli := SessionCredentials(dir, "macmini-rysh")
	app := SessionCredentials(dir, "macmini-rysh-elect")

	if _, err := SaveCredentialsFor(app, "halilagin", "app-password"); err != nil {
		t.Fatalf("save app login: %v", err)
	}

	appCreds, err := LoadCredentialsFor(app)
	if err != nil || appCreds == nil || !appCreds.Verify("halilagin", "app-password") {
		t.Fatalf("app session does not accept its own password: %+v (%v)", appCreds, err)
	}
	if appCreds.Verify("halilagin", "cli-password") {
		t.Fatal("app session still accepts the project password")
	}
	// The other session kept the project login, untouched.
	cliCreds, err := LoadCredentialsFor(cli)
	if err != nil || cliCreds == nil || !cliCreds.Verify("halilagin", "cli-password") {
		t.Fatalf("the CLI session's login was disturbed: %+v (%v)", cliCreds, err)
	}
	// Distinct signing keys, so a token minted for one is not valid on the other.
	if appCreds.Secret == cliCreds.Secret {
		t.Fatal("both sessions share a signing key")
	}
}

// Clearing a session's login means "stop having your own", not "unlock the
// project".
func TestClearSessionCredentialsLeavesTheProjectAlone(t *testing.T) {
	dir := t.TempDir()
	if _, err := SaveCredentials(dir, "project-user", "pw"); err != nil {
		t.Fatal(err)
	}
	app := SessionCredentials(dir, "elect")
	if _, err := SaveCredentialsFor(app, "app-user", "app-pw"); err != nil {
		t.Fatal(err)
	}
	removed, err := ClearCredentialsFor(app)
	if err != nil || !removed {
		t.Fatalf("clear = (%v,%v)", removed, err)
	}
	if _, err := os.Stat(CredentialsPath(dir)); err != nil {
		t.Fatalf("the project credentials file was removed: %v", err)
	}
	// And the session falls back to the project login again.
	got, err := LoadCredentialsFor(app)
	if err != nil || got == nil || got.Username != "project-user" {
		t.Fatalf("fallback after clear = %+v (%v)", got, err)
	}
}

// A session name is used as a filename; it must never escape the directory or
// collide with the project file.
func TestSessionCredentialsPathIsSafe(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"../../etc/passwd", "a/b", "web-auth", "  ", "."} {
		own := SessionCredentials(dir, name).OwnPath()
		if own == "" {
			t.Fatalf("%q produced an empty path", name)
		}
		clean := filepath.Clean(own)
		if rel, err := filepath.Rel(dir, clean); err != nil || rel == ".." || len(rel) > 2 && rel[:3] == ".."+string(filepath.Separator) {
			t.Fatalf("%q escaped the rysh dir: %s", name, clean)
		}
	}
}

// A credentials file is a hash AND the signing key that mints tokens; a project
// that tracks .rysh/ must not be able to commit one. The session SETTINGS in
// the same directory are ordinary state, so the rule is narrow.
func TestSessionCredentialsDirIsGitignored(t *testing.T) {
	dir := t.TempDir()
	ref := SessionCredentials(dir, "macmini-rysh-elect")
	if _, err := SaveCredentialsFor(ref, "u", "p"); err != nil {
		t.Fatal(err)
	}
	gi := filepath.Join(filepath.Dir(ref.OwnPath()), ".gitignore")
	data, err := os.ReadFile(gi)
	if err != nil {
		t.Fatalf("no .gitignore beside the credentials: %v", err)
	}
	if !strings.Contains(string(data), "*-auth.json") {
		t.Fatalf(".gitignore does not cover the credentials file:\n%s", data)
	}
	if strings.Contains(string(data), "\n*\n") {
		t.Fatalf(".gitignore excludes the whole directory, settings included:\n%s", data)
	}
}
