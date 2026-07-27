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

// =============================================================================
// BashTool Tests
// =============================================================================

func TestBashTool_ExecuteSimpleCommand(t *testing.T) {
	workDir := t.TempDir()
	tool := NewBashTool(workDir, 5*time.Second, nil)

	params, _ := json.Marshal(BashParams{Command: "echo hello"})
	out, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", out.ExitCode)
	}
	if strings.TrimSpace(out.Content) != "hello" {
		t.Errorf("expected output 'hello', got %q", out.Content)
	}
}

func TestBashTool_ExitCodeCaptured(t *testing.T) {
	workDir := t.TempDir()
	tool := NewBashTool(workDir, 5*time.Second, nil)

	params, _ := json.Marshal(BashParams{Command: "exit 42"})
	out, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.ExitCode != 42 {
		t.Errorf("expected exit code 42, got %d", out.ExitCode)
	}
}

func TestBashTool_RequiresApproval_DangerousCommands(t *testing.T) {
	tool := NewBashTool("/tmp", 5*time.Second, nil)

	dangerous := []string{
		"rm -rf /",
		"git push --force origin main",
		"git reset --hard HEAD~1",
		"git clean -fd",
	}

	for _, cmd := range dangerous {
		params, _ := json.Marshal(BashParams{Command: cmd})
		if !tool.RequiresApproval(params) {
			t.Errorf("expected RequiresApproval=true for %q", cmd)
		}
	}
}

func TestBashTool_RequiresApproval_SafeCommands(t *testing.T) {
	tool := NewBashTool("/tmp", 5*time.Second, nil)

	safe := []string{
		"ls",
		"cat file.txt",
		"echo hello",
		"go build ./...",
	}

	for _, cmd := range safe {
		params, _ := json.Marshal(BashParams{Command: cmd})
		if tool.RequiresApproval(params) {
			t.Errorf("expected RequiresApproval=false for %q", cmd)
		}
	}
}

func TestBashTool_Timeout(t *testing.T) {
	workDir := t.TempDir()
	tool := NewBashTool(workDir, 1*time.Second, nil)

	params, _ := json.Marshal(BashParams{Command: "sleep 10"})
	start := time.Now()
	out, err := tool.Execute(context.Background(), params)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The command should have been killed by the timeout.
	// Depending on the OS, we may get "command timed out" error or a non-zero exit code
	// from the killed process. Either way, the command should not have run for 10 seconds.
	if elapsed > 5*time.Second {
		t.Errorf("timeout took too long: %v", elapsed)
	}
	if out.ExitCode == 0 {
		t.Errorf("expected non-zero exit code on timeout, got 0")
	}
}

// =============================================================================
// FileReadTool Tests
// =============================================================================

func TestFileReadTool_ReadFile(t *testing.T) {
	workDir := t.TempDir()
	content := "line1\nline2\nline3\n"
	filePath := filepath.Join(workDir, "test.txt")
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewFileReadTool(workDir)
	params, _ := json.Marshal(FileReadParams{FilePath: filePath})
	out, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected tool error: %s", out.Error)
	}
	// Output format is "cat -n" style: line numbers + tab + content
	if !strings.Contains(out.Content, "line1") {
		t.Errorf("expected output to contain 'line1', got %q", out.Content)
	}
	if !strings.Contains(out.Content, "line2") {
		t.Errorf("expected output to contain 'line2', got %q", out.Content)
	}
	if !strings.Contains(out.Content, "line3") {
		t.Errorf("expected output to contain 'line3', got %q", out.Content)
	}
}

