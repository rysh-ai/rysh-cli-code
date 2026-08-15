// SPDX-License-Identifier: Apache-2.0

package actors

// Persistence for `##rysh web start`: the address and tunnel go to this
// SESSION's web settings, the login to the secret store, and the live shape of
// the whole thing to the session record.
//
// The parameters used to survive exactly as long as the process did. A start
// that named `--bind 0.0.0.0 --port 23001 --username u --password p` came back
// after a restart as loopback:23232 with a login nobody could reproduce —
// web-auth.json holds a bcrypt hash, which authenticates a browser but cannot
// re-establish the same login on a new server. Typing those four values is a
// statement about how this session is served; this file makes the next start
// repeat it.
//
// Where each value lands, and why:
//
//	bind/port/ngrok  .rysh/web/<session>.json   desired state for THIS session
//	                                            (see session.WebSettings for why
//	                                            not rysh.config.yaml: a config
//	                                            file belongs to a project, and
//	                                            every session rooted there reads
//	                                            it — one session's port became
//	                                            every session's port)
//	username/password  .rysh/secrets/<scope>/    the secret tier ##secret owns;
//	                   + ${NAME} refs in [web]   YAML gets references, never
//	                                            literals (design 004 G4)
//	live address/URL   session record            what other processes — the
//	                                            desktop app, ##session info —
//	                                            read to find this session

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/session"
	"github.com/rysh-ai/rysh-cli-code/internal/web"
)

// The secret names the web login is stored under. They are ordinary secrets:
// `##secret list` shows them, `##secret delete` removes them, and the [web]
// block refers to them by ${NAME}.
const (
	webUserSecret = "RYSH_WEB_USERNAME"
	webPassSecret = "RYSH_WEB_PASSWORD"
)

// persistWebStart records a `##rysh web start` so the next one repeats it: the
// login into the secret store, the address/tunnel into rysh.config.yaml, and
// auto_start so a restarted daemon brings the server back by itself.
//
// Every failure is reported and then survived. A server that is already serving
// must not be undone because a config file could not be written — the user gets
// told what will not come back after a restart, and the session keeps running.
func (w *WorkspaceActor) persistWebStart(out *strings.Builder, opts webStartOpts, creds *web.Credentials) {
	if opts.NoSave {
		fmt.Fprintf(out, "[rysh] --no-save: these parameters apply to this run only\n")
		return
	}

	// 1. The login, into the secret store — the same tier and the same files
	//    `##secret set --persist` writes, scoped to this workspace.
	savedLogin := false
	if opts.Username != "" && opts.Password != "" {
		savedLogin = w.storeWebLoginSecrets(out, opts.Username, opts.Password)
	}

	// 2. The address, the tunnel and auto_start, into this session's own
	//    settings file. Read-modify-write: a start that named only a port must
	//    not forget the tunnel an earlier one configured.
	dir := strings.TrimSpace(w.cfg.RyshDir)
	if dir == "" {
		fmt.Fprintf(out, "[rysh] no rysh directory — the bind address and port are not persisted\n")
		return
	}
	settings, err := session.LoadWebSettings(dir, w.sessionName)
	if err != nil {
		// A settings file that cannot be read is not overwritten blind: it may
		// hold a configuration worth keeping, and clobbering it is worse than
		// declining to save.
		fmt.Fprintf(out, "[rysh] %v\n", err)
		fmt.Fprintf(out, "[rysh] the server is running, but these parameters are not saved — fix or remove %s\n",
			session.WebSettingsPath(dir, w.sessionName))
		return
	}
	if opts.SharedDoor {
		settings.SharedHost, settings.SharedPort = opts.Host, opts.Port
	} else {
		settings.Host, settings.Port = opts.Host, opts.Port
	}
	settings.AutoStart = true
	if opts.Ngrok {
		settings.Ngrok = true
		if opts.NgrokDomain != "" {
			settings.NgrokDomain = opts.NgrokDomain
		}
	}
	if err := session.SaveWebSettings(dir, w.sessionName, settings); err != nil {
		fmt.Fprintf(out, "[rysh] could not save the web settings: %v\n", err)
		fmt.Fprintf(out, "[rysh] the server is running, but a restart will not bring it back on this address\n")
		return
	}
	// Hold what was written, so everything reading the live settings — the
	// restart line in `##rysh web status` most visibly — answers from what was
	// just saved rather than from what the daemon read at startup.
	w.webSettings = settings

	what := "starts this server again on"
	if opts.SharedDoor {
		what = "serves this session again at"
	}
	// The BIND address, not the browsable one: this line is about what the next
	// start will listen on, where "0.0.0.0" and "localhost" are not the same
	// claim at all.
	fmt.Fprintf(out, "[rysh] saved to %s — a restart of session %q %s %s:%d\n",
		session.WebSettingsPath(dir, w.sessionName), w.sessionName, what,
		bindAddrText(opts.Host), opts.Port)
	if opts.Ngrok {
		fmt.Fprintf(out, "[rysh] and re-establishes the tunnel\n")
	}
	fmt.Fprintf(out, "[rysh] these settings are this session's alone — sibling sessions in this project are untouched\n")
	// The config is only half the restart: the credentials themselves have to be
	// re-establishable, and a bcrypt hash is not.
	if !savedLogin && creds != nil && w.storedWebLogin() == "" {
		fmt.Fprintf(out, "[rysh] the login is not in the secret store — pass --username/--password once so a restart can re-establish it\n")
	}
}

