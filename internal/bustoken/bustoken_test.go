// SPDX-License-Identifier: Apache-2.0

package bustoken

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
)

func tempHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	return h
}

func TestNewIsRandomHex(t *testing.T) {
	a, err := New()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := New()
	if !validToken(a) || !validToken(b) || a == b {
		t.Fatalf("tokens not 64 distinct hex chars (len %d/%d, equal=%v)", len(a), len(b), a == b)
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	home := tempHome(t)
	tok, _ := New()
	if err := Write(4300, tok); err != nil {
		t.Fatal(err)
	}
	got, err := Read(4300)
	if err != nil || got != tok {
		t.Fatalf("Read = (match %v, %v)", got == tok, err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	dir := filepath.Join(home, ".rysh-run")
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v, want 0700", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Join(dir, "nats-4300.token")); fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v, want 0600", fi.Mode().Perm())
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("%d entries in the token dir, want 1 (no temp file left)", len(ents))
	}
}

// No dir, or no file for the port: an owner without auth. Not an error.
func TestReadMissingIsEmpty(t *testing.T) {
	tempHome(t)
	if got, err := Read(4301); got != "" || err != nil {
		t.Fatalf("no dir: Read = (%q, %v)", got, err)
	}
	tok, _ := New()
	if err := Write(4302, tok); err != nil {
		t.Fatal(err)
	}
	if got, err := Read(4301); got != "" || err != nil {
		t.Fatalf("no file: Read = (%q, %v)", got, err)
	}
}

func TestReadRefusesUnsafeFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes and symlinks are not checked on windows")
	}
	cases := map[string]func(t *testing.T, dir, file string){
		"file group-readable":  func(t *testing.T, _, file string) { os.Chmod(file, 0o640) },
		"dir world-searchable": func(t *testing.T, dir, _ string) { os.Chmod(dir, 0o711) },
		"file is a symlink": func(t *testing.T, dir, file string) {
			other := filepath.Join(dir, "other")
			os.Rename(file, other)
			os.Symlink(other, file)
		},
		"dir is a symlink": func(t *testing.T, dir, _ string) {
			real := dir + ".real"
			os.Rename(dir, real)
			os.Symlink(real, dir)
		},
		"file not a token": func(t *testing.T, _, file string) { os.WriteFile(file, []byte("not-a-token\n"), 0o600) },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			home := tempHome(t)
			tok, _ := New()
			if err := Write(4303, tok); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(home, ".rysh-run")
			mut(t, dir, filepath.Join(dir, "nats-4303.token"))
			if got, err := Read(4303); err == nil || got != "" {
				t.Errorf("Read accepted it (token returned: %v)", got != "")
			}
		})
	}
}

func TestWriteRefusesUnsafeDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes and symlinks are not checked on windows")
	}
	for name, mk := range map[string]func(dir string){
		"dir 0755":  func(dir string) { os.Mkdir(dir, 0o755) },
		"symlink":   func(dir string) { real := dir + ".real"; os.Mkdir(real, 0o700); os.Symlink(real, dir) },
		"is a file": func(dir string) { os.WriteFile(dir, nil, 0o600) },
	} {
		t.Run(name, func(t *testing.T) {
			home := tempHome(t)
			mk(filepath.Join(home, ".rysh-run"))
			tok, _ := New()
			if err := Write(4304, tok); err == nil {
				t.Error("Write accepted an unsafe dir")
			}
		})
	}
}

func TestWriteReplacesAnExistingToken(t *testing.T) {
	tempHome(t)
	a, _ := New()
	b, _ := New()
	if err := Write(4305, a); err != nil {
		t.Fatal(err)
	}
	if err := Write(4305, b); err != nil {
		t.Fatal(err)
	}
	if got, _ := Read(4305); got != b {
		t.Fatal("second Write did not replace the token")
	}
}

func TestRemoveOnlyOurs(t *testing.T) {
	tempHome(t)
	a, _ := New()
	b, _ := New()
	Write(4306, a)
	Remove(4306, b) // another owner's token: left alone
	if got, _ := Read(4306); got != a {
		t.Fatal("Remove deleted a token that was not ours")
	}
	Remove(4306, a)
	if got, err := Read(4306); got != "" || err != nil {
		t.Fatalf("Remove left it: (%v, %v)", got != "", err)
	}
}

func TestOptionsRereadsOnEveryConnect(t *testing.T) {
	tempHome(t)
	a, _ := New()
	Write(4307, a)
	opt, err := Option(4307)
	if err != nil {
		t.Fatal(err)
	}
	var o nats.Options
	if err := opt(&o); err != nil || o.TokenHandler == nil {
		t.Fatalf("no token handler: %v", err)
	}
	if o.TokenHandler() != a {
		t.Fatal("handler does not return the token")
	}
	b, _ := New()
	Write(4307, b)
	if o.TokenHandler() != b {
		t.Fatal("handler did not re-read the file (reconnect after an owner restart would fail)")
	}
}

func TestOptionSurfacesAnUnsafeFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes are not checked on windows")
	}
	home := tempHome(t)
	a, _ := New()
	Write(4308, a)
	os.Chmod(filepath.Join(home, ".rysh-run", "nats-4308.token"), 0o644)
	if _, err := Option(4308); err == nil {
		t.Fatal("Option accepted a world-readable token file")
	}
}

func TestIsAuthError(t *testing.T) {
	for _, err := range []error{nats.ErrAuthorization, fmt.Errorf("x: %w", nats.ErrAuthorization), errors.New("nats: Authorization Violation")} {
		if !IsAuthError(err) {
			t.Errorf("IsAuthError(%v) = false", err)
		}
	}
	for _, err := range []error{nil, nats.ErrNoServers, errors.New("dial tcp: connection refused")} {
		if IsAuthError(err) {
			t.Errorf("IsAuthError(%v) = true", err)
		}
	}
}

func TestPathsAreUnderTheHome(t *testing.T) {
	home := tempHome(t)
	p, err := Path(4309)
	if err != nil || p != filepath.Join(home, ".rysh-run", "nats-4309.token") {
		t.Fatalf("Path = %q, %v", p, err)
	}
	for _, c := range SandboxDenyDirs() {
		if c == "" || !filepath.IsAbs(c) {
			t.Errorf("deny candidate %q not absolute", c)
		}
	}
	if !strings.Contains(strings.Join(SandboxDenyDirs(), "\n"), filepath.Join(home, ".rysh-run")) {
		t.Error("SandboxDenyDirs misses the token dir")
	}
}

// The token goes only to a loopback host: a plugin URL from the env that names
// another host must not carry it off the machine.
func TestOptionForURLOnlyLoopback(t *testing.T) {
	tempHome(t)
	tok, _ := New()
	Write(4310, tok)
	for raw, want := range map[string]bool{
		"nats://127.0.0.1:4310":    true,
		"nats://localhost:4310":    true,
		"nats://[::1]:4310":        true,
		"nats://evil.example:4310": false,
		"nats://10.0.0.5:4310":     false,
		"nats://127.0.0.1":         false,
		"not a url":                false,
	} {
		opt, err := OptionForURL(raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		var o nats.Options
		opt(&o)
		if got := o.TokenHandler != nil; got != want {
			t.Errorf("%s: token attached = %v, want %v", raw, got, want)
		}
	}
}
