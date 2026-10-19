// SPDX-License-Identifier: Apache-2.0

package board

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The item model is shared by every way out of the board, so these pin what
// all of them inherit: one item per message, exact worker matching, the last
// N, unwrapped bodies, and refusals that never look like an empty board.

const (
	pane314 = "31431431-aaaa-bbbb-cccc-dddddddddddd"
	pane31  = "31313131-aaaa-bbbb-cccc-dddddddddddd"
	paneMgr = "0a0a0a0a-aaaa-bbbb-cccc-dddddddddddd"
)

func itemRoot(pane, persona, text string, n int, ts int64) *msg.MsgBoardPost {
	p := msg.NewBoardPost(pane, persona, msg.BoardKindMilestone, text, ts)
	p.ThreadID = msg.MintThreadID(pane, n)
	return p
}

func itemReply(pane, persona, text, thread string, ts int64) *msg.MsgBoardPost {
	p := msg.NewBoardPost(pane, persona, msg.BoardKindReply, text, ts)
	p.ThreadID = thread
	return p
}

// fleet: issue-314 opens thread 1 (t=1s); issue-31 opens its thread (t=2s);
// the manager replies in 314's thread LATER (t=3s); 314 posts again (t=4s).
// Arrival order by thread differs from time order on purpose.
func fleet() *Store {
	s := New(0)
	s.Apply(itemRoot(pane314, "issue-314", "STATUS: codec wired", 1, 1_000))
	s.Apply(itemRoot(pane31, "issue-31", "BLOCKED: waiting on CI", 1, 2_000))
	s.Apply(itemReply(paneMgr, "mgr", "ack, ship it", msg.MintThreadID(pane314, 1), 3_000))
	s.Apply(itemRoot(pane314, "issue-314", "PR https://github.com/o/r/pull/9", 2, 4_000))
	return s
}

func reply(s *Store) *QueryReply { r := Answer(s, Query{}); return &r }

func texts(items []Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Post.Text)
	}
	return out
}

func TestFlattenOrdersByTimeAcrossThreadsAndCountsReplies(t *testing.T) {
	items := Flatten(reply(fleet()))
	want := []string{"STATUS: codec wired", "BLOCKED: waiting on CI", "ack, ship it", "PR https://github.com/o/r/pull/9"}
	if got := texts(items); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("items = %q, want %q (by ts, replies separate)", got, want)
	}
	if !items[2].Reply || items[2].ThreadKey != msg.MintThreadID(pane314, 1) {
		t.Fatalf("the reply lost its identity: %+v", items[2])
	}
}

func TestSelect(t *testing.T) {
	items := Flatten(reply(fleet()))

	all, m, total := Select(items, "", 0)
	if len(all) != 4 || m != 4 || total != 4 {
		t.Fatalf("all: %d picked, %d matched, %d total", len(all), m, total)
	}
	last2, m, _ := Select(items, "", 2)
	if got := texts(last2); len(got) != 2 || got[1] != "PR https://github.com/o/r/pull/9" || got[0] != "ack, ship it" || m != 4 {
		t.Fatalf("last 2 = %q (matched %d)", got, m)
	}
	big, m, _ := Select(items, "", 99)
	if len(big) != 4 || m != 4 {
		t.Fatalf("N larger than the count: %d", len(big))
	}

	// issue-31 must not match issue-314.
	w31, m, total := Select(items, "issue-31", 0)
	if got := texts(w31); len(got) != 1 || got[0] != "BLOCKED: waiting on CI" || m != 1 || total != 4 {
		t.Fatalf("issue-31 = %q (%d of %d)", got, m, total)
	}
	w314, m, _ := Select(items, "issue-314", 1)
	if got := texts(w314); len(got) != 1 || got[0] != "PR https://github.com/o/r/pull/9" || m != 2 {
		t.Fatalf("issue-314 last 1 = %q of %d", got, m)
	}
	if none, m, _ := Select(items, "issue-3", 0); len(none) != 0 || m != 0 {
		t.Fatal("a persona prefix matched")
	}

	// A pane id whole, or by a prefix of at least 8.
	for _, id := range []string{pane31, pane31[:8], "pane-" + pane31[:8]} {
		if got, _, _ := Select(items, id, 0); len(got) != 1 {
			t.Fatalf("worker %q picked %d", id, len(got))
		}
	}
	if got, _, _ := Select(items, pane31[:7], 0); len(got) != 0 {
		t.Fatal("a 7-character pane-id prefix matched")
	}
}

