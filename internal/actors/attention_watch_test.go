// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"sync"
	"testing"
	"time"
)

type fakeScreen struct {
	mu   sync.Mutex
	rows []string
}

func (f *fakeScreen) Render() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rows...)
}

// The watcher posts a raise once the screen is quiet, and a clear when the
// read loop stops. It runs on its own goroutine next to the actor: run with
// -race.
func TestWatchAttentionRaisesAndClearsOnStop(t *testing.T) {
	p := &PaneActor{id: "watch-pane"}
	p.programAtomic.Store("claude")
	got := make(chan bool, 4)
	p.attentionSend.Store(func(active bool) { got <- active })
	p.lastOutputNano.Store(time.Now().Add(-time.Second).UnixNano())

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		p.watchAttention(&fakeScreen{rows: promptRows}, stop)
		close(done)
	}()

	select {
	case v := <-got:
		if !v {
			t.Fatal("first verdict was a clear")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no raise from a quiet prompt screen")
	}
	close(stop)
	select {
	case v := <-got:
		if v {
			t.Fatal("stop sent a raise")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not clear a raised pane")
	}
	<-done
}

func TestWatchAttentionWithoutAScreenReturns(t *testing.T) {
	p := &PaneActor{id: "watch-pane"}
	done := make(chan struct{})
	go func() {
		p.watchAttention(nil, make(chan struct{}))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher with no screen did not return")
	}
}
