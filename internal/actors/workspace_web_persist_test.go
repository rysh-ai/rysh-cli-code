// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
	"github.com/rysh-ai/rysh-cli-code/internal/web"
)

// newPersistWorkspace builds the smallest WorkspaceActor persistWebStart needs:
// a rysh dir to write the session's settings into, and a secret store. The
// test's CWD is the temp project, because the persisted secret tier is
// CWD-relative (.rysh/secrets).
func newPersistWorkspace(t *testing.T) (*WorkspaceActor, string) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	ryshDir := filepath.Join(dir, ".rysh")
	w := &WorkspaceActor{
		cfg: config.Config{
			ConfigFile: filepath.Join(dir, "rysh.config.yaml"),
			RyshDir:    ryshDir,
		},
		workspaceName: "demo",
		sessionName:   "demo",
		secrets:       newSecretStore(nil, nil),
	}
	return w, ryshDir
}

// The four values a start is given must all outlive the process: the address
// and the tunnel in the config, the login in the secret store, and auto_start
// so the next daemon does it again without being asked.
func TestPersistWebStartWritesConfigAndSecrets(t *testing.T) {
	w, ryshDir := newPersistWorkspace(t)
	var out strings.Builder

	w.persistWebStart(&out, webStartOpts{
		Host: "0.0.0.0", Port: 23001,
		Username: "alice", Password: "s3cret",
		Ngrok: true, NgrokDomain: "rysh-web.ngrok.app",
	}, nil)

	saved, err := session.LoadWebSettings(ryshDir, w.sessionName)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if !saved.AutoStart || saved.Host != "0.0.0.0" || saved.Port != 23001 {
		t.Fatalf("address not saved: %+v", saved)
	}
	if !saved.Ngrok || saved.NgrokDomain != "rysh-web.ngrok.app" {
		t.Fatalf("tunnel not saved: %+v", saved)
	}
	// The settings file must never hold a credential — it is ordinary state,
	// read and edited freely; the login lives in the secret tier.
	raw, err := os.ReadFile(session.WebSettingsPath(ryshDir, w.sessionName))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if strings.Contains(string(raw), "s3cret") || strings.Contains(string(raw), "alice") {
		t.Fatalf("the login was written into the settings file:\n%s", raw)
	}
	// And nothing was written to the PROJECT's config file, which every sibling
	// session in this project reads.
	if _, err := os.Stat(w.cfg.ConfigFile); !os.IsNotExist(err) {
		cfgRaw, _ := os.ReadFile(w.cfg.ConfigFile)
		t.Fatalf("the project config was written:\n%s", cfgRaw)
	}

	// It is in the secret store instead, readable back by the same names the
	// config refers to.
	scope, _ := w.secretWorkspaceScope()
	if v, _, ok := w.secrets.Get(scope, webUserSecret); !ok || v != "alice" {
		t.Fatalf("username secret = (%q,%v)", v, ok)
	}
	if v, _, ok := w.secrets.Get(scope, webPassSecret); !ok || v != "s3cret" {
		t.Fatalf("password secret = (%q,%v)", v, ok)
	}
	// And on disk with the ##secret layout, so a restarted daemon finds it
	// before its session KV exists.
	if data, rerr := os.ReadFile(filepath.Join(".rysh", "secrets", scope, webPassSecret)); rerr != nil {
		t.Fatalf("persisted secret: %v", rerr)
	} else if strings.TrimSpace(string(data)) != "s3cret" {
		t.Fatalf("persisted secret = %q", data)
	}
	// storedWebCredentials is what the auto-start path re-establishes from.
	if u, p, ok := w.storedWebCredentials(); !ok || u != "alice" || p != "s3cret" {
		t.Fatalf("storedWebCredentials = (%q,%q,%v)", u, p, ok)
	}
}

// A shared door is a different address from the server's own, and must be
// persisted under its own keys — writing it as [web] port would record the
// desktop app's ephemeral port as the session's address.
func TestPersistWebStartSharedDoorUsesOwnKeys(t *testing.T) {
	w, ryshDir := newPersistWorkspace(t)
	var out strings.Builder

	w.persistWebStart(&out, webStartOpts{
		Host: "0.0.0.0", Port: 23001,
		Username: "alice", Password: "s3cret",
		SharedDoor: true,
	}, nil)

	saved, err := session.LoadWebSettings(ryshDir, w.sessionName)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if saved.SharedHost != "0.0.0.0" || saved.SharedPort != 23001 {
		t.Fatalf("shared address not saved under its own keys: %+v", saved)
	}
	// The primary keys must be untouched, so an app-spawned server still binds
	// the port the app gives it.
	if saved.Host != "" || saved.Port != 0 {
		t.Fatalf("shared address leaked into the primary keys: %+v", saved)
	}
	if saved.PublishPort() != 23001 {
		t.Fatalf("PublishPort = %d, want the shared door", saved.PublishPort())
	}
}

