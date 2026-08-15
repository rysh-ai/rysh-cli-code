// SPDX-License-Identifier: Apache-2.0

package session

// Per-SESSION web-server settings: how this one session is served, and whether
// it is published.
//
// These used to be written into `[web]` in rysh.config.yaml, which was wrong in
// a way that only shows up on a machine running more than one session. A config
// file belongs to a PROJECT, and every session rooted at that project reads it —
// so `##rysh web start --port 23002` in one session told every OTHER session to
// auto-start on 23002 too, where they would collide on the port and fight over
// the same tunnel. On this machine that meant a command-line session silently
// reconfiguring the desktop app's.
//
// A web server is a property of a session (it binds one port, it serves one
// session's panes), so its settings live per session, beside the registry
// record: <ryshDir>/web/<session>.json.
//
// Deliberately NOT in the registry record itself (<ryshDir>/sessions/<name>.json).
// That record is LIVE state — pid, attached TUIs, the port right now — rebuilt by
// whichever daemon is running and cleared when nothing is. These are DESIRED
// state: they must survive a stop, which is the entire point.
//
// The login is not here. It lives in the secret store (RYSH_WEB_USERNAME /
// RYSH_WEB_PASSWORD, workspace-scoped) with only its hash in web-auth.json, so
// this file never holds a credential and can be read, diffed and edited freely.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// WebSettings is what `##rysh web start` learned about how to serve a session.
type WebSettings struct {
	// AutoStart brings the server up when the session's daemon starts.
	AutoStart bool `json:"auto_start"`
	// Host and Port are the bind address of the session's own server.
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	// SharedHost/SharedPort are the SECOND address a session is served at — the
	// shared door opened onto an already-running server, which is the normal
	// case under the desktop app (its sidecar starts one as its own transport,
	// on a port the app picks and passes in RYSH_WEB_PORT). Kept apart from
	// Host/Port because they answer different questions: what this daemon
	// binds, versus what other people were given.
	SharedHost string `json:"shared_host,omitempty"`
	SharedPort int    `json:"shared_port,omitempty"`
	// Ngrok publishes the server at a public HTTPS URL on every start;
	// NgrokDomain pins a reserved domain so the URL survives restarts.
	Ngrok       bool   `json:"ngrok,omitempty"`
	NgrokDomain string `json:"ngrok_domain,omitempty"`
	// UpdatedAt is when the settings were last written.
	UpdatedAt time.Time `json:"updated_at"`
}

// PublishPort is the port a tunnel should point at: the shared door when one is
// configured (the address meant for other people), else the server's own.
func (s WebSettings) PublishPort() int {
	if s.SharedPort > 0 {
		return s.SharedPort
	}
	return s.Port
}

// Configured reports whether anything was ever saved for this session.
func (s WebSettings) Configured() bool {
	return s.AutoStart || s.Port > 0 || s.SharedPort > 0 || s.Ngrok
}

// webSettingsDir is the directory holding per-session web settings. It is a
// sibling of sessions/, NOT a file inside it: Store.List treats every *.json
// under sessions/ as a session record, so a settings file there would appear in
// `##session list` as a session that does not exist.
func webSettingsDir(ryshDir string) string { return filepath.Join(ryshDir, "web") }

// WebSettingsPath is the file backing one session's web settings.
func WebSettingsPath(ryshDir, session string) string {
	return filepath.Join(webSettingsDir(ryshDir), sanitizeName(session)+".json")
}

// LoadWebSettings reads a session's web settings. A missing file is not an
// error — it means the session was never configured — and returns the zero
// value. A file that cannot be parsed IS an error: silently serving the
// defaults would be a session quietly changing how it is exposed.
func LoadWebSettings(ryshDir, session string) (WebSettings, error) {
	data, err := os.ReadFile(WebSettingsPath(ryshDir, session))
	if err != nil {
		if os.IsNotExist(err) {
			return WebSettings{}, nil
		}
		return WebSettings{}, fmt.Errorf("read web settings: %w", err)
	}
	var s WebSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return WebSettings{}, fmt.Errorf("parse %s: %w", WebSettingsPath(ryshDir, session), err)
	}
	return s, nil
}

// SaveWebSettings writes a session's web settings, stamping UpdatedAt. The
// write is atomic (temp file + rename) so a daemon reading it at startup can
// never see half a file.
func SaveWebSettings(ryshDir, session string, s WebSettings) error {
	dir := webSettingsDir(ryshDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	s.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode web settings: %w", err)
	}
	path := WebSettingsPath(ryshDir, session)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write web settings: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write web settings: %w", err)
	}
	return nil
}

// ClearWebSettings forgets a session's web settings, reporting whether there
// was anything to forget.
func ClearWebSettings(ryshDir, session string) (bool, error) {
	err := os.Remove(WebSettingsPath(ryshDir, session))
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, err
	}
}
