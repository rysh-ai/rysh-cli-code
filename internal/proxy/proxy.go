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
}

func (a AuditLine) String() string {
	red := ""
	if a.RedactionHits > 0 {
		red = fmt.Sprintf(" %d redaction(s)", a.RedactionHits)
	}
	return fmt.Sprintf("[proxy] %s %s model=%s %din/%dout tok status=%d%s",
		a.Dialect, a.Endpoint, a.Model, a.InTokens, a.OutTokens, a.Status, red)
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

	budget *budgetChecker // ledger-backed ceiling enforcement (design 001 §4.4)

	client  *http.Client
	httpSrv *http.Server
	ln      net.Listener
	baseURL string

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
	return &Server{
		pub:          pub,
		snat:         snat,
		keys:         keys,
		upstreams:    upstreams,
		auditContent: auditContent,
		budget:       newBudgetChecker(pub),
		client: &http.Client{
			// No overall timeout: agentic responses stream for minutes. The
			// per-request context (client disconnect) bounds it instead.
			Timeout: 0,
		},
	}
}

// Start binds a loopback listener (ephemeral port unless addr specifies one),
// serves in the background, and publishes the endpoint for pane env injection.
// Returns the base URL (http://127.0.0.1:PORT).
func (s *Server) Start(addr string) (string, error) {
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
	SetEndpoint(s.baseURL)
	slog.Info("governance proxy started", "url", s.baseURL, "redaction", s.snat != nil)
	return s.baseURL, nil
}

// Stop clears the endpoint and shuts the server down.
func (s *Server) Stop() {
	SetEndpoint("")
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

	// Budget enforcement (design 001 §4.4) — before any upstream work, and
	// before reading the body, so a pane that is over its ceiling costs
	// nothing. The ledger reply is cached 2s per pane.
	if allowed, chk := s.budget.check(paneID); !allowed {
		s.writeBudgetExceeded(w, d, paneID, chk)
		return
	}

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

	// Build the upstream request.
	upURL := strings.TrimRight(base, "/") + "/" + rest
	if r.URL.RawQuery != "" {
		upURL += "?" + r.URL.RawQuery
	}
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upURL, strings.NewReader(string(body)))
	if err != nil {
		http.Error(w, "proxy: build request: "+err.Error(), http.StatusBadGateway)
		return
	}
	copyHeaders(upReq.Header, r.Header)
	s.applyAuth(d, upReq)

	resp, err := s.client.Do(upReq)
	if err != nil {
		http.Error(w, "proxy: upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Stream the response to the client while capturing head+tail for usage.
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	capr := &usageCapture{limit: 32 << 10}
	var dst io.Writer = w
	if f, ok := w.(http.Flusher); ok {
		dst = &flushWriter{w: w, f: f}
	}
	_, _ = io.Copy(io.MultiWriter(dst, capr), resp.Body)

	// Extract usage + model and record.
	u := d.ExtractUsage([]byte(capr.combined()))
	if u.Model == "" {
		u.Model = parseModel(string(body))
	}
	if s.pub != nil && (u.InTokens > 0 || u.OutTokens > 0) {
		_ = s.pub.SendUsageRecord(&msg.MsgUsageRecord{
			PaneID:     paneID,
			Provider:   dialect,
			Model:      u.Model,
			Source:     msg.UsageSourceProxy,
			InTokens:   u.InTokens,
			OutTokens:  u.OutTokens,
			CacheRead:  u.CacheRead,
			CacheWrite: u.CacheWrite,
			TS:         time.Now(),
		})
	}
	audit := AuditLine{
		TS: time.Now(), PaneID: paneID, Dialect: dialect, Model: u.Model,
		Endpoint: "/" + rest, ReqBytes: reqBytes, RedactionHits: redactionHits,
		Status: resp.StatusCode, InTokens: u.InTokens, OutTokens: u.OutTokens,
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
			InTokens: u.InTokens, OutTokens: u.OutTokens, TS: audit.TS,
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
	}
}

// writeBudgetExceeded emits the dialect-shaped 429, records it in the audit
// ring, and tells the pane why — so the breach is visible in rysh, not only to
// the wrapped CLI that received the error.
func (s *Server) writeBudgetExceeded(
	w http.ResponseWriter, d Dialect, paneID string, chk msg.MsgUsageCheckReply,
) {
	message := budgetMessage(chk)
	d.WriteBudgetExceeded(w, message)

	audit := AuditLine{
		TS: time.Now(), PaneID: paneID, Dialect: d.Name(),
		Endpoint: "(budget)", Status: http.StatusTooManyRequests,
	}
	s.recordAudit(audit)
	slog.Warn("proxy: request refused — pane over token ceiling",
		"pane", paneID, "spent", chk.SpentTokens, "ceiling", chk.CeilingTokens)
	if s.pub != nil {
		// A refusal is governed traffic too — it must appear in the durable
		// trail, not only in the pane's rysh buffer. BudgetState=exceeded lets a
		// consumer distinguish a blocked request from a forwarded one.
		_ = s.pub.SendProxyAudit(&msg.MsgProxyRequestAudit{
			PaneID: paneID, Dialect: d.Name(), Endpoint: "(budget)",
			BudgetState: msg.ProxyBudgetExceeded, Status: http.StatusTooManyRequests,
			TS: audit.TS,
		})
		_ = s.pub.SendPaneRyshOutput(paneID, "[proxy] 429 "+message+"\n")
	}
}

// applyAuth injects the real API key server-side per dialect convention. When
// no key is configured, the incoming Authorization/x-api-key headers pass
// through unchanged (already copied). Which headers a key lands in is the
// dialect's own business (Dialect.ApplyAuth).
func (s *Server) applyAuth(d Dialect, req *http.Request) {
	key := s.keys[d.Name()]
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
	return len(p), nil
}

func (c *usageCapture) combined() string { return string(c.head) + "\n" + string(c.tail) }

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
