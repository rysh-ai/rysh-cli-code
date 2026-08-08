package proxy

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rysh-ai/rysh-cli-shared/secretnat"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// DefaultUpstreams maps a dialect to its real provider base URL, as declared
// by each registered Dialect.
func DefaultUpstreams() map[string]string {
	out := make(map[string]string, len(dialects))
	for name, d := range dialects {
		out[name] = d.Upstream()
	}
	return out
}

const maxRequestBody = 32 << 20 // 32 MiB

// AuditLine is a metadata-only record of one proxied request (never content).
type AuditLine struct {
	TS            time.Time
	PaneID        string
	Dialect       string
	Model         string
	Endpoint      string
	ReqBytes      int
	RedactionHits int
	Status        int
	InTokens      int
	OutTokens     int
	// Upstream is the base URL that actually served the request, and Attempts
	// how many it took (1 = no failover). Both matter only once failover is on,
	// and both are what makes a silent fallback visible after the fact.
	Upstream string
	Attempts int
	// Tenant attributes the request to a customer (design 022 §4.3).
	Tenant string
	// EchoHits counts plaintext secrets the RESPONSE carried back (001 §3
	// step 8). Non-zero means a live credential reached the pane; rysh reports
	// it and does not rewrite the body (echo.go).
	EchoHits int
}

func (a AuditLine) String() string {
	red := ""
	if a.RedactionHits > 0 {
		red = fmt.Sprintf(" %d redaction(s)", a.RedactionHits)
	}
	// Only annotate the upstream when it took more than one attempt — the
	// common line stays exactly as it was.
	over := ""
	if a.Attempts > 1 {
		over = fmt.Sprintf(" via %s after %d attempts", hostOf(a.Upstream), a.Attempts)
	}
	// An echoed secret is rare and consequential enough to sit on the request's
	// own line, not only in the separate warning.
	echo := ""
	if a.EchoHits > 0 {
		echo = fmt.Sprintf(" %d secret(s) echoed in the response", a.EchoHits)
	}
	return fmt.Sprintf("[proxy] %s %s model=%s %din/%dout tok status=%d%s%s%s",
		a.Dialect, a.Endpoint, a.Model, a.InTokens, a.OutTokens, a.Status, red, over, echo)
}

// Server is the loopback governance proxy. It runs on its own HTTP goroutines
// (data plane). The mutex guards only the small audit ring buffer — a documented
// exception to the no-mutex rule, at the HTTP/actor boundary.
type Server struct {
	pub          *msg.NATSPublisher
	snat         *secretnat.Manager // may be nil (no redaction)
	keys         map[string]string  // dialect → real upstream key; missing ⇒ pass through incoming
	upstreams    map[string]string
	auditContent bool
	// auditDir, when auditContent is on, is the directory redacted request
	// bodies are written to (design 001 §4.5). Empty ⇒ never write bodies even
	// if auditContent is set, so the feature is inert without an explicit sink.
	auditDir string

	budget   *budgetChecker             // ledger-backed ceiling enforcement (design 001 §4.4)
	rate     *rateLimiter               // request-rate rules (design 022 §4.2); nil ⇒ unlimited
	failover config.ProxyFailoverConfig // upstream retry (design 022 §4.1); disabled ⇒ one attempt
	tr       *http.Transport            // owned so header timeouts are tunable
	gov      *govWatch                  // ungoverned-CLI observation (design 022 §4.4)
	tenants  *tenantResolver            // per-customer policy (design 022 §4.3); nil ⇒ untenanted
	rateCfg  config.ProxyRateLimitConfig
	panes    *paneRegistry // panes rysh injected an endpoint into (paneid.go)
	models   *modelRules   // model allowlist (design 001 §4.7); nil ⇒ every model
	// lease is the org-wide ceiling (design 023 §4.2); nil ⇒ per-machine only,
	// which is the default and the OSS standalone behaviour.
	lease LeaseGate
	// strict escalates the ungoverned-CLI warning to a block (design 022 §8.2).
	// atomic because it is written by the workspace actor and read from pane
	// goroutines, exactly like the endpoint globals.
	strict atomic.Value // bool

	client  *http.Client
	httpSrv *http.Server
	ln      net.Listener
	baseURL string
	// published records whether this server owns the process-global endpoint
	// (Start) or is a private one that must leave it alone (StartPrivate).
	published bool

	mu     sync.Mutex
	audits []AuditLine // ring buffer (most recent last)
}

