// SPDX-License-Identifier: Apache-2.0

package actors

// The public door: an ngrok tunnel in front of the web server, established by
// `##rysh web start --ngrok` and re-established by itself on every later start
// of the session (the ngrok flag in .rysh/web/<session>.json).
//
// A bind address answers "who on this network may reach the UI"; a tunnel
// answers "may anyone, from anywhere". Keeping the second one in the session's
// own hands — rather than a hand-run `ngrok http` or a LaunchAgent — is what
// makes a restarted session reachable at the same URL without a second command
// nobody remembers to run.
//
// Tunnel establishment itself (adopt / create / spawn, and the single-agent-
// session rule that dictates that order) lives in internal/tunnel.

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/tunnel"
)

// webTunnelTimeout bounds a tunnel start. Generous for a cold ngrok dial, short
// enough that a start does not appear to hang.
const webTunnelTimeout = 25 * time.Second

// tunnelOptions builds the tunnel configuration for a port from the workspace's
// config: the reserved domain, the binary and the ngrok config file, plus a log
// beside the session's other state so a failed start has a reason to quote.
func (w *WorkspaceActor) tunnelOptions(port int) tunnel.Options {
	opts := tunnel.Options{
		Port:       port,
		Domain:     strings.TrimSpace(w.cfg.WebNgrokDomain),
		Binary:     strings.TrimSpace(w.cfg.WebNgrokBinary),
		ConfigFile: strings.TrimSpace(w.cfg.WebNgrokConfig),
		Timeout:    webTunnelTimeout,
	}
	if dir := strings.TrimSpace(w.cfg.RyshDir); dir != "" {
		opts.LogPath = filepath.Join(dir, "logs", fmt.Sprintf("ngrok-%d.log", port))
	}
	return opts
}

// startWebTunnel publishes port at a public URL, reporting to out (which may be
// nil on the auto-start path, where there is no pane to write to). It returns
// the public URL, or "" when no tunnel could be established.
//
// An existing tunnel for this port is reused rather than replaced: on a restart
// the ngrok agent often outlives the session, and the URL it is already serving
// is the one people have.
func (w *WorkspaceActor) startWebTunnel(out *strings.Builder, port int, domain string) string {
	if w.webTunnel != nil && w.webTunnel.Port == port && w.webTunnel.URL != "" {
		if out != nil {
			fmt.Fprintf(out, "[rysh] already published at %s\n", w.webTunnel.URL)
		}
		return w.webTunnel.URL
	}
	opts := w.tunnelOptions(port)
	if d := strings.TrimSpace(domain); d != "" {
		opts.Domain = d
	}
	ctx, cancel := context.WithTimeout(context.Background(), webTunnelTimeout+5*time.Second)
	defer cancel()

	tun, err := tunnel.Start(ctx, opts)
	if err != nil {
		if out != nil {
			fmt.Fprintf(out, "\n[rysh] could not publish port %d: %v\n", port, err)
			fmt.Fprintf(out, "[rysh] the web server is running locally; the public URL is what failed\n")
		}
		slog.Error("web tunnel failed", "port", port, "err", err)
		return ""
	}
	w.webTunnel = tun
	if out != nil {
		switch tun.Origin {
		case tunnel.OriginAdopted:
			fmt.Fprintf(out, "[rysh] published at %s (an ngrok agent was already serving this port)\n", tun.URL)
		case tunnel.OriginCreated:
			fmt.Fprintf(out, "[rysh] published at %s (added to the running ngrok agent)\n", tun.URL)
		default:
			fmt.Fprintf(out, "[rysh] published at %s (ngrok started for this session)\n", tun.URL)
		}
		// Only a tunnel WE opened gets the ephemeral-URL warning: an adopted one
		// is served under whatever the running agent was configured with, which
		// is usually a reserved domain and none of our business to second-guess.
		if opts.Domain == "" && tun.Origin != tunnel.OriginAdopted {
			fmt.Fprintf(out, "[rysh] this URL changes on every restart — pin one with --ngrok-domain <domain>\n")
		}
		fmt.Fprintf(out, "[rysh] anyone with the URL reaches the sign-in page; the login still guards the session\n")
	}
	slog.Info("web tunnel established", "port", port, "url", tun.URL, "origin", tun.Origin)
	return tun.URL
}

