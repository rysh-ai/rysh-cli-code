// SPDX-License-Identifier: Apache-2.0

package web

import (
	"sync"
	"time"
)

// Failed logins are capped per server, not per client address: behind the
// proxies and tunnels this UI is reached through, every request can carry the
// same address, so a per-address limit would be no limit at all. The price is
// that someone guessing can make the owner wait out the window too; a browser
// already signed in keeps its token and is not affected.
const (
	loginFailMax    = 10
	loginFailWindow = 10 * time.Minute
)

// loginLimiter remembers the failed logins inside the window. The zero value
// is ready to use.
type loginLimiter struct {
	mu    sync.Mutex
	fails []time.Time
	now   func() time.Time // nil ⇒ time.Now; tests set it
}

func (l *loginLimiter) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// prune drops failures older than the window. Callers hold mu.
func (l *loginLimiter) prune(now time.Time) {
	keep := l.fails[:0]
	for _, t := range l.fails {
		if now.Sub(t) < loginFailWindow {
			keep = append(keep, t)
		}
	}
	l.fails = keep
}

// blocked returns how long until the next attempt is allowed, or 0 when one
// is allowed now.
func (l *loginLimiter) blocked() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	l.prune(now)
	if len(l.fails) < loginFailMax {
		return 0
	}
	return l.fails[0].Add(loginFailWindow).Sub(now)
}

// fail records one failed attempt.
func (l *loginLimiter) fail() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	l.prune(now)
	l.fails = append(l.fails, now)
}
