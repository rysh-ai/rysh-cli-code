// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Scoping the fleet-wide interrupt (design 028 §6.6, `E-41`).
//
// THE ASSERTIONS ARE ON THE WIRE, not on what the command reported, for the
// reason the rest of ansa_interrupt_test.go gives: a stop that reaches one pane
// too many is completely invisible in a return value. A test that checks "it
// said 2 panes" passes just as happily when a third fleet's worker also got
// ESC — and with several fleets in a session, that third pane belongs to work
// nobody asked about.

// twoFleetSnapshot is a session running TWO fleets plus panes that belong to
// neither: the shape this whole wave exists for.
func twoFleetSnapshot() *domain.WorkspaceSnapshot {
	return &domain.WorkspaceSnapshot{
		Tabs: []domain.TabSnapshot{{
			ID: "tab-1",
			Lanes: []domain.LaneSnapshot{{
				ID: "lane-1",
				PaneGroups: []domain.PaneGroupSnapshot{{
					ID: "group-1",
					Panes: []domain.PaneSnapshot{
						{ID: "pane-07-mgr", GivenName: "mgr-07",
							Meta: map[string]string{"fleet.role": "manager", "fleet.name": "epic-07"}},
						{ID: "pane-07-wkr", GivenName: "wkr-07",
							Meta: map[string]string{"fleet.role": "worker", "fleet.name": "epic-07"}},
						{ID: "pane-08-mgr", GivenName: "mgr-08",
							Meta: map[string]string{"fleet.role": "manager", "fleet.name": "epic-08"}},
						{ID: "pane-08-wkr", GivenName: "wkr-08",
							Meta: map[string]string{"fleet.role": "worker", "fleet.name": "epic-08"}},
						// The human's own shell. No fleet meta, spared by construction.
						{ID: "pane-human", GivenName: "shell"},
						// An agent in NO fleet: it carries a role but no name, so a
						// NAMED --fleet must not reach it either.
						{ID: "pane-loner", GivenName: "solo",
							Meta: map[string]string{"fleet.role": "worker"}},
					},
				}},
			}},
		}},
	}
}

