// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 M1 — email search invocation.
//
// The backend half of search has been done for a long time: the wire carries a
// SUBJECT substring filter (msg.MsgHumanoidEmailList.Search), the humanoid
// honours it (actors/humanoid.go handleEmailListQuery → adapter.ListEmails), and
// the agent-facing tool already drives it (tools/email_list.go). Only an *agent*
// could search the inbox; a *human sitting at the TUI had no way in* — "Search"
// appeared zero times in email_view.go.
//
// These tests pin the missing front half: a keybinding, a query line, and the
// typed query reaching the same ListEmails path the tool uses.

// emailSearchModel builds a Model in email mode with one pane and initialised
// per-pane email state. emailViewFor writes into m.emailViews, which paneModel
// leaves nil, so the map has to exist before any key is delivered.
func emailSearchModel(t *testing.T) (Model, *emailViewState, domain.PaneSnapshot) {
	t.Helper()
	pane := domain.PaneSnapshot{ID: "p-mail", RegisteredHumanoid: "mailbot"}
	m := paneModel(pane)
	m.emailViews = map[string]*emailViewState{
		pane.ID: {humanoid: "mailbot", focusCol: emailColList},
	}
	return m, m.emailViews[pane.ID], pane
}

// typeRunes delivers each rune to the email key handler, as a user typing would.
func typeRunes(m Model, s string) {
	for _, r := range s {
		m.updateEmailMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// TestEmailSearchKeyOpensQueryLine — "/" is the search keybinding. Before this
// change "/" fell through updateEmailMode's switch and did nothing at all.
func TestEmailSearchKeyOpensQueryLine(t *testing.T) {
	m, ev, _ := emailSearchModel(t)

	m.updateEmailMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})

	if !ev.searching {
		t.Fatalf("after \"/\": searching = false — the email view has no way to invoke search, so a human still cannot search the inbox")
	}
}

// TestEmailSearchTypedQueryReachesListRequest is the crux: whatever the user
// types must land in the Search field of the request that goes to the humanoid,
// which is the same field tools/email_list.go passes to ListEmails.
func TestEmailSearchTypedQueryReachesListRequest(t *testing.T) {
	m, ev, _ := emailSearchModel(t)

	m.updateEmailMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	typeRunes(m, "invoice")
	_, cmd := m.updateEmailMode(tea.KeyMsg{Type: tea.KeyEnter})

	if ev.searching {
		t.Errorf("still capturing input after enter: the query line never closed")
	}
	if ev.search != "invoice" {
		t.Fatalf("active filter = %q, want %q — the typed query was not committed", ev.search, "invoice")
	}
	if cmd == nil {
		t.Errorf("enter returned no command: the query was accepted but no inbox fetch was issued, so the list never changes")
	}
	if got := emailListRequest(ev.folder, ev.search).Search; got != "invoice" {
		t.Errorf("MsgHumanoidEmailList.Search = %q, want %q — the query never reaches the ListEmails path", got, "invoice")
	}
}

// TestEmailSearchSpaceAndBackspaceEdit — the query line is a text input, so a
// multi-word query and a correction both have to work. tea.KeySpace is
// delivered separately from KeyRunes, exactly as in updateEmailCompose.
func TestEmailSearchSpaceAndBackspaceEdit(t *testing.T) {
	m, ev, _ := emailSearchModel(t)

	m.updateEmailMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	typeRunes(m, "past")
	m.updateEmailMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m.updateEmailMode(tea.KeyMsg{Type: tea.KeyBackspace})
	m.updateEmailMode(tea.KeyMsg{Type: tea.KeySpace})
	typeRunes(m, "due")
	m.updateEmailMode(tea.KeyMsg{Type: tea.KeyEnter})

	if ev.search != "past due" {
		t.Errorf("active filter = %q, want %q (space and backspace must edit the query)", ev.search, "past due")
	}
}

// TestEmailSearchEscAbandonsQuery — esc while typing cancels the edit and must
// not disturb the filter that was already active.
func TestEmailSearchEscAbandonsQuery(t *testing.T) {
	m, ev, _ := emailSearchModel(t)
	ev.search = "invoice"

	m.updateEmailMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	typeRunes(m, "zzz")
	m.updateEmailMode(tea.KeyMsg{Type: tea.KeyEsc})

	if ev.searching {
		t.Errorf("esc left the query line open")
	}
	if ev.search != "invoice" {
		t.Errorf("active filter = %q after cancelling an edit, want it untouched at %q", ev.search, "invoice")
	}
}

