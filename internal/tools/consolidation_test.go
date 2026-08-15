// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBashAllowlist exercises the safe-by-default allowlist in
// BashTool.RequiresApproval: read-only/idempotent commands run without
// approval; mutating, chained-unsafe, redirected, or unknown commands gate.
func TestBashAllowlist(t *testing.T) {
	tool := NewBashTool("/tmp", 5*time.Second, nil)
	cases := []struct {
		cmd        string
		wantApprov bool
	}{
		// safe / no approval
		{"ls -la", false},
		{"git status", false},
		{"git diff --color=always", false},
		{"go build ./...", false},
		{"grep -rn foo .", false},
		{"git log | head -20", false}, // chained, both safe
		{"ls && go vet ./...", false}, // chained, both safe
		// gated / approval
		{"mkdir newdir", true},               // unknown mutating
		{"rm file.go", true},                 // unknown mutating
		{"echo hi > out.txt", true},          // redirection
		{"cat $(whoami)", true},              // command substitution
		{"git status && rm -rf build", true}, // chained, one unsafe
		{"git push --force", true},           // dangerous
		{"find . -delete", true},             // find with action flag
		{"sudo ls", true},                    // sudo
	}
	for _, c := range cases {
		params, _ := json.Marshal(BashParams{Command: c.cmd})
		if got := tool.RequiresApproval(params); got != c.wantApprov {
			t.Errorf("RequiresApproval(%q) = %v, want %v", c.cmd, got, c.wantApprov)
		}
	}
}

// TestEditMultiHunkAtomic verifies the merged edit tool applies multiple hunks
// atomically and rolls back the whole change when any hunk fails to match.
func TestEditMultiHunkAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	original := "alpha\nbeta\ngamma\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	tool := NewEditTool(dir)

	// Success: two hunks applied together.
	params, _ := json.Marshal(EditParams{
		FilePath: path,
		Edits: []EditPair{
			{OldString: "alpha", NewString: "ALPHA"},
			{OldString: "gamma", NewString: "GAMMA"},
		},
	})
	out, err := tool.Execute(context.Background(), params)
	if err != nil || out.Error != "" {
		t.Fatalf("multi-hunk edit failed: err=%v toolErr=%s", err, out.Error)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "ALPHA\nbeta\nGAMMA\n" {
		t.Fatalf("unexpected content after multi-edit: %q", string(data))
	}

	// Atomic rollback: second hunk doesn't match → nothing written.
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	params, _ = json.Marshal(EditParams{
		FilePath: path,
		Edits: []EditPair{
			{OldString: "alpha", NewString: "ALPHA"},
			{OldString: "nope", NewString: "X"},
		},
	})
	out, _ = tool.Execute(context.Background(), params)
	if out.Error == "" {
		t.Fatal("expected error when a hunk does not match")
	}
	data, _ = os.ReadFile(path)
	if string(data) != original {
		t.Fatalf("file must be unchanged on rollback, got %q", string(data))
	}
}

// TestEditSingleForm verifies the single old_string/new_string form still works
// (backwards compatible with the former file_edit tool).
func TestEditSingleForm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("one two three\n"), 0644); err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(EditParams{FilePath: path, OldString: "two", NewString: "TWO"})
	out, err := NewEditTool(dir).Execute(context.Background(), params)
	if err != nil || out.Error != "" {
		t.Fatalf("single-form edit failed: err=%v toolErr=%s", err, out.Error)
	}
	if !strings.Contains(out.Content, "+one TWO three") {
		t.Errorf("expected diff with replacement, got %q", out.Content)
	}
}
