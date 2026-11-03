// SPDX-License-Identifier: Apache-2.0

package conpty

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// errFakeBroken stands in for ERROR_BROKEN_PIPE: what a read of the console's
// output pipe returns once the pseudo-console has been closed.
var errFakeBroken = errors.New("fake: broken pipe")

func isFakeBroken(err error) bool { return errors.Is(err, errFakeBroken) }

// fakeConsoleOutput is the console's output pipe on any OS: Read blocks until
// the pseudo-console is released, which breaks the pipe, as ClosePseudoConsole
// does on Windows. Nothing else ends it: a shell exiting alone does not.
type fakeConsoleOutput struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func newFakeConsoleOutput() *fakeConsoleOutput {
	r, w := io.Pipe()
	return &fakeConsoleOutput{r: r, w: w}
}

func (f *fakeConsoleOutput) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	return eofOnBrokenPipe(n, err, isFakeBroken)
}

func (f *fakeConsoleOutput) releaseConsole() { _ = f.w.CloseWithError(errFakeBroken) }

// The pane's read loop ends on io.EOF. On Windows nothing ended it: the
// pseudo-console keeps its output pipe open after the shell exits (e20 review
// of 5c2f2aa, follow-up 1). watchExit must release the console once the shell
// has exited, and the broken pipe that follows must read as EOF.
func TestShellExitEndsTheConsoleRead(t *testing.T) {
	out := newFakeConsoleOutput()
	exited := make(chan struct{})
	var rel releaser
	rel.release = out.releaseConsole
	go watchExit(exited, &rel)

	got := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := out.Read(buf); err != nil {
				got <- err
				return
			}
		}
	}()

	select {
	case err := <-got:
		t.Fatalf("read ended before the shell exited: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(exited)
	select {
	case err := <-got:
		if err != io.EOF {
			t.Fatalf("read after shell exit = %v, want io.EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read still blocked 2s after the shell exited: the console was never released")
	}
}

// The shell's exit and the pane's Close both release the console; whichever
// comes first does it, and the other must not (ClosePseudoConsole twice is a
// double free of the HPCON).
func TestReleaserRunsOnce(t *testing.T) {
	var calls atomic.Int32
	var rel releaser
	rel.release = func() { calls.Add(1) }
	exited := make(chan struct{})
	go watchExit(exited, &rel)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); rel.do() }()
	}
	close(exited)
	wg.Wait()
	deadline := time.Now().Add(time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let watchExit's call land, if it were a second one
	if n := calls.Load(); n != 1 {
		t.Fatalf("release ran %d times, want exactly 1", n)
	}
}

func TestEOFOnBrokenPipe(t *testing.T) {
	other := errors.New("other")
	for _, tc := range []struct {
		n       int
		err     error
		wantN   int
		wantErr error
	}{
		{5, nil, 5, nil},
		{0, errFakeBroken, 0, io.EOF},
		{3, errFakeBroken, 3, io.EOF},
		{0, other, 0, other},
		{0, io.EOF, 0, io.EOF},
	} {
		n, err := eofOnBrokenPipe(tc.n, tc.err, isFakeBroken)
		if n != tc.wantN || err != tc.wantErr {
			t.Errorf("eofOnBrokenPipe(%d, %v) = (%d, %v), want (%d, %v)", tc.n, tc.err, n, err, tc.wantN, tc.wantErr)
		}
	}
}
