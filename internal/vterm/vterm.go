// Package vterm wraps a virtual terminal emulator (vt10x) that interprets
// PTY output into a 2D screen buffer suitable for rendering in the TUI.
// This enables support for interactive programs like vim, htop, and less.
package vterm

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/rysh-ai/rysh-cli-code/internal/vterm/vt10x"
)

// vt10x glyph mode attribute flags (matching private constants in vt10x/state.go).
const (
	glyphReverse   int16 = 1 << iota // 0x0001
	glyphUnderline                   // 0x0002
	glyphBold                        // 0x0004
	_                                // 0x0008 (gfx — not rendered)
	glyphItalic                      // 0x0010
	glyphBlink                       // 0x0020
	_                                // 0x0040 (wrap — not rendered)
	glyphFaint                       // 0x0080 (SGR 2 — faint/dim, e.g. ghost text)
)

// VTerm wraps a virtual terminal emulator that interprets PTY output
// into a 2D screen buffer suitable for rendering in the TUI.
type VTerm struct {
	mu        sync.Mutex
	term      vt10x.Terminal
	rows      int
	cols      int
	dirty     bool
	altScreen bool // true when alternate screen buffer is active
	// inAlt tracks our own authoritative alternate-screen state so we can drop
	// redundant alt-screen reset sequences before vt10x toggles on them (see
	// filterRedundantAltScreen).
	inAlt bool

	// pending holds a trailing INCOMPLETE escape sequence carried over from the
	// previous Write, so a sequence split across Write() boundaries is reassembled
	// before the per-write filters (stripUnsupportedCSI/filterRedundantAltScreen)
	// and vt10x ever scan it. This matters because the same byte stream is fed in
	// different chunk boundaries on the source (PTY reads) vs a mirror subscriber
	// (coalesced share frames): a frame ending mid-ESC[...] would otherwise corrupt
	// interactive output (top, claude) on the subscriber while the source stayed
	// clean. Capped at maxPendingEscape; a longer unterminated run is flushed as-is.
	// Guarded by mu.
	pending []byte

	// Render cache. RenderANSI/RenderANSIWithCursor memoize their output so a
	// burst of snapshot reads against an idle screen does not re-render every
	// cell (the previous behaviour: a full row-by-row SGR rebuild on every
	// snapshot tick). Invalidated on each content mutation (Write/Resize) via
	// invalidateRenderCache. Each render allocates a fresh slice, so a cached
	// slice handed to a caller is never mutated in place.
	//
	// Distinct from `dirty`, which is consumed externally (IsDirty/ClearDirty)
	// to drive snapshot/repaint scheduling and must not be disturbed here.
	ansiCache        []string
	ansiCacheValid   bool
	cursorCache      []string
	cursorCacheValid bool
}

// invalidateRenderCache clears the memoized ANSI renders. Caller must hold v.mu.
func (v *VTerm) invalidateRenderCache() {
	v.ansiCacheValid = false
	v.cursorCacheValid = false
}

// New creates a VTerm with the given dimensions.
func New(rows, cols int) *VTerm {
	return NewWithWriter(rows, cols, nil)
}

// NewWithWriter creates a VTerm with a response writer. The writer is used by
// terminal query responses such as CPR (ESC[6n -> ESC[row;colR), which
// interactive programs expect from a real terminal.
func NewWithWriter(rows, cols int, w io.Writer) *VTerm {
	vt := &VTerm{
		rows: rows,
		cols: cols,
	}
	opts := []vt10x.TerminalOption{vt10x.WithSize(cols, rows)}
	if w != nil {
		opts = append(opts, vt10x.WithWriter(w))
	}
	vt.term = vt10x.New(opts...)
	vt.term.SetScrollbackMax(scrollbackMaxLines)
	return vt
}

