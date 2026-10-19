// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
)

// E17 (k1): a lifecycle command addressed to a meeting id that exists in
// ANOTHER workspace (or session) must not act on it. The meeting registry is
// per workspace — each WorkspaceActor builds its own meeting.Manager, and each
// session is its own daemon process — so a foreign id resolves to "no
// meeting", exactly like an unknown one. This pins that: the desktop app's
// meeting_command (web) and the TUI panel both rely on it.
func TestMeetingCommandForeignIDIsUnknown(t *testing.T) {
	owner, other := &WorkspaceActor{}, &WorkspaceActor{}
	if owner.meetingManager() == other.meetingManager() {
		t.Fatal("two workspaces share one meeting registry")
	}

	reg, ok := meetingRestoreCommand("pane-1", map[string]string{
		paneMetaMeetingID:   "m-aaaaaa",
		paneMetaMeetingKind: meeting.KindWeb,
		paneMetaMeetingURL:  "https://meet.google.com/abc-defg-hij",
	})
	if !ok {
		t.Fatal("setup: could not build the register command")
	}
	if r := owner.meetingManager().Do(reg, meeting.Credentials{}, nil); r.Error != "" {
		t.Fatalf("setup: register in the owning workspace: %s", r.Error)
	}

	for _, verb := range []string{"pending", "approve", "reject", "speak", "pause", "resume", "cancel"} {
		r := other.meetingManager().Do(meeting.Command{Action: verb, ID: "m-aaaaaa", TurnID: "t1"}, meeting.Credentials{}, nil)
		if r.Error == "" || !strings.Contains(r.Error, "no meeting m-aaaaaa") {
			t.Errorf("%s on a foreign meeting id: reply %+v, want \"no meeting m-aaaaaa\"", verb, r)
		}
	}
	// The owner still has it.
	if r := owner.meetingManager().Do(meeting.Command{Action: "pending", ID: "m-aaaaaa"}, meeting.Credentials{}, nil); strings.Contains(r.Error, "no meeting") {
		t.Fatalf("the owning workspace lost its meeting: %+v", r)
	}
}
