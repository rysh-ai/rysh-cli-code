// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 M1 read-state: unread mail is marked in the inbox list, opening a
// message marks it read, and "u" toggles read/unread on the server.

func readStateModel(t *testing.T) (Model, *emailViewState) {
	t.Helper()
	m, ev, _ := emailSearchModel(t)
	ev.list = []msgpkg.EmailSummary{
		{ID: "a1", UID: 11, From: "alice@x.io", Subject: "unread one", Seen: false},
		{ID: "b2", UID: 12, From: "bob@x.io", Subject: "read one", Seen: true},
	}
	return m, ev
}

// TestEmailListMarksUnread: an unread row carries the unread marker, a read
// row does not, including when the unread row is the selected one.
func TestEmailListMarksUnread(t *testing.T) {
	m, ev := readStateModel(t)
	for _, sel := range []int{0, 1} {
		ev.selected = sel
		rows := m.emailListRows(ev, 60, 10)
		if len(rows) < 3 {
			t.Fatalf("rows = %v, want a header and two mails", rows)
		}
		unread, read := ansi.Strip(rows[1]), ansi.Strip(rows[2])
		if !strings.Contains(unread, emailUnreadMark) {
			t.Errorf("selected=%d: unread row %q has no %q marker", sel, unread, emailUnreadMark)
		}
		if strings.Contains(read, emailUnreadMark) {
			t.Errorf("selected=%d: read row %q carries the unread marker", sel, read)
		}
	}
	// The header counts unread mail.
	if h := ansi.Strip(m.emailListRows(ev, 60, 10)[0]); !strings.Contains(h, "1 unread") {
		t.Errorf("list header %q does not count unread mail", h)
	}
}

// TestEmailToggleSeenKey: "u" flips the selected row locally and asks the
// humanoid to store the new flag.
func TestEmailToggleSeenKey(t *testing.T) {
	m, ev := readStateModel(t)
	ev.selected = 0

	_, cmd := m.updateEmailMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	if !ev.list[0].Seen {
		t.Fatal("u on an unread row did not mark it read")
	}
	if cmd == nil {
		t.Fatal("u fired no request: the flag would only change in this terminal, not on the server")
	}
	_, _ = m.updateEmailMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	if ev.list[0].Seen {
		t.Fatal("second u did not mark the row unread again")
	}

	if req := emailSetSeenRequest("", 12, false); req.UID != 12 || req.Seen {
		t.Fatalf("emailSetSeenRequest(\"\", 12, false) = %+v", req)
	}
}

// TestEmailOpenMarksSeen: when an opened message's detail arrives, its list
// row is marked read (the server's non-PEEK read already set \Seen).
func TestEmailOpenMarksSeen(t *testing.T) {
	m, ev := readStateModel(t)
	m.applyEmailEvent(emailEvent{kind: emailEvtDetail, humanoid: "mailbot", detail: &msgpkg.EmailDetail{UID: 11}})
	if !ev.list[0].Seen {
		t.Fatal("opening an unread message left it marked unread in the list")
	}
	if !ev.list[1].Seen {
		t.Fatal("an unrelated row changed state")
	}
}

// TestEmailActionBarNamesToggle: the key is discoverable.
func TestEmailActionBarNamesToggle(t *testing.T) {
	m, ev := readStateModel(t)
	if bar := m.emailActionBar(ev); !strings.Contains(bar, "u read/unread") {
		t.Fatalf("action bar %q does not name the u key", bar)
	}
}
