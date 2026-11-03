// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

// TestSyncPolicyGate_PublishesFSRules: the fs: rules (design 068) reach the
// process-wide check on load/reload, anchored at the parent of the rysh dir; a
// policy without fs: publishes nothing (today's behaviour); a broken policy
// publishes fail-closed rules.
func TestSyncPolicyGate_PublishesFSRules(t *testing.T) {
	clearPolicyGlobals(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ryshDir := filepath.Join(root, ".rysh")
	if err := os.MkdirAll(ryshDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ryshDir, policy.FileName), []byte("fs:\n  deny: [.env]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := &WorkspaceActor{cfg: config.Config{RyshDir: ryshDir}}
	w.policy = w.loadPolicy()
	w.syncPolicyGate()
	if err := policy.CheckFS(filepath.Join(root, ".env"), policy.FSRead); err == nil {
		t.Fatal("published fs.deny must refuse <root>/.env")
	}
	if err := policy.CheckFS(filepath.Join(root, "main.go"), policy.FSWrite); err != nil {
		t.Fatalf("unrelated path refused: %v", err)
	}

	// No fs section: nothing is published.
	if err := os.WriteFile(filepath.Join(ryshDir, policy.FileName), []byte("bash:\n  deny: [rm]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.policy = w.loadPolicy()
	w.syncPolicyGate()
	if policy.ActiveFS() != nil {
		t.Fatal("a policy without fs: must publish no fs rules")
	}

	// Broken policy: fail closed.
	w.policy = &policy.Policy{Path: ".rysh/policy.yaml", Err: errors.New("parse error")}
	w.syncPolicyGate()
	if err := policy.CheckFS(filepath.Join(root, "main.go"), policy.FSRead); err == nil {
		t.Fatal("a broken policy must refuse fs access (unknown rules)")
	}
	if !policy.ActiveFS().Mounts().FailClosed {
		t.Fatal("the sandbox hand-off must say FailClosed on a broken policy")
	}
}
