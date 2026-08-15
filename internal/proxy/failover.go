// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// Provider failover (design 022 §4.1).
//
// THE INVARIANT THIS WHOLE FILE RESTS ON
// --------------------------------------
// A request may only be retried while the client has seen NOTHING. ServeHTTP
// calls copyHeaders + WriteHeader immediately after a successful attempt, so
// the window between Do() returning and WriteHeader() is the only safe retry
// point. Once one byte of a response has been forwarded, an upstream that dies
// mid-stream stays a truncated stream — retrying there would splice two
// different completions into one response body, which is worse than the
// truncation it tried to fix.
//
// Body replay is free: ServeHTTP already buffers the (redacted) request body,
// so each attempt just wraps the same bytes in a fresh reader.
//
// WHAT FAILOVER DOES NOT PROMISE
// ------------------------------
// Not exactly-once. An upstream that returns 503 may already have started
// generating, and may bill for it. Retrying is still the right default — a
// 5xx with no usable response is worth another attempt — but the guarantee is
// "at most one response reaches the client", not "at most one generation is
// paid for".

// defaultRetryStatus is what triggers the next upstream when config says
// nothing. 501 is deliberately absent: "not implemented" is a stable answer,
// not a transient failure, and retrying it just multiplies the same error.
var defaultRetryStatus = []int{
	http.StatusTooManyRequests,     // 429
	http.StatusInternalServerError, // 500
	http.StatusBadGateway,          // 502
	http.StatusServiceUnavailable,  // 503
	http.StatusGatewayTimeout,      // 504
}

// upstreamTarget is one attempt destination. Key empty ⇒ use the dialect key.
type upstreamTarget struct {
	URL string
	Key string
}

// targetsFor builds the ordered attempt list for a dialect: the primary
// (already resolved from the upstreams map by the caller) followed by the
// configured fallbacks.
//
// With failover disabled the list is exactly one entry, so the attempt loop
// collapses to the single call the proxy has always made.
func (s *Server) targetsFor(dialect, primary string) []upstreamTarget {
	targets := []upstreamTarget{{URL: primary}}
	if !s.failover.Enabled {
		return targets
	}
	for _, u := range s.failover.Upstreams[dialect] {
		if u.URL == "" || u.URL == primary {
			continue // never queue the primary twice
		}
		targets = append(targets, upstreamTarget{URL: u.URL, Key: u.Key})
	}
	return targets
}

// maxAttempts bounds the loop. <= 0 means "every configured upstream once".
func (s *Server) maxAttempts(targets []upstreamTarget) int {
	n := len(targets)
	if m := s.failover.MaxAttempts; m > 0 && m < n {
		return m
	}
	return n
}

// shouldRetry reports whether an upstream status warrants the next upstream.
func (s *Server) shouldRetry(status int) bool {
	list := s.failover.RetryStatus
	if len(list) == 0 {
		list = defaultRetryStatus
	}
	for _, c := range list {
		if c == status {
			return true
		}
	}
	return false
}

// forwardResult reports which upstream actually served the request and how
// many attempts it took, so the audit trail can show a silent failover.
type forwardResult struct {
	resp     *http.Response
	upstream string
	attempts int
}

// forward runs the attempt loop and returns the response to hand to the client.
//
// The LAST attempt's response is returned verbatim even when it is retryable —
// a 503 from the final upstream reaches the client as a 503. Synthesizing a 502
// there would replace the provider's own error (which the wrapped CLI knows how
// to read) with one it does not.
func (s *Server) forward(
	r *http.Request, d Dialect, targets []upstreamTarget, body []byte, rest, paneID, tenant string,
) (forwardResult, error) {
	max := s.maxAttempts(targets)
	if max == 0 {
		return forwardResult{}, errors.New("no upstream configured")
	}

	var lastErr error
	for i := 0; i < max; i++ {
		// A client that has gone away wants nothing, least of all a retry
		// against a second provider it will never read.
		if err := r.Context().Err(); err != nil {
			return forwardResult{attempts: i}, err
		}
		t := targets[i]
		last := i+1 >= max

		upReq, err := s.buildUpstreamRequest(r, d, t, body, rest, tenant)
		if err != nil {
			return forwardResult{attempts: i + 1}, err
		}

		resp, err := s.client.Do(upReq)
		if err != nil {
			lastErr = err
			if last || r.Context().Err() != nil {
				return forwardResult{upstream: t.URL, attempts: i + 1}, err
			}
			s.reportFailover(paneID, t.URL, targets[i+1].URL, 0, err)
			continue
		}

		if !last && s.shouldRetry(resp.StatusCode) {
			// Drain a bounded prefix before closing so the connection can be
			// reused; an unread body would otherwise poison keep-alive.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
			_ = resp.Body.Close()
			s.reportFailover(paneID, t.URL, targets[i+1].URL, resp.StatusCode, nil)
			continue
		}

		return forwardResult{resp: resp, upstream: t.URL, attempts: i + 1}, nil
	}

	if lastErr == nil {
		lastErr = errors.New("all upstreams exhausted")
	}
	return forwardResult{attempts: max}, lastErr
}

