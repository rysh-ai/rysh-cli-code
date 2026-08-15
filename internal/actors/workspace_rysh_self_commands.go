// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"strings"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/web"
)

// handleRyshSelfCommand implements the `##rysh` command family — rysh managing
// itself: the embedded web server, and the `new`/`tab`/`lane` aliases for the
// top-level commands of the same name.

func (w *WorkspaceActor) handleRyshSelfCommand(ctx actor.Context, out *strings.Builder, paneID string, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if sub == "" {
		sub = "help"
	}
	switch sub {
	case "new":
		// ##rysh new <tab|lane|pane> [args] -- alias of ##new.
		w.handleNewInstance(ctx, out, paneID, args[1:])
	case "web":
		action := ""
		if len(args) > 1 {
			action = args[1]
		}
		switch action {
		case "start":
			// The parameters — bind address, port, login, and --control — come
			// from the command line, falling back to [web] host/port in
			// rysh.config.yaml (and RYSH_WEB_HOST/PORT). See parseWebStartArgs
			// in workspace_web_args.go. --control enables the mutating endpoints
			// (channel start/stop, pairing approve/allow, humanoid governance)
			// and forces a loopback bind (R2).
			//
			// The UI is guarded by a username/password login, so the server does
			// not start without one — passed here with --username/--password, or
			// stored earlier by `##rysh web auth`. Control mode is the exception
			// (see webLoginRequired).
			// Defaults come from THIS SESSION's saved settings (falling back to
			// the project-wide [web] block for a session that has none), so a
			// bare `##rysh web start` repeats what this session was last told —
			// not what some sibling session in the same project was told.
			sessionWeb := w.sessionWebSettings()
			opts, warnings := parseWebStartArgs(args[2:], sessionWeb.Host, sessionWeb.Port)
			for _, warn := range warnings {
				fmt.Fprintf(out, "\n[rysh] %s\n", warn)
			}
			if w.webServer != nil && w.webServer.IsRunning() {
				// The server is already up — which, in the desktop app, it always
				// is: the app spawns its daemon with the web server running as
				// its own transport. Asking for a bind/port here is therefore not
				// a mistake to report but a request to SHARE this session at an
				// address of one's own: a second door onto the same UI, always
				// behind the login (web/server_shared.go).
				if opts.Explicit {
					w.openSharedWebDoor(out, opts)
					break
				}
				fmt.Fprintf(out, "\n[rysh] web server already running at %s (bind %s)\n",
					webBaseURL(w.webServer.Host(), w.webServer.Port()), webBindLabel(w.webServer.Host()))
				// A login passed with no address is still worth keeping: it is
				// what a later share, or a later start, will check against.
				if opts.Username != "" || opts.Password != "" {
					creds, ok := w.resolveWebStartLogin(out, opts)
					if !ok {
						break
					}
					setWebCredentials(w.webServer, creds, out)
				}
				fmt.Fprintf(out, "[rysh] share it at an address of your own: ##rysh web start --bind 127.0.0.1 --port <n> --username <u> --password <p>\n")
				break
			}
			// Resolve the login BEFORE building the server, so a missing or
			// half-given password costs nothing and leaves no half-made state.
			creds, ok := w.resolveWebStartLogin(out, opts)
			if !ok {
				break
			}
			w.webServer = web.NewServer(opts.Port, w.sessionName, w.pub, w.nc, w.pub.Codecs())
			// Follow the workspace's credentials file, not just the snapshot
			// resolved above: a sibling daemon rooted at this project shares it
			// and rotating the key there must not 401 everyone here (F-9).
			w.webServer.TrackCredentialsRef(w.credentialsRef())
			w.webServer.SetFSBrowser(w.webFSBrowser())
			if opts.Host != "" {
				w.webServer.SetHost(opts.Host)
			}
			if opts.Control {
				// SetControl also switches the bind to 127.0.0.1 —
				// the control plane is never exposed off-host.
				w.webServer.SetControl(true)
			}
			// The login travels with the workspace, so it applies to every
			// server start without being re-entered.
			setWebCredentials(w.webServer, creds, out)
			w.wireWebPresence(w.webServer)
			w.configureWebParity(w.webServer)
			if err := w.webServer.Start(); err != nil {
				w.failRysh("%v", err)
				fmt.Fprintf(out, "\n[rysh] failed to start web server on %s:%d: %v\n",
					webBindLabel(opts.Host), opts.Port, err)
				w.webServer = nil
				break
			}
			base := webBaseURL(opts.Host, opts.Port)
			if opts.Control {
				fmt.Fprintf(out, "\n[rysh] control plane ENABLED (loopback only) — channels, pairings and humanoids are manageable from the dashboard\n")
			}
			if creds != nil {
				fmt.Fprintf(out, "\n[rysh] web server started at %s — sign in as %q\n", base, creds.Username)
				fmt.Fprintf(out, "[rysh] bind address: %s   port: %d\n", webBindLabel(opts.Host), opts.Port)
				fmt.Fprintf(out, "[rysh] the browser keeps the login for 30 days, then asks again\n")
			} else {
				fmt.Fprintf(out, "\n[rysh] web server started at %s (control mode — no login, loopback only)\n", base)
				fmt.Fprintf(out, "[rysh] bind address: %s   port: %d\n", webBindLabel(opts.Host), opts.Port)
			}
			// The default bind is loopback, so a user expecting to open the UI
			// from a phone or another machine gets told how, instead of
			// debugging a silent connection refused.
			if webBindIsLoopback(opts.Host) && !opts.Ngrok {
				fmt.Fprintf(out, "[rysh] not reachable from other machines — restart with --bind 0.0.0.0 to expose it on your network\n")
			}
			// The public door, when one was asked for. It goes up after the
			// server so the tunnel never points at a port nothing is serving.
			if opts.Ngrok || sessionWeb.Ngrok {
				w.startWebTunnel(out, opts.Port, opts.NgrokDomain)
			}
			// Persist what was typed, so the next start of this session repeats
			// it: address and tunnel to rysh.config.yaml, login to the secret
			// store, auto_start with them.
			w.persistWebStart(out, opts, creds)
			// Advertise the endpoint on the session record: this is how the
			// desktop app adopts a command-line session's daemon, which it can
			// otherwise never reach (its renderer speaks only HTTP/WebSocket,
			// and it knows the port of nothing it did not spawn itself). The
			// bind address, login name and public URL travel with it, so what
			// serves this session is legible without attaching to it.
			w.recordWebMeta(w.webServer)
		case "stop":
			// `--shared` closes only the shared address, leaving the server (and
			// the desktop app on it) running. Checked before everything else: it
			// is not a stop of the server at all.
			if hasWebShared(args[2:]) {
				if w.webServer == nil || !w.webServer.IsRunning() {
					fmt.Fprintf(out, "\n[rysh] web server is not running\n")
					w.failRysh("web server is not running")
					break
				}
				host, port := w.webServer.SharedAddr()
				if port == 0 {
					fmt.Fprintf(out, "\n[rysh] this session is not shared at any address\n")
					break
				}
				if err := w.webServer.StopShared(); err != nil {
					fmt.Fprintf(out, "\n[rysh] %v\n", err)
					w.failRysh("%v", err)
					break
				}
				fmt.Fprintf(out, "\n[rysh] stopped serving %s — the session itself is untouched\n", webBaseURL(host, port))
				// The tunnel published THAT address; leaving it up would point
				// the public URL at a door that no longer opens.
				if w.webTunnel != nil && w.webTunnel.Port == port {
					w.stopWebTunnel(out)
				}
				w.recordWebMeta(w.webServer)
				break
			}
			if w.webServer == nil {
				fmt.Fprintf(out, "\n[rysh] web server is not running\n")
				w.failRysh("web server is not running")
			} else if n := w.richClients.Load(); w.webServer.IsRunning() && webStopNeedsForce(n, args[2:]) {
				// Someone is on the other end of this server right now — a
				// desktop app window or a browser tab — and they reach this
				// daemon ONLY through this port. Stopping it leaves their panes
				// silent mid-session with nothing on screen to explain it.
				//
				// The question is "is anything connected", not "is this the
				// app's daemon": a control-mode server nobody is attached to is
				// free to stop, and a plain `##rysh web start` serving a phone
				// deserves the same warning. Asking the live client count is
				// what makes the answer true in both directions.
				fmt.Fprintf(out, "\n[rysh] %d client(s) connected to this web server right now (desktop app or browser)\n", n)
				fmt.Fprintf(out, "[rysh] stopping it disconnects them — their panes stop receiving until they reconnect\n")
				fmt.Fprintf(out, "[rysh] stop it anyway with: ##rysh web stop --force\n")
				w.failRyshUsage("##rysh web stop would disconnect %d connected client(s)", n)
			} else if w.webServer.IsRunning() {
				_ = w.webServer.Stop()
				w.webServer = nil
				// A tunnel outliving the server it publishes would point the
				// public URL at a closed port. One this session did not open is
				// still left alone (tunnel.Stop).
				w.stopWebTunnel(nil)
				// Withdraw the advertised endpoint in the same breath, so a
				// desktop app looking for a door to adopt is never sent to a
				// port nothing is listening on.
				w.recordWebMeta(nil)
				fmt.Fprintf(out, "\n[rysh] web server stopped\n")
			} else {
				// A server object that is not running is a dead instance
				// (its listener died after start) — release what it still
				// holds and forget it, so the next start is clean.
				_ = w.webServer.Stop()
				w.webServer = nil
				w.stopWebTunnel(nil)
				w.recordWebMeta(nil)
				fmt.Fprintf(out, "\n[rysh] web server is not running\n")
				w.failRysh("web server is not running")
			}
		case "status":
			if w.webServer != nil && w.webServer.IsRunning() {
				port := w.webServer.Port()
				host := w.webServer.Host()
				base := webBaseURL(host, port)
				user := w.webServer.LoginUsername()
				switch {
				case w.webServer.ControlEnabled():
					fmt.Fprintf(out, "\n[rysh] web server running at %s (control mode — the desktop app's own connection, no sign-in)\n", base)
					fmt.Fprintf(out, "[rysh] bind address: %s   port: %d\n", webBindLabel(host), port)
				case user != "":
					fmt.Fprintf(out, "\n[rysh] web server running at %s\n", base)
					fmt.Fprintf(out, "[rysh] bind address: %s   port: %d\n", webBindLabel(host), port)
					fmt.Fprintf(out, "[rysh] login: user %q — sign in at %s/ (kept 30 days)\n", user, base)
				default:
					fmt.Fprintf(out, "\n[rysh] web server running at %s (no login)\n", base)
					fmt.Fprintf(out, "[rysh] bind address: %s   port: %d\n", webBindLabel(host), port)
				}
				// The one case a running server cannot resolve for itself. It
				// follows the credentials FILE (TrackCredentialsFile), so a
				// login another daemon rotates is picked up without a restart —
				// but a file it cannot parse is deliberately NOT adopted, since
				// reading a half-written file as "no login" would drop the gate
				// at the worst possible moment. That leaves the server on the
				// last good login it loaded, which to a user just looks like the
				// password not working. Say so, with the remedy (F-9).
				if _, err := web.LoadCredentialsFor(w.credentialsRef()); err != nil {
					fmt.Fprintf(out, "[rysh] the stored login is unreadable: %v\n", err)
					fmt.Fprintf(out, "[rysh] the server is still serving the last login it read — set it again with `##rysh web auth username=<u> password=<p>`\n")
				}
				// The shared door, when one is open: the same session at a second
				// address, always behind the login.
				if sh, sp := w.webServer.SharedAddr(); sp > 0 {
					fmt.Fprintf(out, "[rysh] shared at %s — sign in as %q (##rysh web stop --shared to close)\n",
						webBaseURL(sh, sp), user)
				}
				// The public door and what a restart will do with all of it —
				// the two things a status query about a shared session is
				// actually asking.
				if w.webTunnel != nil && w.webTunnel.URL != "" {
					fmt.Fprintf(out, "[rysh] published at %s (%s)\n", w.webTunnel.URL, w.webTunnel.Origin)
				}
				w.writeWebRestartStatus(out)
			} else {
				// "It is not running" is the answer to a status query, not a
				// failure — the same rule ##replay status and ##upstream status
				// follow. `stop` still fails, because it asked the server to do
				// something and it could not.
				fmt.Fprintf(out, "\n[rysh] web server is not running\n")
			}
		case "ngrok", "publish", "tunnel":
			w.handleWebNgrok(out, args[2:])
		case "auth":
			w.handleWebAuth(out, args[2:])
		case "token":
			// Retired with access-token auth. Named explicitly so muscle memory
			// gets an answer instead of the generic unknown-subcommand usage.
			fmt.Fprintf(out, "\n[rysh] access tokens are gone — the web UI uses a username/password login\n")
			fmt.Fprintf(out, "[rysh] set one with: ##rysh web auth username=<u> password=<p>\n")
			fmt.Fprintf(out, "[rysh] see who can sign in with: ##rysh web status\n")
			w.failRyshUsage("##rysh web token has been removed")
		default:
			writeWebUsage(out)
			w.failRyshUsage("unknown ##rysh web subcommand")
		}
	case "tab":
		// ##rysh tab name <tab-name>
		action := ""
		if len(args) > 1 {
			action = args[1]
		}
		if action == "name" {
			if len(args) < 3 {
				ryshWriter(out).UsageLine("##rysh tab name <tab-name>")
				w.failRyshUsage("usage: %s", "##rysh tab name <tab-name>")
			} else {
				name := strings.Join(args[2:], " ")
				if w.renameActiveTab(name) {
					fmt.Fprintf(out, "\n[rysh] tab renamed to %q\n", name)
				} else {
					fmt.Fprintf(out, "\n[rysh] no active tab to rename\n")
					w.failRysh("no active tab to rename")
				}
			}
		} else {
			ryshWriter(out).UsageLine("##rysh tab name <tab-name>")
			w.failRyshUsage("usage: %s", "##rysh tab name <tab-name>")
		}
	case "lane":
		// ##rysh lane name <lane-name>
		action := ""
		if len(args) > 1 {
			action = args[1]
		}
		if action == "name" {
			if len(args) < 3 {
				ryshWriter(out).UsageLine("##rysh lane name <lane-name>")
				w.failRyshUsage("usage: %s", "##rysh lane name <lane-name>")
			} else {
				name := strings.Join(args[2:], " ")
				if w.renameActiveLane(name) {
					fmt.Fprintf(out, "\n[rysh] active lane renamed to %q\n", name)
				} else {
					fmt.Fprintf(out, "\n[rysh] no active lane to rename\n")
					w.failRysh("no active lane to rename")
				}
			}
		} else {
			ryshWriter(out).UsageLine("##rysh lane name <lane-name>")
			w.failRyshUsage("usage: %s", "##rysh lane name <lane-name>")
		}
	default:
		ryshWriter(out).Unknown("rysh", sub,
			"##rysh new tab               create a new tab",
			"##rysh new lane [tab]        create a new lane (default: active tab)",
			"##rysh new pane [tab] [lane] create a pane at the bottom of a lane",
			"##rysh tab name <tab-name>   rename the active tab",
			"##rysh lane name <lane-name> rename the active lane",
			"##rysh web start [--bind <addr>] [--port <n>] [--username <u> --password <p>] [--control]  start the web UI server",
			"##rysh web stop [--force] stop the web server",
			"##rysh web status          show bind address, port + login",
			"##rysh web auth username=<u> password=<p>  set the web UI login",
		)
		w.failRyshUsage("unknown subcommand for ##%s: %q", "rysh", sub)
	}
}
