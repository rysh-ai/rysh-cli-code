// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/web"
)

// writeWebUsage prints the `##rysh web` usage block. Kept in one place so the
// bare-`##rysh web`, unknown-action and top-level ##help surfaces cannot drift.
func writeWebUsage(out io.Writer) {
	ryshWriter(out).Usage(
		"##rysh web start [bind] [port] [flags]  start the web UI server (sign-in required)",
		"    --username <u>  login username (also: username=<u>)",
		"    --password <p>  login password (also: password=<p>)",
		fmt.Sprintf("    --bind <addr>   bind address (default: %s, this machine only; 0.0.0.0 exposes it on your network)", web.DefaultHost),
		fmt.Sprintf("    --port <n>      TCP port (default: %d, or [web] port in rysh.config.yaml)", defaultWebPort),
		"    --control       enable channel/pairing/humanoid management from the dashboard (loopback only)",
		"    --ngrok         also publish it at a public HTTPS URL (ngrok)",
		"    --ngrok-domain <d>  publish at a reserved domain, so the URL survives restarts",
		"    --no-save       apply to this run only (do not write rysh.config.yaml or the secrets)",
		"  what you pass is SAVED FOR THIS SESSION: bind/port/ngrok to .rysh/web/<session>.json and the",
		"  login to the secret store (RYSH_WEB_USERNAME / RYSH_WEB_PASSWORD), with auto_start — so",
		"  restarting THIS session serves it again, tunnel included, and sibling sessions are untouched",
		"  the login is stored for this workspace, so later starts need no flags",
		"  e.g. ##rysh web start --username alice --password s3cret",
		"  when the server is ALREADY running (the desktop app starts one), naming an",
		"  address SHARES this session there instead — same UI, always behind the login,",
		"  while the app keeps its own connection:",
		"       ##rysh web start --bind 127.0.0.1 --port 23001 --username alice --password s3cret",
		"       ##rysh web start --bind 0.0.0.0 --port 23001 --username alice --password s3cret   (phone)",
		"##rysh web stop [--force|--shared]  stop the server (--shared closes only the shared address)",
		"##rysh web status          show bind address, port, login + shared address",
		"##rysh web auth username=<u> password=<p>  set or replace the login",
		"    the browser keeps the issued token for 30 days, then asks again",
		"    ##rysh web auth        show whether a login is configured",
		"    ##rysh web auth clear  remove the login (the UI will not start without one)",
		"per-session state: .rysh/web/<session>.json   ·   project-wide defaults: [web] host/port in",
		"rysh.config.yaml, or RYSH_WEB_HOST/RYSH_WEB_PORT (used only until a session saves its own)",
	)
}

// Argument parsing for `##rysh web start`. The command takes the bind address
// and port — settable on the command line, in rysh.config.yaml ([web]
// host/port) or via the environment (RYSH_WEB_HOST / RYSH_WEB_PORT) — plus the
// username/password login that guards the UI. Command-line flags win over
// config, which wins over the built-in defaults.
//
//	##rysh web start [bind] [port] [--bind <addr>] [--port <n>] [--username <u> --password <p>]
//
// Parsing lives here (not inline in the ##rysh switch) so it is unit-testable
// without standing up a WorkspaceActor.

