// SPDX-License-Identifier: Apache-2.0

package fleet

import "testing"

// Attention is observed, like activity, and never touches declared work: the
// two axes of design 054 stay separate.
func TestObserveAttentionIsAColumnOfItsOwn(t *testing.T) {
	r := ledger(t)
	t0 := int64(1_000_000)
	r.Observe(pW1, ActivityIdle, t0)
	declare(t, r, pW1, StatusUpdate{Work: WorkProgress, Task: "parser"}, t0)

	r.ObserveAttention(pW1, "prompt", t0+1)
	st := member(r, pW1).Status
	if st.Attention != "prompt" {
		t.Fatalf("Attention = %q, want prompt", st.Attention)
	}
	if st.Work != WorkProgress || st.Task != "parser" {
		t.Fatalf("declared work changed: work=%q task=%q", st.Work, st.Task)
	}
	if st.Activity != ActivityIdle {
		t.Fatalf("activity changed to %q", st.Activity)
	}

	r.ObserveAttention(pW1, "", t0+2)
	if a := member(r, pW1).Status.Attention; a != "" {
		t.Fatalf("Attention not cleared: %q", a)
	}
}
