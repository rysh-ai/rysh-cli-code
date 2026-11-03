// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"strings"
	"testing"
)

// The policy summary states what strict mode covers, and no more
// (root-order-039).
func TestSummaryStatesWhatStrictCovers(t *testing.T) {
	p := &Policy{Path: "policy.yaml", Loaded: true, ProxyStrict: true}
	want := "  proxy strict:  ON (an ungoverned agent CLI is stopped, not just warned)\n" +
		"                 covers the allowed CLIs at their known install locations, by real executable;\n" +
		"                 not an anti-evasion boundary: a copy elsewhere is unknown and not watched\n"
	if got := p.Summary(); !strings.Contains(got, want) {
		t.Fatalf("summary does not state strict's coverage exactly.\nwant:\n%s\ngot:\n%s", want, got)
	}
}
