// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Design 093 H3: the fleet actor publishes a fleet.LedgerEvent on
// msg.FleetEventSubject for every member whose ledger row changes — at once on
// a declaration, and never twice for the same row.
func TestFleetActorPublishesLedgerChanges(t *testing.T) {
	nc := newABLATestNATS(t)
	startFleetActor(t, nc, newFleetTestKV(t, nc))
	registerFleet(t, nc, fleet.Fleet{Name: "family-f1", State: fleet.StateUp})
	if _, err := fleet.Send(nc, fleet.Update{Op: fleet.OpMemberUpsert, Name: "family-f1",
		Member: fleet.Member{PaneID: "pane-ada", Label: "ada"}}, fleetTestTimeout); err != nil {
		t.Fatal(err)
	}

	sub, err := nc.SubscribeSync(msg.FleetEventSubject())
	if err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()
	next := func() (fleet.LedgerEvent, bool) {
		m, err := sub.NextMsg(500 * time.Millisecond)
		if err == nats.ErrTimeout {
			return fleet.LedgerEvent{}, false
		}
		if err != nil {
			t.Fatal(err)
		}
		var ev fleet.LedgerEvent
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			t.Fatal(err)
		}
		return ev, true
	}

	pct := 60
	status := fleet.Update{Op: fleet.OpStatus, PaneID: "pane-ada",
		Status: fleet.StatusUpdate{Work: fleet.WorkProgress, Task: "lunch", Note: "setting the table", Pct: &pct}}
	if _, err := fleet.Send(nc, status, fleetTestTimeout); err != nil {
		t.Fatal(err)
	}
	ev, ok := next()
	if !ok {
		t.Fatal("no ledger event after a status declaration")
	}
	if ev.Fleet != "family-f1" || ev.PaneID != "pane-ada" || ev.Label != "ada" || ev.State != fleet.WorkProgress ||
		ev.Note != "setting the table" || ev.Task != "lunch" || ev.Pct == nil || *ev.Pct != 60 || ev.At == 0 {
		t.Fatalf("event = %+v", ev)
	}
	// The watch re-derives health right after a declaration (one more event
	// when it changes); let the feed settle.
	for {
		if _, ok := next(); !ok {
			break
		}
	}
	// The same declaration again changes nothing visible: no event.
	if _, err := fleet.Send(nc, status, fleetTestTimeout); err != nil {
		t.Fatal(err)
	}
	if ev, ok := next(); ok {
		t.Fatalf("unchanged row re-announced: %+v", ev)
	}
	if _, err := fleet.Send(nc, fleet.Update{Op: fleet.OpStatus, PaneID: "pane-ada",
		Status: fleet.StatusUpdate{Work: fleet.WorkDone, KeepTask: true, Note: "served"}}, fleetTestTimeout); err != nil {
		t.Fatal(err)
	}
	if ev, ok := next(); !ok || ev.State != fleet.WorkDone || ev.Note != "served" {
		t.Fatalf("done declaration: %+v %v", ev, ok)
	}
}
