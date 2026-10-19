// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 M1 folder rail. The rail used to be decorative — INBOX live, Drafts/Sent
// dim "(soon)" text, and no branch handled focusCol == emailColFolders. Now it
// lists the account's real folders and selecting one lists that folder.

var railFolders = []msgpkg.EmailFolder{
	{Name: "INBOX", Label: "INBOX", Role: "inbox"},
	{Name: "[Gmail]/Sent Mail", Label: "Sent", Role: "sent"},
	{Name: "Work", Label: "Work"},
}

func railModel(t *testing.T) (Model, *emailViewState) {
	t.Helper()
	m, ev, _ := emailSearchModel(t)
	m.applyEmailEvent(emailEvent{kind: emailEvtFolders, humanoid: "mailbot", folders: railFolders})
	return m, ev
}

func TestEmailFoldersEventFillsRail(t *testing.T) {
	m, ev := railModel(t)
	if len(ev.folders) != 3 {
		t.Fatalf("folders = %+v, want the 3 from the event", ev.folders)
	}
	rows := m.emailFolderRows(ev, 12)
	joined := ansi.Strip(strings.Join(rows, "\n"))
	for _, want := range []string{"INBOX", "Sent", "Work"} {
		if !strings.Contains(joined, want) {
			t.Errorf("rail %q does not show folder %q", joined, want)
		}
	}
	if strings.Contains(joined, "(soon)") {
		t.Errorf("rail still shows the decorative \"(soon)\" placeholder: %q", joined)
	}
}

// TestEmailSelectFolderListsIt is the crux: j/enter in the folder column
// switches the listing to that folder.
func TestEmailSelectFolderListsIt(t *testing.T) {
	m, ev := railModel(t)
	ev.list = []msgpkg.EmailSummary{{UID: 1, Subject: "inbox mail"}}
	ev.focusCol = emailColFolders

	m.updateEmailMode(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	_, cmd := m.updateEmailMode(tea.KeyMsg{Type: tea.KeyEnter})

	if ev.folder != "[Gmail]/Sent Mail" {
		t.Fatalf("folder = %q, want \"[Gmail]/Sent Mail\" after j + enter on the rail", ev.folder)
	}
	if cmd == nil {
		t.Fatal("selecting a folder fired no list request")
	}
	if got := emailListRequest(ev.folder, ev.search).Folder; got != "[Gmail]/Sent Mail" {
		t.Fatalf("list request folder = %q, want the selected folder", got)
	}
	if len(ev.list) != 0 || ev.focusCol != emailColList {
		t.Fatalf("after switching: list=%v focus=%d, want the old folder's mail cleared and focus on the list", ev.list, ev.focusCol)
	}
	if h := ansi.Strip(m.emailListRows(ev, 40, 5)[0]); !strings.Contains(h, "Sent") {
		t.Errorf("list header %q does not name the selected folder", h)
	}
}

// TestEmailListReplyForOtherFolderIgnored: a reply for a folder this pane is
// no longer showing (slow IMAP, or a second pane on the same humanoid) must
// not overwrite the listing.
func TestEmailListReplyForOtherFolderIgnored(t *testing.T) {
	m, ev := railModel(t)
	ev.folder = "Work"
	m.applyEmailEvent(emailEvent{kind: emailEvtList, humanoid: "mailbot", folder: "", emails: []msgpkg.EmailSummary{{UID: 9, Subject: "inbox"}}})
	if len(ev.list) != 0 {
		t.Fatalf("an INBOX reply overwrote the Work listing: %+v", ev.list)
	}
	m.applyEmailEvent(emailEvent{kind: emailEvtList, humanoid: "mailbot", folder: "Work", emails: []msgpkg.EmailSummary{{UID: 3, Subject: "work"}}})
	if len(ev.list) != 1 || ev.list[0].UID != 3 {
		t.Fatalf("the Work reply was not applied: %+v", ev.list)
	}
}

// TestEmailRequestsCarryFolder: UIDs are per mailbox, so reading or flagging a
// mail in Sent must name Sent.
func TestEmailRequestsCarryFolder(t *testing.T) {
	if r := emailReadRequest("Work", 3); r.Folder != "Work" || r.UID != 3 {
		t.Fatalf("emailReadRequest = %+v", r)
	}
	if r := emailSetSeenRequest("Work", 3, true); r.Folder != "Work" || r.UID != 3 || !r.Seen {
		t.Fatalf("emailSetSeenRequest = %+v", r)
	}
}

// TestEmailActivationRequestsFolders: the first activation also asks for the
// folder list, so the rail is populated when the pane shows.
func TestEmailActivationRequestsFolders(t *testing.T) {
	pane := domain.PaneSnapshot{ID: "p-mail", RegisteredHumanoid: "mailbot", EnabledModes: []string{"shell", "email"}}
	m := activateModel(pane)
	if cmd := m.applyActivateMode(activateModeItem{paneID: pane.ID, mode: "email"}); cmd == nil {
		t.Fatal("activation fired nothing")
	}
	if !m.emailViews[pane.ID].foldersRequested {
		t.Fatal("activation did not request the folder list: the rail would stay INBOX-only")
	}
}