// Write feeds raw PTY output bytes into the VT emulator.
// Thread-safe: can be called from the PTY read goroutine.
//
// Before forwarding to vt10x, CSI sequences with private parameter markers
// '>', '<', or '=' are stripped. These are modern terminal protocol extensions
// (Kitty keyboard protocol, xterm modifyOtherKeys, etc.) that vt10x does not
// understand. Without filtering, vt10x misinterprets e.g. ESC[>1u (Kitty
// keyboard push) as ESC[u (SCORC — restore cursor), resetting the cursor to
// the origin and breaking inline TUI layout (Claude Code, Bubble Tea apps).
func (v *VTerm) Write(p []byte) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	origLen := len(p)
	// Reassemble an escape sequence split across Write() boundaries: prepend any
	// incomplete tail held from the previous Write, then hold back a new incomplete
	// tail (if this Write ends mid-sequence) for the next one. This guarantees the
	// per-write filters and vt10x never see a partial sequence — the root cause of
	// mirror-subscriber scramble when share frames split a sequence at a boundary
	// the source's PTY reads did not.
	if len(v.pending) > 0 {
		combined := make([]byte, 0, len(v.pending)+len(p))
		combined = append(combined, v.pending...)
		combined = append(combined, p...)
		p = combined
		v.pending = v.pending[:0]
	}
	idx := splitTrailingIncompleteEscape(p)
	if u := splitTrailingIncompleteUTF8(p); u < idx {
		idx = u
	}
	if idx < len(p) {
		v.pending = append(v.pending[:0], p[idx:]...)
		p = p[:idx]
	}
	filtered := stripUnsupportedCSI(p)
	filtered = v.filterRedundantAltScreen(filtered)
	_, err := v.term.Write(filtered)
	v.dirty = true
	v.invalidateRenderCache()
	// Detect alternate screen buffer transitions via the mode flag. vt10x is
	// now kept in sync because filterRedundantAltScreen suppresses the
	// redundant sequences that would toggle it the wrong way.
	v.altScreen = v.term.Mode()&vt10x.ModeAltScreen != 0
	// Return the ORIGINAL caller length — the caller "wrote" origLen bytes
	// regardless of internal buffering/filtering.
	return origLen, err
}

// isAltScreenMode reports whether a DEC private mode number swaps the alternate
// screen buffer in vt10x (1047, 1049, and the legacy 47).
func isAltScreenMode(n int) bool { return n == 47 || n == 1047 || n == 1049 }

// filterRedundantAltScreen drops alternate-screen RESET sequences
// (ESC[?47l / ESC[?1047l / ESC[?1049l) that arrive when the alt screen is
// already inactive.
//
// vt10x toggles its ModeAltScreen flag with XOR on every alt-screen mode
// sequence, and its guard treats a reset-while-not-in-alt as a reason to swap
// — so a redundant reset spuriously ENTERS the alt buffer. Interactive
// programs frequently emit more than one alt reset on teardown (e.g. terminfo
// rmcup expands to ESC[?1047l then ESC[?1049l, and Claude Code's exit emits a
// reset that the shell follows with another). The spurious re-entry left the
// pane stuck in raw mode rendering the shell's PS1 instead of restoring the
// rysh prompt.
//
// We track the alt-screen state ourselves (v.inAlt) and forward genuine
// transitions while dropping redundant resets, keeping vt10x in sync. Other
// bytes (including non-alt DEC private modes such as ?25l, ?2004h, ?1006l) pass
// through unchanged. A sequence split across Write calls is passed through
// as-is (the rare fallback to vt10x's native handling).
func (v *VTerm) filterRedundantAltScreen(p []byte) []byte {
	hasESC := false
	for _, b := range p {
		if b == 0x1b {
			hasESC = true
			break
		}
	}
	if !hasESC {
		return p
	}

	out := make([]byte, 0, len(p))
	i := 0
	for i < len(p) {
		// RIS (ESC c) — full terminal reset. vt10x's reset() assigns
		// t.mode = ModeWrap, clearing ModeAltScreen WITHOUT going through
		// setMode, so our v.inAlt latch never learns about it. Left desynced,
		// the next genuine alt-screen EXIT (?1049l) is forwarded while vt10x
		// believes it is not in alt — and vt10x swaps on `!set || !alt`, so the
		// exit ENTERS an empty alt buffer and every subsequent write becomes
		// invisible. `cat` of a binary file, and `reset`/`tput reset`, all emit
		// RIS. Re-sync the latch as the sequence passes through.
		if p[i] == 0x1b && i+1 < len(p) && p[i+1] == 'c' {
			v.inAlt = false
			out = append(out, p[i], p[i+1])
			i += 2
			continue
		}
		// Match a complete CSI DEC-private mode set/reset: ESC [ ? <params> (h|l)
		if p[i] == 0x1b && i+2 < len(p) && p[i+1] == '[' && p[i+2] == '?' {
			j := i + 3
			for j < len(p) && ((p[j] >= '0' && p[j] <= '9') || p[j] == ';') {
				j++
			}
			if j < len(p) && (p[j] == 'h' || p[j] == 'l') {
				set := p[j] == 'h'
				hasAlt := false
				for _, ps := range strings.Split(string(p[i+3:j]), ";") {
					if ps == "" {
						continue
					}
					if n, err := strconv.Atoi(ps); err == nil && isAltScreenMode(n) {
						hasAlt = true
						break
					}
				}
				if hasAlt {
					if set {
						v.inAlt = true // entering (vt10x is idempotent for set)
					} else if v.inAlt {
						v.inAlt = false // genuine exit
					} else {
						// Redundant reset while not in alt — drop it so vt10x
						// does not toggle back into the alt buffer.
						i = j + 1
						continue
					}
				}
				out = append(out, p[i:j+1]...)
				i = j + 1
				continue
			}
			// Incomplete sequence (likely split across chunks) — fall through
			// and copy byte-by-byte so vt10x can buffer it.
		}
		out = append(out, p[i])
		i++
	}
	return out
}

