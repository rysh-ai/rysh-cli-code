// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// A web.sync write must announce itself: the web server re-reads which panes
// are sync-paused from the structural snapshot it fetches on layoutDirty
// (web/pane_sync.go), so without the signal `##pane sync off` would only take
// effect at the next periodic resync. Every other meta key stays quiet — fleet
// tooling writes several per pane at launch, and a layoutDirty is a full
// layout snapshot pushed to every browser.
func TestSetMetaSignalsLayoutDirtyOnlyForWebSync(t *testing.T) {
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	sub, err := nc.SubscribeSync(msg.T("ws", "layoutDirty"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	p := &PaneActor{id: "sync-pane", pub: pub}

	p.setMeta("fleet.role", "worker")
	if _, err := sub.NextMsg(150 * time.Millisecond); err == nil {
		t.Fatal("an unrelated meta key fired layoutDirty")
	}

	p.setMeta(domain.PaneMetaWebSync, domain.WebSyncOff)
	if _, err := sub.NextMsg(time.Second); err != nil {
		t.Fatalf("web.sync=off did not fire layoutDirty: %v", err)
	}
	if !domain.WebSyncPaused(p.metaCopy()) {
		t.Fatalf("meta after pause = %v", p.metaCopy())
	}

	p.setMeta(domain.PaneMetaWebSync, "") // resume = delete
	if _, err := sub.NextMsg(time.Second); err != nil {
		t.Fatalf("clearing web.sync did not fire layoutDirty: %v", err)
	}
	if _, ok := p.metaCopy()[domain.PaneMetaWebSync]; ok {
		t.Fatalf("web.sync still present after resume: %v", p.metaCopy())
	}
}
