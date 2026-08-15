// SPDX-License-Identifier: Apache-2.0

package board

import (
	"encoding/json"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Tests for the board's READ path (query.go).
//
// The filtering rules are pure, so they are tested here without NATS. The
// round trip through a real recorder — and the case that matters most, a query
// nobody answers — lives in internal/actors/abla_query_test.go, because it
// needs a real ABLA to be present and then absent.

func qpost(paneID, persona, text, threadID string, ts int64) *msg.MsgBoardPost {
	return &msg.MsgBoardPost{
		V:        msg.BoardSchemaVersion,
		PaneID:   paneID,
		Persona:  persona,
		Kind:     msg.BoardKindMilestone,
		Text:     text,
		ThreadID: threadID,
		TS:       ts,
	}
}

const (
	qPaneA = "aaaa1111-f8bb-45fb-9d02-2657b16706ae"
	qPaneB = "bbbb2222-ff1c-4435-84bc-ab2eed4aa435"
)

// TestAnswerSinceFiltersByTheThreadsLatestActivity: --since keeps a thread
// whose ROOT is old but which has been replied to recently.
//
// The tempting implementation — filter individual posts by TS — would drop the
// old root and hand back the replies as an orphan pile, silently changing what
// the answer means. Filtering whole threads by their latest activity is the
// same whole-thread rule the store's eviction already follows.
func TestAnswerSinceFiltersByTheThreadsLatestActivity(t *testing.T) {
	s := New(0)
	// An old thread nobody has touched.
	s.Apply(qpost(qPaneA, "planner", "old and quiet", msg.MintThreadID(qPaneA, 1), 1000))
	// An old thread with a RECENT reply.
	s.Apply(qpost(qPaneA, "planner", "old but live", msg.MintThreadID(qPaneA, 2), 1100))
	s.Apply(qpost(qPaneB, "builder", "still working on it", msg.MintThreadID(qPaneA, 2), 9000))

	got := Answer(s, Query{Since: 5000})

	if len(got.Threads) != 1 {
		t.Fatalf("--since returned %d threads, want 1: %+v", len(got.Threads), got.Threads)
	}
	if got.Threads[0].Root == nil || got.Threads[0].Root.Text != "old but live" {
		t.Fatalf("--since kept the wrong thread: %+v", got.Threads[0].Root)
	}
	if len(got.Threads[0].Replies) != 1 {
		t.Fatalf("--since split a thread: kept root with %d replies, want 1",
			len(got.Threads[0].Replies))
	}
	if got.Filtered != 1 {
		t.Fatalf("Filtered = %d, want 1 — a filtered answer that does not say what it dropped "+
			"presents a window as the whole board", got.Filtered)
	}
	// Stats describe the whole board, not the window: that is how a caller
	// knows there is more than it was shown.
	if got.Stats.Threads != 2 {
		t.Fatalf("Stats.Threads = %d, want 2 (the WHOLE board, not the window)", got.Stats.Threads)
	}
}

// TestAnswerLimitKeepsTheTailAndSaysWhatItWithheld: --limit is a tail, so it
// keeps the NEWEST threads, and it reports the count it dropped.
func TestAnswerLimitKeepsTheTailAndSaysWhatItWithheld(t *testing.T) {
	s := New(0)
	s.Apply(qpost(qPaneA, "planner", "first", msg.MintThreadID(qPaneA, 1), 1000))
	s.Apply(qpost(qPaneA, "planner", "second", msg.MintThreadID(qPaneA, 2), 2000))
	s.Apply(qpost(qPaneA, "planner", "third", msg.MintThreadID(qPaneA, 3), 3000))

	got := Answer(s, Query{Limit: 2})

	if len(got.Threads) != 2 {
		t.Fatalf("--limit 2 returned %d threads", len(got.Threads))
	}
	if got.Threads[0].Root.Text != "second" || got.Threads[1].Root.Text != "third" {
		t.Fatalf("--limit kept the head, not the tail: %q, %q",
			got.Threads[0].Root.Text, got.Threads[1].Root.Text)
	}
	if got.Withheld != 1 {
		t.Fatalf("Withheld = %d, want 1", got.Withheld)
	}
}

// TestAnswerWithNoBoundsReturnsEverything pins the zero Query.
func TestAnswerWithNoBoundsReturnsEverything(t *testing.T) {
	s := New(0)
	s.Apply(qpost(qPaneA, "planner", "one", msg.MintThreadID(qPaneA, 1), 1000))
	s.Apply(qpost(qPaneB, "builder", "two", "", 2000))

	got := Answer(s, Query{})
	if len(got.Threads) != 2 || got.Filtered != 0 || got.Withheld != 0 {
		t.Fatalf("zero Query bounded the answer: %d threads, %d filtered, %d withheld",
			len(got.Threads), got.Filtered, got.Withheld)
	}
}

// TestQueryReplyMarshalsWithStableLowerCaseKeys: `rysh board tail --json` is a
// contract a script can be written against, so the wire keys must be the ones
// documented and not a rendering of Go field names.
func TestQueryReplyMarshalsWithStableLowerCaseKeys(t *testing.T) {
	s := New(0)
	s.Apply(qpost(qPaneA, "planner", "one", msg.MintThreadID(qPaneA, 1), 1000))
	s.Register(msg.NewBoardRegister(qPaneA, "planner", 1000))

	data, err := json.Marshal(Answer(s, Query{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, want := range []string{"threads", "roster", "stats", "filtered", "withheld"} {
		if _, ok := generic[want]; !ok {
			t.Errorf("QueryReply has no %q key: %s", want, data)
		}
	}
	threads, _ := generic["threads"].([]any)
	if len(threads) != 1 {
		t.Fatalf("threads did not marshal: %s", data)
	}
	th, _ := threads[0].(map[string]any)
	for _, want := range []string{"key", "root", "replies", "provisional"} {
		if _, ok := th[want]; !ok {
			t.Errorf("Thread has no %q key: %s", want, data)
		}
	}
	roster, _ := generic["roster"].([]any)
	if len(roster) != 1 {
		t.Fatalf("roster did not marshal: %s", data)
	}
	re, _ := roster[0].(map[string]any)
	if _, ok := re["pane_id"]; !ok {
		t.Errorf("RosterEntry has no %q key: %s", "pane_id", data)
	}
}
