package actors

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-shared/secretnat"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/proxy"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// startProxy launches the governance proxy (design 001) if not already running.
// The real provider key (config.APIKey) is injected proxy-side; SecretNAT is the
// session-wide manager shared with rysh's own agents (one token namespace).
func (w *WorkspaceActor) startProxy() {
	if w.proxyServer != nil {
		return
	}
	var mgr *secretnat.Manager
	if w.agSetup != nil {
		mgr = w.agSetup.SecretNAT
	}
	// Resolve one key per dialect. A dialect with no key is left unrewritten so
	// the wrapped CLI's own credential passes through — governance does not
	// require rysh to hold the credential.
	keys := map[string]string{}
	for _, dialect := range []string{"anthropic", "openai", "gemini"} {
		if k := w.cfg.ProxyKeyFor(dialect); k != "" {
			keys[dialect] = k
		}
	}
	srv := proxy.New(w.pub, mgr, keys, w.cfg.Proxy.Upstreams, w.cfg.Proxy.AuditContent)
	// audit_content body sink (design 001 §4.5): project-local, per session.
	// rysh state is always project-local under RyshDir — the design doc's
	// ~/.local/state path predates that decision. Inert unless AuditContent is on.
	if w.cfg.RyshDir != "" {
		srv.SetAuditDir(filepath.Join(w.cfg.RyshDir, "proxy-audit", w.sessionName))
	}
	if _, err := srv.Start(""); err != nil {
		slog.Error("governance proxy: start failed", "err", err)
		return
	}
	w.proxyServer = srv
	// Record the governed endpoint in the session registry (design 001) so
	// ##session info and registry-inspecting tools see it without attaching.
	session.UpdateProxyPort(w.cfg, w.currentSessionName(), srv.Port())
}

// stopProxy shuts the proxy down and clears the pane-env endpoint.
func (w *WorkspaceActor) stopProxy() {
	if w.proxyServer != nil {
		w.proxyServer.Stop()
		w.proxyServer = nil
		session.UpdateProxyPort(w.cfg, w.currentSessionName(), 0)
	}
}

// handleProxyCommand implements ##proxy (design 001):
//
//	##proxy [status]   show state, endpoint, redaction
//	##proxy on|off     start/stop the loopback proxy (effective on next shell)
//	##proxy audit      tail recent per-request audit lines
func (w *WorkspaceActor) handleProxyCommand(out *strings.Builder, paneID string, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = strings.ToLower(strings.TrimSpace(args[0]))
	}
	switch sub {
	case "on":
		if w.proxyServer != nil {
			fmt.Fprintf(out, "proxy already running at %s\n", w.proxyServer.BaseURL())
			return
		}
		w.startProxy()
		if w.proxyServer == nil {
			fmt.Fprintf(out, "proxy: failed to start (see logs)\n")
			return
		}
		fmt.Fprintf(out, "proxy started at %s\n", w.proxyServer.BaseURL())
		fmt.Fprintf(out, "note: env injection applies to the NEXT shell/agent process in a pane.\n")
	case "off":
		if w.policy != nil && w.policy.ProxyRequired {
			fmt.Fprintf(out, "proxy: cannot disable — policy requires the proxy (.rysh/policy.yaml: proxy.required)\n")
			return
		}
		if w.proxyServer == nil {
			fmt.Fprintf(out, "proxy is not running\n")
			return
		}
		w.stopProxy()
		fmt.Fprintf(out, "proxy stopped (existing pane processes keep their injected env until restart)\n")
	case "audit":
		w.renderProxyAudit(out)
	case "", "status":
		w.renderProxyStatus(out)
	default:
		fmt.Fprintf(out, "usage: ##proxy [status] | ##proxy on|off | ##proxy audit\n")
	}
}

func (w *WorkspaceActor) renderProxyStatus(out *strings.Builder) {
	if w.proxyServer == nil {
		fmt.Fprintf(out, "governance proxy: OFF\n")
		fmt.Fprintf(out, "  enable with  ##proxy on  (or [proxy] enabled: true / RYSH_PROXY_ENABLED=1)\n")
		return
	}
	redaction := "off"
	if w.agSetup != nil && w.agSetup.SecretNAT != nil {
		redaction = "on (SecretNAT)"
	}
	key := "pass-through (pane's own key)"
	if strings.TrimSpace(w.cfg.APIKey) != "" {
		key = "injected proxy-side from config"
	}
	fmt.Fprintf(out, "governance proxy: ON\n")
	fmt.Fprintf(out, "  endpoint    %s\n", w.proxyServer.BaseURL())
	fmt.Fprintf(out, "  redaction   %s\n", redaction)
	fmt.Fprintf(out, "  upstream key %s\n", key)
	fmt.Fprintf(out, "  dialects    anthropic (full), openai/gemini (redact+forward)\n")
	fmt.Fprintf(out, "  panes route via ANTHROPIC_BASE_URL=%s/anthropic/{paneID}\n", w.proxyServer.BaseURL())
}

func (w *WorkspaceActor) renderProxyAudit(out *strings.Builder) {
	// Prefer the durable trail (design 001 §4.5): the ProxyAuditActor reads the
	// JetStream-backed stream, so history survives a proxy toggle or a restart —
	// unlike the proxy's in-process ring. Fall back to that ring only when the
	// durable trail is unavailable (e.g. JetStream down).
	if w.renderProxyAuditDurable(out) {
		return
	}

	if w.proxyServer == nil {
		fmt.Fprintf(out, "governance proxy: OFF (no audit)\n")
		return
	}
	lines := w.proxyServer.RecentAudits(20)
	if len(lines) == 0 {
		fmt.Fprintf(out, "no proxied requests yet\n")
		return
	}
	fmt.Fprintf(out, "(in-process only — durable trail unavailable)\n")
	for _, a := range lines {
		fmt.Fprintf(out, "%s  %s\n", a.TS.Format("15:04:05"), a.String())
	}
}

// renderProxyAuditDurable queries the ProxyAuditActor for the durable trail and
// renders it. Returns true when it handled the render (durable trail reachable),
// false to let the caller fall back to the in-process ring.
func (w *WorkspaceActor) renderProxyAuditDurable(out *strings.Builder) bool {
	reply, err := w.pub.Request(msg.ProxyAuditInboxSubject(),
		&msg.MsgProxyAuditSnapshotRequest{Limit: 20}, 3*time.Second)
	if err != nil {
		return false
	}
	snap, ok := reply.(*msg.MsgProxyAuditSnapshotReply)
	if !ok || snap == nil || !snap.Durable {
		return false // JetStream unavailable — the actor could not build a trail
	}
	if len(snap.Records) == 0 {
		fmt.Fprintf(out, "no proxied requests yet\n")
		return true
	}
	for _, a := range snap.Records {
		fmt.Fprintf(out, "%s  %s\n", a.TS.Format("15:04:05"), proxyAuditLine(a))
	}
	return true
}

// proxyAuditLine renders one durable audit record in the same shape as the
// proxy's in-process AuditLine.String(), so ##proxy audit reads identically
// whichever source served it.
func proxyAuditLine(a msg.MsgProxyRequestAudit) string {
	red := ""
	if a.RedactionHits > 0 {
		red = fmt.Sprintf(" %d redaction(s)", a.RedactionHits)
	}
	return fmt.Sprintf("[proxy] %s %s model=%s %din/%dout tok status=%d%s",
		a.Dialect, a.Endpoint, a.Model, a.InTokens, a.OutTokens, a.Status, red)
}
