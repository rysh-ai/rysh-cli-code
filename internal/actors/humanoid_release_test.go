// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 (o2), design 020 §5.7: unregistering a humanoid's output pane disabled
// only the humanoid's own dynamic mode. The visible surface it had enabled —
// "email" (or "external") — and the pane's humanoid binding stayed, so the
// pane kept offering an email client wired to a humanoid that had left. The
// humanoid now RELEASES the pane it leaves; the pane drops the surface and the
// binding only if that humanoid still owns it, so a newer owner is untouched.

func TestHumanoidReleasesThePaneItLeaves(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	sub, err := nc.SubscribeSync(msg.T("pane", "p1", "inbox"))
	if err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()

	h := &HumanoidActor{
		name:               "mail-h",
		pub:                pub,
		emailAdapter:       channels.NewEmailAdapter(msg.ChannelConfig{}),
		registeredPanes:    map[string]string{},
		activeChatPaneID:   "p1",
		llmPromptExecInbox: msg.T("pane", "mail-h", "llm_prompt_execution", "inbox"),
	}
	h.updateOutputRouting() // p1 was unregistered: no panes left

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := sub.NextMsg(500 * time.Millisecond)
		if err != nil {
			break
		}
		if r, ok := decodeEnvelope(t, codecs, raw).(*msg.MsgPaneReleaseHumanoid); ok {
			if r.Humanoid != "mail-h" || r.Surface != "email" {
				t.Fatalf("release = %+v, want humanoid mail-h, surface email", r)
			}
			return
		}
	}
	t.Fatal("the humanoid left pane p1 without releasing its email surface")
}

func TestPaneReleaseOnlyByItsOwner(t *testing.T) {
	p := newModeTestPane(t)
	p.enabledModes = append(p.enabledModes, "email")
	p.registeredHumanoid = "other-h" // re-registered to someone else since

	p.releaseHumanoid(&msg.MsgPaneReleaseHumanoid{Humanoid: "mail-h", Surface: "email"})
	if !modesContain(p.enabledModes, "email") || p.registeredHumanoid != "other-h" {
		t.Fatalf("a stale release from mail-h changed other-h's pane: modes=%v humanoid=%q", p.enabledModes, p.registeredHumanoid)
	}

	p.registeredHumanoid = "mail-h"
	p.releaseHumanoid(&msg.MsgPaneReleaseHumanoid{Humanoid: "mail-h", Surface: "email"})
	if modesContain(p.enabledModes, "email") {
		t.Fatalf("email mode still enabled after its humanoid released the pane: %v", p.enabledModes)
	}
	if p.registeredHumanoid != "" {
		t.Fatalf("pane still bound to %q after release", p.registeredHumanoid)
	}
}