func TestParseItemArgs(t *testing.T) {
	same := func(a, b []string, output bool) {
		t.Helper()
		x, err1 := ParseItemArgs(a, output)
		y, err2 := ParseItemArgs(b, output)
		if err1 != nil || err2 != nil || x != y {
			t.Fatalf("%q = %+v (%v); %q = %+v (%v)", a, x, err1, b, y, err2)
		}
	}
	same([]string{"5", "pdf"}, []string{"pdf", "5"}, true)
	same([]string{"issue-314", "3"}, []string{"3", "issue-314"}, false)
	same([]string{"issue-314", "3", "pdf"}, []string{"pdf", "3", "issue-314"}, true)

	a, err := ParseItemArgs([]string{"issue-314", "3", "pdf", "--no-open", "--out", "/x", "--board=etl-fix"}, true)
	if err != nil || a != (ItemArgs{Worker: "issue-314", N: 3, Format: FormatPDF, NoOpen: true, Out: "/x", Board: "etl-fix"}) {
		t.Fatalf("output args = %+v, %v", a, err)
	}
	if a, err := ParseItemArgs(nil, false); err != nil || a != (ItemArgs{}) {
		t.Fatalf("copy with no args = %+v, %v; want all messages", a, err)
	}
	// For copy, html is just a worker name.
	if a, _ := ParseItemArgs([]string{"html"}, false); a.Worker != "html" {
		t.Fatalf("copy html = %+v", a)
	}

	if _, err := ParseItemArgs([]string{"issue-314"}, true); !errors.Is(err, ErrNeedsFormat) {
		t.Fatalf("missing format = %v", err)
	}
	if _, err := ParseItemArgs([]string{"issue-314", "docx"}, true); err == nil || !strings.Contains(err.Error(), "unknown format") {
		t.Fatalf("unknown format = %v", err)
	}
	for _, bad := range [][]string{{"a", "b"}, {"3", "4"}, {"--agnet", "x"}, {"--no-open"}, {"--out", "/x"}, {"--board"}} {
		if _, err := ParseItemArgs(bad, false); err == nil {
			t.Errorf("copy %q accepted", bad)
		}
	}
	if _, err := ParseItemArgs([]string{"html", "pdf", "3"}, true); err == nil {
		t.Error("two formats accepted")
	}
}