// webStartOpts is the resolved configuration for one `##rysh web start`.
type webStartOpts struct {
	// Host is the bind address. Empty selects web.DefaultHost (127.0.0.1),
	// resolved by web.Server.listenAddr.
	Host string
	// Port is the TCP port to listen on.
	Port int
	// Username and Password set (or replace) the workspace's web login. Both
	// empty means "use the stored login" — the common case after the first
	// start. Only one of the two is a usage error, caught by the caller.
	Username string
	Password string
	// Control enables the mutating control-plane endpoints (channel start/stop,
	// pairing approve/allow, humanoid governance) and forces a loopback bind
	// (R2, --control).
	Control bool
	// Ngrok publishes the server at a public HTTPS URL, and persists that
	// choice so every later start of this session does it again. NgrokDomain
	// pins a reserved domain — without one the public URL is different after
	// every restart, which defeats half the point of persisting it.
	Ngrok       bool
	NgrokDomain string
	// SharedDoor marks a start that opened a SECOND address onto an already
	// running server, rather than starting one. Set by the caller, never by the
	// command line: it decides which config keys the address is persisted under.
	SharedDoor bool
	// NoSave suppresses persistence: the parameters apply to this run only and
	// rysh.config.yaml, the secret store and the session record are left alone.
	NoSave bool
	// Explicit records that the command line named an address or a port, rather
	// than falling back to config and defaults. On a server that is ALREADY
	// running it is the difference between "you forgot it is up" and "serve this
	// session at this address too" — the second opens a shared door.
	Explicit bool
}

// parseWebStartArgs resolves the bind address, port and login for
// `##rysh web start`. defHost/defPort carry the config-resolved defaults
// (cfg.WebHost / cfg.WebPort).
//
// Accepted forms, in any order:
//
//	--bind <addr>     --bind=<addr>       (aliases: --host, --address, --addr)
//	--port <n>        --port=<n>
//	--username <u>    --username=<u>      username=<u>   (aliases: --user)
//	--password <p>    --password=<p>      password=<p>   (aliases: --pass)
//	<n>                                   bare port     (backward compatible)
//	<addr>                                bare bind address
//
// An address may be a bare host (127.0.0.1, 0.0.0.0, ::1, localhost) or a
// host:port pair (127.0.0.1:8080, :8080, [::1]:8080); when it carries a port
// that port is applied too. Unrecognised arguments are returned as warnings
// rather than failing the command, so a typo never leaves the user without a
// running server or an explanation. The retired access-token flags are warned
// about by name rather than as "unknown flag", so a stale command line says
// what replaced them.
func parseWebStartArgs(args []string, defHost string, defPort int) (webStartOpts, []string) {
	opts := webStartOpts{Host: strings.TrimSpace(defHost), Port: defPort}
	if opts.Port <= 0 {
		opts.Port = defaultWebPort
	}

	var warnings []string
	warnf := func(format string, a ...any) {
		warnings = append(warnings, fmt.Sprintf(format, a...))
	}

	// applyBind sets the host, and the port too when the spec carries one.
	applyBind := func(flag, spec string) {
		host, port, err := splitBindSpec(spec)
		if err != nil {
			warnf("ignoring %s %q: %v", flag, spec, err)
			return
		}
		opts.Explicit = true
		opts.Host = host
		if port > 0 {
			opts.Port = port
		}
	}

	// next returns the value for a space-separated flag, advancing i.
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, bool) {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				warnf("ignoring %s: missing value", a)
				return "", false
			}
			i++
			return args[i], true
		}

		switch {
		case a == "--bind", a == "--host", a == "--address", a == "--addr":
			if v, ok := next(); ok {
				applyBind(a, v)
			}
		case strings.HasPrefix(a, "--bind="), strings.HasPrefix(a, "--host="),
			strings.HasPrefix(a, "--address="), strings.HasPrefix(a, "--addr="):
			flag, v, _ := strings.Cut(a, "=")
			applyBind(flag, v)

		case a == "--port", a == "-p":
			if v, ok := next(); ok {
				if p, err := parsePort(v); err != nil {
					warnf("ignoring %s %q: %v", a, v, err)
				} else {
					opts.Port, opts.Explicit = p, true
				}
			}
		case strings.HasPrefix(a, "--port="):
			v := strings.TrimPrefix(a, "--port=")
			if p, err := parsePort(v); err != nil {
				warnf("ignoring --port %q: %v", v, err)
			} else {
				opts.Port, opts.Explicit = p, true
			}

		case a == "--username", a == "--user":
			if v, ok := next(); ok {
				opts.Username = v
			}
		case strings.HasPrefix(a, "--username="), strings.HasPrefix(a, "--user="):
			_, v, _ := strings.Cut(a, "=")
			opts.Username = v

		case a == "--password", a == "--pass":
			if v, ok := next(); ok {
				opts.Password = v
			}
		case strings.HasPrefix(a, "--password="), strings.HasPrefix(a, "--pass="):
			_, v, _ := strings.Cut(a, "=")
			opts.Password = v

		case a == "--token", a == "--no-token", a == "--auth", a == "--no-auth",
			a == "--token-auto", strings.HasPrefix(a, "--token="):
			// Access tokens are gone (internal/web/auth.go): the UI is guarded by
			// a username/password login. Say so by name — a stale command line
			// deserves better than "unknown flag".
			if a == "--token" {
				// Swallow its value so it is not re-read as a bind address.
				_, _ = next()
			}
			warnf("%s is no longer supported — the web UI uses a login: --username <u> --password <p>", a)

		case a == "--control":
			opts.Control = true

		case a == "--ngrok", a == "--publish":
			opts.Ngrok = true
		case a == "--ngrok-domain", a == "--domain":
			if v, ok := next(); ok {
				opts.Ngrok, opts.NgrokDomain = true, v
			}
		case strings.HasPrefix(a, "--ngrok-domain="), strings.HasPrefix(a, "--domain="):
			_, v, _ := strings.Cut(a, "=")
			opts.Ngrok, opts.NgrokDomain = true, v

		case a == "--no-save", a == "--once":
			opts.NoSave = true

		case strings.HasPrefix(a, "-"):
			warnf("ignoring unknown flag %q", a)

		case strings.HasPrefix(a, "username="), strings.HasPrefix(a, "user="):
			// The `key=value` form `##rysh web auth` uses, accepted here too so
			// one habit works for both commands.
			_, v, _ := strings.Cut(a, "=")
			opts.Username = v
		case strings.HasPrefix(a, "password="), strings.HasPrefix(a, "pass="):
			_, v, _ := strings.Cut(a, "=")
			opts.Password = v

		default:
			// Bare positional: a port when it is all digits, otherwise a bind
			// address. Checking digits first keeps `##rysh web start 8080`
			// working exactly as before.
			if p, err := strconv.Atoi(a); err == nil {
				if p <= 0 || p > 65535 {
					warnf("ignoring port %q: out of range (1-65535)", a)
				} else {
					opts.Port, opts.Explicit = p, true
				}
				continue
			}
			applyBind("bind address", a)
		}
	}

	return opts, warnings
}

