package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// seedTree creates a small nested directory of .go files for walk tests.
func seedTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range []string{"a.go", "sub/b.go", "sub/deep/c.go"} {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("package x\n// foo\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestGrep_HonoursCancelledContext proves the recursive grep walk aborts and
// reports ErrKindCancelled when ctx is already cancelled (follow-up 5b).
func TestGrep_HonoursCancelledContext(t *testing.T) {
	root := seedTree(t)
	tool := NewGrepTool(root)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before executing

	out, err := tool.Execute(ctx, json.RawMessage(`{"pattern":"foo"}`))
	if err != nil {
		t.Fatalf("Execute returned a Go error: %v", err)
	}
	if out.ErrorKind != ErrKindCancelled {
		t.Fatalf("expected ErrKindCancelled, got kind=%q error=%q", out.ErrorKind, out.Error)
	}
}

// TestGlob_HonoursCancelledContext proves the recursive ** glob walk aborts and
// reports ErrKindCancelled when ctx is already cancelled (follow-up 5b).
func TestGlob_HonoursCancelledContext(t *testing.T) {
	root := seedTree(t)
	tool := NewGlobTool(root)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out, err := tool.Execute(ctx, json.RawMessage(`{"pattern":"**/*.go"}`))
	if err != nil {
		t.Fatalf("Execute returned a Go error: %v", err)
	}
	if out.ErrorKind != ErrKindCancelled {
		t.Fatalf("expected ErrKindCancelled, got kind=%q error=%q", out.ErrorKind, out.Error)
	}
}

// TestGlob_CompletesWhenNotCancelled is a control: a live context returns matches.
func TestGlob_CompletesWhenNotCancelled(t *testing.T) {
	root := seedTree(t)
	tool := NewGlobTool(root)

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"**/*.go"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.ErrorKind != "" {
		t.Fatalf("unexpected error kind %q: %s", out.ErrorKind, out.Error)
	}
	if out.Content == "" {
		t.Fatalf("expected glob matches, got empty content")
	}
}