// TestFormatItemsIsUnwrappedAndByteIdentical: a multi-line body with a
// 200-character URL comes out exactly as posted, under a full date-time header.
func TestFormatItemsIsUnwrappedAndByteIdentical(t *testing.T) {
	url := ("https://ci.example.com/runs/" + strings.Repeat("a1b2c3d4", 30))[:200]
	body := "STATUS: green\nRUN: " + url + "\n\tsha 0123456789abcdef0123456789abcdef01234567\nDEPENDS ON: #312"
	s := New(0)
	s.Apply(itemRoot(pane314, "issue-314", body, 1, 1_000))
	s.Apply(itemRoot(pane31, "issue-31", "second", 1, 2_000))

	res, err := BuildCopy(reply(s), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := "● issue-314  [milestone]  " + time.UnixMilli(1_000).Format("2006-01-02 15:04:05") + "\n" + body +
		"\n\n● issue-31  [milestone]  " + time.UnixMilli(2_000).Format("2006-01-02 15:04:05") + "\nsecond"
	if res.Text != want {
		t.Fatalf("copy text =\n%q\nwant\n%q", res.Text, want)
	}
	if got := res.Summary(); got != "[board] copied 2 of 2 messages to the clipboard" {
		t.Fatalf("summary = %q", got)
	}
}

func TestBuildCopyCountsAndRefusals(t *testing.T) {
	res, err := BuildCopy(reply(fleet()), "issue-314", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Summary(); got != "[board] copied 1 of 2 messages from issue-314 to the clipboard" {
		t.Fatalf("summary = %q", got)
	}

	_, err = BuildCopy(reply(fleet()), "nobody", 0)
	var none ErrNoMessages
	if !errors.As(err, &none) || err.Error() != "[board] no messages from nobody" {
		t.Fatalf("no match = %v", err)
	}
	if _, err := BuildCopy(reply(New(0)), "", 0); err == nil || err.Error() != "[board] no messages" {
		t.Fatalf("empty board = %v", err)
	}
	// Recorder unavailable: refused, not an empty copy.
	if res, err := BuildCopy(nil, "", 0); !errors.Is(err, ErrNoRecorder) || res.Text != "" {
		t.Fatalf("nil answer = %q, %v", res.Text, err)
	}
	if _, err := Ask(nil, "", Query{}, time.Second); !errors.Is(err, ErrNoRecorder) {
		t.Fatalf("Ask with no connection = %v", err)
	}
}

func TestBuildCopyRefusesOverTheClipboardCap(t *testing.T) {
	s := New(0)
	s.Apply(itemRoot(pane314, "issue-314", strings.Repeat("x", CopyMaxBytes+1), 1, 1_000))
	_, err := BuildCopy(reply(s), "", 0)
	if err == nil || !strings.Contains(err.Error(), "with N") || !strings.Contains(err.Error(), "worker") {
		t.Fatalf("oversized copy = %v; want a refusal suggesting N or a worker", err)
	}
}

func TestEscapeSequencesAreStrippedFromEveryOutput(t *testing.T) {
	s := New(0)
	s.Apply(itemRoot(pane314, "issue-314",
		"a\x1b[2Jb\x1b]0;pwned\x07c\x1b[1;31md\u009b31me\x1bPdcs\x1b\\f\r\ng\x00h", 1, 1_000))
	bad := []string{"\x1b", "\u009b", "\x07", "\x00", "[2J", "[31m", "pwned", "dcs"}

	res, err := BuildCopy(reply(s), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	page, err := RenderHTML(HTMLPage{Board: "b", Picked: Flatten(reply(s)), Matched: 1, Total: 1, Now: time.Unix(0, 0)})
	if err != nil {
		t.Fatal(err)
	}
	js, _ := json.Marshal(ItemsReply(reply(s), Flatten(reply(s))))
	for name, out := range map[string]string{"copy": res.Text, "html": string(page), "json": string(js)} {
		for _, b := range bad {
			if strings.Contains(out, b) {
				t.Fatalf("%s: %q survived", name, b)
			}
		}
	}
	if !strings.HasSuffix(res.Text, "\nabcdef\ngh") {
		t.Fatalf("sanitising damaged the text: %q", res.Text)
	}
	if got := s.Threads()[0].Root.Text; !strings.Contains(got, "\x1b[2J") {
		t.Fatal("Flatten mutated the live store")
	}
}

func TestItemsReplyKeepsTheQueryReplyShape(t *testing.T) {
	r := reply(fleet())
	picked, _, _ := Select(Flatten(r), "mgr", 0)
	got := ItemsReply(r, picked)
	if len(got.Threads) != 1 || got.Threads[0].Root != nil || len(got.Threads[0].Replies) != 1 ||
		got.Threads[0].Key != msg.MintThreadID(pane314, 1) || got.Stats.Posts != 4 {
		t.Fatalf("ItemsReply = %+v", got)
	}
}
