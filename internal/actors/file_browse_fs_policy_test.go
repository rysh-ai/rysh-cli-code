// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

// File browse (share relay and embedded web UI) honours the fs: rules
// (design 068): a denied path cannot be listed, stat'ed or read, and denied
// entries are hidden from a listing. Without rules nothing changes.
func TestFileBrowse_FSPolicy(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"README.md", ".env", "secrets/db.txt"} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "secrets"), filepath.Join(root, "innocent")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { policy.SetActiveFS(nil) })

	listNames := func() []string {
		abs, rel, code, msg := resolveSandboxedPath(root, "", false)
		if code != "" {
			t.Fatalf("resolve root: %s %s", code, msg)
		}
		reply, code, msg := fsListCore(root, abs, rel, "")
		if code != "" {
			t.Fatalf("list root: %s %s", code, msg)
		}
		var names []string
		for _, e := range reply.Entries {
			names = append(names, e.Name)
		}
		return names
	}

	// No rules: everything is visible and readable (today's behaviour).
	policy.SetActiveFS(nil)
	if got := strings.Join(listNames(), ","); !strings.Contains(got, ".env") || !strings.Contains(got, "secrets") {
		t.Fatalf("without rules the listing must be complete: %s", got)
	}
	if _, _, code, _ := resolveSandboxedPath(root, "secrets/db.txt", false); code != "" {
		t.Fatalf("without rules secrets/db.txt must resolve, got %s", code)
	}

	dir := filepath.Join(root, ".rysh")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, policy.FileName), []byte("fs:\n  deny: [.env, secrets]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	policy.SetActiveFS(policy.Load(dir).ResolveFS(root))

	got := strings.Join(listNames(), ",")
	if strings.Contains(got, ".env") || strings.Contains(got, "secrets") || strings.Contains(got, "innocent") {
		t.Errorf("denied entries (and a symlink to one) must be hidden: %s", got)
	}
	if !strings.Contains(got, "README.md") {
		t.Errorf("allowed entries must stay: %s", got)
	}
	for _, p := range []string{".env", "secrets", "secrets/db.txt", "innocent/db.txt", "secrets/../secrets/db.txt"} {
		_, _, code, msg := resolveSandboxedPath(root, p, false)
		if code != "denied" || !strings.Contains(msg, "policy: ") {
			t.Errorf("%s: want denied with a policy message, got %q %q", p, code, msg)
		}
	}
}