// TestInterruptingOneFleetLeavesTheOtherAlone is THE test for this wave.
//
// Before it, `--fleet` selected every pane carrying fleet.role — correct for
// one fleet (027 ruling 8) and a session-wide stop for two. Twenty-five fleets
// made "stop my fleet" mean "stop all twenty-five".
func TestInterruptingOneFleetLeavesTheOtherAlone(t *testing.T) {
	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	serveAnsaSnapshot(t, nc, codecs, twoFleetSnapshot())

	sevenMgr := watchRawInput(t, nc, codecs, "pane-07-mgr")
	sevenWkr := watchRawInput(t, nc, codecs, "pane-07-wkr")
	eightMgr := watchRawInput(t, nc, codecs, "pane-08-mgr")
	eightWkr := watchRawInput(t, nc, codecs, "pane-08-wkr")
	human := watchRawInput(t, nc, codecs, "pane-human")
	loner := watchRawInput(t, nc, codecs, "pane-loner")

	r := &ansaRouter{tr: &natsAnsaTransport{pub: pub}}
	outcomes, refusal := r.InterruptFleet("epic-07")
	if refusal != nil {
		t.Fatalf("InterruptFleet(epic-07) refused: %s", refusal.Error)
	}

	// Reached first — these block, which also flushes the bus so the silence
	// below means something.
	if got := sevenMgr.next(t, "pane-07-mgr"); string(got.Data) != string(ansaInterruptKey) {
		t.Errorf("pane-07-mgr got %q, want ESC", string(got.Data))
	}
	if got := sevenWkr.next(t, "pane-07-wkr"); string(got.Data) != string(ansaInterruptKey) {
		t.Errorf("pane-07-wkr got %q, want ESC", string(got.Data))
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // let any WRONG publish land before claiming silence

	// The other fleet is untouched. This is the assertion the wave exists for.
	eightMgr.requireSilent(t, "pane-08-mgr")
	eightWkr.requireSilent(t, "pane-08-wkr")
	// And the two panes that were already spared, still are.
	human.requireSilent(t, "pane-human")
	loner.requireSilent(t, "pane-loner")

	if len(outcomes) != 2 {
		t.Fatalf("interrupted %d panes, want epic-07's two: %+v", len(outcomes), outcomes)
	}
}

// TestAllFleetsStillReachesEveryFleet — the wide form did not disappear, it
// just has to be typed. 027 ruling 8 chose that blast radius knowingly, and
// `--all-fleets` is where it now lives.
func TestAllFleetsStillReachesEveryFleet(t *testing.T) {
	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	serveAnsaSnapshot(t, nc, codecs, twoFleetSnapshot())

	human := watchRawInput(t, nc, codecs, "pane-human")

	r := &ansaRouter{tr: &natsAnsaTransport{pub: pub}}
	outcomes, refusal := r.InterruptFleet("")
	if refusal != nil {
		t.Fatalf("InterruptFleet(all) refused: %s", refusal.Error)
	}
	// Five panes carry fleet.role: both fleets' four, plus the loner. The
	// human's shell does not, and is spared by construction.
	if len(outcomes) != 5 {
		t.Fatalf("interrupted %d panes, want 5: %+v", len(outcomes), outcomes)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	human.requireSilent(t, "pane-human")
}

// TestAnUnknownFleetNameRefusesAndNamesTheFleetsThatExist.
//
// A typo and a fleet that is already quiet produce the same silence, so the
// refusal has to distinguish them: "no pane carries fleet.name=epic-7" plus the
// list of fleets that ARE running is the difference between retyping and
// concluding the stop worked.
func TestAnUnknownFleetNameRefusesAndNamesTheFleetsThatExist(t *testing.T) {
	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	serveAnsaSnapshot(t, nc, codecs, twoFleetSnapshot())

	watchers := map[string]*rawInputWatch{}
	for _, id := range []string{"pane-07-mgr", "pane-07-wkr", "pane-08-mgr", "pane-08-wkr", "pane-human"} {
		watchers[id] = watchRawInput(t, nc, codecs, id)
	}

	r := &ansaRouter{tr: &natsAnsaTransport{pub: pub}}
	outcomes, refusal := r.InterruptFleet("epic-7") // the typo: 7, not 07
	if refusal == nil {
		t.Fatalf("a fleet that does not exist was accepted, interrupting %+v", outcomes)
	}
	if len(outcomes) != 0 {
		t.Fatalf("a refused fleet interrupt still touched %d panes", len(outcomes))
	}
	for _, want := range []string{"epic-7", "epic-07", "epic-08"} {
		if !strings.Contains(refusal.Error, want) {
			t.Errorf("refusal does not mention %q, so the operator cannot tell a typo "+
				"from an already-quiet fleet: %s", want, refusal.Error)
		}
	}

	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	for id, w := range watchers {
		w.requireSilent(t, id)
	}
}

// TestABareFleetFlagIsRefused. Design 028 §6.6: inheriting the wide blast
// radius silently is not the decision ruling 8 made, so the bare flag stops the
// command instead of meaning "all". Both defaults would be wrong invisibly —
// "all" stops fleets nobody named, "mine" guesses which fleet was meant.
func TestABareFleetFlagIsRefused(t *testing.T) {
	w := &WorkspaceActor{}

	var out strings.Builder
	err := w.ansaInterrupt(&out, []string{"--fleet"})
	if err == nil {
		t.Fatal("a bare --fleet was accepted; it used to mean every fleet in the session")
	}
	for _, want := range []string{"--fleet <name>", "--all-fleets"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the refusal does not offer %q: %q", want, out.String())
		}
	}

	// `--fleet=` is the same mistake spelled differently and gets the same answer.
	out.Reset()
	if err := w.ansaInterrupt(&out, []string{"--fleet="}); err == nil {
		t.Fatal("--fleet= was accepted")
	}

	// And a flag directly after --fleet is not its value.
	out.Reset()
	if err := w.ansaInterrupt(&out, []string{"--fleet", "--all-fleets"}); err == nil {
		t.Fatal("--fleet consumed a flag as its fleet name")
	}
}

// TestInterruptRefusesAFleetAndATargetTogether: naming both is a misparse, and
// on this verb a misparse costs an agent the turn it was in the middle of.
func TestInterruptRefusesAFleetAndATargetTogether(t *testing.T) {
	w := &WorkspaceActor{}
	var out strings.Builder
	if err := w.ansaInterrupt(&out, []string{"--fleet", "epic-07", "pane-abc"}); err == nil {
		t.Fatal("a fleet AND a target were accepted together")
	}
	out.Reset()
	if err := w.ansaInterrupt(&out, []string{"--all-fleets", "pane-abc"}); err == nil {
		t.Fatal("--all-fleets AND a target were accepted together")
	}
}

// TestFleetSelectionIgnoresPanesInAnotherFleetEvenWithARole is the unit-level
// statement of the same rule: fleet.role says "this is an agent", fleet.name
// says "this is MY agent", and a NAMED fleet needs both.
func TestFleetSelectionIgnoresPanesInAnotherFleetEvenWithARole(t *testing.T) {
	panes := []ansaPane{
		{ID: "a", Meta: map[string]string{"fleet.role": "worker", "fleet.name": "epic-07"}},
		{ID: "b", Meta: map[string]string{"fleet.role": "worker", "fleet.name": "epic-08"}},
		{ID: "c", Meta: map[string]string{"fleet.role": "worker"}}, // a role, no fleet
		{ID: "d", Meta: map[string]string{}},                       // the human's shell
	}

	got := ansaFleetPanes(panes, "epic-07")
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("--fleet epic-07 selected %v, want only pane a", ids(got))
	}
	// Case-insensitive, because a fleet name reaches this from a human's
	// keyboard as often as from a manifest.
	if got := ansaFleetPanes(panes, "EPIC-07"); len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("--fleet EPIC-07 selected %v, want only pane a", ids(got))
	}
	if got := ansaFleetPanes(panes, ""); len(got) != 3 {
		t.Fatalf("--all-fleets selected %v, want every pane carrying a role", ids(got))
	}
	if names := ansaFleetNames(panes); strings.Join(names, ",") != "epic-07,epic-08" {
		t.Fatalf("ansaFleetNames = %v, want the two fleets in a stable order", names)
	}
}

