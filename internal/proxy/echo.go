package proxy

import (
	"fmt"
	"log/slog"
)

// Response secret-echo scan — step 8 of design 001 §3's pipeline, never built.
//
// WHAT IT IS FOR, AND WHAT IT IS NOT
// ----------------------------------
// Requests are the security boundary: SNAT redacts them before they leave the
// machine, and 001 §2 is explicit that response-body redaction is a non-goal.
// Responses are not rewritten here either — the provider only ever saw tokens,
// and a response that echoes a TOKEN is correct behaviour (§4.3).
//
// What is worth knowing is the other case: a response carrying a real, live
// credential in plaintext. It happens — a tool result the CLI fetched, a config
// file the model read back, a provider echoing a request it received on another
// path. rysh cannot un-send it, and pretending otherwise by rewriting the body
// would break the CLI's own parsing for no security gain. So this REPORTS, once
// per response, into the pane's own output.
//
// The scan runs through the pane's own SecretNAT session on purpose:
// Sanitize is idempotent and skips values that are already tokens in that
// session's table, so rysh's own stand-ins never trip it. A throwaway session
// would flag every one of them — `sk_live_SNAT000001` is deliberately shaped
// like a live Stripe key.

// scanResponseEcho reports how many plaintext secrets the response body echoed,
// and warns the pane when there were any.
//
// It runs AFTER the whole response has been forwarded to the client, so it can
// delay nothing the caller is waiting on — the "async, best-effort" property
// 001 §3 asked for, without the nondeterminism of a goroutine that a test (or
// an operator reading pane output) cannot order against anything.
func (s *Server) scanResponseEcho(paneID, responseText string) int {
	if s.snat == nil || responseText == "" {
		return 0
	}
	sess := s.snat.Session(paneID)
	before := sess.Stats().Detected
	// The sanitized text is deliberately DISCARDED: this is a detector, not a
	// rewriter. The side effect that remains — a value seen only in a response
	// gains a mapping — is the one we want, since the next REQUEST carrying it
	// is then redacted like any other known secret.
	_ = sess.Sanitize(responseText)
	hits := sess.Stats().Detected - before
	if hits <= 0 {
		return 0
	}

	slog.Warn("proxy: response echoed a secret in plaintext",
		"pane", paneID, "hits", hits)
	if s.pub != nil {
		_ = s.pub.SendPaneRyshOutput(paneID, echoWarning(hits))
	}
	return hits
}

// echoWarning is the pane line. It says what was seen, what rysh did not do
// about it, and what to do next — the response body itself is never quoted, for
// the obvious reason.
func echoWarning(hits int) string {
	return fmt.Sprintf(
		"[proxy] warning: the provider response echoed %s in plaintext. "+
			"Requests are redacted; responses are forwarded unchanged by design "+
			"(001 §4.3), so this value has already reached the pane. Rotate it if "+
			"it is live; `##snat` shows what rysh recognises.\n",
		plural(hits, "secret value"))
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
