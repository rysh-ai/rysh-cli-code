// SPDX-License-Identifier: Apache-2.0

package daemontest

// gap02 slice 2: a detached daemon tells you when a long run in a pane ends.
//
// A real daemon, no UI attached, with `notify: needs_you: done_after:` and a
// webhook on 127.0.0.1. A pane runs a command that prints a marker and
// sleeps past done_after. The server must receive exactly one "done" POST
// naming the pane, with none of the screen.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const doneScreenMarker = "GAP02-DONE-SCREEN-MARKER"

func TestDetachedDaemonNotifiesWhenALongRunEnds(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
	}))
	defer srv.Close()
	got := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}

	s := FreshWithConfig(t, fmt.Sprintf(`notify:
  needs_you:
    after: 1h
    cooldown: 1h
    done_after: 2s
    webhook: %q
`, srv.URL))

	// The watcher samples (a look every min(doneLook, done_after) = 2s, plus the
	// 0.5s program poll), so it times a run from its first busy look to its first
	// idle one and can see it up to ~2.5s short, more under load. A run of
	// done_after + 2s (sleep 4) then read as under done_after and was dropped
	// (QA-30, load ~80); sleep 8 leaves three look intervals of slack.
	if out, code := s.CLI(t, "send", s.Name, "echo "+doneScreenMarker+"; sleep 8", "--mode", "shell"); code != 0 {
		t.Fatalf("send: exit %d\n%s", code, out)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && len(got()) == 0 {
		time.Sleep(100 * time.Millisecond)
	}
	if len(got()) == 0 {
		info, _ := s.Exec(t, "##pane list --json")
		t.Fatalf("no done notification within 30s; panes:\n%s", info)
	}
	time.Sleep(4 * time.Second)
	all := got()
	if len(all) != 1 {
		t.Fatalf("%d notifications for one run:\n%s", len(all), strings.Join(all, "\n"))
	}
	body := all[0]
	for _, want := range []string{`"event":"done"`, `"reason":"done"`, `"session":"` + s.Name + `"`, `"ran_seconds":`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
	if strings.Contains(body, doneScreenMarker) || strings.Contains(body, "sleep") {
		t.Fatalf("screen or command text left the daemon: %s", body)
	}
}