// stripUnsupportedCSI removes CSI sequences whose first parameter byte is a
// private-use marker ('>', '<', '=') that vt10x does not handle. These
// sequences are used by modern protocol extensions:
//
//   - ESC[>1u   — Kitty keyboard "push" (misread as SCORC → cursor to 0,0)
//   - ESC[<u    — Kitty keyboard "pop"  (misread as SCORC → cursor to 0,0)
//   - ESC[>4;2m — xterm modifyOtherKeys (misread as SGR)
//   - ESC[=...c — terminal identification
//
// CSI sequences with '?' (DEC private modes like ESC[?25l, ESC[?1049h) are
// kept because vt10x handles those correctly.
//
// The function scans for "ESC [" followed by '>', '<', or '=' and skips
// forward to the final byte (0x40–0x7E inclusive), removing the entire
// sequence from the output.
func stripUnsupportedCSI(p []byte) []byte {
	if len(p) == 0 {
		return p
	}
	// Fast path: if there is no ESC byte, return as-is (common case for
	// plain text output).
	hasESC := false
	for _, b := range p {
		if b == 0x1b {
			hasESC = true
			break
		}
	}
	if !hasESC {
		return p
	}

	out := make([]byte, 0, len(p))
	i := 0
	for i < len(p) {
		// Look for CSI introducer: ESC [
		if p[i] == 0x1b && i+2 < len(p) && p[i+1] == '[' {
			marker := p[i+2]
			if marker == '>' || marker == '<' || marker == '=' {
				// Skip the entire CSI sequence through its final byte.
				j := i + 3
				terminated := false
				for j < len(p) {
					if p[j] >= 0x40 && p[j] <= 0x7E {
						j++ // include the final byte
						terminated = true
						break
					}
					j++
				}
				if terminated {
					i = j
					continue
				}
				// No final byte in this buffer. Skipping to j (== len(p)) would
				// silently DISCARD everything after the introducer — real
				// command output included. Write's pending buffer normally
				// prevents an unterminated tail reaching us, but it gives up
				// past maxPendingEscape, so a long enough run lands here. Pass
				// the bytes through instead: vt10x may misparse a pathological
				// sequence, which is far better than destroying the output that
				// follows it.
				out = append(out, p[i:]...)
				i = len(p)
				continue
			}
		}
		out = append(out, p[i])
		i++
	}
	return out
}

// maxPendingEscape bounds how many trailing bytes Write holds back as a (possibly)
// incomplete escape sequence. Real sequences are short; a longer unterminated run
// is treated as data and flushed rather than buffered indefinitely.
const maxPendingEscape = 256

