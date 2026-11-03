// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/fleetboard"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// `##fleet board` (design 030 W4): the fleet board's rows, as text.
func TestW4FleetBoardCommand(t *testing.T) {
	w, nc := newBoardTestWorkspace(t)
	w.nc = nc

	var out strings.Builder
	if err := w.handleFleetCommand(&out, "pA", []string{"board"}); err == nil ||
		!strings.Contains(out.String(), "fleet board") {
		t.Fatalf("no fleet board actor: err=%v out=%q; want a refusal naming the fleet board", err, out.String())
	}

	now := time.Now().UnixMilli()
	sub, err := nc.Subscribe(msg.FleetBoardQuerySubject(), func(m *nats.Msg) {
		data, _ := json.Marshal(fleetboard.MakeReply([]fleetboard.Row{{Fleet: "epic-a", State: "up", Members: 2,
			LastActivity: now, LastStatus: &fleetboard.Post{Kind: "milestone", Text: "M1"}}}, now))
		_ = m.Respond(data)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()

	out.Reset()
	if err := w.handleFleetCommand(&out, "pA", []string{"board"}); err != nil {
		t.Fatalf("##fleet board: %v (%s)", err, out.String())
	}
	if !strings.Contains(out.String(), "epic-a") || !strings.Contains(out.String(), "MILESTONE: M1") {
		t.Fatalf("##fleet board output:\n%s", out.String())
	}
}

// No pane roster is seeded into the fleet board (design 030 §4.3): its
// members are fleets, not panes.
func TestW4NoPaneRosterForTheFleetBoard(t *testing.T) {
	w, nc := newBoardTestWorkspace(t)
	rec := recordSubject(t, nc, msg.BoardRegisterSubject(msg.FleetBoardID))
	w.announceExistingPanesToBoard("fleet")
	settle(t, nc)
	if subs, _ := rec.seen(); len(subs) != 0 {
		t.Fatalf("%d pane registrations were announced to the fleet board", len(subs))
	}
}
