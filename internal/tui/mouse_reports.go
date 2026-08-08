package tui

// Terminal mouse-reporting mode strings, used by the PTY relay when it takes
// the real terminal over from Bubble Tea.
const (
	// mouseTrackingOff disables every tracking mode and both extended
	// encodings, so the terminal stops emitting mouse reports entirely.
	mouseTrackingOff = "\x1b[?1006l\x1b[?1015l\x1b[?1003l\x1b[?1002l\x1b[?1000l\x1b[?9l"
	// mouseTrackingOn restores the tracking the TUI itself runs with. It must
	// match the Bubble Tea program options in cmd/rysh/main.go:
	// tea.WithMouseCellMotion is \x1b[?1002h plus SGR encoding \x1b[?1006h.
	mouseTrackingOn = "\x1b[?1002h\x1b[?1006h"
)

// maxPendingMouseReport bounds how many bytes the filter will hold waiting for
// a mouse report to complete. Real reports are far shorter (an SGR report with
// three-digit coordinates is 13 bytes); the cap exists so a malformed prefix
// can never stall input.
const maxPendingMouseReport = 24

// mouseReportFilter strips terminal mouse reports out of a raw input stream.
//
// The PTY relay uses it. While a relay is active the terminal has been told to
// stop reporting mouse events, so anything mouse-shaped still arriving is a
// report that was already in flight when the disable was sent — or one from a
// terminal that never processed it (a laggy mosh or ssh link). Those bytes are
// pumped verbatim into the relayed program, which never asked for mouse input:
// the relay is entered for any full-screen app regardless of its mouse mode.
// Claude Code, for one, has no mouse parser at all — it eats the escape
// introducer and types the remainder into its prompt as "<65;50;54M".
//
// Both wire formats are recognized: SGR (\x1b[< Cb ; Cx ; Cy {M|m}) and the
// legacy X10 form (\x1b[M followed by exactly three bytes). A report split
// across two reads is held in pending until it completes.
//
// A bare "\x1b" or "\x1b[" tail is deliberately NOT held: those also begin a
// lone Esc keypress and every arrow/function key, and the relay's double-Esc
// gesture depends on a lone Esc arriving without delay. Only the unambiguous
// "\x1b[<" and "\x1b[M" prefixes are buffered.
//
// If the relay ever starts enabling mouse tracking for the relayed child, this
// filter has to go — it would eat the child's own events too.
type mouseReportFilter struct {
	pending []byte
}

// filter returns data with any complete mouse reports removed. The returned
// slice may alias data when there is nothing to strip.
func (f *mouseReportFilter) filter(data []byte) []byte {
	if len(f.pending) == 0 && !containsESC(data) {
		return data // fast path: ordinary typing
	}
	if len(f.pending) > 0 {
		joined := make([]byte, 0, len(f.pending)+len(data))
		joined = append(joined, f.pending...)
		joined = append(joined, data...)
		data = joined
		f.pending = f.pending[:0]
	}

	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); {
		n, complete := mouseReportLen(data[i:])
		switch {
		case n == 0: // not a mouse report
			out = append(out, data[i])
			i++
		case complete:
			i += n // drop it
		case len(data)-i <= maxPendingMouseReport:
			// Truncated report at the end of the read: hold it for the next one.
			f.pending = append(f.pending[:0], data[i:]...)
			return out
		default:
			// Too long to be a real report — whatever it is, it is not ours.
			return append(out, data[i:]...)
		}
	}
	return out
}

func containsESC(data []byte) bool {
	for _, b := range data {
		if b == 0x1b {
			return true
		}
	}
	return false
}

// mouseReportLen reports the length of the mouse report at the front of b.
// n is 0 when b does not start with one. complete is false when b starts with a
// report that is cut short by the end of the buffer, in which case n is len(b).
func mouseReportLen(b []byte) (n int, complete bool) {
	if len(b) < 3 || b[0] != 0x1b || b[1] != '[' {
		return 0, false
	}
	switch b[2] {
	case 'M':
		// Legacy X10: \x1b[M plus exactly three coordinate/button bytes.
		if len(b) < 6 {
			return len(b), false
		}
		return 6, true
	case '<':
		// SGR: \x1b[< digits and semicolons, terminated by M or m.
		for i := 3; i < len(b); i++ {
			switch {
			case b[i] == 'M' || b[i] == 'm':
				return i + 1, true
			case b[i] >= '0' && b[i] <= '9', b[i] == ';':
				continue
			default:
				return 0, false // not a mouse report after all
			}
		}
		return len(b), false
	}
	return 0, false
}
