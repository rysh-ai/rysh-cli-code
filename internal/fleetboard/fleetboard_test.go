// SPDX-License-Identifier: Apache-2.0

package fleetboard

import (
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The fleet board's rows (design 030 §9.2, wave W2). Pure: no NATS, no KV.

const (
	headA   = "aaaa0000-0000-0000-0000-000000000001"
	workerA = "aaaa0000-0000-0000-0000-000000000002"
	headB   = "bbbb0000-0000-0000-0000-000000000001"
	other   = "cccc0000-0000-0000-0000-000000000009"
)

var t0 = time.Date(2026, 11, 1, 15, 0, 0, 0, time.UTC).UnixMilli()

func twoFleets() []fleet.Fleet {
	return []fleet.Fleet{
		{Name: "epic-a", BoardID: "epic-a", State: fleet.StateUp, Members: []fleet.Member{
			{PaneID: workerA, Role: "worker", Parent: headA},
			{PaneID: headA, Role: "ceo"},
		}},
		{Name: "epic-b", BoardID: "epic-b", State: fleet.StateDown, Members: []fleet.Member{
			{PaneID: headB, Role: "ceo"},
		}},
	}
}

func post(pane, kind, text string, ts int64) *msg.MsgBoardPost {
	return &msg.MsgBoardPost{PaneID: pane, Persona: pane[:4], Kind: kind, Text: text, TS: ts}
}

func row(t *testing.T, b *Board, name string) Row {
	t.Helper()
	for _, r := range b.Rows() {
		if r.Fleet == name {
			return r
		}
	}
	t.Fatalf("no row for %s in %+v", name, b.Rows())
	return Row{}
}

func TestOneRowPerRegisteredFleet(t *testing.T) {
	b := New()
	changed, removed := b.ApplyRegistry(twoFleets(), t0)
	if len(changed) != 2 || len(removed) != 0 {
		t.Fatalf("changed %d removed %d, want 2/0", len(changed), len(removed))
	}
	a := row(t, b, "epic-a")
	if a.State != fleet.StateUp || a.Members != 2 || a.BoardID != "epic-a" || a.HeadPane != headA {
		t.Fatalf("row a = %+v", a)
	}
	// Unchanged registry: nothing to publish.
	if changed, _ := b.ApplyRegistry(twoFleets(), t0+1); len(changed) != 0 {
		t.Fatalf("an unchanged registry changed %d rows", len(changed))
	}
}

func TestAForgottenFleetLosesItsRow(t *testing.T) {
	b := New()
	b.ApplyRegistry(twoFleets(), t0)
	_, removed := b.ApplyRegistry(twoFleets()[:1], t0+1)
	if len(removed) != 1 || removed[0] != "epic-b" || len(b.Rows()) != 1 {
		t.Fatalf("removed %v rows %+v", removed, b.Rows())
	}
}

func TestKindsAreNormalised(t *testing.T) {
	for _, tc := range []struct{ kind, text, want string }{
		{"MILESTONE", "x", "milestone"},
		{"Blocked", "x", "blocked"},
		{"", "[epic-a] DONE: shipped", "done"},
		{"note", "[epic-a] START: up", "start"},
		{"", "REPORT: weekly", "report"},
		{"", "milestone reached", "milestone"},
		{"chat", "hello there", ""},
		{"", "[epic-a] hello", ""},
	} {
		if got := NormalizeKind(tc.kind, tc.text); got != tc.want {
			t.Errorf("NormalizeKind(%q, %q) = %q, want %q", tc.kind, tc.text, got, tc.want)
		}
	}
}

func TestOwnBoardPostsSetActivityStatusAndHead(t *testing.T) {
	b := New()
	b.ApplyRegistry(twoFleets(), t0)

	if b.ApplyPost("epic-a", post(workerA, "chat", "working on it", t0+1000)) == nil {
		t.Fatal("a post on the fleet's own board must change its row (activity)")
	}
	b.ApplyPost("epic-a", post(headA, "MILESTONE", "M1 done\nsecond line", t0+2000))
	b.ApplyPost("epic-a", post(workerA, "chat", "next", t0+3000))

	a := row(t, b, "epic-a")
	if a.LastActivity != t0+3000 {
		t.Errorf("last activity %d, want %d", a.LastActivity, t0+3000)
	}
	if a.Head == nil || a.Head.Text != "M1 done" {
		t.Errorf("head post %+v, want the head's milestone, first line only", a.Head)
	}
	if a.LastStatus == nil || a.LastStatus.Kind != "milestone" || a.LastByKind["milestone"].TS != t0+2000 {
		t.Errorf("status %+v / %+v", a.LastStatus, a.LastByKind)
	}
	if a.SinceStatus != 1 {
		t.Errorf("posts since the status post %d, want 1", a.SinceStatus)
	}
	if got := a.Summary(); got != "MILESTONE: M1 done (+1 posts since)" {
		t.Errorf("summary %q", got)
	}
}

// A post on the reserved fleet board counts for the fleet whose MEMBER pane
// posted it (registry membership, not the text prefix), and is not activity on
// the fleet's own board.
func TestFleetBoardPostsCountForThePostersFleet(t *testing.T) {
	b := New()
	b.ApplyRegistry(twoFleets(), t0)

	if b.ApplyPost(msg.FleetBoardID, post(headA, "", "[epic-b] BLOCKED: lying prefix", t0+5000)) == nil {
		t.Fatal("a member's fleet-board post must change its fleet's row")
	}
	a, bb := row(t, b, "epic-a"), row(t, b, "epic-b")
	if a.LastStatus == nil || a.LastStatus.Kind != "blocked" {
		t.Errorf("epic-a status %+v, want blocked (posted by its head)", a.LastStatus)
	}
	if bb.LastStatus != nil {
		t.Errorf("epic-b took a status from a post its members did not make: %+v", bb.LastStatus)
	}
	if a.LastActivity != 0 {
		t.Errorf("a fleet-board post moved own-board activity to %d", a.LastActivity)
	}
	// A non-member's post to the fleet board is attributed to nobody (W3 tags it).
	if b.ApplyPost(msg.FleetBoardID, post(other, "DONE", "x", t0+6000)) != nil {
		t.Error("a non-member's fleet-board post changed a row")
	}
	// A post on a board no fleet owns changes nothing.
	if b.ApplyPost("random-board", post(headA, "DONE", "x", t0+7000)) != nil {
		t.Error("a post on an unowned board changed a row")
	}
}

func TestAnOlderPostNeverReplacesANewerStatus(t *testing.T) {
	b := New()
	b.ApplyRegistry(twoFleets(), t0)
	b.ApplyPost("epic-a", post(headA, "DONE", "new", t0+9000))
	b.ApplyPost("epic-a", post(headA, "BLOCKED", "old", t0+1000))
	a := row(t, b, "epic-a")
	if a.LastStatus.Kind != "done" || a.Head.Text != "new" || a.LastActivity != t0+9000 {
		t.Fatalf("row %+v: the late, older post won", a)
	}
	if a.LastByKind["blocked"].Text != "old" {
		t.Errorf("the older blocked post is still the last blocked post: %+v", a.LastByKind)
	}
}

func TestHealthOrder(t *testing.T) {
	b := New()
	b.ApplyRegistry(twoFleets(), t0)
	now := t0 + time.Hour.Milliseconds()

	if h := row(t, b, "epic-b").Health(now); h != HealthDown {
		t.Errorf("down fleet: %s", h)
	}
	if h := row(t, b, "epic-a").Health(now); h != HealthQuiet {
		t.Errorf("no posts: %s, want quiet", h)
	}
	b.ApplyPost("epic-a", post(workerA, "chat", "x", now-time.Minute.Milliseconds()))
	if h := row(t, b, "epic-a").Health(now); h != HealthActive {
		t.Errorf("recent post: %s, want active", h)
	}
	if h := row(t, b, "epic-a").Health(now + QuietAfter.Milliseconds()); h != HealthQuiet {
		t.Errorf("31 min later: %s, want quiet", h)
	}
	b.ApplyPost("epic-a", post(headA, "BLOCKED", "waiting", now))
	if h := row(t, b, "epic-a").Health(now); h != HealthBlocked {
		t.Errorf("latest status blocked: %s", h)
	}
}

func TestPostTextIsClipped(t *testing.T) {
	b := New()
	b.ApplyRegistry(twoFleets(), t0)
	b.ApplyPost("epic-a", post(headA, "DONE", strings.Repeat("é", 500), t0+1))
	if n := len([]rune(row(t, b, "epic-a").Head.Text)); n > textClip {
		t.Fatalf("head text is %d runes, want at most %d", n, textClip)
	}
}

// Rows loaded from storage are served before any registry answer or post.
func TestLoadedRowsAreServedAsIs(t *testing.T) {
	b := New()
	b.ApplyRegistry(twoFleets(), t0)
	b.ApplyPost("epic-a", post(headA, "MILESTONE", "M1", t0+1))
	saved := b.Rows()

	c := New()
	c.Load(saved)
	if got := row(t, c, "epic-a"); got.LastStatus == nil || got.LastStatus.Text != "M1" || got.HeadPane != headA {
		t.Fatalf("loaded row %+v", got)
	}
	// Membership comes back with the rows, so a fleet-board post is
	// attributed before the first registry answer.
	if c.ApplyPost(msg.FleetBoardID, post(headA, "DONE", "d", t0+2)) == nil {
		t.Fatal("a loaded board did not attribute a member's fleet-board post")
	}
}
