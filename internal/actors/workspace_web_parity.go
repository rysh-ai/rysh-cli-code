package actors

// workspace_web_parity.go — wires the web server's electronAPI-parity surface
// (web_electron_roadmap Phase 3, W7–W10 + W12) with the daemon-side data it
// serves: voice config, the bash-completion flag, the current workspace and
// the on-disk session registry (recent workspaces). Called at BOTH web-server
// creation sites (auto-start and `##rysh web start`) so the browser UI gets
// the same capabilities regardless of how the server was started.

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/rysh-ai/rysh-cli-code/internal/session"
	"github.com/rysh-ai/rysh-cli-code/internal/web"
)

// recordWebEndpoint publishes a running web server's loopback endpoint (port)
// onto this session's registry record, or clears it when the server is not
// running. Call it after every Start/Stop at both start sites.
//
// This is the hand-off that lets the desktop app open a session it did not
// create. The app's renderer reaches a daemon only over HTTP/WebSocket, so
// without a recorded endpoint it could adopt only daemons it spawned itself and
// therefore knew the port of — which is precisely why command-line sessions
// used to be unreachable from the app. With the endpoint on the record, any
// front-end can find a live daemon's door.
//
// The endpoint is guarded exactly as `##rysh web start` left it — the
// workspace's login, or nothing in control mode; recording it changes nothing
// about who can reach the port, only whether a local front-end can discover it.
func (w *WorkspaceActor) recordWebEndpoint(srv *web.Server) {
	if srv == nil || !srv.IsRunning() {
		session.UpdateWebEndpoint(w.cfg, w.sessionName, 0)
		return
	}
	session.UpdateWebEndpoint(w.cfg, w.sessionName, srv.Port())
}

// configureWebParity injects everything the parity API (/api/env,
// /api/workspaces, /api/voice/*, ws completion, server-side web panes) needs.
// Must run before srv.Start(). Captures immutable copies only (config,
// session name) — the injected closures run on web-server goroutines.
func (w *WorkspaceActor) configureWebParity(srv *web.Server) {
	cfg := w.cfg

	// W10 — voice: same [voice]/[voice_control] keys the TUI and the desktop
	// app read; the key stays server-side.
	srv.SetVoice(web.VoiceSettings{
		Enabled:  cfg.Voice.Enabled,
		Provider: cfg.VoiceControl.TTSProviderName,
		APIKey:   cfg.VoiceControl.APIKey,
		Hotkey:   cfg.Voice.Hotkey,
		Language: cfg.Voice.Language,
	})

	// W7 — completion: bash programmable-completion delegation follows the
	// same [ui] shell_bash_completion switch as the TUI.
	srv.SetBashCompletion(cfg.ShellBashCompletion)

	// W8/W12 — current workspace (also the root for web-pane browser profiles).
	wd := cfg.WorkingDirectory
	if wd == "" {
		wd, _ = os.Getwd()
	}
	srv.SetWorkspaceInfo(wd, w.workspaceName)

	// W8 — recent workspaces from the on-disk session registry (the same
	// records `rysh list-sessions` reads), newest first, deduped by path.
	srv.SetWorkspaceLister(func() []web.WorkspaceEntry {
		store, err := session.NewStore(cfg)
		if err != nil {
			return nil
		}
		records, err := store.List()
		if err != nil {
			return nil
		}
		sort.Slice(records, func(i, j int) bool {
			return records[i].UpdatedAt.After(records[j].UpdatedAt)
		})
		seen := make(map[string]bool, len(records))
		out := make([]web.WorkspaceEntry, 0, len(records))
		for _, rec := range records {
			if rec.Path == "" || seen[rec.Path] {
				continue
			}
			seen[rec.Path] = true
			out = append(out, web.WorkspaceEntry{
				Path:        rec.Path,
				Name:        filepath.Base(rec.Path),
				LastOpened:  rec.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
				LastSession: rec.Name,
			})
		}
		return out
	})
}
