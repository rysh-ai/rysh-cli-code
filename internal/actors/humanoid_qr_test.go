// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestHandlePairQRRendersScannableQR is X4's honesty proof: the pane now renders a
// real half-block QR (not a framed text block), keeps the raw payload as a
// render-independent fallback, and no longer claims the dashboard renders the
// scannable image (design 009).
func TestHandlePairQRRendersScannableQR(t *testing.T) {
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	h := &HumanoidActor{name: "linker", pub: pub, nc: nc, activeChatPaneID: "pane-x"}

	sub, err := nc.SubscribeSync(msg.T("pane", "pane-x", "output", "rysh"))
	if err != nil {
		t.Fatalf("subscribe pane output: %v", err)
	}

	const payload = "sgnl://linkdevice?uuid=abc&pub_key=def"
	h.handlePairQR(&msg.MsgChannelPairQR{Channel: "signal", QR: payload})

	// Decode the pane-output message content (raw JSON escapes '&' → &, so
	// assert against the decoded string).
	var out strings.Builder
	for _, env := range drainSubject(t, sub, 500*time.Millisecond) {
		var m struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(env.Payload, &m) == nil {
			out.WriteString(m.Message.Content)
		}
	}
	s := out.String()

	if !strings.Contains(s, "█") {
		t.Fatalf("no half-block QR rendered into the pane:\n%s", s)
	}
	if !strings.Contains(s, payload) {
		t.Fatalf("raw-payload fallback missing:\n%s", s)
	}
	if strings.Contains(s, "dashboard renders this payload as a scannable QR image") {
		t.Fatal("still makes the false 'dashboard renders the image' claim")
	}
}
