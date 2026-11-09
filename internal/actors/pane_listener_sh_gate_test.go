// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	sharedagentic "github.com/rysh-ai/rysh-cli-shared/agentic"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// A listened pane's output may carry any text (a viewed file, a log, a remote
// party). A shell event line in it runs a command in the LISTENING pane only
// when that listen allowed it, and never past the installed approval policy.
func TestListenerShellEventsNeedOptInAndPassPolicy(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	var mu sync.Mutex
	execs := 0
	sub, err := nc.Subscribe(msg.T("pane", "owner", "inbox"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		if d, err := codecs.Decode(env.TypeTag, env.Payload); err == nil {
			if _, ok := d.(*msg.MsgPaneExecShell); ok {
				mu.Lock()
				execs++
				mu.Unlock()
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	count := func() int {
		_ = nc.Flush()
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		return execs
	}
	line := shSoftdevPrefix + "golang:development\n" // a real softdevCommands key
	t.Cleanup(func() { setInstalledApprovalPolicy(nil) })

	l := NewPaneSharedOutputListenerActor("owner", "target", "A", pub, nc)
	l.handleOutput(line)
	if n := count(); n != 0 {
		t.Fatalf("a listen without shell opt-in ran %d command(s)", n)
	}

	l.allowShell = true
	setInstalledApprovalPolicy(func(tool string, _ json.RawMessage) (sharedagentic.PolicyDecision, string) {
		if tool == "bash" {
			return sharedagentic.PolicyGate, "approval.always_gate[0]"
		}
		return sharedagentic.PolicyDefault, ""
	})
	l.handleOutput(line)
	if n := count(); n != 0 {
		t.Fatalf("a gated shell command ran %d time(s)", n)
	}

	setInstalledApprovalPolicy(nil)
	l.handleOutput(line)
	if n := count(); n != 1 {
		t.Fatalf("with the opt-in and no gate the event should run once, ran %d", n)
	}
}
