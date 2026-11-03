// SPDX-License-Identifier: Apache-2.0

// Package familyserve is `rysh family serve` (design 093 §12.3 H3): a headless
// bridge between rysh-server and the family's rysh session inside its hosted
// container.
//
// The server keeps one long-lived `docker exec -i … rysh_local family serve`
// per family and speaks newline-delimited JSON with it over stdin/stdout. The
// protocol is fixed in out/rysh-family-h3-protocol.md (monorepo root):
//
//	server → serve   prompt{id,pane,text} · cancel{id} · ping
//	serve → server   ready · chunk · done · board · status · pong · error
//
// stdout carries protocol lines only; logs go to stderr. serve exits 0 when
// stdin closes.
//
// This package is deliberately separate from internal/familyclient: that one
// is a client of rysh-server's HTTP API, this one runs inside the family's
// session and talks to its daemon over the session NATS.
package familyserve

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"sync"
	"time"
	"unicode/utf8"
)

// Protocol limits.
const (
	// MaxLineBytes bounds one protocol line in either direction (64 KiB).
	MaxLineBytes = 64 * 1024
	// MaxPromptBytes bounds a prompt's text (8 KiB).
	MaxPromptBytes = 8 * 1024
	// DefaultQueueMax is how many prompts may wait behind a pane's running
	// turn; one more is answered `done` status `busy` at once.
	DefaultQueueMax = 8
	// chunkTextMax splits one streamed output into several chunk lines, so a
	// chunk line always fits MaxLineBytes even after JSON escaping.
	chunkTextMax = 8 * 1024
)

// Ops (server → serve).
const (
	OpPrompt = "prompt"
	OpCancel = "cancel"
	OpPing   = "ping"
)

// Event names (serve → server).
const (
	EvReady  = "ready"
	EvChunk  = "chunk"
	EvDone   = "done"
	EvBoard  = "board"
	EvStatus = "status"
	EvPong   = "pong"
	EvError  = "error"
)

// done statuses.
const (
	StatusDone      = "done"
	StatusError     = "error"
	StatusCancelled = "cancelled"
	StatusBusy      = "busy"
)

var (
	idRE   = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
	uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// ValidID reports whether id is a protocol turn id (≤64 chars of [A-Za-z0-9._:-]).
func ValidID(id string) bool { return idRE.MatchString(id) }

// ValidPane reports whether p is a pane uuid.
func ValidPane(p string) bool { return uuidRE.MatchString(p) }

// inLine is one decoded server line. Unknown fields are ignored.
type inLine struct {
	Op   string `json:"op"`
	ID   string `json:"id"`
	Pane string `json:"pane"`
	Text string `json:"text"`
}

// Outgoing events. Field names are the protocol's.
type readyEvent struct {
	Ev      string   `json:"ev"`
	Session string   `json:"session"`
	Version string   `json:"version"`
	Panes   []string `json:"panes"`
}

type chunkEvent struct {
	Ev   string `json:"ev"`
	ID   string `json:"id"`
	Pane string `json:"pane"`
	Text string `json:"text"`
}

type doneEvent struct {
	Ev     string `json:"ev"`
	ID     string `json:"id"`
	Pane   string `json:"pane"`
	Status string `json:"status"`
	Text   string `json:"text"`
	Error  string `json:"error"`
	// Truncated is an extension (the protocol ignores unknown fields): set
	// when Text was cut to keep the line within MaxLineBytes.
	Truncated bool `json:"truncated,omitempty"`
}

type boardEvent struct {
	Ev     string `json:"ev"`
	Kind   string `json:"kind"`
	Thread string `json:"thread"`
	From   string `json:"from"`
	To     string `json:"to"`
	Text   string `json:"text"`
	At     string `json:"at"`
}

type statusEvent struct {
	Ev    string `json:"ev"`
	Pane  string `json:"pane"`
	State string `json:"state"`
	Note  string `json:"note"`
	Pct   *int   `json:"pct"`
}

type simpleEvent struct {
	Ev string `json:"ev"`
}

type errorEvent struct {
	Ev      string `json:"ev"`
	Message string `json:"message"`
}

// boardTime renders a board post's unix-millis timestamp as RFC 3339 (UTC,
// millisecond precision).
func boardTime(ms int64) string {
	if ms <= 0 {
		ms = time.Now().UnixMilli()
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// lineWriter writes one JSON object per line and never interleaves two.
type lineWriter struct {
	mu  sync.Mutex
	w   io.Writer
	err error
}

func encodeLine(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil // Encode appends the newline
}

// emit writes v as one line. Events that carry free text go through emitFit;
// this only guards against a bug producing an oversized line.
func (lw *lineWriter) emit(v any) {
	b, err := encodeLine(v)
	if err != nil {
		return
	}
	if len(b) > MaxLineBytes {
		b, _ = encodeLine(errorEvent{Ev: EvError, Message: "internal: an event exceeded the line limit and was dropped"})
	}
	lw.write(b)
}

func (lw *lineWriter) write(b []byte) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.err != nil {
		return
	}
	_, lw.err = lw.w.Write(b)
}

// emitFit writes build(text) and, when the line would exceed MaxLineBytes,
// cuts text on a UTF-8 boundary until it fits. It reports whether text was cut.
func (lw *lineWriter) emitFit(text string, build func(text string, cut bool) any) bool {
	cut := false
	for {
		b, err := encodeLine(build(text, cut))
		if err != nil {
			return cut
		}
		if len(b) <= MaxLineBytes {
			lw.write(b)
			return cut
		}
		if text == "" {
			return cut // nothing left to cut; never write an oversized line
		}
		over := len(b) - MaxLineBytes
		n := len(text) - over - 64
		if q := len(text) * 3 / 4; n > q {
			n = q
		}
		if n < 0 {
			n = 0
		}
		text = truncUTF8(text, n)
		cut = true
	}
}

// truncUTF8 cuts s to at most n bytes without splitting a rune.
func truncUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// splitText splits s into pieces of at most max bytes on rune boundaries.
func splitText(s string, max int) []string {
	var out []string
	for len(s) > max {
		n := max
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		if n == 0 {
			n = max
		}
		out = append(out, s[:n])
		s = s[n:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}
