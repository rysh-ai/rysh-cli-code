package actors

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// cmdWorkspaceCwd shows or sets the ACTIVE workspace's working directory — the
// directory that panes created from now on start their shell in. With no
// argument it prints the current value.
//
// A successful set does three things:
//  1. updates this (active) workspace actor's in-memory cfg.WorkingDirectory;
//  2. broadcasts the change to existing tabs/lanes/groups so panes added to
//     already-open groups also use it (running panes keep their shells);
//  3. persists it to this workspace's entry in rysh.config.yaml so it survives a
//     restart.
//
// It deliberately does NOT touch the session registry record — the working dir
// is per-workspace, whereas the registry Path is the session's project root.
//
// Note on scope: only this active workspace's actor and the on-disk config are
// updated. The farm's in-memory config and sibling (inactive) workspace actors
// keep their values until a restart, when the config is re-read — which is
// harmless because reconcile never restarts running workspaces.
func (w *WorkspaceActor) cmdWorkspaceCwd(out *strings.Builder, arg string) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		cur := strings.TrimSpace(w.cfg.WorkingDirectory)
		if cur == "" {
			cur = "(daemon working directory)"
		}
		fmt.Fprintf(out, "\n[rysh] workspace %q working dir: %s\n", w.workspaceName, cur)
		fmt.Fprintf(out, "  set with: ##workspace cwd <path>\n")
		return
	}

	// Resolve: expand ~, and resolve a relative path against the active
	// workspace's current working dir (or the daemon cwd when none is set) — cd
	// semantics, not relative-to-config-file.
	dir := config.ExpandHome(arg)
	if !filepath.IsAbs(dir) {
		base := strings.TrimSpace(w.cfg.WorkingDirectory)
		if base == "" {
			if cwd, err := os.Getwd(); err == nil {
				base = cwd
			}
		}
		dir = filepath.Join(base, dir)
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}

	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(out, "\n[rysh] not a directory: %s\n", dir)
		fmt.Fprintf(out, "  usage: ##workspace cwd <path>\n")
		return
	}

	w.cfg.WorkingDirectory = dir
	w.broadcastWorkingDir(dir)

	// Persist to this workspace's entry in rysh.config.yaml (matched by index).
	cfgWritten := false
	var cfgErr error
	if cfgFile := strings.TrimSpace(w.cfg.ConfigFile); cfgFile != "" {
		if err := config.SetWorkspaceWorkingDir(cfgFile, w.workspaceIdx, w.workspaceName, dir); err == nil {
			cfgWritten = true
		} else {
			cfgErr = err
		}
	}

	fmt.Fprintf(out, "\n[rysh] workspace %q working dir set to: %s\n", w.workspaceName, dir)
	fmt.Fprintf(out, "  new panes will start here (existing panes keep their shells)\n")
	if cfgWritten {
		fmt.Fprintf(out, "  written to %s\n", w.cfg.ConfigFile)
	} else if cfgErr != nil {
		fmt.Fprintf(out, "  warning: could not update rysh.config.yaml: %v\n", cfgErr)
	}
}

// broadcastWorkingDir fans a working-directory change out to every tab in this
// workspace, which forwards it down to its lanes and pane groups (see
// MsgSetWorkingDir). w.tabs are the active workspace's own tabs, so the
// broadcast is naturally scoped to this workspace.
func (w *WorkspaceActor) broadcastWorkingDir(dir string) {
	for _, t := range w.tabs {
		_ = w.pub.Send(msg.T("tab", t.id, "inbox"), &msg.MsgSetWorkingDir{Dir: dir})
	}
}