func TestFileReadTool_OffsetAndLimit(t *testing.T) {
	workDir := t.TempDir()
	lines := "line0\nline1\nline2\nline3\nline4\n"
	filePath := filepath.Join(workDir, "test.txt")
	if err := os.WriteFile(filePath, []byte(lines), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewFileReadTool(workDir)
	// Offset=2 means skip first 2 lines (line0, line1), limit=2 means read 2 lines (line2, line3)
	params, _ := json.Marshal(FileReadParams{FilePath: filePath, Offset: 2, Limit: 2})
	out, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected tool error: %s", out.Error)
	}
	if !strings.Contains(out.Content, "line2") {
		t.Errorf("expected output to contain 'line2', got %q", out.Content)
	}
	if !strings.Contains(out.Content, "line3") {
		t.Errorf("expected output to contain 'line3', got %q", out.Content)
	}
	if strings.Contains(out.Content, "line0") {
		t.Errorf("expected output NOT to contain 'line0', got %q", out.Content)
	}
	if strings.Contains(out.Content, "line1") {
		t.Errorf("expected output NOT to contain 'line1', got %q", out.Content)
	}
	if strings.Contains(out.Content, "line4") {
		t.Errorf("expected output NOT to contain 'line4', got %q", out.Content)
	}
}

func TestFileReadTool_NonExistentFile(t *testing.T) {
	workDir := t.TempDir()
	tool := NewFileReadTool(workDir)

	params, _ := json.Marshal(FileReadParams{FilePath: filepath.Join(workDir, "no_such_file.txt")})
	out, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Error == "" {
		t.Errorf("expected an error for non-existent file, got none")
	}
	if !strings.Contains(out.Error, "not found") {
		t.Errorf("expected 'not found' in error, got %q", out.Error)
	}
}

// =============================================================================
// FileEditTool Tests
// =============================================================================

func TestFileEditTool_EditFile(t *testing.T) {
	workDir := t.TempDir()
	filePath := filepath.Join(workDir, "edit_test.txt")
	originalContent := "hello world\nfoo bar\nbaz qux\n"
	if err := os.WriteFile(filePath, []byte(originalContent), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewEditTool(workDir)
	params, _ := json.Marshal(EditParams{
		FilePath:  filePath,
		OldString: "foo bar",
		NewString: "foo replaced",
	})
	out, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected tool error: %s", out.Error)
	}
	// Output should contain a diff
	if !strings.Contains(out.Content, "-foo bar") {
		t.Errorf("expected diff to contain '-foo bar', got %q", out.Content)
	}
	if !strings.Contains(out.Content, "+foo replaced") {
		t.Errorf("expected diff to contain '+foo replaced', got %q", out.Content)
	}

	// Verify file was actually modified
	data, _ := os.ReadFile(filePath)
	if !strings.Contains(string(data), "foo replaced") {
		t.Errorf("file was not modified: %s", string(data))
	}
}

func TestFileEditTool_OldStringNotFound(t *testing.T) {
	workDir := t.TempDir()
	filePath := filepath.Join(workDir, "edit_test.txt")
	if err := os.WriteFile(filePath, []byte("hello world\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tool := NewEditTool(workDir)
	params, _ := json.Marshal(EditParams{
		FilePath:  filePath,
		OldString: "nonexistent string",
		NewString: "replacement",
	})
	out, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Error == "" {
		t.Error("expected an error for non-existent old_string")
	}
	if !strings.Contains(out.Error, "not found") {
		t.Errorf("expected 'not found' in error, got %q", out.Error)
	}
}

func TestFileEditTool_RequiresApproval(t *testing.T) {
	tool := NewEditTool("/tmp")
	params, _ := json.Marshal(EditParams{
		FilePath:  "any_file.txt",
		OldString: "old",
		NewString: "new",
	})
	if !tool.RequiresApproval(params) {
		t.Error("expected FileEditTool.RequiresApproval to always return true")
	}
}

// =============================================================================
// FileWriteTool Tests
// =============================================================================

func TestFileWriteTool_WriteNewFile(t *testing.T) {
	workDir := t.TempDir()
	tool := NewFileWriteTool(workDir)

	filePath := filepath.Join(workDir, "new_file.txt")
	content := "this is new content\nline two\n"

	params, _ := json.Marshal(FileWriteParams{
		FilePath: filePath,
		Content:  content,
	})
	out, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected tool error: %s", out.Error)
	}

	// Verify file exists and has correct content
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("file was not created: %v", err)
	}
	if string(data) != content {
		t.Errorf("expected content %q, got %q", content, string(data))
	}
}

