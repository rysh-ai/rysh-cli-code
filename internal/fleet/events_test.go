// SPDX-License-Identifier: Apache-2.0

package fleet

import "testing"

func TestLedgerState(t *testing.T) {
	for _, c := range []struct {
		s    *MemberStatus
		want string
	}{
		{nil, WorkIdle},
		{&MemberStatus{}, WorkIdle},
		{&MemberStatus{Activity: ActivityWorking}, WorkProgress},
		{&MemberStatus{Activity: ActivityUnknown}, WorkIdle},
		{&MemberStatus{Work: WorkBlocked, Activity: ActivityWorking}, WorkBlocked},
		{&MemberStatus{Work: WorkDone}, WorkDone},
	} {
		if got := LedgerState(c.s); got != c.want {
			t.Errorf("LedgerState(%+v) = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestLedgerTrackerReportsOnlyChanges(t *testing.T) {
	tr := NewLedgerTracker()
	pct := 10
	fleets := []Fleet{{Name: "f", Members: []Member{
		{PaneID: "a", Status: &MemberStatus{Work: WorkProgress, Note: "n", Pct: &pct}},
		{PaneID: "b"},
	}}}
	if got := tr.Changes(fleets, 1); len(got) != 2 {
		t.Fatalf("first call = %+v, want both members", got)
	}
	// Timestamps and observation times move; the visible row does not.
	fleets[0].Members[0].Status.UpdatedAt = 99
	fleets[0].Members[0].Status.ActiveAt = 99
	if got := tr.Changes(fleets, 2); len(got) != 0 {
		t.Fatalf("no visible change reported %+v", got)
	}
	p2 := 20
	fleets[0].Members[0].Status.Pct = &p2
	got := tr.Changes(fleets, 3)
	if len(got) != 1 || got[0].PaneID != "a" || *got[0].Pct != 20 || got[0].At != 3 {
		t.Fatalf("pct change = %+v", got)
	}
	// A member that leaves and comes back is announced again.
	fleets[0].Members = fleets[0].Members[:1]
	tr.Changes(fleets, 4)
	fleets[0].Members = append(fleets[0].Members, Member{PaneID: "b"})
	if got := tr.Changes(fleets, 5); len(got) != 1 || got[0].PaneID != "b" {
		t.Fatalf("returning member = %+v", got)
	}
}
