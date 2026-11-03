// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"errors"
	"testing"
	"time"
)

// The Windows console has no SIGWINCH, so the relay polls the console size and
// forwards only changes. pollTerminalSize is that loop.
func TestPollTerminalSizeSendsOnlyChanges(t *testing.T) {
	sizes := []struct {
		c, r int
		err  error
	}{
		{80, 24, nil}, // unchanged from the start size: nothing sent
		{100, 30, nil},
		{100, 30, nil},                     // unchanged
		{0, 0, errors.New("console gone")}, // an error sends nothing
		{120, 40, nil},
	}
	tick := make(chan time.Time)
	done := make(chan struct{})
	i := 0
	get := func() (int, int, error) { s := sizes[i]; i++; return s.c, s.r, s.err }
	var sent [][2]int
	finished := make(chan struct{})
	go func() {
		pollTerminalSize(done, tick, 80, 24, get, func(c, r int) { sent = append(sent, [2]int{c, r}) })
		close(finished)
	}()
	for range sizes {
		tick <- time.Time{}
	}
	close(done)
	<-finished
	want := [][2]int{{100, 30}, {120, 40}}
	if len(sent) != len(want) || sent[0] != want[0] || sent[1] != want[1] {
		t.Errorf("sent %v, want %v", sent, want)
	}
}
