// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 M1 threading. MessageID / InReplyTo have been on EmailSummary since the
// desktop client needed them, but the TUI list was flat (design 020 §2.2: "No
// thread view — MessageID/InReplyTo are carried but unused for grouping").

// mail builds a summary; the server lists newest first.
func mail(uid int, id, inReplyTo, subject string) msgpkg.EmailSummary {
	return msgpkg.EmailSummary{UID: uid, ID: subject[:1], MessageID: id, InReplyTo: inReplyTo, From: "x@y.z", Subject: subject, Seen: true}
}

func uids(list []msgpkg.EmailSummary) []int {
	out := make([]int, len(list))
	for i, e := range list {
		out[i] = e.UID
	}
	return out
}

func eqInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestThreadEmailsGroupsReplies: replies follow their parent in conversation
// (oldest-first) order, threads are ordered by their newest message, and a
// reply whose parent is not in the listing stands as its own thread.
func TestThreadEmailsGroupsReplies(t *testing.T) {
	newestFirst := []msgpkg.EmailSummary{
		mail(5, "<c>", "<a>", "C re A"),
		mail(4, "<d>", "", "D alone"),
		mail(3, "<b>", "<a>", "B re A"),
		mail(2, "<a>", "", "A root"),
		mail(1, "<e>", "<missing>", "E orphan"),
	}
	got, depth := threadEmails(newestFirst)
	if want := []int{2, 3, 5, 4, 1}; !eqInts(uids(got), want) {
		t.Fatalf("thread order UIDs = %v, want %v", uids(got), want)
	}
	if want := []int{0, 1, 1, 0, 0}; !eqInts(depth, want) {
		t.Fatalf("depths = %v, want %v", depth, want)
	}
}

func TestThreadEmailsNestsChains(t *testing.T) {
	got, depth := threadEmails([]msgpkg.EmailSummary{
		mail(3, "<c>", "<b>", "C re B"),
		mail(2, "<b>", "<a>", "B re A"),
		mail(1, "<a>", "", "A root"),
	})
	if !eqInts(uids(got), []int{1, 2, 3}) || !eqInts(depth, []int{0, 1, 2}) {
		t.Fatalf("chain = %v depths %v, want [1 2 3] depths [0 1 2]", uids(got), depth)
	}
}

// TestThreadEmailsSurvivesCycles: malformed headers that reply to each other
// must not hang or drop mail — every message appears exactly once.
func TestThreadEmailsSurvivesCycles(t *testing.T) {
	got, _ := threadEmails([]msgpkg.EmailSummary{
		mail(2, "<a>", "<b>", "A re B"),
		mail(1, "<b>", "<a>", "B re A"),
		mail(3, "<s>", "<s>", "S re self"),
	})
	seen := map[int]int{}
	for _, e := range got {
		seen[e.UID]++
	}
	if len(got) != 3 || seen[1] != 1 || seen[2] != 1 || seen[3] != 1 {
		t.Fatalf("cyclic listing threaded to %v, want each of 1,2,3 exactly once", uids(got))
	}
}

func threadModel(t *testing.T) (Model, *emailViewState) {
	t.Helper()
	m, ev, _ := emailSearchModel(t)
	m.applyEmailEvent(emailEvent{kind: emailEvtList, humanoid: "mailbot", emails: []msgpkg.EmailSummary{
		mail(5, "<c>", "<a>", "C re A"),
		mail(4, "<d>", "", "D alone"),
		mail(3, "<b>", "<a>", "B re A"),
		mail(2, "<a>", "", "A root"),
	}})
	return m, ev
}

// TestEmailListShowsThreads: the list event threads the listing, replies are
// drawn indented under their root and the root counts its thread.
func TestEmailListShowsThreads(t *testing.T) {
	m, ev := threadModel(t)
	if !eqInts(uids(ev.list), []int{2, 3, 5, 4}) {
		t.Fatalf("listing not threaded: %v", uids(ev.list))
	}
	rows := m.emailListRows(ev, 60, 10)
	root, reply := ansi.Strip(rows[1]), ansi.Strip(rows[2])
	if !strings.Contains(root, "A root") || !strings.Contains(root, "(3)") {
		t.Errorf("root row %q does not count its 3-message thread", root)
	}
	if !strings.Contains(reply, "↳") || !strings.Contains(reply, "B re A") {
		t.Errorf("reply row %q is not drawn as a reply", reply)
	}
	if lone := ansi.Strip(rows[4]); strings.Contains(lone, "↳") || strings.Contains(lone, "(1)") {
		t.Errorf("a single-message thread %q is decorated", lone)
	}
}

// TestEmailThreadToggle: t switches between the threaded and the server's
// flat newest-first order, keeping per-mail state such as read flags.
func TestEmailThreadToggle(t *testing.T) {
	m, ev := threadModel(t)
	ev.list[1].Seen = false // a mail flipped unread while threaded (UID 3)

	m.updateEmailMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	if !eqInts(uids(ev.list), []int{5, 4, 3, 2}) {
		t.Fatalf("t did not restore the flat newest-first order: %v", uids(ev.list))
	}
	if ev.list[2].Seen {
		t.Fatal("toggling threads lost the read flag of UID 3")
	}
	if rows := m.emailListRows(ev, 60, 10); strings.Contains(ansi.Strip(strings.Join(rows, "\n")), "↳") {
		t.Fatal("flat view still draws reply markers")
	}

	m.updateEmailMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	if !eqInts(uids(ev.list), []int{2, 3, 5, 4}) {
		t.Fatalf("second t did not re-thread: %v", uids(ev.list))
	}
	if bar := m.emailActionBar(ev); !strings.Contains(bar, "t ") {
		t.Errorf("action bar %q does not name the t key", bar)
	}
}