// New builds a proxy server. upstreams nil ⇒ DefaultUpstreams.
//
// keys maps a dialect (anthropic|openai|gemini) to the real upstream API key.
// A dialect absent from the map is NOT rewritten — the caller's own credential
// passes through. Passing one key for every dialect is what previously sent an
// Anthropic key as the OpenAI bearer and broke wrapped OpenAI CLIs.
func New(pub *msg.NATSPublisher, snat *secretnat.Manager, keys map[string]string, upstreams map[string]string, auditContent bool) *Server {
	if upstreams == nil {
		upstreams = DefaultUpstreams()
	}
	if keys == nil {
		keys = map[string]string{}
	}
	// Own the transport so failover can install a response-header timeout
	// (design 022 §4.1). Cloning the default keeps proxy/env handling and the
	// connection-pool defaults intact.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	return &Server{
		pub:          pub,
		snat:         snat,
		keys:         keys,
		upstreams:    upstreams,
		auditContent: auditContent,
		budget:       newBudgetChecker(pub),
		tr:           tr,
		gov:          newGovWatch(),
		panes:        newPaneRegistry(),
		client: &http.Client{
			Transport: tr,
			// No overall timeout: agentic responses stream for minutes. The
			// per-request context (client disconnect) bounds it instead.
			Timeout: 0,
		},
	}
}

// Start binds a loopback listener (ephemeral port unless addr specifies one),
// serves in the background, and publishes the endpoint for pane env injection.
// Returns the base URL (http://127.0.0.1:PORT).
func (s *Server) Start(addr string) (string, error) { return s.start(addr, true) }

// StartPrivate is Start without the process-global publication: the server runs
// and serves, but proxy.Endpoint() and proxy.Current() keep pointing at whatever
// was already there.
//
// This exists because `##proxy check` (wirecheck) starts a second, throwaway
// proxy INSIDE the live daemon. With the publishing Start, that probe overwrote
// the session's endpoint on the way in and cleared it on the way out — after
// which every pane spawned got no base-URL injection and was silently
// ungoverned, while ##proxy status still reported ON.
func (s *Server) StartPrivate(addr string) (string, error) { return s.start(addr, false) }

func (s *Server) start(addr string, publish bool) (string, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("proxy: listen: %w", err)
	}
	s.ln = ln
	s.baseURL = "http://" + ln.Addr().String()
	s.httpSrv = &http.Server{Handler: s}
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("proxy: serve", "err", err)
		}
	}()
	s.published = publish
	if publish {
		SetEndpoint(s.baseURL)
		current.Store(s)
	}
	slog.Info("governance proxy started", "url", s.baseURL,
		"redaction", s.snat != nil, "published", publish)
	return s.baseURL, nil
}

// Stop shuts the server down, and clears the endpoint only if THIS server is the
// one that published it. A private probe proxy therefore cannot unpublish the
// session's real endpoint, and neither can a stale Stop after a restart.
func (s *Server) Stop() {
	if s.published && Current() == s {
		SetEndpoint("")
		current.Store((*Server)(nil))
	}
	s.published = false
	if s.httpSrv != nil {
		_ = s.httpSrv.Close()
	}
}

// BaseURL returns the running base URL ("" if not started).
func (s *Server) BaseURL() string { return s.baseURL }

// Port returns the loopback port the proxy bound, or 0 if not started. Used to
// record the governed endpoint in the session registry (design 001).
func (s *Server) Port() int {
	if s.ln == nil {
		return 0
	}
	if a, ok := s.ln.Addr().(*net.TCPAddr); ok {
		return a.Port
	}
	return 0
}

// SetAuditDir sets the directory redacted request bodies are written to when
// audit_content is enabled (design 001 §4.5). Call before Start. With no dir
// set, audit_content records metadata only — the body sink stays inert.
func (s *Server) SetAuditDir(dir string) { s.auditDir = dir }

