// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"

	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 M1 (d2): when a reply's In-Reply-To parent is not in the listing, it is
// threaded under the NEAREST ancestor from its References chain that is.

func mailRefs(uid int, id, inReplyTo, subject string, refs ...string) msgpkg.EmailSummary {
	e := mail(uid, id, inReplyTo, subject)
	e.References = refs
	return e
}

func TestThreadEmailsUsesReferences(t *testing.T) {
	got, depth := threadEmails([]msgpkg.EmailSummary{
		// parent <c> is outside the window; <b> is the nearest listed ancestor
		mailRefs(4, "<d>", "<c>", "D re C", "<a>", "<b>", "<c>"),
		mailRefs(3, "<e>", "<gone>", "E re gone", "<a>", "<gone>"),
		mailRefs(2, "<b>", "<a>", "B re A", "<a>"),
		mail(1, "<a>", "", "A root"),
	})
	// A ← B ← D (via References <b>); A ← E (via References <a>).
	if !eqInts(uids(got), []int{1, 2, 4, 3}) {
		t.Fatalf("order = %v, want [1 2 4 3]", uids(got))
	}
	if !eqInts(depth, []int{0, 1, 2, 1}) {
		t.Fatalf("depths = %v, want [0 1 2 1]: D belongs under B (nearest listed ancestor), E under A", depth)
	}
}

// TestThreadEmailsReferencesCycle: References that loop must neither hang nor
// drop or duplicate mail.
func TestThreadEmailsReferencesCycle(t *testing.T) {
	got, _ := threadEmails([]msgpkg.EmailSummary{
		mailRefs(2, "<a>", "<gone1>", "A", "<b>"),
		mailRefs(1, "<b>", "<gone2>", "B", "<a>"),
		mailRefs(3, "<s>", "", "S", "<s>"),
	})
	count := map[int]int{}
	for _, e := range got {
		count[e.UID]++
	}
	if len(got) != 3 || count[1] != 1 || count[2] != 1 || count[3] != 1 {
		t.Fatalf("cyclic References threaded to %v, want each of 1,2,3 exactly once", uids(got))
	}
}
