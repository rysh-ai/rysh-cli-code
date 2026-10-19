// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The status scout over a real bus (design 056 §3): a stalled agent's check-in
// is answered by the scout — no CHECK-IN reaches the agent — and when the
// scout cannot tell, the agent is asked exactly as before.

func scoutFleet(t *testing.T, name, pane string, reply string) (*recorder[msg.MsgBoardPost], *int32) {
	t.Helper()
	oldTick, oldGap, oldCall := afaWatchInterval, afaObserveGap, scoutCall
	afaWatchInterval, afaObserveGap = 150*time.Millisecond, 30*time.Millisecond
	var calls int32
	scoutCall = func(_ context.Context, _, model, system, prompt string) (string, error) {
		atomic.AddInt32(&calls, 1)
		if model != "haiku" || !strings.Contains(system, "Answer ONLY with one JSON object") || !strings.Contains(prompt, "Check-in reason: stalled") {
			t.Errorf("scout called with model %q and an unexpected prompt:\n%s", model, prompt)
		}
		return reply, nil
	}
	t.Cleanup(func() { afaWatchInterval, afaObserveGap, scoutCall = oldTick, oldGap, oldCall })

	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	servePaneVT(t, nc, codecs, pane, func() []string { return []string{"$ go test ./...", "> "} })
	posts := record[msg.MsgBoardPost](t, nc, msg.BoardPostSubject(name))
	startFleetActor(t, nc, newFleetTestKV(t, nc))
	registerFleet(t, nc, fleet.Fleet{Name: name})
	for _, u := range []fleet.Update{
		{Op: fleet.OpState, Name: name, State: fleet.StateUp},
		{Op: fleet.OpMemberUpsert, Name: name, Member: fleet.Member{PaneID: pane, Label: "wkr-01", Role: "worker"}},
		{Op: fleet.OpWatch, Name: name, Watch: fleet.Watch{IntervalSec: 600, StallSec: 1, Answer: fleet.AnswerScout}},
		{Op: fleet.OpGoalExec, Name: name, GoalExec: fleet.GoalExec{Mode: fleet.GoalExecOff}},
	} {
		if _, err := fleet.Send(nc, u, fleetTestTimeout); err != nil {
			t.Fatal(err)
		}
	}
	declareOverBus(t, nc, pane, fleet.StatusUpdate{Work: fleet.WorkProgress, Task: "build the parser"})
	return posts, &calls
}

func TestScoutAnswersAStallSoTheAgentIsNotWoken(t *testing.T) {
	pane := "abababab-0000-4000-8000-0000000000ab"
	posts, calls := scoutFleet(t, "sc1", pane,
		`{"answerable": true, "work": "progress", "task": "", "note": "go test is running", "confidence": 0.92}`)
	posts.waitFor(t, "the scout's status, posted as the member", func(p *msg.MsgBoardPost) bool {
		return p.PaneID == pane && strings.Contains(p.Text, "answered by the status scout (haiku): go test is running")
	})
	time.Sleep(400 * time.Millisecond)
	posts.mu.Lock()
	defer posts.mu.Unlock()
	for _, p := range posts.got {
		if p.Kind == msg.BoardKindCmd && p.ToPaneID == pane && strings.Contains(p.Text, "CHECK-IN") {
			t.Fatalf("the agent was woken although the scout answered:\n%s", p.Text)
		}
	}
	if atomic.LoadInt32(calls) < 1 {
		t.Fatal("the scout was never called")
	}
}

func TestScoutThatCannotTellAsksTheAgent(t *testing.T) {
	pane := "cdcdcdcd-0000-4000-8000-0000000000cd"
	posts, _ := scoutFleet(t, "sc2", pane, `{"answerable": false, "reason": "the screen shows a question to the human", "confidence": 0.3}`)
	posts.waitFor(t, "the CHECK-IN to the agent", func(p *msg.MsgBoardPost) bool {
		return p.Kind == msg.BoardKindCmd && p.ToPaneID == pane && strings.Contains(p.Text, "CHECK-IN") && strings.Contains(p.Text, "stalled")
	})
}