// --no-save is the escape hatch: a one-off start must leave the config and the
// secret store exactly as it found them.
func TestPersistWebStartNoSave(t *testing.T) {
	w, ryshDir := newPersistWorkspace(t)
	var out strings.Builder

	w.persistWebStart(&out, webStartOpts{
		Host: "0.0.0.0", Port: 23001,
		Username: "alice", Password: "s3cret",
		NoSave: true,
	}, nil)

	saved, err := session.LoadWebSettings(ryshDir, w.sessionName)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if saved.Configured() {
		t.Fatalf("--no-save saved anyway: %+v", saved)
	}
	scope, _ := w.secretWorkspaceScope()
	if _, _, ok := w.secrets.Get(scope, webPassSecret); ok {
		t.Fatal("--no-save stored the password anyway")
	}
	if !strings.Contains(out.String(), "--no-save") {
		t.Fatalf("no explanation of what was skipped: %q", out.String())
	}
}

// With no rysh directory there is nowhere to persist to. That is worth saying —
// a running server whose settings will not survive is exactly the surprise this
// whole change exists to remove — and must not be an error.
func TestPersistWebStartWithoutRyshDirExplains(t *testing.T) {
	w, _ := newPersistWorkspace(t)
	w.cfg.RyshDir = ""
	var out strings.Builder

	w.persistWebStart(&out, webStartOpts{Host: "0.0.0.0", Port: 23001, Username: "a", Password: "b"}, nil)

	if !strings.Contains(out.String(), "not persisted") {
		t.Fatalf("silent about the missing rysh directory: %q", out.String())
	}
	// The login still goes to the secret store, which needs no config file.
	scope, _ := w.secretWorkspaceScope()
	if v, _, ok := w.secrets.Get(scope, webUserSecret); !ok || v != "a" {
		t.Fatalf("login not stored: (%q,%v)", v, ok)
	}
}

// The restart line is the answer to "will this survive?", and it must be honest
// in both directions.
func TestWriteWebRestartStatus(t *testing.T) {
	w, _ := newPersistWorkspace(t)

	var off strings.Builder
	w.writeWebRestartStatus(&off)
	if !strings.Contains(off.String(), "will NOT bring this back") {
		t.Fatalf("unsaved session reported as durable: %q", off.String())
	}

	w.webSettings = session.WebSettings{
		AutoStart: true, Host: "0.0.0.0", Port: 23001,
		Ngrok: true, NgrokDomain: "rysh-web.ngrok.app",
	}
	w.webSettingsLoaded = true
	_ = w.secrets.Set(mustScope(t, w), webUserSecret, "alice", true)

	var on strings.Builder
	w.writeWebRestartStatus(&on)
	for _, want := range []string{"on restart", "23001", "rysh-web.ngrok.app", `"alice"`} {
		if !strings.Contains(on.String(), want) {
			t.Errorf("restart line missing %q: %q", want, on.String())
		}
	}
}

func mustScope(t *testing.T, w *WorkspaceActor) string {
	t.Helper()
	scope, _ := w.secretWorkspaceScope()
	return scope
}

// A start with no flags, no web-auth.json, but the login sitting in the secret
// store must serve — the secrets are the durable copy, and refusing there was
// the gap that made a hand-run `##rysh web auth` necessary before every first
// start on a fresh machine.
func TestResolveWebStartLoginFallsBackToSecrets(t *testing.T) {
	w, _ := newPersistWorkspace(t)
	scope, _ := w.secretWorkspaceScope()
	if err := w.secrets.Set(scope, webUserSecret, "alice", true); err != nil {
		t.Fatalf("seed username: %v", err)
	}
	if err := w.secrets.Set(scope, webPassSecret, "s3cret", true); err != nil {
		t.Fatalf("seed password: %v", err)
	}

	var out strings.Builder
	creds, ok := w.resolveWebStartLogin(&out, webStartOpts{Port: 23001})
	if !ok || creds == nil {
		t.Fatalf("start refused despite a login in the secret store: %q", out.String())
	}
	if creds.Username != "alice" {
		t.Fatalf("username = %q, want alice", creds.Username)
	}
	if !strings.Contains(out.String(), "secret store") {
		t.Fatalf("no explanation of where the login came from: %q", out.String())
	}
	// The hash file exists now, so a browser has something to check against —
	// this session's own, not the project's.
	stored, err := web.LoadCredentialsFor(w.credentialsRef())
	if err != nil || stored == nil || stored.Username != "alice" {
		t.Fatalf("credentials not written back: %+v (%v)", stored, err)
	}
	if got := w.credentialsRef().OwnPath(); !strings.Contains(got, w.sessionName+"-auth.json") {
		t.Fatalf("login written to %s, want this session's own file", got)
	}
}

