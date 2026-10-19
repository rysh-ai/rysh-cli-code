// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 (l), E-13 "dock accumulates drafts". The email client's AI-draft dock
// rendered the pane's WHOLE external buffer (email_view.go emailDock →
// pane.ExternalOutput), and nothing ever clears that buffer: it collects the
// echoed prompts (pane_exec.go), every earlier draft (humanoid streamToPane),
// inbound-mail echoes and the "send" turn. So the second draft opened with the
// first one in it. The dock must show the CURRENT draft only: what arrived
// after this draft was asked for, without the echoed prompt lines.

const oldTraffic = "--- New email ---\nFrom: ana@x.io\nSubject: plan\n" +
	"\n\x02Draft a reply to this email.\n" +
	"OLD DRAFT: thanks Ana, Friday works.\n" +
	"\n\x02send\n" +
	"Email sent to ana@x.io\n"

// dockModel is an email pane whose external buffer already holds an earlier
// draft-and-send exchange, with an email open in the reader.
func dockModel(t *testing.T) (Model, *emailViewState, domain.PaneSnapshot) {
	t.Helper()
	m, ev, pane := emailSearchModel(t)
	pane.ExternalOutput = oldTraffic
	m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes[0] = pane
	m.snapshot.ActiveTabID = "t1"
	ev.detail = &msgpkg.EmailDetail{UID: 7, From: "bob@x.io", Subject: "budget", Body: "numbers?"}
	return m, ev, pane
}

func dockText(m Model, pane domain.PaneSnapshot, ev *emailViewState) string {
	return ansi.Strip(m.emailDock(pane, ev, 70, 12))
}

func TestEmailDockShowsOnlyTheCurrentDraft(t *testing.T) {
	m, ev, pane := dockModel(t)
	m.updateEmailMode(key("r")) // ask for a draft of a reply to Bob

	// Nothing new yet: the dock waits, it does not show the old draft.
	if got := dockText(m, pane, ev); strings.Contains(got, "OLD DRAFT") || !strings.Contains(got, "drafting") {
		t.Fatalf("right after r the dock shows:\n%s\nwant only the drafting placeholder", got)
	}

	// The new draft streams in after the echoed prompt.
	pane.ExternalOutput += "\n\x02Draft a reply to this email.\nNEW DRAFT: Bob, the numbers are attached.\n"
	got := dockText(m, pane, ev)
	if !strings.Contains(got, "NEW DRAFT") {
		t.Fatalf("dock lacks the current draft:\n%s", got)
	}
	for _, stale := range []string{"OLD DRAFT", "Email sent to ana", "Subject: plan", "Draft a reply to this email."} {
		if strings.Contains(got, stale) {
			t.Errorf("dock still shows %q from earlier traffic or the prompt echo:\n%s", stale, got)
		}
	}
}

// TestEmailDockSurvivesBufferTrim: the TUI caps the external buffer by
// trimming its head, and a snapshot can replace it wholesale. Either way the
// dock still shows the current draft only.
func TestEmailDockSurvivesBufferTrim(t *testing.T) {
	m, ev, pane := dockModel(t)
	m.updateEmailMode(key("r"))
	full := pane.ExternalOutput + "\n\x02Draft a reply to this email.\nNEW DRAFT: Bob, the numbers are attached.\n"

	// Head trimmed, but the end of the pre-draft traffic is still there.
	pane.ExternalOutput = full[20:]
	if got := dockText(m, pane, ev); !strings.Contains(got, "NEW DRAFT") || strings.Contains(got, "OLD DRAFT") {
		t.Errorf("after a head trim the dock shows:\n%s", got)
	}

	// Replaced by a snapshot that no longer contains the pre-draft traffic:
	// fall back to what follows the last draft prompt.
	pane.ExternalOutput = "OLD DRAFT tail\n\x02Draft a reply to this email.\nNEW DRAFT: Bob, the numbers are attached.\n"
	if got := dockText(m, pane, ev); !strings.Contains(got, "NEW DRAFT") || strings.Contains(got, "OLD DRAFT") {
		t.Errorf("after a snapshot replace the dock shows:\n%s", got)
	}
}
