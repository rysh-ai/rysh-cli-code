// SPDX-License-Identifier: Apache-2.0

package daemontest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/daemontest"
)

// The incident these two tests exist for: `##cmd --capture` had no read-back,
// so the only way to poll the file was ANOTHER broadcast — `cat <file>` typed
// into every pane in scope. The panes running claude answered it as a prompt,
// the sentinel never appeared, and the poller retried; 64 stray pane-writes in
// two minutes, interleaved with what the operator was typing.
//
// A unit test can check the parsing. Only a live daemon can show that the
// round trip works end to end and that the filter actually keeps a broadcast
// off a pane that is busy.

// captureManifestPane pulls the pane id out of the --capture manifest, which is
// the only thing a caller is handed to read the result back with.
func captureManifestPane(t *testing.T, manifest string) string {
	t.Helper()
	lines := strings.Split(manifest, "\n")
	for i, l := range lines {
		if !strings.Contains(l, "capture:") || i+1 >= len(lines) {
			continue
		}
		fields := strings.Fields(lines[i+1])
		if len(fields) >= 2 {
			return fields[0]
		}
	}
	t.Fatalf("no pane id in the --capture manifest:\n%s", manifest)
	return ""
}

// waitForCapture polls the read-back the way a caller is meant to: with
// `##cmd capture`, which touches no pane.
func waitForCapture(t *testing.T, s *daemontest.Session, paneID string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = s.MustSucceed(t, "##cmd capture "+paneID)
		if strings.Contains(last, "exit ") {
			return last
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("capture for %s never finished; last read-back:\n%s", paneID, last)
	return ""
}

// TestCaptureRoundTripsWithoutTypingIntoAPane is the whole fix in one pass: run
// a captured command, then get its output back through `##cmd capture`.
func TestCaptureRoundTripsWithoutTypingIntoAPane(t *testing.T) {
	s := daemontest.Fresh(t)

	manifest := s.MustSucceed(t, "##cmd pane --capture echo hello-from-the-capture")
	paneID := captureManifestPane(t, manifest)

	got := waitForCapture(t, s, paneID)
	if !strings.Contains(got, "hello-from-the-capture") {
		t.Errorf("read-back did not carry the command's output:\n%s", got)
	}
	if !strings.Contains(got, "exit 0") {
		t.Errorf("read-back did not report the exit status:\n%s", got)
	}

	// The listing is the other half: which captures exist, and their state.
	list := s.MustSucceed(t, "##cmd capture")
	if !strings.Contains(list, paneID) {
		t.Errorf("listing does not mention the capture just taken:\n%s", list)
	}

	// An id prefix is enough — the ids are UUIDs.
	if len(paneID) >= 8 {
		if short := s.MustSucceed(t, "##cmd capture "+paneID[:8]); !strings.Contains(short, "hello-from-the-capture") {
			t.Errorf("an unambiguous id prefix did not resolve:\n%s", short)
		}
	}
}

// TestCaptureExitStatusSurvivesTheRoundTrip: "finished with no output" and
// "still running" are indistinguishable without the sentinel, and a non-zero
// status is the reason most callers are reading at all.
func TestCaptureExitStatusSurvivesTheRoundTrip(t *testing.T) {
	s := daemontest.Fresh(t)

	manifest := s.MustSucceed(t, "##cmd pane --capture sh -c 'exit 3'")
	paneID := captureManifestPane(t, manifest)

	got := waitForCapture(t, s, paneID)
	if !strings.Contains(got, "exit 3") {
		t.Errorf("a failing captured command did not report its status:\n%s", got)
	}
}

// TestCaptureSkipsAPaneThatIsNotAtAShell is the broadcast half of the fix. The
// pane is made busy with a foreground program — which is exactly what a pane
// running claude looks like to the filter — and a --capture fan-out must leave
// it alone and SAY it did.
func TestCaptureSkipsAPaneThatIsNotAtAShell(t *testing.T) {
	s := daemontest.Fresh(t)

	// Occupy the pane. Nothing is captured from it and nothing should be typed
	// into it; before the fix this pane would have received the command as
	// keystrokes.
	s.MustSucceed(t, "##cmd pane sleep 30")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(s.MustSucceed(t, "##pane list"), "sleep") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Every pane in the workspace is busy, so a --capture fan-out matches none
	// of them — and that has to be a non-zero exit that names the reason, not a
	// quiet success or a bare "selector matched nothing".
	out := s.MustFail(t, "##cmd ws --capture pwd")
	if !strings.Contains(out, "--capture") || !strings.Contains(out, "shell") {
		t.Errorf("a --capture fan-out that hit no shell must explain the implied filter:\n%s", out)
	}

	// The same broadcast WITHOUT --capture still reaches the busy pane: the
	// filter is scoped to capture, and fanning a prompt out to agents is a
	// thing rysh is for.
	s.MustSucceed(t, "##cmd ws echo still-reaches-a-busy-pane")
}
