// SPDX-License-Identifier: Apache-2.0

package channels

// Sender authentication for ordinary inbound email (security-lead backlog 2,
// D1-D5): with authserv_id set, every inbound mail is marked sender_auth=pass
// only when the receiving server vouches for its From domain, else fail; with
// no authserv_id it is unchecked, as before.

import (
	"net/mail"
	"strings"
)

const (
	emailSenderAuthPass      = "pass"
	emailSenderAuthFail      = "fail"
	emailSenderAuthUnchecked = "unchecked"
)

// emailSenderAuth returns the sender_auth result for one mail and, on fail,
// its class (never an address or domain).
//
// D1: no authserv_id means unchecked. D2: otherwise anything but dmarc=pass
// fails, and only the TOPMOST Authentication-Results header counts, which must
// carry our authserv-id: a header further down is never read, because a
// sender can plant any header below the receiving server's. Unlike an approval
// answer, an aligned dkim=pass alone is not enough. D3: the dmarc result's
// header.from must equal the From domain, exactly (case-insensitive).
func emailSenderAuth(headerLines []string, authservID string) (result, class string) {
	authservID = strings.TrimSpace(authservID)
	if authservID == "" {
		return emailSenderAuthUnchecked, ""
	}
	fail := func(c string) (string, string) { return emailSenderAuthFail, c }

	froms := headerValues(headerLines, "From")
	if len(froms) != 1 {
		return fail("from-not-single")
	}
	list, err := mail.ParseAddressList(froms[0])
	if err != nil || len(list) != 1 {
		return fail("from-not-single")
	}
	addr := list[0].Address
	at := strings.LastIndexByte(addr, '@')
	if at < 0 || !isMailDomain(addr[at+1:]) {
		return fail("from-no-domain")
	}
	fromDomain := strings.ToLower(addr[at+1:])

	value, found := topmostHeader(headerLines, "Authentication-Results")
	if !found {
		return fail("no-authentication-results")
	}
	parts := strings.Split(stripHeaderComments(value), ";")
	if f := strings.Fields(parts[0]); len(f) == 0 || !strings.EqualFold(f[0], authservID) {
		return fail("foreign-authserv-id")
	}
	class = "dmarc-missing"
	for _, resinfo := range parts[1:] {
		f := strings.Fields(resinfo)
		if len(f) == 0 {
			continue
		}
		method, res, _ := strings.Cut(strings.ToLower(f[0]), "=")
		if method != "dmarc" {
			continue
		}
		headerFrom := ""
		for _, p := range f[1:] {
			if k, v, ok := strings.Cut(p, "="); ok && strings.EqualFold(k, "header.from") {
				headerFrom = strings.ToLower(strings.Trim(v, `"`))
			}
		}
		switch {
		case res == "pass" && headerFrom == fromDomain:
			return emailSenderAuthPass, ""
		case res == "pass":
			class = "dmarc-misaligned"
		case res == "fail" || res == "none" || res == "temperror" || res == "permerror":
			class = "dmarc-" + res
		default:
			class = "dmarc-other"
		}
	}
	return fail(class)
}
