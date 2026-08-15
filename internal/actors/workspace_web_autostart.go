// SPDX-License-Identifier: Apache-2.0

package actors

// Bringing the web server back up when a session starts — the auto_start flag
// in .rysh/web/<session>.json, written by `##rysh web start`.
//
// The point is that a restart is not a downgrade. A session that was served on
// 0.0.0.0:23001, behind a login, at a public ngrok URL, comes back exactly
// there: the address and the tunnel from the config, the login from the secret
// store, and the shared door — the second address the desktop app's sessions
// are reached at — re-opened alongside the app's own connection.
//
// Nothing here reports to a pane, because at this point there is none. Failures
// go to the log, and a failure to publish never stops the server from serving
// locally.

import (
	"log/slog"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/session"
	"github.com/rysh-ai/rysh-cli-code/internal/web"
)

// autoStartWebServer starts the session's web server from config. The web
// server is a session singleton (it binds one port), so only the primary
// workspace calls this.
func (w *WorkspaceActor) autoStartWebServer() {
	settings := w.sessionWebSettings()
	port := settings.Port
	if port <= 0 {
		port = defaultWebPort
	}
	w.webServer = web.NewServer(port, w.sessionName, w.pub, w.nc, w.pub.Codecs())
	w.webServer.SetFSBrowser(w.webFSBrowser())
	if settings.Host != "" {
		w.webServer.SetHost(settings.Host)
	}
	// A `##rysh web auth` login set earlier in this workspace applies here too —
	// there is no pane to report a broken file to, so a failure only skips the
	// login. NewServer has already read RYSH_WEB_CONTROL, so setWebCredentials
	// can see that this is the desktop app's own sidecar and leave it ungated: a
	// stored login must never lock the app out of the daemon it just spawned.
	w.applyWebCredentials(w.webServer, nil)
	// A credentials file holds a bcrypt hash — enough to check a password,
	// useless for re-establishing one. The secret store holds the literal
	// login, so it is what makes a session serve the SAME credentials after a
	// restart. Two cases send us there:
	//
	//   - no login at all (a fresh machine, a cleaned .rysh, a session moved
	//     between checkouts): the UI would otherwise refuse to start;
	//   - a login INHERITED from the project while this session has secrets of
	//     its own that do not match it. It inherited only because it had
	//     nothing to say; now it does, and continuing to ask for another
	//     session's password would ignore what was set here.
	if w.webServer.LoginUsername() == "" || w.inheritedLoginIsStale() {
		w.restoreWebLoginFromSecrets()
	}
	// Auto-start serves the desktop app's own sidecar (control mode, loopback),
	// which runs without a login by design. Anywhere else, refuse to auto-start
	// an unauthenticated UI rather than quietly exposing the workspace — [web]
	// host 0.0.0.0 + auto_start with no login is exactly the accident worth
	// refusing.
	if w.webServer.LoginUsername() == "" && webLoginRequired(w.webServer.ControlEnabled()) {
		slog.Error("auto-start web server skipped: no web login configured",
			"hint", "##rysh web start --username <u> --password <p>")
		w.webServer = nil
		return
	}
	w.wireWebPresence(w.webServer)
	w.configureWebParity(w.webServer)
	if err := w.webServer.Start(); err != nil {
		slog.Error("auto-start web server failed", "err", err)
		w.webServer = nil
		return
	}
	slog.Info("web server auto-started", "session", w.sessionName, "host", settings.Host,
		"port", port, "login", w.webServer.LoginUsername())

	// The shared door: the address people were actually given, when it is not
	// the one the server just bound. Under the desktop app the primary port is
	// whatever the app picked (RYSH_WEB_PORT), so the address a phone uses only
	// exists as this second listener.
	publish := port
	if sharedPort := w.reopenSharedDoor(settings); sharedPort > 0 {
		publish = sharedPort
	}

	// And the public door.
	if settings.Ngrok {
		w.startWebTunnel(nil, publish, settings.NgrokDomain)
	}

	// Advertise the endpoint on the session record so any local front-end —
	// including a desktop app that did not spawn this daemon — can discover and
	// adopt it, with the bind address, login and public URL alongside.
	w.recordWebMeta(w.webServer)
}

// restoreWebLoginFromSecrets re-establishes the stored login on a server that
// has none, from the secret store (or the ${NAME} references the config
// resolved). It writes web-auth.json back too, so the hash a browser checks
// against exists again.
func (w *WorkspaceActor) restoreWebLoginFromSecrets() {
	user, pass, ok := w.storedWebCredentials()
	if !ok {
		// The config's own resolved values are the same secrets by another
		// route: [web] username/password as ${NAME} references, expanded at
		// load. They cover the case where the session KV is not up yet.
		user, pass = strings.TrimSpace(w.cfg.WebUsername), strings.TrimSpace(w.cfg.WebPassword)
		if user == "" || pass == "" {
			return
		}
	}
	creds, err := web.SaveCredentialsFor(w.credentialsRef(), user, pass)
	if err != nil {
		slog.Error("restoring the web login from the secret store failed", "err", err)
		return
	}
	setWebCredentials(w.webServer, creds, nil)
	slog.Info("web login restored from the secret store", "user", creds.Username)
}

// inheritedLoginIsStale reports whether this session is serving the PROJECT's
// login while its own secrets say something else. Costs one bcrypt compare, at
// startup only.
func (w *WorkspaceActor) inheritedLoginIsStale() bool {
	ref := w.credentialsRef()
	if ref.HasOwn() {
		return false // its own login; the secrets wrote it
	}
	user, pass, ok := w.storedWebCredentials()
	if !ok {
		return false // nothing of its own to prefer
	}
	creds, err := web.LoadCredentialsFor(ref)
	if err != nil || creds == nil {
		return false
	}
	return !creds.Verify(user, pass)
}

// reopenSharedDoor re-opens the second address a previous `##rysh web start`
// configured ([web] shared_host / shared_port), returning its port, or 0 when
// there is none to open. A shared address that duplicates the primary one is
// skipped — the server is already there.
func (w *WorkspaceActor) reopenSharedDoor(settings session.WebSettings) int {
	port := settings.SharedPort
	if port <= 0 || w.webServer == nil {
		return 0
	}
	host := strings.TrimSpace(settings.SharedHost)
	if port == w.webServer.Port() && sameBindHost(host, w.webServer.Host()) {
		return 0
	}
	// A shared door is never served without a login: it is the door for other
	// people. Control mode exempts only the app's own loopback connection.
	if w.webServer.LoginUsername() == "" {
		slog.Error("shared web address not re-opened: no login configured",
			"host", host, "port", port)
		return 0
	}
	if err := w.webServer.StartShared(host, port); err != nil {
		slog.Error("shared web address not re-opened", "host", host, "port", port, "err", err)
		return 0
	}
	slog.Info("shared web address re-opened", "host", host, "port", port,
		"login", w.webServer.LoginUsername())
	return port
}

// sameBindHost compares two bind addresses, treating the empty string as the
// default loopback bind so "" and "127.0.0.1" are not read as two addresses.
func sameBindHost(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimSpace(s)
		if s == "" {
			return web.DefaultHost
		}
		return s
	}
	return norm(a) == norm(b)
}
