// SPDX-License-Identifier: Apache-2.0

package bus

// N3 (security-lead, D-55 B review ledger 66): a project opened through a
// symlinked path is the same workspace as its real path. The join check and
// the guest-of warning resolve symlinks before they call two paths different;
// a path that does not resolve still compares only as cleaned text.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func symlinkedProject(t *testing.T) (real, link string) {
	t.Helper()
	base := t.TempDir()
	real = filepath.Join(base, "proj")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(base, "proj-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	return real, link
}

func TestSameWorkspace(t *testing.T) {
	real, link := symlinkedProject(t)
	other := t.TempDir()
	for _, tc := range []struct {
		name string
		a, b string
		want bool
	}{
		{"same text", real, real, true},
		{"trailing slash", real, real + "/", true},
		{"symlink to it", link, real, true},
		{"another dir", real, other, false},
		{"symlink vs another dir", link, other, false},
		{"missing paths, different text", "/nonexistent/a", "/nonexistent/b", false},
		{"missing paths, same text", "/nonexistent/a", "/nonexistent/a/", true},
	} {
		if got := sameWorkspace(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: sameWorkspace(%q, %q) = %v, want %v", tc.name, tc.a, tc.b, got, tc.want)
		}
	}
}

// N4 (security-lead, ledger 93): a relative host claim is never resolved
// against this process's working directory.
func TestSameWorkspaceNeverResolvesARelativePath(t *testing.T) {
	real, link := symlinkedProject(t)
	t.Chdir(filepath.Dir(link))
	for _, tc := range []struct{ a, b string }{
		{filepath.Base(link), real},
		{real, filepath.Base(link)},
		{filepath.Base(real), real},
	} {
		if sameWorkspace(tc.a, tc.b) {
			t.Errorf("sameWorkspace(%q, %q) = true; a relative path must not resolve against the working directory", tc.a, tc.b)
		}
	}
}

func TestRefuseForeignHostJoinsItsOwnWorkspaceThroughASymlink(t *testing.T) {
	real, link := symlinkedProject(t)
	port := freePort(t)
	startHostBus(t, port, "proj-app-session", real)
	g, err := New(Config{Mode: "embedded", Port: port, SessionName: "proj-def456",
		Workspace: link, RefuseForeignHost: true, HostQueryTimeout: time.Second})
	if err != nil {
		t.Fatalf("the same project opened through a symlink was refused: %v", err)
	}
	defer g.Close()
	if _, ok := g.GuestOf(); !ok {
		t.Error("it should have joined as a guest of its own workspace's server")
	}
}

func TestRefuseForeignHostStillRefusesASymlinkElsewhere(t *testing.T) {
	_, link := symlinkedProject(t)
	port := freePort(t)
	startHostBus(t, port, "other-proj-abc123", t.TempDir())
	g, err := New(Config{Mode: "embedded", Port: port, SessionName: "proj-def456",
		Workspace: link, RefuseForeignHost: true, HostQueryTimeout: time.Second})
	if err == nil {
		g.Close()
		t.Fatal("a symlink to this project joined another project's server")
	}
}
