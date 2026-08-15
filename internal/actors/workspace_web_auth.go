// SPDX-License-Identifier: Apache-2.0

package actors

// `##rysh web auth` — the username/password login for the web UI, and the
// login resolution shared with `##rysh web start --username/--password`.
//
// This login is the ONLY way into the UI; the access token it replaced is gone
// (internal/web/auth.go). Credentials are stored per SESSION in
// <ryshDir>/web/<session>-auth.json — falling back to the project-wide
// <ryshDir>/web-auth.json while a session has none — as a bcrypt hash plus the
// HS256 signing key, never the password; the browser trades them for a
// one-month JWT it keeps in localStorage. (Per session because a session is
// what gets served: see web.CredentialsRef.) When the token expires the login form comes back — there is no
// refresh token to implement or leak.
//
// Setting credentials mints a fresh signing key, so changing the password
// immediately invalidates every token issued under the old one.

import (
	"fmt"
	"io"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/web"
)

// writeWebAuthUsage prints the `##rysh web auth` usage block.
func writeWebAuthUsage(out io.Writer) {
	ryshWriter(out).Usage(
		"##rysh web auth username=<u> password=<p>  set the web UI login",
		"##rysh web auth                            show whether a login is configured",
		"##rysh web auth clear                      remove the login (the UI will not start without one)",
		"the browser keeps the issued token for 30 days, then asks again",
	)
}

// webAuthArgs is one parsed `##rysh web auth` invocation.
type webAuthArgs struct {
	Username string
	Password string
	// Clear requests removal of the stored credentials.
	Clear bool
	// Show is true for the bare form, which only reports the current state.
	Show bool
}

// parseWebAuthArgs reads the `key=value` argument form. Unrecognised arguments
// come back as warnings rather than failing the command, matching
// parseWebStartArgs — a typo should never leave the user without an
// explanation of what was actually applied.
func parseWebAuthArgs(args []string) (webAuthArgs, []string) {
	var (
		parsed   webAuthArgs
		warnings []string
	)
	if len(args) == 0 {
		parsed.Show = true
		return parsed, nil
	}
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "" {
			continue
		}
		switch strings.ToLower(arg) {
		case "clear", "--clear", "off", "remove":
			parsed.Clear = true
			continue
		}
		key, value, ok := strings.Cut(arg, "=")
		if !ok {
			warnings = append(warnings, fmt.Sprintf("ignoring %q (expected username=<u> or password=<p>)", arg))
			continue
		}
		// The value is taken verbatim: a password may legitimately contain
		// anything except the whitespace the command line already split on.
		switch strings.ToLower(strings.TrimPrefix(key, "--")) {
		case "username", "user":
			parsed.Username = value
		case "password", "pass":
			parsed.Password = value
		default:
			warnings = append(warnings, fmt.Sprintf("ignoring unknown option %q", key))
		}
	}
	return parsed, warnings
}

// handleWebAuth implements `##rysh web auth [username=<u> password=<p>|clear]`.
func (w *WorkspaceActor) handleWebAuth(out *strings.Builder, args []string) {
	parsed, warnings := parseWebAuthArgs(args)
	for _, warn := range warnings {
		fmt.Fprintf(out, "\n[rysh] %s\n", warn)
	}

	switch {
	case parsed.Clear:
		removed, err := web.ClearCredentialsFor(w.credentialsRef())
		if err != nil {
			fmt.Fprintf(out, "\n[rysh] %v\n", err)
			w.failRysh("%v", err)
			return
		}
		// Applied to the live server too, so the login stops working now
		// rather than at the next restart. Removing a gate never locks anyone
		// out, so this needs no control-mode exception.
		if w.webServer != nil {
			w.webServer.SetCredentials(nil)
		}
		if removed {
			fmt.Fprintf(out, "\n[rysh] web login removed — `##rysh web start` will not start the UI until a new one is set\n")
		} else {
			fmt.Fprintf(out, "\n[rysh] no web login was configured\n")
		}

	case parsed.Show:
		creds, err := web.LoadCredentialsFor(w.credentialsRef())
		if err != nil {
			fmt.Fprintf(out, "\n[rysh] %v\n", err)
			w.failRysh("%v", err)
			return
		}
		if creds == nil {
			fmt.Fprintf(out, "\n[rysh] no web login configured — the UI cannot start until one is set\n")
			writeWebAuthUsage(out)
			return
		}
		fmt.Fprintf(out, "\n[rysh] web login configured for user %q (set %s)\n", creds.Username, creds.UpdatedAt)
		fmt.Fprintf(out, "[rysh] credentials: %s\n", w.credentialsRef().OwnPath())

	default:
		if parsed.Username == "" || parsed.Password == "" {
			writeWebAuthUsage(out)
			w.failRyshUsage("##rysh web auth needs both username= and password=")
			return
		}
		creds, err := web.SaveCredentialsFor(w.credentialsRef(), parsed.Username, parsed.Password)
		if err != nil {
			fmt.Fprintf(out, "\n[rysh] %v\n", err)
			w.failRysh("%v", err)
			return
		}
		fmt.Fprintf(out, "\n[rysh] web login set for user %q\n", creds.Username)
		fmt.Fprintf(out, "[rysh] credentials: %s (password hashed; not recoverable)\n", w.credentialsRef().OwnPath())
		// The secret tier gets it too, so the two never drift. They did once:
		// a login set here and secrets set separately meant the running server
		// checked one password while the durable copy held another, and a
		// restart could resurrect the older of the two.
		w.storeWebLoginSecrets(out, parsed.Username, parsed.Password)
		fmt.Fprintf(out, "[rysh] any browser signed in with the previous password must sign in again\n")
		if w.webServer != nil {
			setWebCredentials(w.webServer, creds, out)
		}
		// The password was typed on a command line, so it is sitting in this
		// pane's scrollback — worth saying out loud on a shared or recorded tab.
		fmt.Fprintf(out, "[rysh] the password is in this pane's scrollback — clear it if this tab is shared\n")
		if w.webServer == nil || !w.webServer.IsRunning() {
			fmt.Fprintf(out, "[rysh] start the UI to use it: ##rysh web start\n")
		}
	}
}

