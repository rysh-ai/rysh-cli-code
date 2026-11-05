// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// While a pin applies to the turn, a humanoid's pane_send reaches only the
// pinned pane; without one it is the plain tool. It publishes on the
// session's own NATS connection only (pane.<id>.inbox), so it cannot reach
// another session.
func TestPinnedPaneSendRefusesOtherPanes(t *testing.T) {
	nc := newTestNATS(t)
	pinned := ""
	got := make(chan string, 4)
	sub, err := nc.Subscribe(">", func(m *nats.Msg) { got <- m.Subject })
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()
	tool := NewPinnedPaneSendTool(NewPaneSendTool(nc, "s"), func() string { return pinned })

	pinned = "pane-pinned"
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"pane_id":"pane-other","input":"ls"}`))
	if err != nil || out.Error == "" || !strings.Contains(out.Error, "pinned") {
		t.Fatalf("a send to another pane during a pin must be refused: %v %+v", err, out)
	}
	out, err = tool.Execute(context.Background(), json.RawMessage(`{"pane_id":"pane-pinned","input":"ls"}`))
	if err != nil || out.Error != "" {
		t.Fatalf("the pinned pane must be reachable: %v %+v", err, out)
	}
	pinned = ""
	if out, err := tool.Execute(context.Background(), json.RawMessage(`{"pane_id":"pane-other","input":"ls"}`)); err != nil || out.Error != "" {
		t.Fatalf("without a pin the plain tool applies: %v %+v", err, out)
	}
	_ = nc.Flush()
	time.Sleep(100 * time.Millisecond)
	subjects := map[string]int{}
	for len(got) > 0 {
		s := <-got
		switch {
		case strings.Contains(s, "pane-other") && strings.HasSuffix(s, "inbox"):
			subjects["other"]++
		case strings.Contains(s, "pane-pinned") && strings.HasSuffix(s, "inbox"):
			subjects["pinned"]++
		}
	}
	if subjects["other"] != 1 || subjects["pinned"] != 1 {
		t.Fatalf("published to %v; want one send to the pinned pane and one (unpinned) to the other", subjects)
	}
	if tool.Spec().Name != "pane_send" || !tool.RequiresApproval(nil) {
		t.Fatal("the wrapper must keep pane_send's name and approval")
	}
}
