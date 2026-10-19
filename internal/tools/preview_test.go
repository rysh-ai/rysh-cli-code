// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

// The approval gate shows Preview's diff and calls Execute only after the user
// approves (design 024 "Article 021 live demonstration": edits used to reach
// disk before the prompt, and a rejection did not revert them).
var (
	_ sharedtools.Previewer = (*EditTool)(nil)
	_ sharedtools.Previewer = (*FileWriteTool)(nil)
)

func TestEditPreviewDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cart.go")
	orig := "func total(s float64) float64 {\n\treturn s\n}\n"
	if err := os.WriteFile(path, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}
	tool := NewEditTool(dir)
	params, _ := json.Marshal(EditParams{FilePath: "cart.go", OldString: "return s", NewString: "return s * 1.2"})

	out, err := tool.Preview(context.Background(), params)
	if err != nil || out.Error != "" {
		t.Fatalf("preview failed: %v %q", err, out.Error)
	}
	if !strings.Contains(out.Content, "+\treturn s * 1.2") {
		t.Fatalf("preview diff missing the change:\n%s", out.Content)
	}
	if got, _ := os.ReadFile(path); string(got) != orig {
		t.Fatalf("Preview wrote to disk:\n%s", got)
	}

	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), "return s * 1.2") {
		t.Fatalf("Execute did not apply the edit:\n%s", got)
	}
}

func TestEditPreviewReportsValidationWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	_ = os.WriteFile(path, []byte("hello\n"), 0644)
	params, _ := json.Marshal(EditParams{FilePath: "a.txt", OldString: "absent", NewString: "x"})
	out, err := NewEditTool(dir).Preview(context.Background(), params)
	if err != nil || out.Error == "" {
		t.Fatalf("expected a validation error, got err=%v out=%+v", err, out)
	}
	if got, _ := os.ReadFile(path); string(got) != "hello\n" {
		t.Fatalf("file changed: %q", got)
	}
}

func TestFileWritePreviewDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	tool := NewFileWriteTool(dir)

	// New file: Preview must not create it (nor its parent directory).
	params, _ := json.Marshal(FileWriteParams{FilePath: "sub/new.txt", Content: "hi\n"})
	out, err := tool.Preview(context.Background(), params)
	if err != nil || out.Error != "" {
		t.Fatalf("preview failed: %v %+v", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub")); !os.IsNotExist(err) {
		t.Fatalf("Preview created files/directories (stat err=%v)", err)
	}

	// Existing file: Preview must not overwrite it.
	path := filepath.Join(dir, "keep.txt")
	_ = os.WriteFile(path, []byte("old\n"), 0644)
	params, _ = json.Marshal(FileWriteParams{FilePath: "keep.txt", Content: "new\n"})
	out, err = tool.Preview(context.Background(), params)
	if err != nil || !strings.Contains(out.Content, "+new") || out.Metadata["operation"] != "overwrite" {
		t.Fatalf("unexpected preview: err=%v out=%+v", err, out)
	}
	if got, _ := os.ReadFile(path); string(got) != "old\n" {
		t.Fatalf("Preview overwrote the file: %q", got)
	}

	if _, err := tool.Execute(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new\n" {
		t.Fatalf("Execute did not write: %q", got)
	}
}

// Mirrors the approval gate's sequence with the real tool: the diff comes from
// Preview (disk unchanged), then "yes" runs Execute (disk changed) and "no"
// runs nothing (disk unchanged).
func TestEditApprovalSequence(t *testing.T) {
	for _, approve := range []bool{true, false} {
		dir := t.TempDir()
		path := filepath.Join(dir, "cart.go")
		orig := "return sum - sum*discountPct/10\n"
		_ = os.WriteFile(path, []byte(orig), 0644)
		var tool sharedtools.ToolExecutor = NewEditTool(dir)
		params, _ := json.Marshal(EditParams{FilePath: "cart.go", OldString: "/10", NewString: "/100"})

		prev := tool.(sharedtools.Previewer)
		if out, err := prev.Preview(context.Background(), params); err != nil || !strings.Contains(out.Content, "/100") {
			t.Fatalf("preview: err=%v out=%+v", err, out)
		}
		if got, _ := os.ReadFile(path); string(got) != orig {
			t.Fatalf("disk changed while waiting for approval: %q", got)
		}
		if approve {
			if _, err := tool.Execute(context.Background(), params); err != nil {
				t.Fatal(err)
			}
		}
		got, _ := os.ReadFile(path)
		if approve && !strings.Contains(string(got), "/100") {
			t.Fatalf("approved edit not applied: %q", got)
		}
		if !approve && string(got) != orig {
			t.Fatalf("rejected edit reached disk: %q", got)
		}
	}
}