// resolveWebStartLogin works out which credentials a `##rysh web start` should
// run under, reporting anything that stops it. The returned creds are nil ONLY
// for control mode, which runs without a login by design (webLoginRequired).
//
// Order: credentials given on the command line are saved (replacing any stored
// ones), otherwise the workspace's stored login is used. With neither, the
// start is refused — the UI is never served unauthenticated, so there is no
// safe default left to fall back to.
func (w *WorkspaceActor) resolveWebStartLogin(out *strings.Builder, opts webStartOpts) (*web.Credentials, bool) {
	user, pass := strings.TrimSpace(opts.Username), opts.Password

	// Half a login is a typo, not an intention — say which half is missing
	// rather than silently starting under the stored (or no) credentials.
	if (user == "") != (pass == "") {
		missing := "--password"
		if user == "" {
			missing = "--username"
		}
		fmt.Fprintf(out, "\n[rysh] %s is missing — pass both to set the web login\n", missing)
		writeWebUsage(out)
		w.failRyshUsage("##rysh web start needs both --username and --password")
		return nil, false
	}

	if user != "" {
		creds, err := web.SaveCredentialsFor(w.credentialsRef(), user, pass)
		if err != nil {
			fmt.Fprintf(out, "\n[rysh] %v\n", err)
			w.failRysh("%v", err)
			return nil, false
		}
		fmt.Fprintf(out, "\n[rysh] web login set for user %q — stored, so later starts need no flags\n", creds.Username)
		// The password was typed on a command line, so it is sitting in this
		// pane's scrollback — worth saying out loud on a shared or recorded tab.
		fmt.Fprintf(out, "[rysh] the password is in this pane's scrollback — clear it if this tab is shared\n")
		return creds, true
	}

	creds, err := web.LoadCredentialsFor(w.credentialsRef())
	if err != nil {
		// A corrupt credentials file must not silently downgrade the UI to no
		// auth at all, so it stops the start outright.
		fmt.Fprintf(out, "\n[rysh] %v\n", err)
		w.failRysh("%v", err)
		return nil, false
	}
	// No hash on disk, but the workspace may still hold the login as secrets —
	// set by this command on an earlier machine, restored from a backup, or put
	// there directly with `##secret set RYSH_WEB_USERNAME/-_PASSWORD`. They are
	// the durable copy (web-auth.json is only a hash), so a start that has them
	// available should never refuse for want of a login.
	if creds == nil {
		if user, pass, ok := w.storedWebCredentials(); ok {
			restored, serr := web.SaveCredentialsFor(w.credentialsRef(), user, pass)
			if serr != nil {
				fmt.Fprintf(out, "\n[rysh] the login in the secret store could not be applied: %v\n", serr)
			} else {
				fmt.Fprintf(out, "\n[rysh] login taken from the secret store (%s / %s) — user %q\n",
					webUserSecret, webPassSecret, restored.Username)
				creds = restored
			}
		}
	}
	if creds == nil && webLoginRequired(opts.Control) {
		fmt.Fprintf(out, "\n[rysh] no web login configured — the UI is not served without one\n")
		fmt.Fprintf(out, "[rysh] set one as you start: ##rysh web start --username <u> --password <p>\n")
		fmt.Fprintf(out, "[rysh] or beforehand:        ##rysh web auth username=<u> password=<p>\n")
		w.failRyshUsage("##rysh web start needs a login")
		return nil, false
	}
	return creds, true
}

