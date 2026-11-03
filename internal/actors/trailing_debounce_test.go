// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func TestTrailingDebounce_BurstFiresOnceAfter(t *testing.T) {
	var n atomic.Int64
	d := newTrailingDebounce(80*time.Millisecond, time.Second, func() { n.Add(1) })
	for i := 0; i < 10; i++ {
		d.Trigger()
		time.Sleep(10 * time.Millisecond)
	}
	if n.Load() != 0 {
		t.Fatal("fired during the burst")
	}
	waitTrue(t, "the trailing fire", func() bool { return n.Load() == 1 })
	time.Sleep(200 * time.Millisecond)
	if got := n.Load(); got != 1 {
		t.Fatalf("fires = %d, want 1", got)
	}
}

// A burst that never pauses still fires about once per maxWait, and once more
// at its end.
func TestTrailingDebounce_MaxWaitDuringEndlessBurst(t *testing.T) {
	var n atomic.Int64
	d := newTrailingDebounce(100*time.Millisecond, 250*time.Millisecond, func() { n.Add(1) })
	end := time.Now().Add(1100 * time.Millisecond)
	for time.Now().Before(end) {
		d.Trigger()
		time.Sleep(20 * time.Millisecond)
	}
	during := n.Load()
	if during < 3 || during > 5 {
		t.Fatalf("fires during a 1.1 s burst with maxWait 250 ms = %d, want 3-5", during)
	}
	waitTrue(t, "the final fire", func() bool { return n.Load() == during+1 })
}

func TestTrailingDebounce_StopDropsPending(t *testing.T) {
	var n atomic.Int64
	d := newTrailingDebounce(50*time.Millisecond, time.Second, func() { n.Add(1) })
	d.Trigger()
	d.Stop()
	time.Sleep(150 * time.Millisecond)
	if n.Load() != 0 {
		t.Fatal("a stopped debounce fired")
	}
}

// R8: ten focus changes in quick succession send one watch note, carrying the
// final set.
func TestMirrorWatchNotes_Debounced(t *testing.T) {
	nc := startInProcessNATS(t)
	r := &MirrorTabListenerActor{
		shareID: "s", workspace: "w", remoteNC: nc, connected: true,
		vterms: map[string]*mirrorPaneVTerm{}, outputSubs: map[string]*nats.Subscription{},
		subscriberID: "sub-1",
	}
	r.watchNotes = newTrailingDebounce(60*time.Millisecond, time.Second, r.publishWatchSet)
	t.Cleanup(r.watchNotes.Stop)

	var notes atomic.Int64
	var last atomic.Value
	sub, err := nc.Subscribe("ws.w.share.s.subscriber", func(m *nats.Msg) {
		notes.Add(1)
		last.Store(string(m.Data))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()
	_ = nc.Flush()

	for i := 0; i < 10; i++ {
		p := fmt.Sprintf("p%d", i)
		r.applyWatchSet([]string{p}, []string{p})
	}
	waitTrue(t, "one watch note", func() bool { return notes.Load() >= 1 })
	time.Sleep(200 * time.Millisecond)
	if got := notes.Load(); got != 1 {
		t.Fatalf("watch notes = %d, want 1", got)
	}
	var note struct {
		Action  string   `json:"action"`
		PaneIDs []string `json:"pane_ids"`
	}
	if err := json.Unmarshal([]byte(last.Load().(string)), &note); err != nil {
		t.Fatal(err)
	}
	if note.Action != "watch" || len(note.PaneIDs) != 1 || note.PaneIDs[0] != "p9" {
		t.Fatalf("note = %+v, want the final set [p9]", note)
	}
}
