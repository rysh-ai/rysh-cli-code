// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"strings"
	"testing"
)

func TestGenerate_BasicChange(t *testing.T) {
	old := "line1\nline2\nline3\nline4\nline5\n"
	new := "line1\nline2\nmodified\nline4\nline5\n"

	result := Generate(old, new, "test.go")

	if !strings.Contains(result, "--- a/test.go") {
		t.Error("expected --- a/test.go header")
	}
	if !strings.Contains(result, "+++ b/test.go") {
		t.Error("expected +++ b/test.go header")
	}
	if !strings.Contains(result, "-line3") {
		t.Error("expected removed line")
	}
	if !strings.Contains(result, "+modified") {
		t.Error("expected added line")
	}
}

func TestGenerate_NoChange(t *testing.T) {
	content := "same\ncontent\n"
	result := Generate(content, content, "test.go")

	if result != "" {
		t.Errorf("expected empty diff for identical content, got: %s", result)
	}
}

func TestGenerate_Addition(t *testing.T) {
	old := "line1\nline2\n"
	new := "line1\nline2\nline3\n"

	result := Generate(old, new, "test.go")

	if !strings.Contains(result, "+line3") {
		t.Error("expected added line3")
	}
}

func TestGenerate_Deletion(t *testing.T) {
	old := "line1\nline2\nline3\n"
	new := "line1\nline3\n"

	result := Generate(old, new, "test.go")

	if !strings.Contains(result, "-line2") {
		t.Error("expected removed line2")
	}
}

func TestGenerateForNewFile(t *testing.T) {
	content := "new file content\nsecond line\n"
	result := GenerateForNewFile(content, "new_file.go")

	if !strings.Contains(result, "+++ b/new_file.go") {
		t.Error("expected new file header")
	}
	if !strings.Contains(result, "+new file content") {
		t.Error("expected added content")
	}
	if !strings.Contains(result, "+second line") {
		t.Error("expected second line")
	}
}

func TestGenerateForDelete(t *testing.T) {
	content := "deleted content\n"
	result := GenerateForDelete(content, "old_file.go")

	if !strings.Contains(result, "--- a/old_file.go") {
		t.Error("expected deleted file header")
	}
	if !strings.Contains(result, "-deleted content") {
		t.Error("expected removed content")
	}
}

func TestRender_Colors(t *testing.T) {
	diff := "--- a/file.go\n+++ b/file.go\n@@ -1,3 +1,3 @@\n context\n-old\n+new\n"
	result := Render(diff)

	// Should contain ANSI codes
	if !strings.Contains(result, "\033[") {
		t.Error("expected ANSI escape codes in rendered output")
	}
	if !strings.Contains(result, "\033[31m") {
		t.Error("expected red color for removed lines")
	}
	if !strings.Contains(result, "\033[32m") {
		t.Error("expected green color for added lines")
	}
}

func TestRenderCompact_Truncation(t *testing.T) {
	// Create a long diff
	var lines []string
	lines = append(lines, "--- a/file.go")
	lines = append(lines, "+++ b/file.go")
	lines = append(lines, "@@ -1,20 +1,20 @@")
	for i := 0; i < 20; i++ {
		lines = append(lines, "+added line")
	}
	diff := strings.Join(lines, "\n")

	result := RenderCompact(diff, 5)

	// Should be truncated
	rendered := strings.Split(result, "\n")
	// Count non-empty lines
	count := 0
	for _, l := range rendered {
		if l != "" {
			count++
		}
	}
	if count > 7 { // 5 lines + "..." message + possibly empty trailing
		t.Errorf("expected truncated output, got %d lines", count)
	}
}