// buildUpstreamRequest wraps the already-redacted body for one attempt. Each
// attempt gets a fresh reader over the same bytes — bytes.NewReader, not the
// string round trip the single-attempt path used to do.
func (s *Server) buildUpstreamRequest(
	r *http.Request, d Dialect, t upstreamTarget, body []byte, rest, tenant string,
) (*http.Request, error) {
	upURL := strings.TrimRight(t.URL, "/") + "/" + rest
	if r.URL.RawQuery != "" {
		upURL += "?" + r.URL.RawQuery
	}
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("proxy: build request: %w", err)
	}
	copyHeaders(upReq.Header, r.Header)
	// rysh's own control headers must not cross the wire (design 022 §4.3).
	// copyHeaders forwards everything non-hop-by-hop, so without this the
	// provider would be told which customer the request belongs to.
	stripRyshHeaders(upReq.Header)
	// Drop the CLIENT's Accept-Encoding and let Go's transport negotiate its
	// own. This is a metering correctness fix, not a nicety: when the header is
	// forwarded verbatim, net/http will not transparently decompress the
	// response, so usage extraction scanned gzip bytes and every Node-based CLI
	// (they all send `Accept-Encoding: gzip, deflate` by default) metered
	// 0 in / 0 out — silently, with ceilings that could therefore never bind.
	// With the header removed the transport adds `gzip` itself, decompresses,
	// and strips Content-Encoding/Content-Length from the response it hands back.
	upReq.Header.Del("Accept-Encoding")
	s.applyAuth(d, upReq, t.Key, tenant)
	return upReq, nil
}

// reportFailover makes a fallback visible. A silent failover is a support
// incident waiting to happen: the pane keeps working, nobody learns the primary
// is down, and the bill lands on an upstream nobody chose.
func (s *Server) reportFailover(paneID, from, to string, status int, err error) {
	reason := fmt.Sprintf("status %d", status)
	if err != nil {
		reason = err.Error()
	}
	slog.Warn("proxy: upstream failed, trying the next one",
		"pane", paneID, "from", from, "to", to, "reason", reason)
	if s.pub != nil {
		_ = s.pub.SendPaneRyshOutput(paneID,
			fmt.Sprintf("[proxy] upstream %s failed (%s) → %s\n",
				hostOf(from), reason, hostOf(to)))
	}
}

// hostOf shortens a base URL to its host for pane output. The full URL is in
// the audit record; the pane line just needs to say which one.
func hostOf(raw string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return raw
	}
	return s
}

// FailoverSummary renders the configured upstream chain for ##proxy status.
func FailoverSummary(c config.ProxyFailoverConfig) string {
	if !c.Enabled {
		return "off (single upstream)"
	}
	var parts []string
	for _, dialect := range []string{"anthropic", "openai", "gemini"} {
		if list := c.Upstreams[dialect]; len(list) > 0 {
			hosts := make([]string, 0, len(list))
			for _, u := range list {
				hosts = append(hosts, hostOf(u.URL))
			}
			parts = append(parts, dialect+" → "+strings.Join(hosts, " → "))
		}
	}
	if len(parts) == 0 {
		return "on (no fallbacks configured)"
	}
	return strings.Join(parts, "; ")
}
