// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Model allowlist (design 001 §4.7).
//
// 001 sketched "model allowlists" inside its v2 rule-engine section and nothing
// was built, so a governed pane could call any model the credential could reach.
// That is the one governance question a token ceiling cannot answer: a ceiling
// bounds how much is spent, not what it is spent ON. An operator who has
// approved a provider has not thereby approved every model that provider will
// ever host, and "no preview models on customer data" is a rule someone has to
// be able to write down.
//
// WHAT THIS IS NOT
// ----------------
// It is not model ROUTING. 001 §4.7 lists routing rules in the same breath, but
// routing raises questions this file cannot answer on its own — whether the
// credential travels with the redirect, whether a redirected model keeps its
// dialect, what the audit record says the request cost. The seam for it is
// targetsFor() in failover.go, which already turns one request into an ordered
// list of destinations. Until it is designed, an unlisted model is REFUSED
// rather than quietly sent somewhere else, because a refusal is legible and a
// silent redirect is not.

// modelRules answers "may this pane call this model?".
type modelRules struct {
	allow []string
	deny  []string
}

// newModelRules returns nil when nothing is configured, so the default path
// costs one nil check — the same shape as the rate limiter and the tenant
// resolver.
func newModelRules(cfg config.ProxyModelRules) *modelRules {
	if !cfg.Any() {
		return nil
	}
	return &modelRules{allow: normalizePatterns(cfg.Allow), deny: normalizePatterns(cfg.Deny)}
}

func normalizePatterns(in []string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// allows reports whether model may be called, and why not when it may not.
//
// An EMPTY model is allowed. Plenty of governed traffic names no model at all —
// `/v1/models`, token counting, health probes — and refusing those would break
// CLIs to enforce a rule that has nothing to say about them.
func (m *modelRules) allows(model string) (bool, string) {
	if m == nil {
		return true, ""
	}
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return true, ""
	}
	if pat, ok := matchAny(m.deny, name); ok {
		return false, "matches the denied pattern " + pat
	}
	if len(m.allow) == 0 {
		return true, ""
	}
	if _, ok := matchAny(m.allow, name); ok {
		return true, ""
	}
	return false, "is not in the allowed model list (" + strings.Join(m.allow, ", ") + ")"
}

// matchAny reports the first pattern that matches, using shell-glob semantics.
// A pattern with no metacharacter is an exact match, which is what someone
// writing a bare model name means.
func matchAny(patterns []string, name string) (string, bool) {
	for _, p := range patterns {
		if ok, err := path.Match(p, name); err == nil && ok {
			return p, true
		}
		// A malformed pattern must not silently match nothing forever; fall
		// back to equality so the rule still binds on the literal name.
		if p == name {
			return p, true
		}
	}
	return "", false
}

// SetModelRules installs the model allowlist (design 001 §4.7). Call before
// Start. Empty ⇒ every model is allowed, as before.
func (s *Server) SetModelRules(cfg config.ProxyModelRules) { s.models = newModelRules(cfg) }

// writeModelRefused emits the dialect-shaped refusal for a model this pane may
// not call, and reports it the same way the ceiling and rate-limit refusals do:
// audit ring, durable record, and a line in the pane — a block nobody can see
// is a support ticket.
//
// 403, not 429: a rate limit says "later", and a model rule never will.
func (s *Server) writeModelRefused(
	w http.ResponseWriter, d Dialect, paneID, tenant, model, why string,
) {
	message := "rysh governance proxy: the model " + model + " " + why +
		". Change it under [proxy] models in rysh.config.yaml, or call an allowed model."
	writeDialectError(w, d, http.StatusForbidden, message)

	audit := AuditLine{
		TS: time.Now(), PaneID: paneID, Dialect: d.Name(), Model: model,
		Endpoint: "(model)", Status: http.StatusForbidden, Tenant: tenant,
	}
	s.recordAudit(audit)
	slog.Warn("proxy: request refused — model not permitted",
		"pane", paneID, "model", model, "reason", why)
	if s.pub != nil {
		s.publishRefusalAudit(audit, msg.ProxyBlocked)
		_ = s.pub.SendPaneRyshOutput(paneID, "[proxy] 403 "+message+"\n")
	}
}

// publishRefusalAudit puts a refusal on the durable trail. A blocked request is
// governed traffic too: an operator reading `##proxy audit` has to see what was
// stopped, not only what was forwarded.
func (s *Server) publishRefusalAudit(a AuditLine, state string) {
	if s.pub == nil {
		return
	}
	_ = s.pub.SendProxyAudit(&msg.MsgProxyRequestAudit{
		PaneID: a.PaneID, Dialect: a.Dialect, Model: a.Model, Endpoint: a.Endpoint,
		Tenant: a.Tenant, BudgetState: state, Status: a.Status, TS: a.TS,
	})
}