// With neither flags, nor a hash, nor secrets, the start still refuses — the UI
// is never served unauthenticated.
func TestResolveWebStartLoginStillRefusesWithNothing(t *testing.T) {
	w, _ := newPersistWorkspace(t)
	var out strings.Builder
	if _, ok := w.resolveWebStartLogin(&out, webStartOpts{Port: 23001}); ok {
		t.Fatal("start allowed with no login anywhere")
	}
}

// `##rysh web auth` writes the secret tier too: a login set there and one set
// by `web start` must not be able to drift, or a restart resurrects whichever
// copy is older.
func TestWebAuthAlsoStoresSecrets(t *testing.T) {
	w, _ := newPersistWorkspace(t)
	var out strings.Builder
	w.handleWebAuth(&out, []string{"username=bob", "password=hunter2"})

	scope, _ := w.secretWorkspaceScope()
	if v, _, ok := w.secrets.Get(scope, webUserSecret); !ok || v != "bob" {
		t.Fatalf("username secret = (%q,%v)", v, ok)
	}
	if v, _, ok := w.secrets.Get(scope, webPassSecret); !ok || v != "hunter2" {
		t.Fatalf("password secret = (%q,%v)", v, ok)
	}
	if u, p, ok := w.storedWebCredentials(); !ok || u != "bob" || p != "hunter2" {
		t.Fatalf("storedWebCredentials = (%q,%q,%v)", u, p, ok)
	}
}

// What was just written to the file must also be true of the config this actor
// holds: otherwise `##rysh web status` answers from the file as it looked when
// the daemon started and calls a saved setup unsaved.
func TestPersistWebStartUpdatesLiveConfig(t *testing.T) {
	w, _ := newPersistWorkspace(t)
	var out strings.Builder

	w.persistWebStart(&out, webStartOpts{
		Host: "0.0.0.0", Port: 23002,
		Username: "alice", Password: "s3cret",
		Ngrok: true, NgrokDomain: "rysh-web.ngrok.app",
	}, nil)

	live := w.sessionWebSettings()
	if !live.AutoStart || live.Host != "0.0.0.0" || live.Port != 23002 {
		t.Fatalf("live settings not updated: %+v", live)
	}
	if !live.Ngrok || live.NgrokDomain != "rysh-web.ngrok.app" {
		t.Fatalf("live tunnel settings not updated: %+v", live)
	}
	var status strings.Builder
	w.writeWebRestartStatus(&status)
	if strings.Contains(status.String(), "will NOT bring this back") {
		t.Fatalf("status calls a just-saved setup unsaved: %q", status.String())
	}
}

// The bug that prompted the move: two sessions rooted at the same project share
// rysh.config.yaml, so a `##rysh web start` in one used to tell the OTHER to
// auto-start on the same port and fight it for the same tunnel.
func TestWebSettingsDoNotLeakBetweenSessionsOfOneProject(t *testing.T) {
	w, ryshDir := newPersistWorkspace(t)
	var out strings.Builder
	w.persistWebStart(&out, webStartOpts{Host: "0.0.0.0", Port: 23002, Ngrok: true}, nil)

	// A sibling session in the SAME project (same rysh dir, same config file).
	sibling := &WorkspaceActor{
		cfg:           w.cfg,
		workspaceName: "demo-elect",
		sessionName:   "demo-elect",
		secrets:       newSecretStore(nil, nil),
	}
	got := sibling.sessionWebSettings()
	if got.Configured() {
		t.Fatalf("a sibling session inherited this session's web settings: %+v", got)
	}
	// And the configuring session still has its own.
	mine, err := session.LoadWebSettings(ryshDir, w.sessionName)
	if err != nil || mine.Port != 23002 {
		t.Fatalf("own settings = %+v (%v)", mine, err)
	}
}

// A session that has never been configured still honours the project-wide
// [web] block — that is how the desktop app's sidecar is driven
// (RYSH_WEB_PORT / RYSH_WEB_AUTO_START land in the same fields).
func TestSessionWebSettingsFallBackToProjectConfig(t *testing.T) {
	w, _ := newPersistWorkspace(t)
	w.cfg.WebAutoStart, w.cfg.WebHost, w.cfg.WebPort = true, "127.0.0.1", 56036

	got := w.sessionWebSettings()
	if !got.AutoStart || got.Host != "127.0.0.1" || got.Port != 56036 {
		t.Fatalf("project defaults not applied: %+v", got)
	}
}

