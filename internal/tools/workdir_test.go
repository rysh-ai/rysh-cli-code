package tools

import (
	"testing"
	"time"
)

// workDirCloner mirrors the (unexported) capabilities the shared registry
// relies on: WorkDirAware + cloneTooler. Every working-directory tool must
// satisfy both so AI-mode tools can follow the pane's shell cwd safely.
type workDirCloner interface {
	SetWorkDir(string)
	CloneTool() ToolExecutor
}

// TestWorkDirToolsImplementInterfaces asserts that every tool constructed with
// a working directory implements SetWorkDir + CloneTool. If a new workDir tool
// is added without the methods (workdir.go), this fails.
func TestWorkDirToolsImplementInterfaces(t *testing.T) {
	mgr := NewBackgroundSessionManager()
	tools := map[string]interface{}{
		"bash_background":     NewBashBackgroundTool("/tmp", mgr),
		"bash":                NewBashTool("/tmp", 5*time.Second, nil),
		"edit":                NewEditTool("/tmp"),
		"file_read":           NewFileReadTool("/tmp"),
		"file_write":          NewFileWriteTool("/tmp"),
		"git_commit":          NewGitCommitTool("/tmp"),
		"glob":                NewGlobTool("/tmp"),
		"grep":                NewGrepTool("/tmp"),
		"memory_edit":         NewMemoryEditTool("/tmp"),
		"monitor":             NewMonitorTool("/tmp"),
		"project_notes":       NewProjectNotesTool("/tmp"),
		"rysh_build_pipeline": NewRyshBuildPipelineTool("/tmp", nil),
		"symbol_search":       NewSymbolSearchTool("/tmp"),
		"test_run":            NewTestRunTool("/tmp"),
	}
	for name, tool := range tools {
		if _, ok := tool.(workDirCloner); !ok {
			t.Errorf("%s does not implement SetWorkDir + CloneTool", name)
		}
	}
}

// TestCloneToolIsolatesWorkDir verifies that CloneTool produces an independent
// copy: changing the clone's workDir must not affect the original.
func TestCloneToolIsolatesWorkDir(t *testing.T) {
	orig := NewBashTool("/orig", 5*time.Second, nil)
	clone := orig.CloneTool().(*BashTool)
	clone.SetWorkDir("/clone")

	if orig.workDir != "/orig" {
		t.Errorf("orig workDir = %q, want /orig (clone must not mutate it)", orig.workDir)
	}
	if clone.workDir != "/clone" {
		t.Errorf("clone workDir = %q, want /clone", clone.workDir)
	}
}

// TestSetWorkDirBlankIsNoop verifies a blank dir leaves the default intact.
func TestSetWorkDirBlankIsNoop(t *testing.T) {
	ed := NewEditTool("/default")
	ed.SetWorkDir("")
	if ed.workDir != "/default" {
		t.Errorf("workDir = %q, want /default after blank SetWorkDir", ed.workDir)
	}
	ed.SetWorkDir("/changed")
	if ed.workDir != "/changed" {
		t.Errorf("workDir = %q, want /changed", ed.workDir)
	}
}