func TestFileWriteTool_RequiresApproval(t *testing.T) {
	tool := NewFileWriteTool("/tmp")
	params, _ := json.Marshal(FileWriteParams{
		FilePath: "any_file.txt",
		Content:  "content",
	})
	if !tool.RequiresApproval(params) {
		t.Error("expected FileWriteTool.RequiresApproval to always return true")
	}
}

// =============================================================================
// GlobTool Tests
// =============================================================================

func TestGlobTool_SimplePattern(t *testing.T) {
	workDir := t.TempDir()

	// Create some .go files and a .txt file
	for _, name := range []string{"main.go", "util.go", "readme.txt"} {
		if err := os.WriteFile(filepath.Join(workDir, name), []byte("content"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	tool := NewGlobTool(workDir)
	params, _ := json.Marshal(GlobParams{Pattern: "*.go"})
	out, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected tool error: %s", out.Error)
	}

	if !strings.Contains(out.Content, "main.go") {
		t.Errorf("expected output to contain 'main.go', got %q", out.Content)
	}
	if !strings.Contains(out.Content, "util.go") {
		t.Errorf("expected output to contain 'util.go', got %q", out.Content)
	}
	if strings.Contains(out.Content, "readme.txt") {
		t.Errorf("expected output NOT to contain 'readme.txt', got %q", out.Content)
	}
}

func TestGlobTool_RecursivePattern(t *testing.T) {
	workDir := t.TempDir()

	// Create nested directory structure
	subDir := filepath.Join(workDir, "sub", "deep")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(workDir, "top.txt"),
		filepath.Join(workDir, "sub", "mid.txt"),
		filepath.Join(subDir, "deep.txt"),
		filepath.Join(subDir, "deep.go"),
	} {
		if err := os.WriteFile(path, []byte("content"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	tool := NewGlobTool(workDir)
	params, _ := json.Marshal(GlobParams{Pattern: "**/*.txt"})
	out, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected tool error: %s", out.Error)
	}

	if !strings.Contains(out.Content, "top.txt") {
		t.Errorf("expected output to contain 'top.txt', got %q", out.Content)
	}
	if !strings.Contains(out.Content, "mid.txt") {
		t.Errorf("expected output to contain 'mid.txt', got %q", out.Content)
	}
	if !strings.Contains(out.Content, "deep.txt") {
		t.Errorf("expected output to contain 'deep.txt', got %q", out.Content)
	}
	if strings.Contains(out.Content, "deep.go") {
		t.Errorf("expected output NOT to contain 'deep.go', got %q", out.Content)
	}
}

// =============================================================================
// GrepTool Tests
// =============================================================================

func TestGrepTool_BasicSearch(t *testing.T) {
	workDir := t.TempDir()

	// Create files with known content
	file1 := filepath.Join(workDir, "file1.go")
	file2 := filepath.Join(workDir, "file2.go")
	file3 := filepath.Join(workDir, "file3.txt")

	os.WriteFile(file1, []byte("package main\nfunc hello() {}\n"), 0644)
	os.WriteFile(file2, []byte("package tools\nfunc world() {}\n"), 0644)
	os.WriteFile(file3, []byte("no functions here\n"), 0644)

	tool := NewGrepTool(workDir)
	params, _ := json.Marshal(GrepParams{
		Pattern:    "func \\w+",
		OutputMode: "content",
	})
	out, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected tool error: %s", out.Error)
	}

	if !strings.Contains(out.Content, "func hello") {
		t.Errorf("expected output to contain 'func hello', got %q", out.Content)
	}
	if !strings.Contains(out.Content, "func world") {
		t.Errorf("expected output to contain 'func world', got %q", out.Content)
	}
}

func TestGrepTool_RegexMatching(t *testing.T) {
	workDir := t.TempDir()
	filePath := filepath.Join(workDir, "data.txt")
	os.WriteFile(filePath, []byte("error: something went wrong\ninfo: all good\nerror: another problem\n"), 0644)

	tool := NewGrepTool(workDir)
	params, _ := json.Marshal(GrepParams{
		Pattern:    "^error:.*",
		OutputMode: "content",
	})
	out, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected tool error: %s", out.Error)
	}
	if !strings.Contains(out.Content, "error: something went wrong") {
		t.Errorf("expected regex match for first error line, got %q", out.Content)
	}
	if !strings.Contains(out.Content, "error: another problem") {
		t.Errorf("expected regex match for second error line, got %q", out.Content)
	}
	if strings.Contains(out.Content, "info: all good") {
		t.Errorf("did not expect 'info: all good' in output, got %q", out.Content)
	}
}

func TestGrepTool_FilesWithMatches(t *testing.T) {
	workDir := t.TempDir()

	file1 := filepath.Join(workDir, "match1.go")
	file2 := filepath.Join(workDir, "match2.go")
	file3 := filepath.Join(workDir, "nomatch.go")

	os.WriteFile(file1, []byte("TODO: fix this\n"), 0644)
	os.WriteFile(file2, []byte("also TODO: handle error\n"), 0644)
	os.WriteFile(file3, []byte("nothing special here\n"), 0644)

	tool := NewGrepTool(workDir)
	params, _ := json.Marshal(GrepParams{
		Pattern:    "TODO",
		OutputMode: "files_with_matches",
	})
	out, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("unexpected tool error: %s", out.Error)
	}

	if !strings.Contains(out.Content, "match1.go") {
		t.Errorf("expected 'match1.go' in output, got %q", out.Content)
	}
	if !strings.Contains(out.Content, "match2.go") {
		t.Errorf("expected 'match2.go' in output, got %q", out.Content)
	}
	if strings.Contains(out.Content, "nomatch.go") {
		t.Errorf("did not expect 'nomatch.go' in output, got %q", out.Content)
	}
}