// SetRateLimit installs the request-rate rules (design 022 §4.2). Call before
// Start. An empty config leaves the limiter nil, which is the unlimited default.
func (s *Server) SetRateLimit(cfg config.ProxyRateLimitConfig) {
	s.rateCfg = cfg
	s.rebuildRateLimiter()
}

// SetTenants installs per-customer policy (design 022 §4.3). Call before Start.
// Empty ⇒ untenanted, and every control keys off the pane as before.
func (s *Server) SetTenants(cfg config.ProxyTenantConfig) {
	s.tenants = newTenantResolver(cfg)
	s.rebuildRateLimiter()
}

// rebuildRateLimiter re-derives the limiter, since a tenant can carry its own
// rate rule and the two setters may arrive in either order.
func (s *Server) rebuildRateLimiter() {
	var tenantRule func(string) config.RateLimitRule
	if s.tenants != nil {
		tenantRule = s.tenants.rateRule
	}
	s.rate = newRateLimiter(s.rateCfg, tenantRule)
}

// SetFailover installs the upstream-retry policy (design 022 §4.1). Call before
// Start. Disabled ⇒ one attempt per request, exactly as before.
//
// HeaderTimeout is applied to the shared transport only when explicitly set.
// It is off by default because a non-streaming completion can legitimately send
// no bytes for minutes while the model generates: any default here would
// eventually reclassify a slow-but-working request as a dead upstream and retry
// it, paying for the same generation twice.
func (s *Server) SetFailover(cfg config.ProxyFailoverConfig) {
	s.failover = cfg
	if s.tr != nil && cfg.HeaderTimeout > 0 {
		s.tr.ResponseHeaderTimeout = cfg.HeaderTimeout
	}
}

// ForgetPane drops every trace of a pane when it closes, so a long-lived daemon
// does not accumulate state per pane it has ever seen — and so a recycled pane
// ID starts from nothing rather than inheriting the last tenant's verdicts.
//
// Called from PaneActor's Stopping handler; it had no callers at all until then,
// which meant none of this was ever released.
func (s *Server) ForgetPane(paneID string) {
	s.rate.forget(paneID)
	s.budget.invalidate(paneID)
	s.gov.forget(paneID)
	s.panes.forget(paneID)
}

// SetStrict turns the ungoverned-CLI warning into a block (design 022 §8.2).
// Call before Start. It is read by the pane side, which owns the only thing
// that can act on it: the process in the pane.
//
// The proxy holds the flag rather than each pane re-deriving it, because it can
// come from either config or policy and the two must not drift.
func (s *Server) SetStrict(v bool) { s.strict.Store(v) }

// Strict reports whether an ungoverned CLI should be stopped rather than warned
// about.
func (s *Server) Strict() bool {
	if s == nil {
		return false
	}
	v, _ := s.strict.Load().(bool)
	return v
}

// NoteForeground records which program a pane is running (design 022 §4.4), so
// a known agent CLI that never reaches the proxy can be reported. Pass "" when
// the pane returns to its shell.
func (s *Server) NoteForeground(paneID, binary string) {
	s.gov.noteForeground(paneID, binary, time.Now())
}

// DueWarning returns the binary a pane should be warned about, once per run.
// Empty when there is nothing to say.
func (s *Server) DueWarning(paneID string) (string, bool) {
	return s.gov.dueWarning(paneID, time.Now())
}

// PaneGovernanceState is the ##proxy status column: governed | starting |
// unproxied? | idle. The question mark is deliberate — this is inferred from
// the absence of traffic, and an idle CLI looks identical to an escaped one.
func (s *Server) PaneGovernanceState(paneID string) string {
	return s.gov.state(paneID, time.Now())
}

// RecentAudits returns up to n most-recent audit lines (newest last).
func (s *Server) RecentAudits(n int) []AuditLine {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 || n > len(s.audits) {
		n = len(s.audits)
	}
	out := make([]AuditLine, n)
	copy(out, s.audits[len(s.audits)-n:])
	return out
}

func (s *Server) recordAudit(a AuditLine) {
	s.mu.Lock()
	s.audits = append(s.audits, a)
	if len(s.audits) > 256 {
		s.audits = s.audits[len(s.audits)-256:]
	}
	s.mu.Unlock()
}