// splitTrailingIncompleteEscape returns the index where a trailing INCOMPLETE
// escape sequence begins, so Write can process p[:idx] and hold p[idx:] for the
// next call. Returns len(p) when p ends on a complete token (nothing to hold).
// Only the LAST ESC needs examining: in a well-formed stream every earlier
// sequence is already complete, so a split can only leave the tail unfinished.
func splitTrailingIncompleteEscape(p []byte) int {
	last := -1
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == 0x1b {
			last = i
			break
		}
	}
	if last < 0 {
		return len(p) // no ESC at all
	}
	if len(p)-last > maxPendingEscape {
		return len(p) // unterminated run too long to be a real sequence — flush it
	}
	if escapeComplete(p[last:]) {
		return len(p) // the trailing sequence is already complete
	}
	return last
}

// splitTrailingIncompleteUTF8 returns the index where a trailing INCOMPLETE UTF-8
// multibyte sequence begins, so Write can hold it for the next call. A continuation
// byte split from its lead byte across a Write boundary would otherwise be
// mis-decoded by vt10x as a replacement char of the wrong width, drifting the
// cursor on a mirror subscriber whose frame boundaries differ from the source's PTY
// reads. Returns len(p) when the tail is a complete character.
func splitTrailingIncompleteUTF8(p []byte) int {
	n := len(p)
	for i := 1; i <= 4 && i <= n; i++ {
		b := p[n-i]
		if b < 0x80 {
			return n // ASCII byte — tail is complete
		}
		if b >= 0xc0 { // UTF-8 lead byte: determine expected length
			need := 2
			switch {
			case b >= 0xf0:
				need = 4
			case b >= 0xe0:
				need = 3
			}
			if i < need {
				return n - i // not enough continuation bytes yet — hold from the lead
			}
			return n // complete
		}
		// 0x80–0xbf: continuation byte — keep scanning back for the lead.
	}
	return n
}

// escapeComplete reports whether seq (which begins with ESC, 0x1b) forms a complete
// escape sequence, so Write can decide whether to hold a trailing fragment.
func escapeComplete(seq []byte) bool {
	if len(seq) < 2 {
		return false // lone ESC
	}
	switch seq[1] {
	case '[': // CSI: ESC [ params... final(0x40-0x7E)
		for i := 2; i < len(seq); i++ {
			if seq[i] >= 0x40 && seq[i] <= 0x7E {
				return true
			}
		}
		return false
	case ']': // OSC: ESC ] ... terminated by BEL or ST (ESC \)
		for i := 2; i < len(seq); i++ {
			if seq[i] == 0x07 {
				return true
			}
			if seq[i] == 0x1b {
				return i+1 < len(seq) && seq[i+1] == '\\'
			}
		}
		return false
	case 'P', 'X', '^', '_': // DCS/SOS/PM/APC: terminated by ST (ESC \)
		for i := 2; i < len(seq); i++ {
			if seq[i] == 0x1b {
				return i+1 < len(seq) && seq[i+1] == '\\'
			}
		}
		return false
	case '(', ')', '*', '+': // charset designator: ESC ( X — needs 3 bytes
		return len(seq) >= 3
	default:
		return true // 2-byte escape (ESC 7, ESC 8, ESC M, ESC =, ESC >, ...)
	}
}

// Resize changes the virtual terminal dimensions.
func (v *VTerm) Resize(rows, cols int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.rows = rows
	v.cols = cols
	v.term.Resize(cols, rows)
	v.dirty = true
	v.invalidateRenderCache()
}

// Render produces the current screen content as a slice of strings (one per row).
func (v *VTerm) Render() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	lines := make([]string, v.rows)
	for row := 0; row < v.rows; row++ {
		var sb strings.Builder
		for col := 0; col < v.cols; col++ {
			cell := v.term.Cell(col, row)
			ch := cell.Char
			if ch == 0 {
				sb.WriteRune(' ')
			} else {
				sb.WriteRune(ch)
			}
		}
		lines[row] = strings.TrimRight(sb.String(), " ")
	}
	return lines
}

