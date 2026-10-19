// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"
	"time"
)

// E17 (j): the startup trigger that resumes interrupted replay panes (g1) —
// and re-registers meeting panes (design 043 D7) — had no test: it was a bare
// `go func() { time.Sleep(3s); … }()` inside Started. It now goes through
// scheduleStartupRestores and the startupRestoreAfter clock seam, so a test
// can fire the wait instead of sleeping through it.
func TestStartupRestoresFireAfterTheWait(t *testing.T) {
	fire := make(chan time.Time, 1)
	var waited time.Duration
	prev := startupRestoreAfter
	startupRestoreAfter = func(d time.Duration) <-chan time.Time { waited = d; return fire }
	t.Cleanup(func() { startupRestoreAfter = prev })

	sent := make(chan any, 4)
	w := &WorkspaceActor{meetingSend: func(m any) { sent <- m }}

	w.scheduleStartupRestores()

	select {
	case m := <-sent:
		t.Fatalf("%T sent before the wait elapsed: restores would race the tabs getting their panes", m)
	case <-time.After(100 * time.Millisecond):
	}
	if waited != 3*time.Second {
		t.Errorf("startup restore wait = %v, want 3s", waited)
	}

	fire <- time.Now()
	got := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case m := <-sent:
			switch m.(type) {
			case *msgReplayRestore:
				got["replay"] = true
			case *msgMeetingRestore:
				got["meeting"] = true
			}
		case <-deadline:
			t.Fatalf("after the wait, sent %v; want msgReplayRestore and msgMeetingRestore", got)
		}
	}
}