// writeAuditBody persists the REDACTED request body to the audit_content sink
// (design 001 §4.5). It is a no-op unless audit_content is on AND a directory
// was configured, so the feature cannot silently write bodies. The body passed
// in is already SNAT-sanitized by ServeHTTP; the pre-redaction body is never
// available here.
func (s *Server) writeAuditBody(paneID, dialect string, ts time.Time, redactedBody []byte) {
	if !s.auditContent || s.auditDir == "" || len(redactedBody) == 0 {
		return
	}
	if err := os.MkdirAll(s.auditDir, 0o700); err != nil {
		slog.Warn("proxy: audit_content mkdir failed", "dir", s.auditDir, "err", err)
		return
	}
	name := fmt.Sprintf("%s-%s-%d.json", sanitizeFileComponent(paneID), dialect, ts.UnixNano())
	path := filepath.Join(s.auditDir, name)
	if err := os.WriteFile(path, redactedBody, 0o600); err != nil {
		slog.Warn("proxy: audit_content write failed", "path", path, "err", err)
	}
}

// sanitizeFileComponent keeps a path component to a safe, filename-only set so a
// hostile paneID can never traverse out of the audit dir.
func sanitizeFileComponent(s string) string {
	if s == "" {
		return "unknown"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}

// ServeHTTP implements the per-request pipeline: parse → SNAT → forward →
// stream back → usage/audit.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	dialect, paneID, rest := splitPath(r.URL.Path)
	base, ok := s.upstreams[dialect]
	if !ok {
		http.Error(w, "unknown dialect: "+dialect, http.StatusNotFound)
		return
	}
	d := lookupDialect(dialect)

	// Whose pane is this? Refused BEFORE any control runs, because every control
	// below keys off this segment and the caller chose it: an empty one skips
	// the budget check and the per-pane bucket outright, and a foreign one
	// inherits another pane's tenant pin (paneid.go).
	if !s.panes.allows(paneID) {
		s.writeUnknownPane(w, d, paneID)
		return
	}

	// Whose request is this (design 022 §4.3)? Resolved before any control
	// runs, because ceilings, rates and the audit record all key off it. A
	// pinned pane's tenant is authoritative; the header is honoured only where
	// the operator has said it may be.
	tenant := s.tenants.resolve(paneID, r.Header)

	// Request-rate limiting (design 022 §4.2) — first, because it is purely
	// local: no ledger round trip, no body read. It is therefore the cheapest
	// possible refusal, and putting it ahead of the budget check also shields
	// the ledger from a wrapped CLI stuck in a retry loop.
	if allowed, wait, scope := s.rate.allow(paneID, dialect, tenant); !allowed {
		s.writeRateLimited(w, d, paneID, tenant, wait, scope)
		return
	}

	// Budget enforcement (design 001 §4.4) — before any upstream work, and
	// before reading the body, so a pane that is over its ceiling costs
	// nothing. The ledger reply is cached 2s per pane.
	if allowed, chk := s.budget.check(paneID, tenant); !allowed {
		s.writeBudgetExceeded(w, d, paneID, tenant, chk)
		return
	}

	// Org-wide ceiling (design 023 §4.2). Checked right after the local one and
	// in exactly the same shape — an in-process read of a leased slice, never a
	// network call — so a workspace that is out of budget refuses identically
	// on every machine.
	if s.lease != nil {
		if ok, why := s.lease.Allow(tenant); !ok {
			s.writeLeaseExceeded(w, d, paneID, tenant, why)
			return
		}
	}

	// Positive evidence that this pane's traffic is governed (design 022 §4.4).
	// Recorded before the upstream call, not after: a request that reaches the
	// proxy is governed regardless of what the provider does with it.
	s.gov.noteGoverned(paneID)

	// Read + optionally redact the request body.
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	_ = r.Body.Close()
	reqBytes := len(body)
	redactionHits := 0
	if s.snat != nil && len(body) > 0 {
		sess := s.snat.Session(paneID)
		before := sess.Stats().Detected
		body = []byte(sess.SanitizeJSON(body))
		redactionHits = sess.Stats().Detected - before
	}

	// Which model is this (design 001 §4.7)? Checked after the body is read —
	// the model name lives in it — but before any upstream call, so a refused
	// model costs nothing and never reaches the provider.
	if s.models != nil {
		reqModel := parseModel(string(body))
		if ok, why := s.models.allows(reqModel); !ok {
			s.writeModelRefused(w, d, paneID, tenant, reqModel, why)
			return
		}
	}

	// Provider-specific adaptation the METER depends on (design 001 §4.2): a
	// streaming OpenAI request has to ask for its usage block or the response
	// carries none. Applied after redaction so the adapted bytes are the ones
	// replayed by failover and the ones written to the audit_content sink.
	body = d.AdaptRequestBody(rest, body)

	// Forward, failing over to the next upstream while nothing has reached the
	// client yet (design 022 §4.1). With failover disabled this is exactly one
	// attempt — the pre-022 behaviour, unchanged.
	fr, err := s.forward(r, d, s.targetsFor(dialect, base), body, rest, paneID, tenant)
	if err != nil {
		http.Error(w, "proxy: upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	resp := fr.resp
	defer func() { _ = resp.Body.Close() }()

	// Stream the response to the client while capturing head+tail for usage.
	// PAST THIS LINE THERE IS NO FAILOVER: the client has seen bytes, so an
	// upstream that dies mid-stream stays a truncated stream.
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	capr := &usageCapture{limit: 32 << 10}
	var dst io.Writer = w
	if f, ok := w.(http.Flusher); ok {
		dst = &flushWriter{w: w, f: f}
	}
	_, _ = io.Copy(io.MultiWriter(dst, capr), resp.Body)

	// Extract usage + model and record.
	captured := capr.combined()
	u := d.ExtractUsage([]byte(captured))
	if u.Model == "" {
		u.Model = parseModel(string(body))
	}
	if s.pub != nil && (u.InTokens > 0 || u.OutTokens > 0) {
		_ = s.pub.SendUsageRecord(&msg.MsgUsageRecord{
			PaneID:     paneID,
			Provider:   dialect,
			Model:      u.Model,
			Source:     msg.UsageSourceProxy,
			Tenant:     tenant,
			InTokens:   u.InTokens,
			OutTokens:  u.OutTokens,
			CacheRead:  u.CacheRead,
			CacheWrite: u.CacheWrite,
			TS:         time.Now(),
		})
	}
	// Step 8 of 001 §3's pipeline: did the response hand a live credential back
	// in plaintext? Reported, never rewritten (echo.go).
	echoHits := s.scanResponseEcho(paneID, captured)

	audit := AuditLine{
		TS: time.Now(), PaneID: paneID, Dialect: dialect, Model: u.Model,
		Endpoint: "/" + rest, ReqBytes: reqBytes, RedactionHits: redactionHits,
		Status: resp.StatusCode, InTokens: u.InTokens, OutTokens: u.OutTokens,
		Upstream: fr.upstream, Attempts: fr.attempts, Tenant: tenant,
		EchoHits: echoHits,
	}
	s.recordAudit(audit)
	if s.pub != nil {
		// Durable audit plane (design 001 §4.5): publish the metadata-only
		// record on the pane's proxy.audit subject. The in-process ring above
		// dies with the daemon; this record is captured by the ProxyAuditActor's
		// JetStream stream and consumed by design 013's org-audit forwarding.
		_ = s.pub.SendProxyAudit(&msg.MsgProxyRequestAudit{
			PaneID: paneID, Dialect: dialect, Model: u.Model, Endpoint: "/" + rest,
			ReqBytes: reqBytes, RedactionHits: redactionHits,
			BudgetState: msg.ProxyBudgetOK, Status: resp.StatusCode,
			InTokens: u.InTokens, OutTokens: u.OutTokens,
			Upstream: fr.upstream, Attempts: fr.attempts, Tenant: tenant, TS: audit.TS,
		})
		_ = s.pub.SendPaneRyshOutput(paneID, audit.String()+"\n")
	}
	// audit_content (off by default): persist the REDACTED request body for
	// debugging. Never the pre-redaction body — `body` is already sanitized above.
	s.writeAuditBody(paneID, dialect, audit.TS, body)
	// This request just metered spend, so the cached ceiling verdict is stale.
	// Dropping it means a breach is caught on the very next request instead of
	// up to a TTL later — which matters for a long streaming turn that blows
	// the ceiling in one shot.
	if u.InTokens > 0 || u.OutTokens > 0 {
		s.budget.invalidate(paneID)
		// Draw the same tokens down against the org-wide lease (023 §4.2).
		if s.lease != nil {
			s.lease.Spend(tenant, int64(u.InTokens+u.OutTokens))
		}
	}
}

// SetLeaseGate installs the org-wide ceiling (design 023 §4.2). Call before
// Start. nil ⇒ per-machine ceilings only, exactly as today.
func (s *Server) SetLeaseGate(g LeaseGate) { s.lease = g }

// writeLeaseExceeded emits the dialect-shaped 429 for an org-wide refusal. It
// mirrors writeBudgetExceeded exactly — same status, same shape, same audit
// state — because to the wrapped CLI the two ARE the same thing: a rate limit.
// The pane line is what says which ceiling bound.
func (s *Server) writeLeaseExceeded(
	w http.ResponseWriter, d Dialect, paneID, tenant, why string,
) {
	message := leaseMessage(tenant)
	d.WriteRateLimited(w, message, budgetRetryAfter)

	audit := AuditLine{
		TS: time.Now(), PaneID: paneID, Dialect: d.Name(),
		Endpoint: "(lease)", Status: http.StatusTooManyRequests, Tenant: tenant,
	}
	s.recordAudit(audit)
	slog.Warn("proxy: request refused — workspace-wide token ceiling",
		"pane", paneID, "tenant", tenant, "reason", why)
	if s.pub != nil {
		s.publishRefusalAudit(audit, msg.ProxyBudgetExceeded)
		_ = s.pub.SendPaneRyshOutput(paneID, "[proxy] 429 "+message+"\n")
	}
}

// writeBudgetExceeded emits the dialect-shaped 429, records it in the audit
// ring, and tells the pane why — so the breach is visible in rysh, not only to
// the wrapped CLI that received the error.
func (s *Server) writeBudgetExceeded(
	w http.ResponseWriter, d Dialect, paneID, tenant string, chk msg.MsgUsageCheckReply,
) {
	message := budgetMessage(chk)
	d.WriteRateLimited(w, message, budgetRetryAfter)

	audit := AuditLine{
		TS: time.Now(), PaneID: paneID, Dialect: d.Name(),
		Endpoint: "(budget)", Status: http.StatusTooManyRequests, Tenant: tenant,
	}
	s.recordAudit(audit)
	slog.Warn("proxy: request refused — pane over token ceiling",
		"pane", paneID, "spent", chk.SpentTokens, "ceiling", chk.CeilingTokens)
	if s.pub != nil {
		// A refusal is governed traffic too — it must appear in the durable
		// trail, not only in the pane's rysh buffer. BudgetState=exceeded lets a
		// consumer distinguish a blocked request from a forwarded one.
		_ = s.pub.SendProxyAudit(&msg.MsgProxyRequestAudit{
			PaneID: paneID, Dialect: d.Name(), Endpoint: "(budget)", Tenant: tenant,
			BudgetState: msg.ProxyBudgetExceeded, Status: http.StatusTooManyRequests,
			TS: audit.TS,
		})
		_ = s.pub.SendPaneRyshOutput(paneID, "[proxy] 429 "+message+"\n")
	}
}

// writeRateLimited emits the dialect-shaped 429 for a request-rate refusal
// (design 022 §4.2) and mirrors writeBudgetExceeded's reporting: audit ring,
// durable record, and a pane line — so a throttle is visible in rysh and not
// only to the wrapped CLI that received it.
//
// The audit state is `rate_limited`, not `exceeded`: both are refusals, but the
// operator response differs. A ceiling means stop spending; a rate limit means
// slow down.
func (s *Server) writeRateLimited(
	w http.ResponseWriter, d Dialect, paneID, tenant string, wait time.Duration, scope rateScope,
) {
	message := s.rate.message(scope, d.Name(), tenant)
	d.WriteRateLimited(w, message, wait)

	audit := AuditLine{
		TS: time.Now(), PaneID: paneID, Dialect: d.Name(),
		Endpoint: "(ratelimit)", Status: http.StatusTooManyRequests, Tenant: tenant,
	}
	s.recordAudit(audit)
	slog.Warn("proxy: request refused — request-rate limit",
		"pane", paneID, "dialect", d.Name(), "scope", string(scope), "retry_after", wait)
	if s.pub != nil {
		_ = s.pub.SendProxyAudit(&msg.MsgProxyRequestAudit{
			PaneID: paneID, Dialect: d.Name(), Endpoint: "(ratelimit)", Tenant: tenant,
			BudgetState: msg.ProxyBudgetRateLimited, Status: http.StatusTooManyRequests,
			TS: audit.TS,
		})
		_ = s.pub.SendPaneRyshOutput(paneID, "[proxy] 429 "+message+"\n")
	}
}

// applyAuth injects the real API key server-side per dialect convention. When
// no key is configured, the incoming Authorization/x-api-key headers pass
// through unchanged (already copied). Which headers a key lands in is the
// dialect's own business (Dialect.ApplyAuth).
//
// targetKey, when non-empty, is a credential that belongs to THIS upstream and
// overrides the dialect key. A corp-gateway fallback normally authenticates
// differently from the vendor endpoint, and sending it the vendor's key would
// fail exactly the request that failover exists to rescue.
func (s *Server) applyAuth(d Dialect, req *http.Request, targetKey, tenant string) {
	key := targetKey
	if key == "" {
		// The tenant's own credential, when it has one (design 022 §8.3).
		// Below the per-upstream key on purpose: a failover target that carries
		// its own key authenticates differently from the vendor endpoint, and
		// signing that request with a tenant's vendor key would fail exactly
		// the request failover exists to rescue.
		key = s.tenants.keyFor(tenant, d.Name())
	}
	if key == "" {
		key = s.keys[d.Name()]
	}
	// Never forward the placeholder we injected into the pane.
	if key == config.ProxyDummyKey {
		key = ""
	}

	d.ApplyAuth(req, key)

	if key == "" {
		// No key for this dialect: the caller's own credentials pass through
		// untouched. This is the correct behaviour, not a gap — governance
		// (redaction, metering, audit, budgets) does not depend on rysh
		// holding the credential.
		slog.Debug("proxy: no key configured for dialect, passing caller credentials through",
			"dialect", d.Name())
	}
}

// splitPath parses /{dialect}/{paneID}/{rest...}.
func splitPath(p string) (dialect, paneID, rest string) {
	parts := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 3)
	switch len(parts) {
	case 3:
		return parts[0], parts[1], parts[2]
	case 2:
		return parts[0], parts[1], ""
	case 1:
		return parts[0], "", ""
	}
	return "", "", ""
}

var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true, "Host": true, "Content-Length": true,
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopByHop[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.f.Flush()
	return n, err
}

// usageCapture keeps the first `limit` bytes (head: message_start / request
// echo) and the last `limit` bytes (tail: final message_delta usage) of the
// response, so usage is extractable from streaming SSE regardless of length,
// without buffering the whole body.
type usageCapture struct {
	head  []byte
	tail  []byte
	limit int
	// n is the total number of bytes written, which is what tells the two
	// windows apart from one window seen twice.
	n int
}

func (c *usageCapture) Write(p []byte) (int, error) {
	if len(c.head) < c.limit {
		n := c.limit - len(c.head)
		if n > len(p) {
			n = len(p)
		}
		c.head = append(c.head, p[:n]...)
	}
	c.tail = append(c.tail, p...)
	if len(c.tail) > c.limit {
		c.tail = c.tail[len(c.tail)-c.limit:]
	}
	c.n += len(p)
	return len(p), nil
}

// combined renders the captured bytes for scanning, WITHOUT repeating any byte.
//
// The naive head+tail concatenation double-counts every response shorter than
// the window — which is most of them — and that is invisible to usage parsing
// (the same number read twice is the same number) but wrong for anything that
// COUNTS occurrences: the response secret-echo scan reported two secrets for a
// short body carrying one.
//
// Only a response longer than both windows is genuinely discontinuous, and
// there the two halves are joined by a newline so a number split across the gap
// cannot be spliced back into a value that was never sent.
func (c *usageCapture) combined() string {
	switch {
	case c.n <= c.limit:
		return string(c.head)
	case c.n < 2*c.limit:
		// The windows overlap; drop the repeated prefix from the tail.
		return string(c.head) + string(c.tail[2*c.limit-c.n:])
	default:
		return string(c.head) + "\n" + string(c.tail)
	}
}

var (
	reModel = regexp.MustCompile(`"model"\s*:\s*"([^"]+)"`)
	reIn    = regexp.MustCompile(`"input_tokens"\s*:\s*(\d+)`)
	reOut   = regexp.MustCompile(`"output_tokens"\s*:\s*(\d+)`)
	reCR    = regexp.MustCompile(`"cache_read_input_tokens"\s*:\s*(\d+)`)
	reCW    = regexp.MustCompile(`"cache_creation_input_tokens"\s*:\s*(\d+)`)

	// OpenAI (chat/completions + responses API) and its many compatible
	// servers. `prompt_tokens`/`completion_tokens` are the classic names;
	// `input_tokens`/`output_tokens` (already covered above) are the newer
	// Responses-API names.
	reOAIIn    = regexp.MustCompile(`"prompt_tokens"\s*:\s*(\d+)`)
	reOAIOut   = regexp.MustCompile(`"completion_tokens"\s*:\s*(\d+)`)
	reOAICache = regexp.MustCompile(`"cached_tokens"\s*:\s*(\d+)`)

	// Gemini native REST reports usageMetadata with camelCase counts.
	reGemIn    = regexp.MustCompile(`"promptTokenCount"\s*:\s*(\d+)`)
	reGemOut   = regexp.MustCompile(`"candidatesTokenCount"\s*:\s*(\d+)`)
	reGemCache = regexp.MustCompile(`"cachedContentTokenCount"\s*:\s*(\d+)`)
	reGemModel = regexp.MustCompile(`"modelVersion"\s*:\s*"([^"]+)"`)
)

// parseUsage extracts token usage from a provider response (JSON or SSE).
//
// Anthropic: input/cache take the FIRST occurrence (message_start), output the
// LAST (message_delta carries the running total).
//
// OpenAI and Gemini report different field names. Before they were handled,
// every wrapped non-Anthropic CLI proxied through rysh was metered as ZERO —
// so `##cost` under-reported instead of failing visibly, and budget ceilings
// could never trigger for those panes.
//
// The dialects are tried in order and the first that yields any tokens wins;
// the field names do not collide, so a single pass is unambiguous.
func parseUsage(s string) (in, out, cr, cw int, model string) {
	if m := reModel.FindStringSubmatch(s); m != nil {
		model = m[1]
	}

	// Anthropic / OpenAI Responses API.
	in = firstInt(reIn, s)
	out = lastInt(reOut, s)
	cr = firstInt(reCR, s)
	cw = firstInt(reCW, s)
	if in > 0 || out > 0 {
		return
	}

	// OpenAI chat/completions.
	if pin, pout := firstInt(reOAIIn, s), lastInt(reOAIOut, s); pin > 0 || pout > 0 {
		return pin, pout, firstInt(reOAICache, s), 0, model
	}

	// Gemini native REST.
	if gin, gout := firstInt(reGemIn, s), lastInt(reGemOut, s); gin > 0 || gout > 0 {
		if model == "" {
			if m := reGemModel.FindStringSubmatch(s); m != nil {
				model = m[1]
			}
		}
		return gin, gout, firstInt(reGemCache, s), 0, model
	}

	return in, out, cr, cw, model
}

func parseModel(s string) string {
	if m := reModel.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

func firstInt(re *regexp.Regexp, s string) int {
	if m := re.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

func lastInt(re *regexp.Regexp, s string) int {
	ms := re.FindAllStringSubmatch(s, -1)
	if len(ms) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(ms[len(ms)-1][1])
	return n
}
