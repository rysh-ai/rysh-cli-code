// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"sort"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// wrapCacheEntry memoizes the result of wrapRows for one pane, with the
// per-logical-line bookkeeping that lets a grown buffer be re-wrapped
// incrementally: lineOff[i] is the byte offset in output where logical line i
// starts, rowOff[i] the index in rows of that line's first row.
type wrapCacheEntry struct {
	output  string
	width   int
	rows    []string
	lineOff []int
	rowOff  []int
}

// maxWrapReuseProbes bounds how many candidate line starts wrapIncremental
// tries when looking for where the old buffer's surviving tail begins in the
// new one. The match is the first or second probe in practice (a buffer grows
// by one chunk, and capTail trims at a line boundary); the bound keeps a
// pathological buffer from costing more than the full re-wrap it replaces.
const maxWrapReuseProbes = 16

// wrapRowsCached returns wrapRows(output, width) for a pane, reusing previous
// work. An unchanged buffer is a string comparison. A buffer that GREW — one
// streamed chunk appended, possibly with whole lines trimmed off the front by
// capTail — reuses the rows of every complete logical line it shares with the
// cached one and wraps only the rest. Re-wrapping the whole 256 KB buffer on
// every chunk was the TUI's dominant cost once panes had filled up (F-70). The
// returned slice is never mutated by callers (they copy into styledLines), so
// sharing the cached slice is safe.
func (m Model) wrapRowsCached(paneID, output string, width int) []string {
	if m.paneWrapCache == nil {
		return wrapRows(output, width)
	}
	e, ok := m.paneWrapCache[paneID]
	if ok && e.width == width && e.output == output {
		return e.rows
	}
	var next wrapCacheEntry
	reused := false
	if ok && e.width == width {
		next, reused = wrapIncremental(e, output)
	}
	if !reused {
		next = wrapFull(output, width)
	}
	m.paneWrapCache[paneID] = next
	return next.rows
}

// wrapRows splits an output string into logical lines and wraps each one to the
// given content width, returning the resulting on-screen display rows. Wrapping
// is ANSI-aware (colour escapes are preserved across wraps). A leading
// right-align sentinel (\x02) is kept on the first row of its logical line so
// the renderer can still right-align prompt echoes. Blank logical lines are
// preserved as a single blank row.
//
// This is the single source of truth for how many rows a pane's output occupies
// on screen; both the renderer and the scroll clamps measure rows through it so
// they can never disagree (which is what made wrapped output scroll/overflow
// incorrectly before).
func wrapRows(output string, width int) []string {
	return wrapFull(output, width).rows
}

// wrapFull wraps output from scratch, recording the line/row offsets.
func wrapFull(output string, width int) wrapCacheEntry {
	e := wrapCacheEntry{output: output, width: width}
	e.rows, e.lineOff, e.rowOff = wrapLinesFrom(output, width, 0, nil, nil, nil)
	return e
}

// wrapIncremental builds the entry for output from the cached entry old,
// provided output == old.output[old.lineOff[j]:] + appended for some line j.
// Every logical line of old from j up to (not including) its last is complete
// and unchanged in output, so its rows are reused as they are; old's last line
// may have been extended by the append, so it is wrapped again together with
// everything after it. Reports false when output does not continue old.
func wrapIncremental(old wrapCacheEntry, output string) (wrapCacheEntry, bool) {
	last := len(old.lineOff) - 1
	if last < 0 {
		return wrapCacheEntry{}, false
	}
	// len(output) = len(old) - lineOff[j] + len(appended), so the dropped
	// prefix is at least len(old)-len(output) bytes: start the search there.
	j := sort.SearchInts(old.lineOff, len(old.output)-len(output))
	for probes := 0; j <= last && probes < maxWrapReuseProbes; j, probes = j+1, probes+1 {
		if !strings.HasPrefix(output, old.output[old.lineOff[j]:]) {
			continue
		}
		drop, dropRows := old.lineOff[j], old.rowOff[j]
		rows := make([]string, 0, len(old.rows)-dropRows+8)
		rows = append(rows, old.rows[dropRows:old.rowOff[last]]...)
		lineOff := make([]int, 0, last-j+8)
		rowOff := make([]int, 0, last-j+8)
		for i := j; i < last; i++ {
			lineOff = append(lineOff, old.lineOff[i]-drop)
			rowOff = append(rowOff, old.rowOff[i]-dropRows)
		}
		rows, lineOff, rowOff = wrapLinesFrom(output, old.width, old.lineOff[last]-drop, rows, lineOff, rowOff)
		return wrapCacheEntry{output: output, width: old.width, rows: rows, lineOff: lineOff, rowOff: rowOff}, true
	}
	return wrapCacheEntry{}, false
}

// wrapLinesFrom wraps output[start:] — start must be the beginning of a
// logical line — appending to rows, lineOff and rowOff. Offsets are relative
// to the whole of output.
func wrapLinesFrom(output string, width, start int, rows []string, lineOff, rowOff []int) ([]string, []int, []int) {
	off := start
	for {
		line := output[off:]
		nl := strings.IndexByte(line, '\n')
		if nl >= 0 {
			line = line[:nl]
		}
		lineOff = append(lineOff, off)
		rowOff = append(rowOff, len(rows))
		rows = appendWrappedLine(rows, line, width)
		if nl < 0 {
			return rows, lineOff, rowOff
		}
		off += nl + 1
	}
}

// appendWrappedLine appends the display rows of one logical line.
func appendWrappedLine(rows []string, line string, width int) []string {
	if width <= 0 {
		return append(rows, line)
	}
	sentinel := ""
	body := line
	if strings.HasPrefix(line, "\x02") {
		sentinel = "\x02"
		body = line[len("\x02"):]
	}
	wrapped := strings.Split(ansi.Wrap(body, width, ""), "\n")
	for i, wl := range wrapped {
		if i == 0 {
			rows = append(rows, sentinel+wl)
		} else {
			rows = append(rows, wl)
		}
	}
	return rows
}
