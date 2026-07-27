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
	fmt.Fprintf(out, "\n[rysh] usage:\n")
	fmt.Fprintf(out, "  ##rysh web start [bind] [port] [flags]  start the web UI server (token-protected by default)\n")
	fmt.Fprintf(out, "      --bind <addr>  bind address (default: %s, this machine only; 0.0.0.0 exposes it on your network)\n", web.DefaultHost)
	fmt.Fprintf(out, "      --port <n>     TCP port (default: %d, or [web] port in rysh.config.yaml)\n", defaultWebPort)
	fmt.Fprintf(out, "      --token <t>    use access token <t> instead of a generated one (open with ?token=<t>)\n")
	fmt.Fprintf(out, "      --no-token     start without any access token (bind to 127.0.0.1 if you do this)\n")
	fmt.Fprintf(out, "      --control      enable channel/pairing/humanoid management from the dashboard (loopback only)\n")
	fmt.Fprintf(out, "    e.g. ##rysh web start --bind 127.0.0.1 --port 8080 --token s3cret\n")
	fmt.Fprintf(out, "         ##rysh web start 0.0.0.0:8080\n")
	fmt.Fprintf(out, "  ##rysh web stop            stop the web UI server\n")
	fmt.Fprintf(out, "  ##rysh web status          show bind address, port + access token\n")
	fmt.Fprintf(out, "  ##rysh web token           print the access token + open URL\n")
	fmt.Fprintf(out, "  config: [web] host/port/token in rysh.config.yaml, or RYSH_WEB_HOST/RYSH_WEB_PORT/RYSH_WEB_TOKEN\n\n")
}

// Argument parsing for `##rysh web start`. The command takes three parameters —
// bind address, port and access token — each settable on the command line, in
// rysh.config.yaml ([web] host/port/token) or via the environment
// (RYSH_WEB_HOST / RYSH_WEB_PORT / RYSH_WEB_TOKEN). Command-line flags win over
// config, which wins over the built-in defaults.
//
//	##rysh web start [bind] [port] [--bind <addr>] [--port <n>] [--token <t>|--no-token]
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
	// Token is the access token to require, or "" when NoToken is set.
	Token string
	// NoToken records an explicit --no-token, so the caller knows not to
	// generate a random token.
	NoToken bool
	// Control enables the mutating control-plane endpoints (channel start/stop,
	// pairing approve/allow, humanoid governance) and forces a loopback bind
	// (R2, --control).
	Control bool
}

// parseWebStartArgs resolves the bind address, port and token for
// `##rysh web start`. defHost/defPort/defToken carry the config-resolved
// defaults (cfg.WebHost / cfg.WebPort / cfg.WebToken).
//
// Accepted forms, in any order:
//
//	--bind <addr>   --bind=<addr>    (aliases: --host, --address, --addr)
//	--port <n>      --port=<n>
//	--token <t>     --token=<t>
//	--no-token      --no-auth        start with no access token
//	--auth          --token-auto     no-op, kept for backward compatibility
//	<n>                              bare port     (backward compatible)
//	<addr>                           bare bind address
//
// An address may be a bare host (127.0.0.1, 0.0.0.0, ::1, localhost) or a
// host:port pair (127.0.0.1:8080, :8080, [::1]:8080); when it carries a port
// that port is applied too. Unrecognised arguments are returned as warnings
// rather than failing the command, so a typo never leaves the user without a
// running server or an explanation.
func parseWebStartArgs(args []string, defHost string, defPort int, defToken string) (webStartOpts, []string) {
	opts := webStartOpts{Host: strings.TrimSpace(defHost), Port: defPort, Token: defToken}
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
					opts.Port = p
				}
			}
		case strings.HasPrefix(a, "--port="):
			v := strings.TrimPrefix(a, "--port=")
			if p, err := parsePort(v); err != nil {
				warnf("ignoring --port %q: %v", v, err)
			} else {
				opts.Port = p
			}

		case a == "--token":
			if v, ok := next(); ok {
				opts.Token = v
				opts.NoToken = false
			}
		case strings.HasPrefix(a, "--token="):
			opts.Token = strings.TrimPrefix(a, "--token=")
			opts.NoToken = false

		case a == "--auth", a == "--token-auto":
			// A random token is generated by default now; these flags are kept
			// for backward compatibility and are no-ops.

		case a == "--no-token", a == "--no-auth":
			opts.NoToken = true
			opts.Token = ""

		case a == "--control":
			opts.Control = true

		case strings.HasPrefix(a, "-"):
			warnf("ignoring unknown flag %q", a)

		default:
			// Bare positional: a port when it is all digits, otherwise a bind
			// address. Checking digits first keeps `##rysh web start 8080`
			// working exactly as before.
			if p, err := strconv.Atoi(a); err == nil {
				if p <= 0 || p > 65535 {
					warnf("ignoring port %q: out of range (1-65535)", a)
				} else {
					opts.Port = p
				}
				continue
			}
			applyBind("bind address", a)
		}
	}

	if opts.NoToken {
		opts.Token = ""
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

// autoStartWebToken resolves the access token for the auto-started web server
// ([web] auto_start / RYSH_WEB_AUTO_START). A configured token is used as-is.
// With no token configured one is GENERATED, mirroring `##rysh web start`, so
// an auto-started UI is never accidentally exposed without access control
// (e.g. [web] host 0.0.0.0 + auto_start with no token). Retrieve it with
// `##rysh web token`.
//
// Control mode is the exception and stays token-less: the Electron sidecar
// spawns the daemon with RYSH_WEB_CONTROL=true and connects without a token,
// and control mode already forces the bind onto 127.0.0.1 — the same trust
// boundary as the TUI.
func autoStartWebToken(configured string, control bool) string {
	if t := strings.TrimSpace(configured); t != "" {
		return t
	}
	if control {
		return ""
	}
	return web.GenerateToken()
}