// TestEmailSearchEscClearsActiveFilter — with a filter applied and the query
// line closed, esc backs out of the filter before it backs out of the email
// client. Without this a user who searches has no way back to the full inbox
// except by quitting the view.
func TestEmailSearchEscClearsActiveFilter(t *testing.T) {
	m, ev, _ := emailSearchModel(t)
	ev.search = "invoice"

	_, cmd := m.updateEmailMode(tea.KeyMsg{Type: tea.KeyEsc})

	if ev.search != "" {
		t.Fatalf("active filter = %q after esc, want it cleared", ev.search)
	}
	if cmd == nil {
		t.Errorf("clearing the filter issued no refetch: the list would still show filtered results")
	}
}

// TestEmailSearchFilterSurvivesRefresh — a refresh (g, or a background
// MsgHumanoidEmailChanged) must re-send the active filter. If it does not, new
// mail silently replaces a filtered list with an unfiltered one.
func TestEmailSearchFilterSurvivesRefresh(t *testing.T) {
	m, ev, _ := emailSearchModel(t)
	ev.search = "invoice"

	if _, cmd := m.updateEmailMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}}); cmd == nil {
		t.Fatalf("g issued no refresh command")
	}
	if ev.search != "invoice" {
		t.Errorf("refresh dropped the active filter (now %q)", ev.search)
	}
	// The changed-push path folds through applyEmailEvent, which must carry the
	// same filter rather than resetting to an unfiltered fetch.
	if cmd := m.applyEmailEvent(emailEvent{kind: emailEvtChanged, humanoid: "mailbot"}); cmd == nil {
		t.Errorf("MsgHumanoidEmailChanged issued no refresh command")
	}
	if got := emailListRequest(ev.folder, ev.search).Search; got != "invoice" {
		t.Errorf("refresh request Search = %q, want %q", got, "invoice")
	}
}

// TestEmailSearchIsDiscoverableAndEchoed — the feature has to be reachable by a
// human, not merely present in the key switch. Two things must be on screen:
// the "/" hint when idle, and the query itself while it is being typed.
func TestEmailSearchIsDiscoverableAndEchoed(t *testing.T) {
	m, ev, _ := emailSearchModel(t)

	if bar := m.emailActionBar(ev); !strings.Contains(bar, "/ search") {
		t.Errorf("action bar = %q, missing the \"/ search\" hint — nothing tells a user the key exists", bar)
	}

	m.updateEmailMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	typeRunes(m, "inv")
	bar := m.emailActionBar(ev)
	if !strings.Contains(bar, "inv") {
		t.Errorf("action bar = %q while typing, does not echo the query — the user types blind", bar)
	}
}

// TestEmailSearchFilterIsVisibleInList — a filtered inbox that renders exactly
// like a full one reads as "my mail disappeared". The header must say so, and
// an empty result must offer the way out.
func TestEmailSearchFilterIsVisibleInList(t *testing.T) {
	m, ev, _ := emailSearchModel(t)
	ev.search = "invoice"

	rows := m.emailListRows(ev, 40, 6)
	if len(rows) == 0 || !strings.Contains(rows[0], "invoice") {
		t.Errorf("list header = %q, does not name the active filter", rows)
	}
	if len(rows) < 2 || !strings.Contains(rows[1], "esc") {
		t.Errorf("empty filtered list = %q, does not tell the user how to clear the filter", rows)
	}
}

// TestEmailSearchDoesNotHijackCompose — "/" is a legal character inside a reply
// body. The compose sub-view must keep capturing it verbatim.
func TestEmailSearchDoesNotHijackCompose(t *testing.T) {
	m, ev, _ := emailSearchModel(t)
	ev.detail = &msgpkg.EmailDetail{UID: 1, From: "a@x.io", Subject: "s"}
	m.updateEmailMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})

	m.updateEmailMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})

	if ev.searching {
		t.Errorf("\"/\" opened the search line while composing a reply")
	}
	if got := ev.editor.Body(); got != "/" {
		t.Errorf("compose body = %q, want %q — \"/\" was stolen from the reply text", got, "/")
	}
}