func ids(panes []ansaPane) []string {
	out := make([]string, 0, len(panes))
	for _, p := range panes {
		out = append(out, p.ID)
	}
	return out
}

// TestScopedInterruptStillStopsTheBoardClaudeLast — 027 ruling 9 applies PER
// FLEET. The board claude carries the dispatch, so its own ESC must land after
// the last target's; scoping the selection must not have reordered that.
func TestScopedInterruptStillStopsTheBoardClaudeLast(t *testing.T) {
	snap := twoFleetSnapshot()
	snap.Tabs[0].Lanes[0].PaneGroups[0].Panes = append(
		snap.Tabs[0].Lanes[0].PaneGroups[0].Panes,
		domain.PaneSnapshot{ID: "pane-07-board", GivenName: boardAgentName,
			Meta: map[string]string{"fleet.role": "board", "fleet.name": "epic-07"}},
	)

	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	serveAnsaSnapshot(t, nc, codecs, snap)

	arrivals := make(chan string, 32)
	sub, err := nc.Subscribe(msg.T("pane", "*", "rawinput"), func(m *nats.Msg) {
		parts := strings.Split(m.Subject, ".")
		if len(parts) >= 3 {
			arrivals <- parts[len(parts)-2]
		}
	})
	if err != nil {
		t.Fatalf("subscribe wildcard rawinput: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	r := &ansaRouter{tr: &natsAnsaTransport{pub: pub}}
	if _, refusal := r.InterruptFleet("epic-07"); refusal != nil {
		t.Fatalf("InterruptFleet(epic-07) refused: %s", refusal.Error)
	}

	var order []string
	deadline := time.After(3 * time.Second)
	for len(order) < 3 { // mgr, wkr, board — epic-08 is not in scope
		select {
		case id := <-arrivals:
			order = append(order, id)
		case <-deadline:
			t.Fatalf("only %d of 3 interrupts arrived: %v", len(order), order)
		}
	}
	if last := order[len(order)-1]; last != "pane-07-board" {
		t.Fatalf("the board claude was interrupted at position %v, not last (%v) — "+
			"its own ESC can cut off the dispatch it is carrying", last, order)
	}
}