// setWebCredentials installs credentials on a server. Always applies them —
// WHERE they are enforced is the server's business, not the caller's.
//
// This used to refuse in control mode, back when credentials meant "gate every
// listener": gating the app's own loopback connection locked it out of the
// daemon it had just spawned. Now the server has two doors (web/server_shared.go)
// and enforces per door — the app's private one stays open in control mode, the
// shared one always asks — so credentials can be held by a control-mode server
// without ever being demanded of the app. Holding them is in fact required:
// they are what the shared door checks a browser's password against.
func setWebCredentials(srv *web.Server, creds *web.Credentials, out io.Writer) bool {
	if srv == nil {
		return false
	}
	srv.SetCredentials(creds)
	if creds != nil && srv.ControlEnabled() && out != nil {
		fmt.Fprintf(out, "[rysh] the desktop app's own connection stays open without it (loopback, control mode)\n")
	}
	return true
}

// applyWebCredentials loads the workspace's stored web login onto a server that
// is about to start, for the auto-start path that has no pane to report to.
// A broken credentials file leaves the server with no credentials, which in
// control mode (the only caller that can reach it) is the normal state.
func (w *WorkspaceActor) applyWebCredentials(srv *web.Server, out io.Writer) {
	if srv == nil {
		return
	}
	// Every server this workspace builds follows the workspace's credentials
	// FILE from here on, not just the snapshot loaded below. Another daemon
	// rooted at the same project shares that file and may rotate the signing
	// key at any moment; without this, this process keeps verifying against the
	// superseded key and 401s every client until it is restarted (F-9).
	srv.TrackCredentialsRef(w.credentialsRef())
	creds, err := web.LoadCredentialsFor(w.credentialsRef())
	if err != nil {
		if out != nil {
			fmt.Fprintf(out, "\n[rysh] web login not loaded: %v\n", err)
		}
		return
	}
	setWebCredentials(srv, creds, out)
}

// openSharedWebDoor serves the RUNNING session at a second address, behind the
// login — `##rysh web start --bind … --port … [--username … --password …]` on a
// server that is already up.
//
// This is what "share the UI" means for a session the desktop app is driving.
// The app's own connection is a loopback port it holds ungated; a second
// listener onto the same routes and the same hub gives a browser or a phone the
// same session at an address of one's choosing, and that one always asks for
// the password. One web server, two doors — see web/server_shared.go.
func (w *WorkspaceActor) openSharedWebDoor(out *strings.Builder, opts webStartOpts) {
	creds, ok := w.resolveWebStartLogin(out, opts)
	if !ok {
		return
	}
	if creds == nil {
		// resolveWebStartLogin only returns nil creds for a control-mode start,
		// which is not what a shared address is for.
		fmt.Fprintf(out, "\n[rysh] no web login configured — a shared address is never served without one\n")
		fmt.Fprintf(out, "[rysh] set one: ##rysh web start --bind %s --port %d --username <u> --password <p>\n",
			webURLHost(opts.Host), opts.Port)
		w.failRyshUsage("##rysh web start needs a login to share an address")
		return
	}
	setWebCredentials(w.webServer, creds, out)

	if err := w.webServer.StartShared(opts.Host, opts.Port); err != nil {
		fmt.Fprintf(out, "\n[rysh] could not serve %s: %v\n", webBaseURL(opts.Host, opts.Port), err)
		w.failRysh("%v", err)
		return
	}

	base := webBaseURL(opts.Host, opts.Port)
	fmt.Fprintf(out, "\n[rysh] this session is now served at %s — sign in as %q\n", base, creds.Username)
	fmt.Fprintf(out, "[rysh] bind address: %s   port: %d\n", webBindLabel(opts.Host), opts.Port)
	if w.webServer.ControlEnabled() {
		fmt.Fprintf(out, "[rysh] the desktop app keeps its own connection on %s (loopback, no sign-in)\n",
			webBaseURL(w.webServer.Host(), w.webServer.Port()))
	}
	if webBindIsLoopback(opts.Host) && !opts.Ngrok {
		fmt.Fprintf(out, "[rysh] not reachable from other machines — use --bind 0.0.0.0 to expose it on your network\n")
	}
	if opts.Ngrok || w.sessionWebSettings().Ngrok {
		w.startWebTunnel(out, opts.Port, opts.NgrokDomain)
	}
	// A shared door is persisted under its OWN keys ([web] shared_host /
	// shared_port). The primary server's port here belongs to whoever started it
	// — under the desktop app, an ephemeral one the app picked and passes in
	// RYSH_WEB_PORT — so writing this address as [web] port would be recording
	// the wrong door and having an env override erase it anyway.
	shared := opts
	shared.SharedDoor = true
	w.persistWebStart(out, shared, creds)
	w.recordWebMeta(w.webServer)
	fmt.Fprintf(out, "[rysh] close it again with: ##rysh web stop --shared\n")
}
