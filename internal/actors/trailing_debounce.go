// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"sync"
	"time"
)

// trailingDebounce coalesces bursts of calls into one fire after the burst:
// fire runs once wait after the last Trigger, and at the latest maxWait after
// the first Trigger of a burst, so a burst that never pauses (a held key) still
// fires about once per maxWait. fire runs on a timer goroutine. Safe for
// concurrent use.
type trailingDebounce struct {
	wait, maxWait time.Duration
	fire          func()

	mu    sync.Mutex
	timer *time.Timer
	first time.Time // first Trigger of the pending burst; zero when idle
	gen   uint64    // bumps on every (re)arm and Stop, so a stale timer does nothing
}

func newTrailingDebounce(wait, maxWait time.Duration, fire func()) *trailingDebounce {
	return &trailingDebounce{wait: wait, maxWait: maxWait, fire: fire}
}

// Trigger (re)arms the timer.
func (d *trailingDebounce) Trigger() {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	if d.first.IsZero() {
		d.first = now
	}
	delay := d.wait
	if left := d.maxWait - now.Sub(d.first); left < delay {
		delay = left
	}
	if delay < 0 {
		delay = 0
	}
	if d.timer != nil {
		d.timer.Stop()
	}
	d.gen++
	gen := d.gen
	d.timer = time.AfterFunc(delay, func() {
		d.mu.Lock()
		if gen != d.gen {
			d.mu.Unlock()
			return
		}
		d.first = time.Time{}
		d.timer = nil
		d.mu.Unlock()
		d.fire()
	})
}

// Stop drops a pending fire.
func (d *trailingDebounce) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	d.first = time.Time{}
	d.gen++
}
