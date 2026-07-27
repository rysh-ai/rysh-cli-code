package actors

// osc7.go — OSC 7 working-directory tracking (Phase 4 of bash-shell-mode).
//
// Terminals learn the shell's cwd through the OSC 7 escape sequence:
//
//	ESC ] 7 ; file://host/path BEL      (or ST = ESC \)
//
// rysh injects a PROMPT_COMMAND for bash panes that emits OSC 7 before every
// prompt (see startShell), and rawReadLoop feeds every PTY chunk through an
// osc7Scanner. The parsed path replaces lsof/proc polling as the primary cwd
// source for tab-completion and the shell prompt — it is push-based, exact,
// and works immediately after `cd` instead of after a 3s cache expiry.
// Shells whose user config already emits OSC 7 (zsh setups, Apple Terminal
// integration) are picked up by the same parser with no injection needed.

import (
	"bytes"
	"net/url"
	"strings"
)

// osc7MaxPending caps the carried-over bytes of an unterminated OSC 7
// sequence at a chunk boundary; anything longer is discarded as garbage.
const osc7MaxPending = 4096

// osc7Scanner extracts OSC 7 payloads from a PTY byte stream, tolerating
// sequences split across read-chunk boundaries. Used only by the pane's
// rawReadLoop goroutine — no locking needed.
type osc7Scanner struct {
	pending []byte
}

var osc7Prefix = []byte("\x1b]7;")

// feed scans a chunk and returns the path of the LAST complete OSC 7
// sequence in it (ok=false when none completed in this chunk).
func (s *osc7Scanner) feed(chunk []byte) (path string, ok bool) {
	data := chunk
	if len(s.pending) > 0 {
		data = append(s.pending, chunk...)
		s.pending = nil
	}
	for {
		i := bytes.Index(data, osc7Prefix)
		if i < 0 {
			// A partial "\x1b]7" at the very end of the chunk may be the
			// start of a sequence whose remainder arrives in the next read —
			// carry the longest data-suffix that prefixes osc7Prefix.
			for l := len(osc7Prefix) - 1; l > 0; l-- {
				if len(data) >= l && bytes.Equal(data[len(data)-l:], osc7Prefix[:l]) {
					s.pending = append(s.pending, data[len(data)-l:]...)
					break
				}
			}
			return path, ok
		}
		rest := data[i+len(osc7Prefix):]
		// Terminator: BEL or ST (ESC \), whichever comes first.
		term, tlen := -1, 0
		if j := bytes.IndexByte(rest, 0x07); j >= 0 {
			term, tlen = j, 1
		}
		if k := bytes.Index(rest, []byte("\x1b\\")); k >= 0 && (term < 0 || k < term) {
			term, tlen = k, 2
		}
		if term < 0 {
			// Unterminated at the chunk boundary — carry it over (bounded).
			if len(data)-i <= osc7MaxPending {
				s.pending = append(s.pending, data[i:]...)
			}
			return path, ok
		}
		if p := parseOSC7Payload(string(rest[:term])); p != "" {
			path, ok = p, true
		}
		data = rest[term+tlen:]
	}
}

// parseOSC7Payload extracts the filesystem path from a "file://host/path"
// OSC 7 payload ("" when the payload is not a file URL).
func parseOSC7Payload(payload string) string {
	rest, found := strings.CutPrefix(payload, "file://")
	if !found {
		return ""
	}
	// Strip the (possibly empty) host portion — the path starts at the first '/'.
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return ""
	}
	p := rest[i:]
	if unescaped, err := url.PathUnescape(p); err == nil {
		p = unescaped
	}
	return p
}
