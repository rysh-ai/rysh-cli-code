// SPDX-License-Identifier: Apache-2.0

package daemontest

// gap02 (G6): a detached daemon tells you when a pane needs you.
//
// A real daemon, no UI attached, with `notify: needs_you:` pointing a webhook
// at a server on 127.0.0.1. A pane runs a stand-in "agent" that prints a
// claude-shaped permission prompt and waits. The server must receive exactly
// one POST, naming the pane and the reason, and carrying none of the screen.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// screenMarker is printed on the fake agent's screen. It must never reach the
// webhook.
const screenMarker = "GAP02-SCREEN-MARKER"

func TestDetachedDaemonNotifiesWhenAPaneNeedsYou(t *testing.T) {
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
    after: 1s
    cooldown: 1h
    webhook: %q
`, srv.URL))

	agent := filepath.Join(s.Dir, "fake-agent.sh")
	script := "#!/bin/sh\n" +
		"printf '\\n " + screenMarker + " Do you want to proceed?\\n'\n" +
		"printf ' ❯ 1. Yes\\n   2. No, and tell Claude what to do differently\\n'\n" +
		"exec sleep 120\n"
	if err := os.WriteFile(agent, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, code := s.CLI(t, "send", s.Name, "sh "+agent, "--mode", "shell"); code != 0 {
		t.Fatalf("send: exit %d\n%s", code, out)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && len(got()) == 0 {
		time.Sleep(100 * time.Millisecond)
	}
	if len(got()) == 0 {
		info, _ := s.Exec(t, "##pane list --json")
		t.Fatalf("no notification within 20s; panes:\n%s", info)
	}
	// One raise, one alert: wait past several looks and count again.
	time.Sleep(4 * time.Second)
	all := got()
	if len(all) != 1 {
		t.Fatalf("%d notifications for one raise:\n%s", len(all), strings.Join(all, "\n"))
	}
	body := all[0]
	for _, want := range []string{`"event":"needs_you"`, `"reason":"prompt"`, `"session":"` + s.Name + `"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
	if strings.Contains(body, screenMarker) || strings.Contains(body, "Do you want") {
		t.Fatalf("screen text left the daemon: %s", body)
	}
}
