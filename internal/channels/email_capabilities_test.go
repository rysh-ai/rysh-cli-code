// SPDX-License-Identifier: Apache-2.0

package channels

// channels (E33), found while adding email to level E: parseCapabilities read
// the capability list up to the end of the line and only trimmed a trailing
// "]". In a bracketed response code with text after it
// ("* OK [CAPABILITY IMAP4rev1 IDLE] ready") the last capability became
// "IDLE]" and the text after the bracket became capabilities, so a server
// that lists IDLE last was polled every 30 s instead of watched with IDLE.

import "testing"

func TestEmailParseCapabilitiesBracketedCode(t *testing.T) {
	for _, tc := range []struct {
		line string
		want []string
		not  []string
	}{
		{"* OK [CAPABILITY IMAP4rev1 IDLE] fake IMAP ready", []string{"IMAP4REV1", "IDLE"}, []string{"IDLE]", "READY", "FAKE"}},
		{"A0001 OK [CAPABILITY IMAP4rev1 MOVE IDLE] Logged in", []string{"MOVE", "IDLE"}, []string{"IDLE]", "LOGGED", "IN"}},
		{"* CAPABILITY IMAP4rev1 IDLE UIDPLUS", []string{"IDLE", "UIDPLUS"}, nil},
	} {
		c := &imapClient{capabilities: map[string]bool{}}
		c.parseCapabilities(tc.line)
		for _, w := range tc.want {
			if !c.hasCapability(w) {
				t.Errorf("%q: missing capability %s; got %v", tc.line, w, c.capabilities)
			}
		}
		for _, n := range tc.not {
			if c.capabilities[n] {
				t.Errorf("%q: %q read as a capability; got %v", tc.line, n, c.capabilities)
			}
		}
	}
}
