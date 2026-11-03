// SPDX-License-Identifier: Apache-2.0

package needsyou

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// state is a pane running program ("" = at its prompt, idle).
func state(id, program string) PaneState {
	return PaneState{ID: id, Name: id, Program: program}
}

func doneIDs(ds []Done) string {
	var s []string
	for _, d := range ds {
		s = append(s, d.ID)
	}
	return strings.Join(s, ",")
}

// gap02 slice 2: a pane busy for at least doneAfter that goes idle is one
// "done" alert. Shorter runs are not worth a phone buzz.
func TestDoneAlertsOnlyForLongRuns(t *testing.T) {
	w := NewDoneWatcher(time.Minute, 0)
	w.Observe(t0, []PaneState{state("p1", "make"), state("p2", "make")})
	// p2 finishes after 30s: too short.
	if got := w.Observe(t0.Add(30*time.Second), []PaneState{state("p1", "make"), state("p2", "")}); len(got) != 0 {
		t.Fatalf("alert for a short run: %v", doneIDs(got))
	}
	got := w.Observe(t0.Add(2*time.Minute), []PaneState{state("p1", ""), state("p2", "")})
	if doneIDs(got) != "p1" {
		t.Fatalf("done = %q, want p1", doneIDs(got))
	}
	if got[0].Reason != ReasonDone || got[0].Ran != 2*time.Minute {
		t.Fatalf("done alert = %+v", got[0])
	}
	if got := w.Observe(t0.Add(3*time.Minute), []PaneState{state("p1", "")}); len(got) != 0 {
		t.Fatalf("second alert for one run: %v", doneIDs(got))
	}
}

func TestDoneOffWhenDoneAfterIsZero(t *testing.T) {
	w := NewDoneWatcher(0, 0)
	w.Observe(t0, []PaneState{state("p1", "make")})
	if got := w.Observe(t0.Add(time.Hour), []PaneState{state("p1", "")}); len(got) != 0 {
		t.Fatalf("done_after 0 must be off: %v", doneIDs(got))
	}
	if w.Busy() {
		t.Fatal("an off watcher tracks nothing")
	}
}

func TestDoneBusyIsAProgramOrAProviderQuery(t *testing.T) {
	for _, p := range []PaneState{state("p1", "claude"), {ID: "p1", Status: "querying provider"}} {
		w := NewDoneWatcher(time.Second, 0)
		w.Observe(t0, []PaneState{p})
		if !w.Busy() {
			t.Fatalf("%+v not tracked as busy", p)
		}
		if got := w.Observe(t0.Add(time.Minute), []PaneState{state("p1", "")}); doneIDs(got) != "p1" {
			t.Fatalf("%+v → idle not done: %q", p, doneIDs(got))
		}
	}
	// The status word alone is not busy: "running shell" outlives the command.
	w := NewDoneWatcher(time.Second, 0)
	w.Observe(t0, []PaneState{{ID: "p1", Status: "running shell"}})
	if w.Busy() {
		t.Fatal(`"running shell" with no program counted as busy`)
	}
}

func TestDoneErrorEndIsReasonFailed(t *testing.T) {
	w := NewDoneWatcher(time.Second, 0)
	w.Observe(t0, []PaneState{state("p1", "make")})
	got := w.Observe(t0.Add(time.Minute), []PaneState{{ID: "p1", Status: "error"}})
	if len(got) != 1 || got[0].Reason != ReasonFailed {
		t.Fatalf("error end = %+v", got)
	}
}

// A pane that ends its run waiting on you is the needs-you notifier's job,
// never a second, "done" alert.
func TestDoneSkipsAPaneThatNeedsYou(t *testing.T) {
	w := NewDoneWatcher(time.Second, 0)
	w.Observe(t0, []PaneState{state("p1", "claude")})
	p := state("p1", "")
	p.NeedsYou = true
	if got := w.Observe(t0.Add(time.Minute), []PaneState{p}); len(got) != 0 {
		t.Fatalf("done alert for a needs-you pane: %v", doneIDs(got))
	}
}

func TestDoneClosedPaneIsForgotten(t *testing.T) {
	w := NewDoneWatcher(time.Second, 0)
	w.Observe(t0, []PaneState{state("p1", "make")})
	if got := w.Observe(t0.Add(time.Minute), nil); len(got) != 0 {
		t.Fatalf("alert for a closed pane: %v", doneIDs(got))
	}
	if w.Busy() {
		t.Fatal("closed pane still tracked")
	}
}

