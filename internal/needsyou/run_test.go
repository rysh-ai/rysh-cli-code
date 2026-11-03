// SPDX-License-Identifier: Apache-2.0

package needsyou

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// fakeSession serves a layout that the test changes, and records subscriptions.
type fakeSession struct {
	mu       sync.Mutex
	tabs     []domain.TabSnapshot
	subjects []string
	dirty    func(interface{})
	requests int
}

func (f *fakeSession) set(tabs ...domain.TabSnapshot) {
	f.mu.Lock()
	f.tabs = tabs
	fn := f.dirty
	f.mu.Unlock()
	if fn != nil {
		fn(nil)
	}
}

func (f *fakeSession) RequestFromSubject(subject string, m interface{}, _ time.Duration) (interface{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if subject != msg.T("ws", "snapshot") {
		return nil, errors.New("unexpected subject " + subject)
	}
	if q, ok := m.(*msg.MsgGetWorkspaceSnapshot); !ok || !q.LayoutOnly {
		return nil, errors.New("want a layout-only snapshot request")
	}
	return &msg.MsgWorkspaceSnapshotReply{Snapshot: domain.WorkspaceSnapshot{Tabs: append([]domain.TabSnapshot(nil), f.tabs...)}}, nil
}

func (f *fakeSession) SubscribeExact(subject string, fn func(interface{})) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subjects = append(f.subjects, subject)
	if subject == msg.T("ws", "layoutDirty") {
		f.dirty = fn
	}
	return func() {}, nil
}

type recordSink struct {
	mu  sync.Mutex
	got []Alert
	err error
}

func (r *recordSink) Name() string { return "record" }
func (r *recordSink) Send(_ context.Context, a Alert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, a)
	return r.err
}
func (r *recordSink) alerts() []Alert {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Alert(nil), r.got...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func needyTab(paneID, reason string) domain.TabSnapshot {
	return tabWith("t1", domain.PaneSnapshot{ID: paneID, GivenName: "builder", Meta: map[string]string{
		domain.PaneMetaAttention:       domain.AttentionNeedsYou,
		domain.PaneMetaAttentionReason: reason,
		domain.PaneMetaAttentionSince:  "42",
	}})
}

func TestRunSendsOneAlertToEverySinkAfterTheDelay(t *testing.T) {
	f := &fakeSession{}
	a, b := &recordSink{}, &recordSink{err: errors.New("down")}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, f, Options{Session: "work", Host: "devbox", After: 150 * time.Millisecond, Cooldown: time.Hour,
			Tick: 20 * time.Millisecond, Sinks: []Sink{a, b}})
	}()
	waitFor(t, "layout subscription", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.dirty != nil })
	f.mu.Lock()
	for _, s := range f.subjects {
		if strings.ContainsAny(s, "*>") {
			t.Errorf("wildcard subscription %q", s)
		}
	}
	f.mu.Unlock()

	f.set(needyTab("p1", "approval"))
	time.Sleep(60 * time.Millisecond)
	if n := len(a.alerts()); n != 0 {
		t.Fatalf("alert before the delay: %d", n)
	}
	waitFor(t, "alert on both sinks", func() bool { return len(a.alerts()) == 1 && len(b.alerts()) == 1 })
	got := a.alerts()[0]
	if got.Session != "work" || got.Host != "devbox" || got.PaneID != "p1" || got.PaneName != "builder" || got.Reason != "approval" || got.Since != 42 {
		t.Fatalf("alert = %+v", got)
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(a.alerts()); n != 1 {
		t.Fatalf("%d alerts for one raise", n)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}

func TestRunClearedBeforeTheDelaySendsNothing(t *testing.T) {
	f := &fakeSession{}
	a := &recordSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = Run(ctx, f, Options{Session: "s", After: 200 * time.Millisecond, Tick: 20 * time.Millisecond, Sinks: []Sink{a}})
	}()
	waitFor(t, "layout subscription", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.dirty != nil })
	f.set(needyTab("p1", "prompt"))
	time.Sleep(60 * time.Millisecond)
	f.set(tabWith("t1", domain.PaneSnapshot{ID: "p1", Meta: map[string]string{}}))
	time.Sleep(400 * time.Millisecond)
	if n := len(a.alerts()); n != 0 {
		t.Fatalf("alert for a pane answered in time: %d", n)
	}
}

func TestRunWithNoSinksRefuses(t *testing.T) {
	if err := Run(context.Background(), &fakeSession{}, Options{}); err == nil {
		t.Fatal("Run with no sinks should refuse")
	}
}