// defaultWebPort is the fallback when neither the command line nor the config
// supplies one.
const defaultWebPort = 23232

// parsePort validates a TCP port string.
func parsePort(s string) (int, error) {
	p, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("not a number")
	}
	if p <= 0 || p > 65535 {
		return 0, fmt.Errorf("out of range (1-65535)")
	}
	return p, nil
}

// splitBindSpec splits a bind specification into host and port. The port is 0
// when the spec carries only a host. Accepted: "127.0.0.1", "0.0.0.0",
// "localhost", "::1", "[::1]", "127.0.0.1:8080", ":8080", "[::1]:8080".
func splitBindSpec(spec string) (string, int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", 0, nil
	}
	// host:port (also "[::1]:8080" and ":8080").
	if h, p, err := net.SplitHostPort(spec); err == nil {
		port := 0
		if p != "" {
			parsed, perr := parsePort(p)
			if perr != nil {
				return "", 0, fmt.Errorf("bad port %q: %v", p, perr)
			}
			port = parsed
		}
		return h, port, nil
	}
	// Host only. Strip brackets so "[::1]" normalises to "::1"; listenAddr
	// re-adds them via net.JoinHostPort.
	host := strings.TrimSuffix(strings.TrimPrefix(spec, "["), "]")
	if err := validateBindHost(host); err != nil {
		return "", 0, err
	}
	return host, 0, nil
}