// RenderANSI produces the current screen content as a slice of strings with
// ANSI escape sequences that preserve foreground/background colours and text
// attributes (bold, italic, underline, blink, reverse). Trailing cells that
// are plain spaces with default colours are trimmed to keep lines compact.
func (v *VTerm) RenderANSI() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.ansiCacheValid {
		return v.ansiCache
	}
	v.ansiCache = v.renderANSILocked(false)
	v.ansiCacheValid = true
	return v.ansiCache
}

// RenderANSIWithCursor is like RenderANSI but draws a block cursor (reverse
// video on the cursor cell) when the cursor is visible. It is used for the
// in-pane VT rendering: the TUI renders the screen as plain text and cannot
// position a real hardware cursor, so without this the cursor of interactive
// programs such as vim would be invisible. When the cursor is hidden
// (\x1b[?25l, e.g. Bubble Tea apps like Claude Code) no cursor is drawn.
func (v *VTerm) RenderANSIWithCursor() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.cursorCacheValid {
		return v.cursorCache
	}
	v.cursorCache = v.renderANSILocked(true)
	v.cursorCacheValid = true
	return v.cursorCache
}

// renderANSILocked renders the screen to ANSI strings. The caller must hold
// v.mu. When drawCursor is true and the cursor is visible, the cursor cell is
// drawn with reverse video.
func (v *VTerm) renderANSILocked(drawCursor bool) []string {
	curRow, curCol := -1, -1
	if drawCursor && v.term.Mode()&vt10x.ModeHide == 0 {
		cur := v.term.Cursor()
		curRow, curCol = cur.Y, cur.X
	}

	lines := make([]string, v.rows)
	for row := 0; row < v.rows; row++ {
		// Find the last column that carries visible content or non-default style.
		lastCol := -1
		for col := v.cols - 1; col >= 0; col-- {
			cell := v.term.Cell(col, row)
			ch := cell.Char
			if ch == 0 {
				ch = ' '
			}
			if ch != ' ' || cell.FG != vt10x.DefaultFG || cell.BG != vt10x.DefaultBG ||
				cell.Mode&(glyphBold|glyphFaint|glyphItalic|glyphUnderline|glyphBlink|glyphReverse) != 0 {
				lastCol = col
				break
			}
		}
		// Ensure the cursor cell is rendered even if it lands on trailing blanks
		// that would otherwise be trimmed away.
		if row == curRow && curCol > lastCol && curCol < v.cols {
			lastCol = curCol
		}
		if lastCol < 0 {
			lines[row] = ""
			continue
		}

		var sb strings.Builder
		prevFG := vt10x.DefaultFG
		prevBG := vt10x.DefaultBG
		var prevMode int16

		for col := 0; col <= lastCol; col++ {
			cell := v.term.Cell(col, row)
			fg, bg, mode := cell.FG, cell.BG, cell.Mode
			// Draw the block cursor by toggling reverse video on its cell. XOR
			// keeps it visible even over an already-reversed cell (e.g. a
			// selection): the cursor always contrasts with its surroundings.
			if row == curRow && col == curCol {
				mode ^= glyphReverse
			}
			if col == 0 || fg != prevFG || bg != prevBG || mode != prevMode {
				sb.WriteString(buildSGR(fg, bg, mode))
				prevFG = fg
				prevBG = bg
				prevMode = mode
			}
			ch := cell.Char
			if ch == 0 {
				ch = ' '
			}
			sb.WriteRune(ch)
		}
		sb.WriteString("\x1b[0m") // reset to prevent style bleeding
		lines[row] = sb.String()
	}
	return lines
}

// scrollbackMaxLines bounds how many scrolled-off main-screen lines each VTerm
// retains for raw-pane scrollback.
const scrollbackMaxLines = 2000

// glyphAt returns the glyph at col, or a default blank glyph when col is past
// the (trimmed) row length.
func glyphAt(cells []vt10x.Glyph, col int) vt10x.Glyph {
	if col >= 0 && col < len(cells) {
		return cells[col]
	}
	return vt10x.Glyph{FG: vt10x.DefaultFG, BG: vt10x.DefaultBG}
}

