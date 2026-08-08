package actors

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-shared/secretnat"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/gateway"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/proxy"
	"github.com/rysh-ai/rysh-cli-code/internal/proxy/wirecheck"
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
	// Request-rate rules (design 022 §4.2). Empty config ⇒ unlimited, so an
	// existing session is never silently throttled by an upgrade.
	srv.SetRateLimit(w.cfg.Proxy.RateLimit)
	// Upstream failover (design 022 §4.1). Disabled ⇒ one attempt, as before.
	srv.SetFailover(w.cfg.Proxy.Failover)
	// Per-tenant policy (design 022 §4.3). Must follow SetRateLimit: a tenant
	// can carry its own rate rule, and the limiter is rebuilt from both.
	srv.SetTenants(w.cfg.Proxy.Tenants)
	// Strict mode (design 022 §8.2): config or policy may switch it on, and
	// policy can only tighten — a project that omits it cannot turn off an org's.
	srv.SetStrict(w.proxyStrict())
	// Model allowlist (design 001 §4.7). Empty ⇒ every model, as before.
	srv.SetModelRules(w.cfg.Proxy.Models)
	// Org-wide ceiling (design 023 §4.2). Installed only when governance is
	// actually on: a typed-nil in the interface would make every request take
	// the lease path and read an empty lease as a partition.
	if w.ledger != nil {
		srv.SetLeaseGate(w.ledger)
	}
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

// startLedgerClient brings up the org-wide control plane (design 023).
//
// It is created unconditionally and left nil unless it is actually enabled, so
// every consumer's nil check is the ONE place the feature is switched off. A
// half-configured upstream (governance on, no api_key) resolves to disabled
// rather than to a client that fails a request per flush.
func (w *WorkspaceActor) startLedgerClient() {
	if w.ledger != nil {
		return
	}
	c := gateway.New(w.cfg.Upstream, w.ledgerDaemonID(), w.pub)
	if !c.Enabled() {
		slog.Debug("gateway: org-wide governance is off (upstream.governance)")
		return
	}
	// Central policy (023 §4.7) arrives on the client's goroutine; hand it to
	// the mailbox, where changing policy is safe. Captured locally, exactly as
	// startCron captures the PID for its ticker.
	self, system := w.selfPID, w.actorSystem
	c.SetPolicyHook(func(body []byte) {
		if self == nil || system == nil {
			return
		}
		system.Root.Send(self, &centralPolicyMsg{body: body})
	})
	c.Start()
	w.ledger = c
	slog.Info("gateway: org-wide governance enabled",
		"workspace", w.cfg.Upstream.Workspace, "on_partition", w.cfg.Upstream.GovernanceOnPartition)
}

// centralPolicyMsg carries a pulled central policy document into the workspace
// mailbox. In-process only, like cronTickMsg — never published to NATS.
type centralPolicyMsg struct{ body []byte }

// ledgerDaemonID identifies this daemon to the server for the (daemon_id, seq)
// idempotency key (023 §4.4) and for lease accounting. The session name is
// stable across a daemon's life and unique per machine within a workspace,
// which is exactly the granularity a lease is held at.
func (w *WorkspaceActor) ledgerDaemonID() string {
	if name := strings.TrimSpace(w.currentSessionName()); name != "" {
		return name
	}
	return "rysh-daemon"
}

// stopLedgerClient flushes and shuts the control plane down. A daemon that goes
// away still owes the server its last numbers.
func (w *WorkspaceActor) stopLedgerClient() {
	if w.ledger == nil {
		return
	}
	w.ledger.Stop()
	w.ledger = nil
}

// proxyStrict reports whether an ungoverned CLI should be stopped rather than
// warned about. Either source can switch it on; neither can switch the other
// off, which is the same strictest-wins rule policy uses everywhere else.
func (w *WorkspaceActor) proxyStrict() bool {
	if w.cfg.Proxy.Strict {
		return true
	}
	return w.policy != nil && w.policy.ProxyStrict
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
			w.failRysh("proxy: failed to start (see logs)")
			return
		}
		fmt.Fprintf(out, "proxy started at %s\n", w.proxyServer.BaseURL())
		fmt.Fprintf(out, "note: env injection applies to the NEXT shell/agent process in a pane.\n")
	case "off":
		if w.policy != nil && w.policy.ProxyRequired {
			fmt.Fprintf(out, "proxy: cannot disable — policy requires the proxy (.rysh/policy.yaml: proxy.required)\n")
			w.failRysh("proxy: cannot disable — policy requires the proxy (.rysh/policy.yaml: proxy.required)")
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
	case "check":
		w.handleProxyCheck(out, paneID, args[1:])
	case "", "status":
		w.renderProxyStatus(out)
	default:
		fmt.Fprintf(out, "usage: ##proxy [status] | ##proxy on|off | ##proxy audit | ##proxy check <cli>\n")
		w.failRyshUsage("unknown ##proxy subcommand: %q", sub)
	}
}