// stopWebTunnel takes down a tunnel this session opened. One that was already
// up when the session found it is left running — see tunnel.Tunnel.Stop.
func (w *WorkspaceActor) stopWebTunnel(out *strings.Builder) {
	if w.webTunnel == nil {
		if out != nil {
			fmt.Fprintf(out, "\n[rysh] this session is not published\n")
		}
		return
	}
	url, adopted := w.webTunnel.URL, w.webTunnel.Adopted()
	if err := w.webTunnel.Stop(); err != nil && out != nil {
		fmt.Fprintf(out, "\n[rysh] %v\n", err)
	}
	w.webTunnel = nil
	if out == nil {
		return
	}
	if adopted {
		fmt.Fprintf(out, "\n[rysh] this session no longer claims %s — the ngrok agent serving it was not started here, so it keeps running\n", url)
		return
	}
	fmt.Fprintf(out, "\n[rysh] stopped publishing %s\n", url)
}

// handleWebNgrok implements `##rysh web ngrok [start|stop|status]`.
func (w *WorkspaceActor) handleWebNgrok(out *strings.Builder, args []string) {
	action := "status"
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		action = strings.ToLower(strings.TrimSpace(args[0]))
	}
	switch action {
	case "start", "on", "publish":
		if w.webServer == nil || !w.webServer.IsRunning() {
			fmt.Fprintf(out, "\n[rysh] the web server is not running — there is nothing to publish\n")
			fmt.Fprintf(out, "[rysh] start it first: ##rysh web start --username <u> --password <p>\n")
			w.failRysh("web server is not running")
			return
		}
		domain := ""
		if len(args) > 1 {
			domain = args[1]
		}
		port := w.publishablePort()
		fmt.Fprintf(out, "\n[rysh] publishing port %d...\n", port)
		if url := w.startWebTunnel(out, port, domain); url != "" {
			w.recordWebMeta(w.webServer)
		}
	case "stop", "off":
		w.stopWebTunnel(out)
		w.recordWebMeta(w.webServer)
	case "status":
		w.writeWebNgrokStatus(out)
	default:
		ryshWriter(out).Usage(
			"##rysh web ngrok [status]        show the public URL for this session",
			"##rysh web ngrok start [domain]  publish the running web server",
			"##rysh web ngrok stop            stop publishing (a tunnel this session did not start keeps running)",
			"saved per session in .rysh/web/<session>.json — set by `##rysh web start --ngrok`",
		)
		w.failRyshUsage("unknown ##rysh web ngrok action: %q", action)
	}
}

// writeWebNgrokStatus reports the public URL, whether or not this session is
// the one that opened it.
func (w *WorkspaceActor) writeWebNgrokStatus(out *strings.Builder) {
	if w.webTunnel != nil && w.webTunnel.URL != "" {
		fmt.Fprintf(out, "\n[rysh] published at %s (port %d, %s)\n",
			w.webTunnel.URL, w.webTunnel.Port, w.webTunnel.Origin)
		return
	}
	if w.webServer == nil || !w.webServer.IsRunning() {
		fmt.Fprintf(out, "\n[rysh] the web server is not running, and this session is not published\n")
		return
	}
	// Not published BY this session — but the port may be published anyway, by
	// an agent someone else started. Reporting that is the difference between
	// "not reachable" and "reachable, just not by our doing".
	port := w.publishablePort()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	url, err := tunnel.Lookup(ctx, "", port)
	switch {
	case err == nil:
		fmt.Fprintf(out, "\n[rysh] port %d is published at %s by an ngrok agent this session did not start\n", port, url)
		fmt.Fprintf(out, "[rysh] adopt it for this session: ##rysh web ngrok start\n")
	case err == tunnel.ErrNoTunnel:
		fmt.Fprintf(out, "\n[rysh] not published — an ngrok agent is running, but nothing forwards port %d\n", port)
		fmt.Fprintf(out, "[rysh] publish it: ##rysh web ngrok start\n")
	default:
		fmt.Fprintf(out, "\n[rysh] not published (no ngrok agent is answering on 127.0.0.1:4040)\n")
		fmt.Fprintf(out, "[rysh] publish it: ##rysh web ngrok start\n")
	}
}

// publishablePort is the port a tunnel should point at: the shared door when
// one is open (the address meant for other people), otherwise the server's own.
func (w *WorkspaceActor) publishablePort() int {
	if w.webServer == nil {
		return 0
	}
	if _, port := w.webServer.SharedAddr(); port > 0 {
		return port
	}
	return w.webServer.Port()
}