// renderGlyphsANSI renders a single row of glyphs to an ANSI string using the
// same SGR-run encoding as renderANSILocked, trimming trailing blank/default
// cells. Used to render scrollback history rows (which carry no cursor).
func renderGlyphsANSI(cells []vt10x.Glyph, cols int) string {
	lastCol := -1
	for col := cols - 1; col >= 0; col-- {
		cell := glyphAt(cells, col)
		ch := cell.Char
		if ch == 0 {
			ch = ' '
		}
		if ch != ' ' || cell.FG != vt10x.DefaultFG || cell.BG != vt10x.DefaultBG ||
			cell.Mode&(glyphBold|glyphFaint|glyphItalic|glyphUnderline|glyphBlink|glyphReverse) != 0 {
			lastCol = col
			break
		}
	}
	if lastCol < 0 {
		return ""
	}

	var sb strings.Builder
	prevFG := vt10x.DefaultFG
	prevBG := vt10x.DefaultBG
	var prevMode int16
	for col := 0; col <= lastCol; col++ {
		cell := glyphAt(cells, col)
		fg, bg, mode := cell.FG, cell.BG, cell.Mode
		if col == 0 || fg != prevFG || bg != prevBG || mode != prevMode {
			sb.WriteString(buildSGR(fg, bg, mode))
			prevFG = fg
			prevBG = bg
			prevMode = mode
		}
		ch := cell.Char
		if ch == 0 {
			ch = ' '
		}
		sb.WriteRune(ch)
	}
	sb.WriteString("\x1b[0m")
	return sb.String()
}

// SetScrollbackMax sets how many scrolled-off lines the emulator retains.
func (v *VTerm) SetScrollbackMax(n int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.term.SetScrollbackMax(n)
}

// ScrollbackLen returns the number of retained scrollback lines.
func (v *VTerm) ScrollbackLen() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.term.ScrollbackLen()
}

// ClearScrollback discards all retained scrollback history.
func (v *VTerm) ClearScrollback() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.term.ClearScrollback()
}

// ScrollbackANSI returns the scrollback history rendered to ANSI strings,
// oldest line first.
func (v *VTerm) ScrollbackANSI() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	sb := v.term.Scrollback()
	out := make([]string, len(sb))
	for i, row := range sb {
		out[i] = renderGlyphsANSI(row, v.cols)
	}
	return out
}

// ScrollbackEvictedTotal returns the monotonic count of lines ever captured
// into scrollback. Lets a consumer compute how many new lines appeared since
// it last checked, even after the ring has dropped older lines.
func (v *VTerm) ScrollbackEvictedTotal() int64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.term.ScrollbackEvictedTotal()
}

// ScrollbackTailANSI returns the last n scrollback lines rendered to ANSI
// strings, oldest of the returned set first (fewer if the ring holds fewer).
func (v *VTerm) ScrollbackTailANSI(n int) []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	tail := v.term.ScrollbackTail(n)
	out := make([]string, len(tail))
	for i, row := range tail {
		out[i] = renderGlyphsANSI(row, v.cols)
	}
	return out
}

// buildSGR returns a single SGR escape sequence encoding the given foreground,
// background, and attribute flags. If everything is at default, it emits a
// reset (\x1b[0m).
func buildSGR(fg, bg vt10x.Color, mode int16) string {
	// Always open a run with a reset (0) so attributes from a prior run on the
	// same line — bold, faint, reverse, … — cannot bleed forward into this one.
	// vt10x cells carry absolute state, so re-specifying from reset is correct,
	// and it keeps faint (Ink/Claude "dimColor" ghost text) from leaking past
	// the dim span. A fully-default run renders as a bare "\x1b[0m".
	parts := []string{"0"}

	// Text attributes
	if mode&glyphBold != 0 {
		parts = append(parts, "1")
	}
	if mode&glyphFaint != 0 {
		parts = append(parts, "2")
	}
	if mode&glyphItalic != 0 {
		parts = append(parts, "3")
	}
	if mode&glyphUnderline != 0 {
		parts = append(parts, "4")
	}
	if mode&glyphBlink != 0 {
		parts = append(parts, "5")
	}
	if mode&glyphReverse != 0 {
		parts = append(parts, "7")
	}

	// Foreground colour
	parts = appendColorSGR(parts, fg, true)

	// Background colour
	parts = appendColorSGR(parts, bg, false)

	return "\x1b[" + strings.Join(parts, ";") + "m"
}