// =============================================================================
// ToolRegistry Tests
// =============================================================================

func TestToolRegistry_RegisterAndGet(t *testing.T) {
	registry := NewToolRegistry()
	workDir := t.TempDir()

	bashTool := NewBashTool(workDir, 5*time.Second, nil)
	registry.Register("bash", bashTool)

	executor, ok := registry.Get("bash")
	if !ok {
		t.Fatal("expected to find 'bash' in registry")
	}
	if executor.Spec().Name != "bash" {
		t.Errorf("expected spec name 'bash', got %q", executor.Spec().Name)
	}
}

func TestToolRegistry_AllSpecs(t *testing.T) {
	registry := NewToolRegistry()
	workDir := t.TempDir()

	bashTool := NewBashTool(workDir, 5*time.Second, nil)
	readTool := NewFileReadTool(workDir)
	globTool := NewGlobTool(workDir)

	registry.Register("bash", bashTool)
	registry.Register("file_read", readTool)
	registry.Register("glob", globTool)

	specs := registry.AllSpecs()
	if len(specs) != 3 {
		t.Fatalf("expected 3 specs, got %d", len(specs))
	}

	// AllSpecs returns sorted by name
	expectedNames := []string{"bash", "file_read", "glob"}
	for i, spec := range specs {
		if spec.Name != expectedNames[i] {
			t.Errorf("expected specs[%d].Name=%q, got %q", i, expectedNames[i], spec.Name)
		}
	}
}

func TestToolRegistry_GetNonExistent(t *testing.T) {
	registry := NewToolRegistry()

	_, ok := registry.Get("does_not_exist")
	if ok {
		t.Error("expected Get to return false for non-existent tool")
	}
}
