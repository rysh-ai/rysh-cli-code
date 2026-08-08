package channels

import "testing"

// The store is shared by ALL of a humanoid's channels, and ApproveLatest is
// what the owner's bare "send" resolves to. Since the channel send tools now
// verify approval IN CODE for slack, email and whatsapp alike, a
// channel-blind ApproveLatest would let a "send" aimed at one channel
// authorise a pending draft on another — these tests pin the channel scoping.

func TestApproveLatestIsChannelScoped(t *testing.T) {
	ds := NewDraftStore()
	emailID := ds.Create("email", "a@example.com", "hi", "email body", "")
	slackID := ds.Create("slack", "C123", "", "slack body", "")

	// A whatsapp-scoped "send" with no whatsapp drafts approves nothing.
	if id, ok := ds.ApproveLatest("whatsapp"); ok {
		t.Fatalf("no whatsapp draft exists, but %q was approved", id)
	}

	// An email-scoped "send" must approve the email draft even though the
	// slack draft is newer.
	id, ok := ds.ApproveLatest("email")
	if !ok || id != emailID {
		t.Fatalf("ApproveLatest(email) = %q, %v; want %q", id, ok, emailID)
	}
	if d, _ := ds.Get(slackID); d.Approved() {
		t.Fatal("the slack draft must remain pending — it was never confirmed")
	}
}

func TestApproveLatestEmptyChannelMatchesAny(t *testing.T) {
	ds := NewDraftStore()
	id := ds.Create("email", "a@example.com", "hi", "body", "")

	got, ok := ds.ApproveLatest("")
	if !ok || got != id {
		t.Fatalf("ApproveLatest(\"\") = %q, %v; want %q", got, ok, id)
	}
}

// TestApproveLatestOnlyConsidersPending re-pins the existing exactly-once
// property under the new signature: a repeated "send" cannot re-approve a
// draft that is already approved (or resurrect a sent-and-deleted one).
func TestApproveLatestOnlyConsidersPending(t *testing.T) {
	ds := NewDraftStore()
	ds.Create("slack", "C123", "", "body", "")

	if _, ok := ds.ApproveLatest("slack"); !ok {
		t.Fatal("first send should approve the pending draft")
	}
	if id, ok := ds.ApproveLatest("slack"); ok {
		t.Fatalf("second send must find nothing pending, approved %q", id)
	}
}

func TestApproveLatestNilStoreIsSafe(t *testing.T) {
	var ds *DraftStore
	if _, ok := ds.ApproveLatest("email"); ok {
		t.Fatal("a nil store can approve nothing")
	}
}
