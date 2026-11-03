// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Credential-shaped inputs are synthetic, built at run time; no test prints,
// records or hashes a value. A failing case reports its name only.

func runMemoryEdit(t *testing.T, tool *MemoryEditTool, action, content string) *ToolOutput {
	t.Helper()
	params, _ := json.Marshal(MemoryEditParams{Action: action, Content: content})
	out, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("%s: Execute error: %v", action, err)
	}
	return out
}

// TestMemoryEditRefusesCredentialShapedContent: RYSH.md is injected into
// every later session, so append and write refuse what the memory scrubber
// refuses (design 010 §3a, QUESTION 2); the refusal names the rule, never the
// value, and the file is left as it was.
func TestMemoryEditRefusesCredentialShapedContent(t *testing.T) {
	known := "correct" + "-horse-" + "battery" + "-staple"
	cases := []struct {
		name, action, content string
		secret                string // the part that must never reach the file or the output
	}{
		{"append-snat-shape", "append", "stripe key " + "sk" + "_" + "test" + "_" + strings.Repeat("ab12", 5), "sk" + "_" + "test" + "_"},
		{"write-snat-shape", "write", "# notes\n" + "sk" + "_" + "live" + "_" + strings.Repeat("cd34", 5) + "\n", "sk" + "_" + "live" + "_"},
		{"append-known-secret", "append", "the deploy pass is " + known, known},
		{"write-assignment", "write", "db " + "pass" + "word=" + "hunter2hunter2", "hunter2hunter2"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		path := filepath.Join(dir, memoryFileName)
		const before = "# Project Memory\n\n- keep me\n"
		if err := os.WriteFile(path, []byte(before), 0644); err != nil {
			t.Fatal(err)
		}
		tool := NewMemoryEditTool(dir)
		tool.BindKnownSecrets(func() []string { return []string{known} })
		out := runMemoryEdit(t, tool, c.action, c.content)
		if out.Error == "" {
			t.Errorf("%s: not refused", c.name)
		}
		if !strings.Contains(out.Error, "rule") {
			t.Errorf("%s: refusal does not name a rule", c.name)
		}
		if strings.Contains(out.Error+out.Content, c.secret) {
			t.Errorf("%s: output echoes the value", c.name)
		}
		got, _ := os.ReadFile(path)
		if string(got) != before {
			t.Errorf("%s: RYSH.md changed", c.name)
		}
	}
}

// TestMemoryEditStillWritesOrdinaryNotes: the guard does not stop durable
// notes, commit shas included (hex digests are allowed).
func TestMemoryEditStillWritesOrdinaryNotes(t *testing.T) {
	dir := t.TempDir()
	tool := NewMemoryEditTool(dir)
	note := "build with make test; release pinned at commit 9086dc35ccd31c28b70bbebf09908cb10cac4ffe"
	if out := runMemoryEdit(t, tool, "append", note); out.Error != "" {
		t.Fatalf("append ordinary note refused: %s", out.Error)
	}
	if out := runMemoryEdit(t, tool, "write", "# Project Memory\n\n- "+note+"\n"); out.Error != "" {
		t.Fatalf("write ordinary note refused: %s", out.Error)
	}
	got, _ := os.ReadFile(filepath.Join(dir, memoryFileName))
	if !strings.Contains(string(got), "make test") {
		t.Errorf("RYSH.md missing the note")
	}
}
