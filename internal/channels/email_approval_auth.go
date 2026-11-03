// SPDX-License-Identifier: Apache-2.0

package channels

// Email approval answers (channels, E33; security-lead ruling 2026-11-01,
// E1-E8). The adapter's part: every mail it sends carries a 128-bit random
// Message-ID (E5), SendWithID reports it so the humanoid can bind an answer
// to its notice (E1), and each inbound mail is marked approval_auth=pass only
// when the RECEIVING server's own Authentication-Results header vouches for
// the From domain (E6). The humanoid applies the rest (E1-E4, E7, E8).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/mail"
	"strings"
)

// newEmailMessageID is the Message-ID for a mail the adapter sends: 128 bits
// from crypto/rand at the sender's domain (E5). A Message-ID is a bearer
// capability once a reply to it can approve, so it must not be guessable.
func newEmailMessageID(from string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("email: crypto/rand failed: " + err.Error())
	}
	domain := "rysh.invalid"
	if _, addr := parseFromHeader(from); addr != "" {
		if i := strings.LastIndexByte(addr, '@'); i >= 0 && isMailDomain(addr[i+1:]) {
			domain = strings.ToLower(addr[i+1:])
		}
	}
	return "<" + hex.EncodeToString(b[:]) + "@" + domain + ">"
}