// Once the session has its own settings, they win over the project defaults.
func TestSessionWebSettingsBeatProjectConfig(t *testing.T) {
	w, ryshDir := newPersistWorkspace(t)
	if err := session.SaveWebSettings(ryshDir, w.sessionName,
		session.WebSettings{AutoStart: true, Host: "0.0.0.0", Port: 23002}); err != nil {
		t.Fatal(err)
	}
	w.cfg.WebAutoStart, w.cfg.WebHost, w.cfg.WebPort = true, "127.0.0.1", 56036

	got := w.sessionWebSettings()
	if got.Host != "0.0.0.0" || got.Port != 23002 {
		t.Fatalf("project config overrode the session's own settings: %+v", got)
	}
}

// The layer the desktop app depends on: a daemon spawned with RYSH_WEB_PORT is
// being waited on at THAT port by the process that spawned it, so it wins over
// anything on disk. Binding what a session file remembers instead would leave
// the app unable to reach the daemon it just started.
func TestSessionWebSettingsEnvWinsOverEverything(t *testing.T) {
	w, ryshDir := newPersistWorkspace(t)
	if err := session.SaveWebSettings(ryshDir, w.sessionName,
		session.WebSettings{AutoStart: true, Host: "0.0.0.0", Port: 23001}); err != nil {
		t.Fatal(err)
	}
	w.cfg.WebPort = 23232
	t.Setenv("RYSH_WEB_PORT", "56036")

	got := w.sessionWebSettings()
	if got.Port != 56036 {
		t.Fatalf("port = %d, want the one this daemon was handed", got.Port)
	}
	// Everything the environment did NOT name still comes from the session.
	if got.Host != "0.0.0.0" || !got.AutoStart {
		t.Fatalf("the rest of the session's settings were lost: %+v", got)
	}
}

// A session that saved ONLY a shared door (the desktop-app shape: the primary
// port comes from the app, the second address is the session's own) must keep
// the project/env primary port rather than falling back to a default.
func TestSessionWebSettingsSharedOnlyKeepsPrimaryPort(t *testing.T) {
	w, ryshDir := newPersistWorkspace(t)
	if err := session.SaveWebSettings(ryshDir, w.sessionName, session.WebSettings{
		AutoStart: true, SharedHost: "127.0.0.1", SharedPort: 23001,
		Ngrok: true, NgrokDomain: "coming-gush-snowfall.ngrok-free.dev",
	}); err != nil {
		t.Fatal(err)
	}
	w.cfg.WebAutoStart, w.cfg.WebPort = true, 56036

	got := w.sessionWebSettings()
	if got.Port != 56036 {
		t.Fatalf("primary port = %d, want the project/env one (56036)", got.Port)
	}
	if got.SharedPort != 23001 || got.SharedHost != "127.0.0.1" {
		t.Fatalf("shared door lost: %+v", got)
	}
	if !got.Ngrok || got.NgrokDomain != "coming-gush-snowfall.ngrok-free.dev" {
		t.Fatalf("tunnel lost: %+v", got)
	}
	if got.PublishPort() != 23001 {
		t.Fatalf("PublishPort = %d, want the shared door", got.PublishPort())
	}
}

// A session serving the PROJECT's login while its own secrets say something
// else is serving the wrong password: it inherited only because it had nothing
// of its own. This is the desktop-app case — two sessions in one project, each
// with its own web secrets.
func TestInheritedLoginIsStaleWhenSessionSecretsDiffer(t *testing.T) {
	w, _ := newPersistWorkspace(t)
	// The project login, as another session left it.
	if _, err := web.SaveCredentials(w.cfg.RyshDir, "halilagin", "cli-password"); err != nil {
		t.Fatal(err)
	}
	if w.inheritedLoginIsStale() {
		t.Fatal("inherited login called stale with no secrets of our own")
	}

	scope, _ := w.secretWorkspaceScope()
	_ = w.secrets.Set(scope, webUserSecret, "halilagin", true)
	_ = w.secrets.Set(scope, webPassSecret, "app-password", true)
	if !w.inheritedLoginIsStale() {
		t.Fatal("a different password in this session's secrets was not noticed")
	}

	// Matching secrets are not stale — no reason to rewrite anything.
	_ = w.secrets.Set(scope, webPassSecret, "cli-password", true)
	if w.inheritedLoginIsStale() {
		t.Fatal("matching secrets reported as stale")
	}

	// And once the session has its OWN credentials file, the question is moot.
	_ = w.secrets.Set(scope, webPassSecret, "app-password", true)
	if _, err := web.SaveCredentialsFor(w.credentialsRef(), "halilagin", "app-password"); err != nil {
		t.Fatal(err)
	}
	if w.inheritedLoginIsStale() {
		t.Fatal("a session with its own login still reported as inheriting")
	}
	// The project file is untouched by all of this.
	proj, err := web.LoadCredentials(w.cfg.RyshDir)
	if err != nil || proj == nil || !proj.Verify("halilagin", "cli-password") {
		t.Fatalf("the project login was disturbed: %+v (%v)", proj, err)
	}
}
