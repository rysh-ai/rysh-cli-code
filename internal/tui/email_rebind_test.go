// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 (o1), design 020 §5.7: re-registering a pane to a DIFFERENT humanoid
// kept the old binding. emailViewFor only filled ev.humanoid while it was
// empty, so the pane kept listing, reading and sending as the first
// humanoid's mailbox. A rebind must switch to the new humanoid with a fresh
// view, and the new mailbox must be fetched.
func TestEmailViewRebindsToANewHumanoid(t *testing.T) {
	pane := domain.PaneSnapshot{ID: "p-mail", RegisteredHumanoid: "alice-mail", EnabledModes: []string{"shell", "email"}}
	m := activateModel(pane)

	ev := m.emailViewFor(pane)
	ev.list = []msgpkg.EmailSummary{{UID: 1, Subject: "alice's mail"}}
	ev.detail = &msgpkg.EmailDetail{UID: 1}
	ev.folder = "Work"
	ev.requested = true

	// The pane is re-registered to another humanoid.
	pane.RegisteredHumanoid = "bob-mail"
	setPane(&m, pane)
	ev = m.emailViewFor(pane)
	if ev.humanoid != "bob-mail" {
		t.Fatalf("after re-register the email view still belongs to %q, want bob-mail", ev.humanoid)
	}
	if len(ev.list) != 0 || ev.detail != nil || ev.folder != "" {
		t.Fatalf("the new humanoid's view still shows alice's mailbox: list=%v detail=%v folder=%q", ev.list, ev.detail, ev.folder)
	}
	// Activating the rebound pane fetches bob's inbox.
	if cmd := m.applyActivateMode(activateModeItem{paneID: pane.ID, mode: "email"}); cmd == nil {
		t.Fatal("rebound pane fired no inbox fetch for the new humanoid")
	}

	// An event for the OLD humanoid no longer lands on this pane.
	m.applyEmailEvent(emailEvent{kind: emailEvtList, humanoid: "alice-mail", emails: []msgpkg.EmailSummary{{UID: 9}}})
	if len(m.emailViews[pane.ID].list) != 0 {
		t.Fatal("alice's list reply landed on the pane now bound to bob")
	}
}
