package actors

import (
	"os"
	"testing"
	"time"
)

// readWithin reads once from r, failing if nothing arrives before the deadline.
func readWithin(t *testing.T, r *os.File, d time.Duration) string {
	t.Helper()
	if err := r.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Skipf("read deadlines unsupported: %v", err)
	}
	buf := make([]byte, 256)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(buf[:n])
}

// readNWithin accumulates reads from r until it holds n bytes. Pipe reads can
// split or merge writes, so tests that care about byte ORDER compare against
// this rather than against individual reads.
func readNWithin(t *testing.T, r *os.File, n int, d time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(d)
	var got string
	for len(got) < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out with %q (%d/%d bytes)", got, len(got), n)
		}
		got += readWithin(t, r, time.Until(deadline))
	}
	return got
}

// TestSubmitToPTYUsesCarriageReturn is the regression test for `##cmd` (and
// pane_send, and a remote-controller command) typing into a pane that runs an
// inline TUI such as claude: the text arrived but was never submitted, because
// the terminator was a line feed. A raw-mode TUI submits on CR — LF just
// inserts a newline in its input box.
func TestSubmitToPTYUsesCarriageReturn(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()

	// A plain pipe has no foreground process group, so computeInteractive falls
	// back to the VTerm heuristics — all off here, i.e. a shell at its prompt.
	p := &PaneActor{ptyFile: w}
	p.submitToPTY("echo hi")
	if got, want := readNWithin(t, r, len("echo hi\r"), time.Second), "echo hi\r"; got != want {
		t.Fatalf("shell submit wrote %q, want %q", got, want)
	}
}

// TestSubmitToPTYDefersEnterForInteractivePane pins the second half of the fix:
// against an interactive program the Enter is a separate write, delayed past
// the burst carrying the text, so the TUI's paste detection cannot swallow it.
func TestSubmitToPTYDefersEnterForInteractivePane(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()

	p := &PaneActor{ptyFile: w, rawMode: true} // alt screen ⇒ interactive
	p.submitToPTY("fix the tests")
	if got, want := readWithin(t, r, time.Second), "fix the tests"; got != want {
		t.Fatalf("interactive submit wrote %q first, want %q with no terminator", got, want)
	}
	if got, want := readWithin(t, r, 2*time.Second), "\r"; got != want {
		t.Fatalf("deferred Enter was %q, want %q", got, want)
	}
}

// TestSubmitToPTYKeepsBackToBackCommandsInOrder is what the submit queue buys:
// a second command handed to an interactive pane while the first one's Enter is
// still pending must not overtake it. Unqueued, these two would reach the TUI as
// "first" + "second" + CR + CR — one concatenated prompt, submitted twice.
func TestSubmitToPTYKeepsBackToBackCommandsInOrder(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()

	p := &PaneActor{ptyFile: w, rawMode: true}
	p.submitToPTY("first")
	p.submitToPTY("second")

	want := "first\rsecond\r"
	if got := readNWithin(t, r, len(want), 5*time.Second); got != want {
		t.Fatalf("back-to-back submits wrote %q, want %q", got, want)
	}
}

// TestSubmitQueueRunsEveryItemOnce covers the drain loop's hand-off: work
// enqueued while the goroutine is draining is picked up by that goroutine
// rather than dropped or run twice.
func TestSubmitQueueRunsEveryItemOnce(t *testing.T) {
	var q ptySubmitQueue
	const n = 50
	order := make(chan int, n)
	for i := 0; i < n; i++ {
		q.enqueue(func() { order <- i })
	}
	for i := 0; i < n; i++ {
		select {
		case got := <-order:
			if got != i {
				t.Fatalf("item %d ran in position %d", got, i)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d items ran", i, n)
		}
	}
	select {
	case extra := <-order:
		t.Fatalf("item %d ran twice", extra)
	case <-time.After(50 * time.Millisecond):
	}
}
