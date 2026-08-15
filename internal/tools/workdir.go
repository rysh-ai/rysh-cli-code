// SPDX-License-Identifier: Apache-2.0

package tools

// workdir.go — SetWorkDir / CloneTool implementations for every tool that
// anchors relative-path resolution to a working directory.
//
// These two methods let AI-mode tools follow the pane's live shell cwd:
//
//   - SetWorkDir(dir) lets the orchestrator re-point a tool at the shell's
//     current directory at the start of each run (see ToolRegistry.SetWorkDir
//     in rysh-shared/tools). It satisfies tools.WorkDirAware.
//   - CloneTool() returns an independent copy so the per-pane / per-run
//     registry clones don't share a mutable workDir (which would leak one
//     pane's cwd into another). It satisfies the registry's cloneTooler
//     interface. A shallow struct copy is correct here: every tool's only
//     mutable field is workDir (a string); pointer/interface fields such as
//     *BackgroundSessionManager and nats.KeyValue are intentionally shared.
//
// A blank dir is ignored so an unresolved cwd never clobbers the default.
//
// Tools without a workDir (context, bash_session, web_*, env_read, …) omit
// these methods and are shared by reference when a registry is cloned.

func setWorkDir(dst *string, dir string) {
	if dir != "" {
		*dst = dir
	}
}

func (t *BashBackgroundTool) SetWorkDir(dir string)    { setWorkDir(&t.workDir, dir) }
func (t *BashBackgroundTool) CloneTool() ToolExecutor  { cp := *t; return &cp }
func (t *BashTool) SetWorkDir(dir string)              { setWorkDir(&t.workDir, dir) }
func (t *BashTool) CloneTool() ToolExecutor            { cp := *t; return &cp }
func (t *EditTool) SetWorkDir(dir string)              { setWorkDir(&t.workDir, dir) }
func (t *EditTool) CloneTool() ToolExecutor            { cp := *t; return &cp }
func (t *FileReadTool) SetWorkDir(dir string)          { setWorkDir(&t.workDir, dir) }
func (t *FileReadTool) CloneTool() ToolExecutor        { cp := *t; return &cp }
func (t *FileWriteTool) SetWorkDir(dir string)         { setWorkDir(&t.workDir, dir) }
func (t *FileWriteTool) CloneTool() ToolExecutor       { cp := *t; return &cp }
func (t *GitCommitTool) SetWorkDir(dir string)         { setWorkDir(&t.workDir, dir) }
func (t *GitCommitTool) CloneTool() ToolExecutor       { cp := *t; return &cp }
func (t *GlobTool) SetWorkDir(dir string)              { setWorkDir(&t.workDir, dir) }
func (t *GlobTool) CloneTool() ToolExecutor            { cp := *t; return &cp }
func (t *GrepTool) SetWorkDir(dir string)              { setWorkDir(&t.workDir, dir) }
func (t *GrepTool) CloneTool() ToolExecutor            { cp := *t; return &cp }
func (t *MemoryEditTool) SetWorkDir(dir string)        { setWorkDir(&t.workDir, dir) }
func (t *MemoryEditTool) CloneTool() ToolExecutor      { cp := *t; return &cp }
func (t *MonitorTool) SetWorkDir(dir string)           { setWorkDir(&t.workDir, dir) }
func (t *MonitorTool) CloneTool() ToolExecutor         { cp := *t; return &cp }
func (t *ProjectNotesTool) SetWorkDir(dir string)      { setWorkDir(&t.workDir, dir) }
func (t *ProjectNotesTool) CloneTool() ToolExecutor    { cp := *t; return &cp }
func (t *RyshBuildPipelineTool) SetWorkDir(dir string) { setWorkDir(&t.workDir, dir) }
func (t *RyshBuildPipelineTool) CloneTool() ToolExecutor {
	cp := *t
	return &cp
}
func (t *SymbolSearchTool) SetWorkDir(dir string)   { setWorkDir(&t.workDir, dir) }
func (t *SymbolSearchTool) CloneTool() ToolExecutor { cp := *t; return &cp }
func (t *TestRunTool) SetWorkDir(dir string)        { setWorkDir(&t.workDir, dir) }
func (t *TestRunTool) CloneTool() ToolExecutor      { cp := *t; return &cp }