// storeWebLoginSecrets writes a login into the workspace's secret store, the
// durable copy every other tier is derived from: web-auth.json holds only a
// hash, and the [web] block only ${NAME} references to these two. Reports what
// happened and whether both landed.
func (w *WorkspaceActor) storeWebLoginSecrets(out *strings.Builder, user, pass string) bool {
	if w.secrets == nil || user == "" || pass == "" {
		return false
	}
	scope, label := w.secretWorkspaceScope()
	if err := w.secrets.Set(scope, webUserSecret, user, true); err != nil {
		fmt.Fprintf(out, "[rysh] could not store the username as a secret: %v\n", err)
		return false
	}
	if err := w.secrets.Set(scope, webPassSecret, pass, true); err != nil {
		fmt.Fprintf(out, "[rysh] could not store the password as a secret: %v\n", err)
		return false
	}
	fmt.Fprintf(out, "[rysh] login stored as secrets %s / %s in workspace %q\n",
		webUserSecret, webPassSecret, label)
	return true
}

// storedWebLogin returns the username the secret store holds for this
// workspace, or "" when the login was never stored there.
func (w *WorkspaceActor) storedWebLogin() string {
	if w.secrets == nil {
		return ""
	}
	scope, _ := w.secretWorkspaceScope()
	v, _, ok := w.secrets.Get(scope, webUserSecret)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

// storedWebCredentials reads the login out of the secret store. Used by the
// auto-start path to re-establish the SAME login on a fresh server, which the
// bcrypt hash in web-auth.json cannot do.
func (w *WorkspaceActor) storedWebCredentials() (user, pass string, ok bool) {
	if w.secrets == nil {
		return "", "", false
	}
	scope, _ := w.secretWorkspaceScope()
	u, _, uok := w.secrets.Get(scope, webUserSecret)
	p, _, pok := w.secrets.Get(scope, webPassSecret)
	u, p = strings.TrimSpace(u), strings.TrimSpace(p)
	if !uok || !pok || u == "" || p == "" {
		return "", "", false
	}
	return u, p, true
}

// recordWebMeta publishes the live shape of the web server — bind address,
// port, login name and public tunnel URL — onto the session record, so any
// other process can find this session's door. A nil server clears it.
func (w *WorkspaceActor) recordWebMeta(srv *web.Server) {
	if srv == nil || !srv.IsRunning() {
		session.UpdateWebMeta(w.cfg, w.sessionName, session.WebMeta{})
		return
	}
	meta := session.WebMeta{
		Port: srv.Port(),
		Host: srv.Host(),
		User: srv.LoginUsername(),
	}
	if w.webTunnel != nil {
		meta.PublicURL = w.webTunnel.URL
	}
	// Port stays the server's OWN port even when a shared door is open: it is
	// the endpoint the desktop app dials to adopt this daemon, and pointing it
	// at the shared address would send the app somewhere it cannot sign in.
	session.UpdateWebMeta(w.cfg, w.sessionName, meta)
}

// writeWebRestartStatus reports what a restart of THIS session will actually
// serve — the question `##rysh web status` leaves unanswered otherwise, and the
// one that used to have the disappointing answer (loopback, no login, no
// tunnel) no matter what the running server was doing.
func (w *WorkspaceActor) writeWebRestartStatus(out *strings.Builder) {
	s := w.sessionWebSettings()
	if !s.AutoStart {
		fmt.Fprintf(out, "[rysh] a restart will NOT bring this back — save it with: ##rysh web start --bind <addr> --port <n> --username <u> --password <p>\n")
		return
	}
	addr := fmt.Sprintf("%s:%d", bindAddrText(s.Host), s.Port)
	if s.SharedPort > 0 {
		addr += fmt.Sprintf(" (shared at %s:%d)", bindAddrText(s.SharedHost), s.SharedPort)
	}
	fmt.Fprintf(out, "[rysh] on restart: serves %s", addr)
	if s.Ngrok {
		if d := strings.TrimSpace(s.NgrokDomain); d != "" {
			fmt.Fprintf(out, ", published at https://%s", d)
		} else {
			// No domain of our own pinned. Whether the URL is stable is then the
			// ngrok agent's business — a reserved domain in its own config keeps
			// it, an ephemeral tunnel does not — so this promises neither.
			fmt.Fprintf(out, ", published through ngrok (no domain pinned here)")
		}
	}
	if user := w.storedWebLogin(); user != "" {
		fmt.Fprintf(out, ", login %q from the secret store", user)
	}
	fmt.Fprintf(out, "\n")
}

// sessionWebSettings returns this session's web settings: the session's own
// file MERGED OVER the project-wide defaults, field by field, with anything
// this daemon was handed in its environment winning outright.
//
// Merged, not replaced, because the three layers answer different questions. A
// session file says what this session was told; `[web]` says what the project
// defaults to; RYSH_WEB_HOST/RYSH_WEB_PORT say what THIS daemon process was
// handed at spawn — which under the desktop app is an ephemeral port the app
// picked and is, at this moment, waiting to connect on. Replacing the whole set
// with the session file would have an app-spawned daemon bind 23232 (or a port
// a shared door once used) while the app dials the port it chose, and the app
// would never reach the daemon it just started.
func (w *WorkspaceActor) sessionWebSettings() session.WebSettings {
	if w.webSettingsLoaded {
		return w.webSettings
	}
	w.webSettingsLoaded = true

	// Layer 1: the project-wide defaults ([web], already carrying any env
	// overrides applied at config load).
	merged := session.WebSettings{
		AutoStart:   w.cfg.WebAutoStart,
		Host:        w.cfg.WebHost,
		Port:        w.cfg.WebPort,
		Ngrok:       w.cfg.WebNgrok,
		NgrokDomain: w.cfg.WebNgrokDomain,
	}

	// Layer 2: what this session saved, per field.
	if dir := strings.TrimSpace(w.cfg.RyshDir); dir != "" {
		s, err := session.LoadWebSettings(dir, w.sessionName)
		if err != nil {
			slog.Error("web settings not loaded", "session", w.sessionName, "err", err)
		} else {
			if s.AutoStart {
				merged.AutoStart = true
			}
			if s.Host != "" {
				merged.Host = s.Host
			}
			if s.Port > 0 {
				merged.Port = s.Port
			}
			// The shared door exists ONLY per session — there is no project-wide
			// notion of a second address.
			merged.SharedHost, merged.SharedPort = s.SharedHost, s.SharedPort
			if s.Ngrok {
				merged.Ngrok = true
			}
			if s.NgrokDomain != "" {
				merged.NgrokDomain = s.NgrokDomain
			}
		}
	}

	// Layer 3: this process's own environment. An address handed to THIS daemon
	// at spawn is the most specific instruction there is — it is the port its
	// parent is waiting on — so it wins over anything on disk.
	if v := strings.TrimSpace(os.Getenv("RYSH_WEB_PORT")); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p <= 65535 {
			merged.Port = p
		}
	}
	if v := strings.TrimSpace(os.Getenv("RYSH_WEB_HOST")); v != "" {
		merged.Host = v
	}

	w.webSettings = merged
	return w.webSettings
}

// bindAddrText renders a bind address for a line about LISTENING, where the
// browser-friendly substitution webURLHost makes (0.0.0.0 → localhost) would
// misreport the reach of the next start.
func bindAddrText(host string) string {
	if h := strings.TrimSpace(host); h != "" {
		return h
	}
	return web.DefaultHost
}

// credentialsRef names the web login this session uses: its own
// (.rysh/web/<session>-auth.json), falling back to the project's web-auth.json
// while it has none.
//
// Per session, because a session is what gets served. Two sessions in one
// project are two doors — different ports, different URLs, potentially
// different people — and while they shared one credentials file, the second one
// to set a password silently changed the first one's and logged its browsers
// out. The fallback is what keeps every existing install working: a session
// inherits the project login until it sets one of its own.
func (w *WorkspaceActor) credentialsRef() web.CredentialsRef {
	return web.SessionCredentials(w.cfg.RyshDir, w.sessionName)
}
