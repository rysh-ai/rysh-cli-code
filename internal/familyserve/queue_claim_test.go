// SPDX-License-Identifier: Apache-2.0

package familyserve

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

// QA-36: serve's prompt is a delivery. It waits behind a run in flight
// instead of cancelling it (Queue), and serve claims only its own run: the
// first run to start on the pane after the send may be someone else's (a
// courier delivery queued just ahead), so the run is matched by the prompt's
// RequestID on its planning status, not by being first.
func TestServe_ClaimsItsOwnRunNotOneQueuedAhead(t *testing.T) {
	w := newWorld(t)
	w.setScript(paneBo, func(_ context.Context, _ []provider.ConversationTurn) (*provider.AgenticResponse, error) {
		return textResp("bo answers"), nil
	})
	// Another delivery's run starts on Bo's pane between serve's send and
	// serve's own run: its planning, then its (empty) done.
	var once sync.Once
	w.mu.Lock()
	w.beforeForward = func(pane string) {
		if pane != paneBo {
			return
		}
		once.Do(func() {
			st := msg.T("pane", paneBo, "llm_prompt_execution", "status")
			_ = w.pub.Send(st, &msg.MsgAgenticStatus{OrchestratorID: "someone-else", RequestID: "courier-1", Phase: "planning"})
			_ = w.pub.Send(st, &msg.MsgAgenticStatus{OrchestratorID: "someone-else", Phase: "done"})
			_ = w.nc.Flush()
		})
	}
	w.mu.Unlock()

	s := w.serve(nil)
	s.next(5*time.Second, ev(EvReady))
	s.sendJSON(map[string]string{"op": "prompt", "id": "B", "pane": paneBo, "text": "hello bo"})
	d, _ := s.next(15*time.Second, ev(EvDone))
	if txt, _ := d["text"].(string); d["status"] != StatusDone || strings.TrimSpace(txt) != "bo answers" {
		t.Fatalf("done = %q: the turn took another run's events as its own", d)
	}
	execs, _ := w.execsSeen()
	if len(execs) == 0 || !execs[0].Queue || execs[0].RequestID == "" {
		t.Fatalf("serve's prompt = %+v: want Queue and a RequestID", execs)
	}
}