func TestDoneCooldownDropsRepeats(t *testing.T) {
	w := NewDoneWatcher(time.Second, 10*time.Minute)
	w.Observe(t0, []PaneState{state("p1", "make")})
	if got := w.Observe(t0.Add(time.Minute), []PaneState{state("p1", "")}); doneIDs(got) != "p1" {
		t.Fatalf("first done missing: %q", doneIDs(got))
	}
	w.Observe(t0.Add(2*time.Minute), []PaneState{state("p1", "make")})
	if got := w.Observe(t0.Add(4*time.Minute), []PaneState{state("p1", "")}); len(got) != 0 {
		t.Fatalf("done inside cooldown: %v", doneIDs(got))
	}
	w.Observe(t0.Add(12*time.Minute), []PaneState{state("p1", "make")})
	if got := w.Observe(t0.Add(14*time.Minute), []PaneState{state("p1", "")}); doneIDs(got) != "p1" {
		t.Fatalf("done after cooldown missing: %q", doneIDs(got))
	}
}

func TestStatesFromTabsSkipsMirrorsAndMarksNeedsYou(t *testing.T) {
	tabs := []domain.TabSnapshot{
		tabWith("t1",
			domain.PaneSnapshot{ID: "p1", GivenName: "build", Program: "make", Status: "running shell"},
			domain.PaneSnapshot{ID: "p2", Status: "idle", Meta: map[string]string{
				domain.PaneMetaAttention: domain.AttentionNeedsYou, domain.PaneMetaAttentionReason: "prompt",
			}}),
		tabWith("mirror:host:t9", domain.PaneSnapshot{ID: "r1", Program: "make"}),
	}
	got := StatesFromTabs(tabs)
	if len(got) != 2 {
		t.Fatalf("states = %+v", got)
	}
	if got[0].ID != "p1" || got[0].Name != "build" || got[0].Program != "make" || got[0].NeedsYou {
		t.Fatalf("p1 = %+v", got[0])
	}
	if got[1].ID != "p2" || !got[1].NeedsYou {
		t.Fatalf("p2 = %+v", got[1])
	}
}

func TestDoneAlertTextAndWireCarryNoScreenText(t *testing.T) {
	a := Alert{Event: EventDone, Session: "work", Host: "devbox", PaneID: "p1", PaneName: "build", Reason: ReasonDone, Ran: 3 * time.Minute}
	if got, want := a.Text(), "rysh: build finished after 3m0s — session work on devbox"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
	a.Reason = ReasonFailed
	if got := a.Text(); !strings.Contains(got, "build failed after 3m0s") {
		t.Fatalf("failed text = %q", got)
	}

	var doc map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		doc = nil
		_ = json.Unmarshal(b, &doc)
	}))
	defer srv.Close()
	a.Reason = ReasonDone
	if err := NewWebhookSink(srv.URL).Send(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if doc["event"] != "done" || doc["reason"] != "done" || doc["ran_seconds"] != float64(180) {
		t.Fatalf("wire doc = %v", doc)
	}
	// The needs-you document keeps its event name and gains no ran_seconds.
	if err := NewWebhookSink(srv.URL).Send(context.Background(), Alert{PaneName: "x", Reason: "prompt"}); err != nil {
		t.Fatal(err)
	}
	if _, has := doc["ran_seconds"]; doc["event"] != "needs_you" || has {
		t.Fatalf("needs-you doc = %v", doc)
	}
}

func TestRunSendsDoneAlert(t *testing.T) {
	f := &fakeSession{}
	a := &recordSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = Run(ctx, f, Options{Session: "work", After: time.Hour, Cooldown: time.Hour, DoneAfter: 100 * time.Millisecond,
			Tick: 20 * time.Millisecond, Sinks: []Sink{a}})
	}()
	waitFor(t, "layout subscription", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.dirty != nil })
	f.set(tabWith("t1", domain.PaneSnapshot{ID: "p1", GivenName: "build", Program: "make", Status: "running shell"}))
	time.Sleep(200 * time.Millisecond)
	// No layoutDirty when a program ends: the runner must look on its own.
	// The status word stays "running shell" after the command ends.
	f.mu.Lock()
	f.tabs = []domain.TabSnapshot{tabWith("t1", domain.PaneSnapshot{ID: "p1", GivenName: "build", Status: "running shell"})}
	f.mu.Unlock()
	waitFor(t, "done alert", func() bool { return len(a.alerts()) == 1 })
	got := a.alerts()[0]
	if got.Event != EventDone || got.PaneID != "p1" || got.PaneName != "build" || got.Reason != ReasonDone || got.Ran < 100*time.Millisecond {
		t.Fatalf("done alert = %+v", got)
	}
}