// appendColorSGR appends the appropriate SGR parameter(s) for a vt10x.Color.
// isFG selects foreground (30/38) vs background (40/48) codes.
func appendColorSGR(parts []string, c vt10x.Color, isFG bool) []string {
	if c == vt10x.DefaultFG || c == vt10x.DefaultBG || c >= 1<<24 {
		return parts // default / sentinel — no colour code
	}
	ci := int(c) // safe: we've excluded sentinels above
	if isFG {
		switch {
		case ci < 8:
			parts = append(parts, fmt.Sprintf("%d", 30+ci))
		case ci < 16:
			parts = append(parts, fmt.Sprintf("%d", 90+ci-8))
		case ci < 256:
			parts = append(parts, fmt.Sprintf("38;5;%d", ci))
		default:
			// True colour: r<<16 | g<<8 | b
			r, g, b := (ci>>16)&0xFF, (ci>>8)&0xFF, ci&0xFF
			parts = append(parts, fmt.Sprintf("38;2;%d;%d;%d", r, g, b))
		}
	} else {
		switch {
		case ci < 8:
			parts = append(parts, fmt.Sprintf("%d", 40+ci))
		case ci < 16:
			parts = append(parts, fmt.Sprintf("%d", 100+ci-8))
		case ci < 256:
			parts = append(parts, fmt.Sprintf("48;5;%d", ci))
		default:
			r, g, b := (ci>>16)&0xFF, (ci>>8)&0xFF, ci&0xFF
			parts = append(parts, fmt.Sprintf("48;2;%d;%d;%d", r, g, b))
		}
	}
	return parts
}

// Repaint returns a byte sequence that, when written to a freshly created
// terminal emulator, reconstructs the current screen contents and cursor
// position. It is used to resume an interactive share session for a
// (re)joining subscriber whose remote VTerm starts empty: the source replays
// this so the subscriber sees the live app immediately instead of waiting for
// the app to redraw on its own.
//
// The sequence clears the screen, then positions the cursor explicitly at the
// start of each row before writing that row's ANSI-styled content (avoiding
// any reliance on auto-wrap), and finally restores the real cursor position.
func (v *VTerm) Repaint() []byte {
	// RenderANSI and CursorPos each take the lock; call them outside any lock
	// held here to avoid a deadlock.
	lines := v.RenderANSI()
	row, col := v.CursorPos()

	var sb strings.Builder
	sb.WriteString("\x1b[2J") // clear entire screen
	for i, line := range lines {
		// Move to the start of row i (1-based) before writing its content.
		fmt.Fprintf(&sb, "\x1b[%d;1H", i+1)
		sb.WriteString(line)
	}
	// Restore the cursor to its current position (1-based).
	fmt.Fprintf(&sb, "\x1b[%d;%dH", row+1, col+1)
	return []byte(sb.String())
}

// CursorPos returns the current cursor position (row, col), 0-indexed.
func (v *VTerm) CursorPos() (row, col int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	cur := v.term.Cursor()
	return cur.Y, cur.X
}

// IsDirty returns true if the screen has been written to since last ClearDirty.
func (v *VTerm) IsDirty() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.dirty
}

// ClearDirty resets the dirty flag.
func (v *VTerm) ClearDirty() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.dirty = false
}

// IsAltScreen returns true if the alternate screen buffer is active.
// This indicates an interactive fullscreen program (vim, htop, less, etc.).
func (v *VTerm) IsAltScreen() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.altScreen
}

// IsMouseEnabled returns true if any mouse tracking mode is active.
// Programs that want mouse input (vim, htop) enable tracking via
// \x1b[?1000h, \x1b[?1002h, \x1b[?1003h, etc. When mouse tracking
// is NOT enabled, mouse events should not be forwarded to the PTY —
// the shell would display them as garbled text.
func (v *VTerm) IsMouseEnabled() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.term.Mode()&vt10x.ModeMouseMask != 0
}