// handleProxyCheck runs the governed-traffic probe for one CLI (design 022
// §4.4).
//
// The probe spawns an external binary and waits on it, so it runs OFF the
// actor: blocking the workspace mailbox for the length of a CLI cold start
// would freeze every pane. The verdict arrives in the pane afterwards.
func (w *WorkspaceActor) handleProxyCheck(out *strings.Builder, paneID string, args []string) {
	if len(args) == 0 {
		fmt.Fprintf(out, "usage: ##proxy check <cli>   (known: %s)\n",
			strings.Join(proxy.KnownCLIs(), ", "))
		w.failRyshUsage("##proxy check needs a CLI name")
		return
	}
	binary := strings.TrimSpace(args[0])
	if _, ok := proxy.ProfileFor(binary); !ok {
		fmt.Fprintf(out, "##proxy check: unknown CLI %q (known: %s)\n",
			binary, strings.Join(proxy.KnownCLIs(), ", "))
		w.failRysh("##proxy check: unknown CLI %s", binary)
		return
	}
	fmt.Fprintf(out, "checking whether %s honours rysh's injected base URL "+
		"(runs it once against a stub upstream)…\n", binary)

	pub := w.pub
	// Project-local scratch, not /tmp: codex refuses to create its helper
	// binaries under a temp dir and warns about it, which makes a passing check
	// read as broken. Empty RyshDir falls back to the temp dir.
	scratch := ""
	if w.cfg.RyshDir != "" {
		scratch = filepath.Join(w.cfg.RyshDir, "proxy-check")
	}
	go func() {
		res, err := wirecheck.Run(context.Background(), binary, proxyCheckTimeout, scratch)
		if pub == nil {
			return
		}
		if err != nil {
			_ = pub.SendPaneRyshOutput(paneID, "[proxy] check "+binary+": "+err.Error()+"\n")
			return
		}
		_ = pub.SendPaneRyshOutput(paneID, res.String())
		if res.CLIError != "" {
			// Surfaced, not obeyed: a CLI can exit non-zero against the stub
			// having already put a governed request on the wire.
			_ = pub.SendPaneRyshOutput(paneID,
				"[proxy]   (note: "+binary+" exited with: "+res.CLIError+
					" — the wire decides, not the exit code)\n")
		}
	}()
}

// proxyCheckTimeout bounds one probe. Generous, because a cold CLI start on a
// slow machine is not evidence of anything.
const proxyCheckTimeout = 120 * time.Second

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
	if proxy.HasTenantKeys(w.cfg.Proxy.Tenants) {
		// A credential boundary the operator cannot see is one they cannot
		// trust, so say when tenants select their own key (design 022 §8.3).
		fmt.Fprintf(out, "  tenant keys per-tenant upstream credentials in use\n")
	}
	fmt.Fprintf(out, "  rate limit  %s\n", proxyRateSummary(w.cfg.Proxy.RateLimit))
	if w.proxyStrict() {
		fmt.Fprintf(out, "  strict      ON — an ungoverned agent CLI is stopped, not just warned\n")
	}
	fmt.Fprintf(out, "  failover    %s\n", proxy.FailoverSummary(w.cfg.Proxy.Failover))
	fmt.Fprintf(out, "  dialects    anthropic (full), openai/gemini (redact+forward)\n")
	fmt.Fprintf(out, "  panes route via ANTHROPIC_BASE_URL=%s/anthropic/{paneID}\n", w.proxyServer.BaseURL())
	w.renderProxyPaneStates(out)
}

// renderProxyPaneStates lists what each pane looks like from the proxy's side
// (design 022 §4.4). "unproxied?" keeps its question mark on purpose: the state
// is inferred from an ABSENCE of traffic, and an idle CLI is indistinguishable
// from one that escaped. `##proxy check` is the deterministic answer.
func (w *WorkspaceActor) renderProxyPaneStates(out *strings.Builder) {
	ids := w.collectAllPaneIDs()
	if len(ids) == 0 {
		return
	}
	sort.Strings(ids)
	var lines []string
	for _, id := range ids {
		if st := w.proxyServer.PaneGovernanceState(id); st != "idle" {
			lines = append(lines, fmt.Sprintf("    %-16s %s", id, st))
		}
	}
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(out, "  panes\n")
	for _, l := range lines {
		fmt.Fprintf(out, "%s\n", l)
	}
}

// tenantCeilingsFromPolicy extracts "tenant:<name>" keys from policy's opaque
// budget map (design 022 §4.3). Keying tenants there needed no policy schema
// change, and Merge's lower-wins rule across org and project policy already
// applies to them for free.
func tenantCeilingsFromPolicy(ceilings map[string]int64) map[string]int64 {
	out := map[string]int64{}
	for k, v := range ceilings {
		if name := strings.TrimPrefix(k, "tenant:"); name != k && name != "" {
			out[name] = v
		}
	}
	return out
}

// proxyRateSummary renders the configured request-rate rules (design 022 §4.2)
// for ##proxy status. "unlimited" is the honest default answer, not an absence.
func proxyRateSummary(c config.ProxyRateLimitConfig) string {
	if !c.Any() {
		return "unlimited"
	}
	var parts []string
	if c.PerPane.Enabled() {
		parts = append(parts, fmt.Sprintf("pane %g/s (burst %d)",
			c.PerPane.Rate, c.PerPane.EffectiveBurst()))
	}
	// Sorted so the line is stable across runs — a status view that reorders
	// itself between invocations is impossible to diff.
	dialects := make([]string, 0, len(c.PerDialect))
	for d := range c.PerDialect {
		dialects = append(dialects, d)
	}
	sort.Strings(dialects)
	for _, d := range dialects {
		if r := c.PerDialect[d]; r.Enabled() {
			parts = append(parts, fmt.Sprintf("%s %g/s (burst %d)", d, r.Rate, r.EffectiveBurst()))
		}
	}
	if c.Global.Enabled() {
		parts = append(parts, fmt.Sprintf("session %g/s (burst %d)",
			c.Global.Rate, c.Global.EffectiveBurst()))
	}
	return strings.Join(parts, ", ")
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