func isMailDomain(d string) bool {
	if d == "" || !strings.Contains(d, ".") {
		return false
	}
	for _, c := range d {
		if !(c == '.' || c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// emailApprovalAuth reports whether a mail's sender is authenticated well
// enough for its answer to decide an approval (E6): the TOPMOST
// Authentication-Results header (the one the receiving server prepends) must
// carry the configured authserv-id and show dmarc=pass for the From domain,
// or dkim=pass with header.d aligned to it. A header further down the mail
// is never read: a sender can plant any header below the receiving server's.
// No authserv-id configured means email approvals are off. The reason never
// names an address or domain (E8).
func emailApprovalAuth(headerLines []string, authservID string) (ok bool, reason string) {
	authservID = strings.TrimSpace(authservID)
	if authservID == "" {
		return false, "email approvals off: no authserv_id configured"
	}
	// L2: exactly one From header naming exactly one address; anything else
	// is ambiguous about who is answering.
	froms := headerValues(headerLines, "From")
	if len(froms) != 1 {
		return false, "not exactly one From header"
	}
	list, err := mail.ParseAddressList(froms[0])
	if err != nil || len(list) != 1 {
		return false, "From does not name exactly one address"
	}
	addr := list[0].Address
	at := strings.LastIndexByte(addr, '@')
	if at < 0 || !isMailDomain(addr[at+1:]) {
		return false, "sender address has no domain"
	}
	fromDomain := strings.ToLower(addr[at+1:])

	value, found := topmostHeader(headerLines, "Authentication-Results")
	if !found {
		return false, "no Authentication-Results header"
	}
	parts := strings.Split(stripHeaderComments(value), ";")
	if f := strings.Fields(parts[0]); len(f) == 0 || !strings.EqualFold(f[0], authservID) {
		return false, "topmost Authentication-Results is not the configured server's"
	}
	for _, resinfo := range parts[1:] {
		f := strings.Fields(resinfo)
		if len(f) == 0 {
			continue
		}
		method, result, _ := strings.Cut(strings.ToLower(f[0]), "=")
		if result != "pass" {
			continue
		}
		props := map[string]string{}
		for _, p := range f[1:] {
			if k, v, ok := strings.Cut(p, "="); ok {
				props[strings.ToLower(k)] = strings.ToLower(strings.Trim(v, `"`))
			}
		}
		switch method {
		case "dmarc":
			if props["header.from"] == fromDomain {
				return true, ""
			}
		case "dkim":
			d := strings.TrimPrefix(props["header.d"], "@")
			if isMailDomain(d) && (fromDomain == d || strings.HasSuffix(fromDomain, "."+d)) {
				return true, ""
			}
		}
	}
	return false, "no dmarc=pass or aligned dkim=pass from the receiving server"
}

// headerValues returns every header named name in stored order, with folded
// continuation lines joined.
func headerValues(lines []string, name string) []string {
	var out []string
	for i := 0; i < len(lines); i++ {
		k, v, ok := strings.Cut(lines[i], ":")
		if !ok || lines[i] == "" || lines[i][0] == ' ' || lines[i][0] == '\t' || !strings.EqualFold(strings.TrimSpace(k), name) {
			continue
		}
		for j := i + 1; j < len(lines) && lines[j] != "" && (lines[j][0] == ' ' || lines[j][0] == '\t'); j++ {
			v += " " + strings.TrimSpace(lines[j])
		}
		out = append(out, strings.TrimSpace(v))
	}
	return out
}

// topmostHeader returns the first header named name in stored order.
func topmostHeader(lines []string, name string) (string, bool) {
	if v := headerValues(lines, name); len(v) > 0 {
		return v[0], true
	}
	return "", false
}

// emailStampedBy reports whether the topmost Authentication-Results carries
// authservID, i.e. the configured receiving server stamped this mail.
func emailStampedBy(headerLines []string, authservID string) bool {
	v, ok := topmostHeader(headerLines, "Authentication-Results")
	if !ok {
		return false
	}
	f := strings.Fields(strings.Split(stripHeaderComments(v), ";")[0])
	return len(f) > 0 && strings.EqualFold(f[0], strings.TrimSpace(authservID))
}

// emailApprovalLatch is the persisted L1 latch (security-lead, 2026-11-01):
// once a mail in the INBOX arrives without the configured server's
// Authentication-Results on top, the provider is not stamping every mail and
// a planted header could pass as its own, so email approvals stay off for
// this authserv_id and IMAP host until the email config changes.
type emailApprovalLatch struct {
	Off        bool   `json:"off"`
	AuthservID string `json:"authserv_id"`
	IMAPHost   string `json:"imap_host"`
}

const emailApprovalLatchState = "email-approvals"

// approvalsLatchedOff reports whether the L1 latch is set for the current
// config (loaded from disk once).
func (e *EmailAdapter) approvalsLatchedOff() bool {
	cfg := e.emailCfg()
	e.latchMu.Lock()
	defer e.latchMu.Unlock()
	if !e.latchLoaded {
		var st emailApprovalLatch
		if loadChannelState(emailApprovalLatchState, cfg.Username, &st) {
			e.latch = st
		}
		e.latchLoaded = true
	}
	return e.latch.Off && e.latch.AuthservID == strings.TrimSpace(cfg.AuthservID) && e.latch.IMAPHost == cfg.IMAPHost
}

// latchApprovalsOff sets and persists the L1 latch. It reports whether this
// call set it (the caller announces it once). The log names no address.
func (e *EmailAdapter) latchApprovalsOff() bool {
	if e.approvalsLatchedOff() {
		return false
	}
	cfg := e.emailCfg()
	e.latchMu.Lock()
	e.latch = emailApprovalLatch{Off: true, AuthservID: strings.TrimSpace(cfg.AuthservID), IMAPHost: cfg.IMAPHost}
	st := e.latch
	e.latchMu.Unlock()
	if err := saveChannelState(emailApprovalLatchState, cfg.Username, st); err != nil {
		slog.Warn("email: approvals latched off, but the latch could not be saved; it holds until restart", "err", err)
	}
	slog.Warn("email: approvals latched off: a mail arrived without the configured server's Authentication-Results on top, so the provider does not stamp every mail; change the email config to turn them back on")
	return true
}

// emailApprovalAuthFor runs the L1 latch and E6 for one inbound mail: it
// returns whether the sender authenticated for an approval, and whether this
// mail just tripped the latch.
func (e *EmailAdapter) emailApprovalAuthFor(headerLines []string) (authOK, tripped bool) {
	authserv := strings.TrimSpace(e.emailCfg().AuthservID)
	if authserv == "" || e.approvalsLatchedOff() {
		return false, false
	}
	if !emailStampedBy(headerLines, authserv) {
		return false, e.latchApprovalsOff()
	}
	ok, _ := emailApprovalAuth(headerLines, authserv)
	return ok, false
}

// stripHeaderComments removes RFC 5322 comments "( ... )", nested, so a ";"
// inside one does not split a result.
func stripHeaderComments(s string) string {
	var b strings.Builder
	depth := 0
	for _, c := range s {
		switch {
		case c == '(':
			depth++
		case c == ')' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// Send delivers a reply email via SMTP.
func (e *EmailAdapter) Send(ctx context.Context, outbound OutboundMessage) error {
	_, err := e.SendWithID(ctx, outbound)
	return err
}