// Mouse tracking protocols a child program can request. These name the tracking
// MODE — which events the child wants reported. The wire ENCODING is a separate
// axis, reported by MouseProtocol's sgr return.
const (
	MouseOff    = ""       // no tracking
	MouseX10    = "x10"    // \x1b[?9h    — button presses only, no release, no wheel
	MouseNormal = "normal" // \x1b[?1000h — press + release
	MouseButton = "button" // \x1b[?1002h — press + release + motion while a button is held
	MouseAny    = "any"    // \x1b[?1003h — press + release + all motion
)

// MouseProtocol reports what the child program actually asked for: its tracking
// mode (one of the Mouse* constants, MouseOff when tracking is off) and whether
// it also enabled SGR extended encoding (\x1b[?1006h).
//
// Both halves matter when synthesizing an event for the child. A program that
// enabled \x1b[?1000h WITHOUT \x1b[?1006h expects the legacy X10 byte encoding
// (\x1b[M Cb+32 Cx+32 Cy+32) and cannot parse an SGR report (\x1b[<0;5;7M) —
// a line editor handed one typically eats the escape introducer and echoes the
// rest into its input buffer as literal text.
func (v *VTerm) MouseProtocol() (mode string, sgr bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	m := v.term.Mode()
	sgr = m&vt10x.ModeMouseSgr != 0
	// vt10x clears the whole tracking mask before setting a new mode, so at most
	// one of these is ever set; the order is belt-and-braces.
	switch {
	case m&vt10x.ModeMouseMany != 0:
		return MouseAny, sgr
	case m&vt10x.ModeMouseMotion != 0:
		return MouseButton, sgr
	case m&vt10x.ModeMouseButton != 0:
		return MouseNormal, sgr
	case m&vt10x.ModeMouseX10 != 0:
		return MouseX10, sgr
	}
	return MouseOff, sgr
}

// ResetMouseModes turns every mouse tracking and encoding mode off
// (\x1b[?9l, ?1000l, ?1002l, ?1003l, ?1006l).
//
// The emulator outlives the programs that run in the pane: it is created once
// and fed every byte that pane's PTY ever produces. A program that enables
// tracking and dies without disabling it — killed with Ctrl+C, crashed, or
// simply sloppy on teardown — therefore leaves the tracking bit set for the
// rest of the pane's life, and every later program looks like it wants mouse
// reports. The TUI then forwards wheel and click events into the PTY of
// something that never asked for them (Claude Code, a bare shell), which shows
// them as literal text like "<65;50;54M".
//
// Callers reset when the pane's foreground program changes; anything that wants
// tracking re-enables it on startup.
func (v *VTerm) ResetMouseModes() {
	v.mu.Lock()
	defer v.mu.Unlock()
	// Fed through the emulator's own parser rather than poking mode bits so this
	// stays correct if vt10x changes how it tracks them. Written straight to the
	// terminal instead of through VTerm.Write: mode changes touch no cells, so
	// the pending partial-escape buffer, the dirty flag and the render caches
	// must all stay as they are. vt10x only ever sees complete sequences (Write
	// holds incomplete tails back), so injecting here lands on a parse boundary.
	_, _ = v.term.Write([]byte("\x1b[?9l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l"))
}

// IsAppCursorKeys returns true while DECCKM (\x1b[?1h, terminfo smkx) is
// active: the child program expects cursor keys in the SS3 "application"
// encoding (\x1bOA..\x1bOD) instead of CSI (\x1b[A..\x1b[D). less and other
// termcap-driven programs IGNORE the CSI form entirely, so web/mobile clients
// must consult this before synthesizing arrow keys.
func (v *VTerm) IsAppCursorKeys() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.term.Mode()&vt10x.ModeAppCursor != 0
}

// IsCursorHidden returns true if the terminal cursor is hidden (\x1b[?25l).
// TUI frameworks like Bubble Tea (used by Claude Code) hide the cursor on
// startup even when they don't use alternate screen mode.
func (v *VTerm) IsCursorHidden() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.term.Mode()&vt10x.ModeHide != 0
}

// Rows returns the current number of rows.
func (v *VTerm) Rows() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.rows
}

// Cols returns the current number of columns.
func (v *VTerm) Cols() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.cols
}
