// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/policy"
	"github.com/rysh-ai/rysh-cli-code/internal/sandbox"
)

// TestSyncPolicyGate_PublishesSandboxRules: every policy load/reload writes this
// workspace's fs rules where `exec-clean --sandbox` reads them (design 066 ×
// 068); a broken policy writes a fail-closed file the sandbox refuses.
func TestSyncPolicyGate_PublishesSandboxRules(t *testing.T) {
	clearPolicyGlobals(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ryshDir := filepath.Join(root, ".rysh")
	if err := os.MkdirAll(ryshDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ryshDir, policy.FileName), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w := &WorkspaceActor{cfg: config.Config{RyshDir: ryshDir}}

	write("fs:\n  read_only: [\"**\"]\n  deny: [.env]\n")
	w.policy = w.loadPolicy()
	w.syncPolicyGate()
	got, err := sandbox.LoadRules(ryshDir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.ReadOnly, "|") != root || strings.Join(got.Deny, "|") != filepath.Join(root, ".env") {
		t.Fatalf("rules = %+v", got)
	}

	// ##policy reload without fs: the file says "no rules", not the old ones.
	write("bash:\n  deny: [rm]\n")
	w.policy = w.loadPolicy()
	w.syncPolicyGate()
	got, err = sandbox.LoadRules(ryshDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ReadOnly)+len(got.Deny)+len(got.ReadWrite) != 0 {
		t.Fatalf("stale rules after reload: %+v", got)
	}

	// Broken policy: the sandbox refuses.
	w.policy = &policy.Policy{Path: ".rysh/policy.yaml", Err: errors.New("parse error")}
	w.syncPolicyGate()
	if _, err := sandbox.LoadRules(ryshDir); err == nil || !strings.Contains(err.Error(), "failed to load") {
		t.Fatalf("broken policy: err = %v", err)
	}
}