// validateBindHost rejects obviously wrong hosts early so the user gets a
// pointed message instead of a generic listen failure. It stays permissive:
// any IP, or any plausible hostname, is accepted — the actual bind is still
// validated by the kernel at Start.
func validateBindHost(host string) error {
	if net.ParseIP(host) != nil {
		return nil
	}
	if strings.ContainsAny(host, " \t/\\?#@") {
		return fmt.Errorf("not a valid IP or hostname")
	}
	if strings.Contains(host, ":") {
		// Colons left over after SplitHostPort failed and it is not a valid IP.
		return fmt.Errorf("not a valid IP address")
	}
	return nil
}

// webURLHost maps a bind address to a host usable in a browser URL. A wildcard
// bind (0.0.0.0 / ::) is shown as localhost because that is what the operator
// can actually click on the machine running the daemon; IPv6 literals are
// bracketed. Empty means the default bind (web.DefaultHost, loopback).
func webURLHost(host string) string {
	switch strings.TrimSpace(host) {
	case "0.0.0.0", "::", "[::]":
		return "localhost"
	case "":
		return web.DefaultHost
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return "[" + host + "]"
	}
	return host
}

// webBindLabel describes the bind address for status output, spelling out both
// the default (loopback — this machine only) and the wildcard case, so neither
// "why can't my phone reach it" nor "why is this on my LAN" is a surprise.
func webBindLabel(host string) string {
	h := strings.TrimSpace(host)
	if h == "" {
		h = web.DefaultHost
	}
	switch h {
	case "0.0.0.0", "::":
		return h + " (all interfaces)"
	case web.DefaultHost, "::1", "localhost":
		return h + " (loopback — this machine only)"
	}
	return h
}

// webBindIsLoopback reports whether a bind address is reachable only from the
// local machine, so `##rysh web start` can hint at --bind 0.0.0.0 when a user
// most likely wanted remote access.
func webBindIsLoopback(host string) bool {
	h := strings.TrimSpace(host)
	if h == "" {
		h = web.DefaultHost
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.TrimSuffix(strings.TrimPrefix(h, "["), "]"))
	return ip != nil && ip.IsLoopback()
}

// webBaseURL builds the browsable base URL for a bind address and port.
func webBaseURL(host string, port int) string {
	return fmt.Sprintf("http://%s:%d", webURLHost(host), port)
}

// webLoginRequired reports whether a web server about to start must have a
// username/password login configured.
//
// Every server does, with one exception: control mode. The Electron sidecar
// spawns its daemon with RYSH_WEB_CONTROL=true and connects with no
// credentials, and control mode already forces the bind onto 127.0.0.1 — the
// same trust boundary as the TUI, and a client that is the very process that
// started the daemon. Demanding a password there would only lock the desktop
// app out of its own sidecar.
func webLoginRequired(control bool) bool { return !control }

// webStopNeedsForce reports whether `##rysh web stop` should refuse and ask for
// --force: it would cut off clients that are connected right now.
//
// The test is the live client count, NOT whether the server is the desktop
// app's (control mode). Control mode answers a different question — "may this
// server manage channels and pairings" — and using it here was wrong in both
// directions: it nagged about a control-mode server nobody was attached to, and
// stayed silent about a plain `##rysh web start` serving a phone.
func webStopNeedsForce(clients int32, args []string) bool {
	return clients > 0 && !hasWebForce(args)
}

// hasWebForce reports whether --force (or -f) is present, the opt-out for
// `##rysh web stop` when clients are connected.
func hasWebForce(args []string) bool {
	for _, a := range args {
		switch strings.TrimSpace(a) {
		case "--force", "-f":
			return true
		}
	}
	return false
}

// hasWebShared reports whether --shared is present: `##rysh web stop --shared`
// closes the shared address without stopping the server behind it.
func hasWebShared(args []string) bool {
	for _, a := range args {
		if strings.TrimSpace(a) == "--shared" {
			return true
		}
	}
	return false
}
