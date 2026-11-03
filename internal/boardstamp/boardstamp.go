// SPDX-License-Identifier: Apache-2.0

// Package boardstamp stamps posts that a door accepted for the reserved fleet
// board, so the recorder can tell them from raw posts that bypassed a door
// (design 030 §9.3, founder ruling D-24, wave W3; security-lead's notes,
// out/e20-030-w3-plan-notes.md @ root 88d9a5726).
//
// THE KEY: 32 bytes from crypto/rand, made once per process, held in memory
// only. It is never written to config, the bus, a log or a file, and it dies
// with the daemon, so a restart invalidates every earlier stamp. If it cannot
// be made, Stamp returns "" and Verify returns false: every fleet-board post
// then reads unverified (fail closed).
//
// WHAT THIS IS NOT: authentication across sessions or processes. Only the
// daemon that made the key can stamp or verify. A post relayed from another
// session has no valid stamp and reads unverified; cross-session trust is the
// signing design's job.
package boardstamp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

var (
	keyOnce sync.Once
	keyVal  []byte
)

// key returns the process key, or nil if it could not be made.
func key() []byte {
	keyOnce.Do(func() {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err == nil {
			keyVal = k
		}
	})
	return keyVal
}

// mac is HMAC-SHA256 over the post's bound fields on board.
func mac(k []byte, board string, p *msg.MsgBoardPost) []byte {
	body := sha256.Sum256([]byte(p.Text))
	h := hmac.New(sha256.New, k)
	for _, part := range []string{
		"rysh-fleetboard-stamp-v1",
		msg.NormalizeBoardID(board),
		p.PaneID,
		p.Kind,
		p.ThreadID,
		strconv.FormatInt(p.TS, 10),
		hex.EncodeToString(body[:]),
	} {
		h.Write([]byte(strconv.Itoa(len(part))))
		h.Write([]byte{':'})
		h.Write([]byte(part))
	}
	return h.Sum(nil)
}

// Stamp returns the stamp a door puts on p for board, or "" without a key.
func Stamp(board string, p *msg.MsgBoardPost) string {
	k := key()
	if k == nil || p == nil {
		return ""
	}
	return hex.EncodeToString(mac(k, board, p))
}

// Verify reports whether p carries a valid stamp for board.
func Verify(board string, p *msg.MsgBoardPost) bool {
	k := key()
	if k == nil || p == nil || len(p.Stamp) != 64 {
		return false
	}
	got, err := hex.DecodeString(p.Stamp)
	if err != nil {
		return false
	}
	return hmac.Equal(got, mac(k, board, p))
}

// ReplayGuard accepts each stamp once, and only for a post whose time is
// within the window of now; it keeps seen stamps only for that window, so its
// memory is bounded by the post rate times twice the window.
type ReplayGuard struct {
	window time.Duration
	mu     sync.Mutex
	seen   map[string]int64 // stamp -> post ts (unix millis)
}

// NewReplayGuard returns a guard for the window (e.g. 5 minutes).
func NewReplayGuard(window time.Duration) *ReplayGuard {
	return &ReplayGuard{window: window, seen: map[string]int64{}}
}

// Accept reports whether p's stamp is new and p's time is within the window
// of now. It does not check the stamp's validity; call Verify for that.
func (g *ReplayGuard) Accept(p *msg.MsgBoardPost, now time.Time) bool {
	if g == nil || p == nil || p.Stamp == "" {
		return false
	}
	n, w := now.UnixMilli(), g.window.Milliseconds()
	if p.TS < n-w || p.TS > n+w {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for s, ts := range g.seen {
		if ts < n-w {
			delete(g.seen, s)
		}
	}
	if _, dup := g.seen[p.Stamp]; dup {
		return false
	}
	g.seen[p.Stamp] = p.TS
	return true
}

// Len is how many stamps the guard holds.
func (g *ReplayGuard) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.seen)
}
