// SPDX-License-Identifier: Apache-2.0

package board

// Getting messages OUT of the board: `##board copy`, `##board output`, the
// board pane's y / Y / o keys, `rysh board copy|output|tail`, and the web
// client's clipboard_copy of a board pane.
//
// ONE ITEM MODEL, AND EVERY SURFACE USES IT. An item is one agent message — a
// thread's root post or one of its replies — never a line. Flatten turns a
// recorder answer into items, Select picks by worker and count, FormatItems
// renders the clipboard text and RenderHTML (html.go) the shareable page.
// There is no second filter anywhere: surfaces that each grew their own would
// disagree about what "issue-314's last 3" means the first time one of them
// was touched.
//
// THE TEXT IS UNWRAPPED. The board pane wraps bodies to its width; that is a
// display concern and never reaches a copy. A body comes out byte for byte as
// posted — newlines, 200-character URLs and shas intact — less only the
// terminal escape sequences SanitiseBody strips, because posts are written by
// agents and an escape pasted into a terminal is live.

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// CopyMaxBytes is the largest clipboard copy any surface makes. It matches
// internal/web/clipboard.go's clipboardMaxBytes (pinned by a test there). It
// bounds the CLIPBOARD only; `output` writes a file and has no cap.
const CopyMaxBytes = 256 * 1024

// Item is one agent message.
type Item struct {
	Post      *msg.MsgBoardPost // a sanitised copy; never the store's own pointer
	ThreadKey string
	Reply     bool
}

// Flatten turns a recorder answer into items, oldest first by the poster's
// clock (ties keep the board's arrival order). Every post is a sanitised COPY:
// Store.Threads() hands out the store's own pointers, and scrubbing them in
// place would scrub the live board.
func Flatten(r *QueryReply) []Item { return FlattenWhere(r, nil) }

// FlattenWhere is Flatten keeping only the posts keep accepts, judged on the
// ORIGINAL post (so a caller can select by identity, e.g. the posts a pane has
// on screen). A nil keep keeps everything.
func FlattenWhere(r *QueryReply, keep func(*msg.MsgBoardPost) bool) []Item {
	if r == nil {
		return nil
	}
	var items []Item
	for _, t := range r.Threads {
		if t.Root != nil && (keep == nil || keep(t.Root)) {
			items = append(items, Item{Post: sanitisedPost(t.Root), ThreadKey: t.Key})
		}
		for _, rep := range t.Replies {
			if rep != nil && (keep == nil || keep(rep)) {
				items = append(items, Item{Post: sanitisedPost(rep), ThreadKey: t.Key, Reply: true})
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Post.TS < items[j].Post.TS })
	return items
}

// Select picks items. worker "" is everyone; otherwise it must match a
// persona EXACTLY — `issue-31` never matches `issue-314`, the mistake a prefix
// match makes on any fleet with more than nine units — or a pane id, whole or
// by a prefix of at least 8 characters (optionally spelt `pane-<prefix>`, the
// persona fallback's spelling). n == 0 is all; otherwise the last n matched.
//
// matched and total are returned so every caller can say "k of m": matched
// counts the worker's items, total the whole window.
func Select(items []Item, worker string, n int) (picked []Item, matched, total int) {
	total = len(items)
	var hits []Item
	for _, it := range items {
		if worker == "" || ItemByWorker(it, worker) {
			hits = append(hits, it)
		}
	}
	matched = len(hits)
	if n > 0 && n < len(hits) {
		hits = hits[len(hits)-n:]
	}
	return hits, matched, total
}

// ItemByWorker is Select's worker predicate.
func ItemByWorker(it Item, worker string) bool {
	p := it.Post
	if p == nil || worker == "" {
		return p != nil
	}
	if p.Persona == worker || msg.BoardPersona(p.Persona, "", p.PaneID) == worker {
		return true
	}
	id := strings.TrimPrefix(worker, "pane-")
	if p.PaneID == "" || id == "" {
		return false
	}
	return p.PaneID == id || (len(id) >= 8 && strings.HasPrefix(p.PaneID, id))
}

// FormatItems is the clipboard text: per item a header line and the raw body,
// a blank line between items, oldest first, no trailing newline (a stray
// newline pasted into a shell runs whatever came before it).
//
//	● <persona>  [<kind>]  <YYYY-MM-DD HH:MM:SS>
//	<original post text>
func FormatItems(items []Item) string {
	blocks := make([]string, 0, len(items))
	for _, it := range items {
		p := it.Post
		head := "● " + SanitiseInline(msg.BoardPersona(p.Persona, "", p.PaneID))
		if k := SanitiseInline(p.Kind); k != "" {
			head += "  [" + k + "]"
		}
		head += "  " + ItemClock(p.TS)
		blocks = append(blocks, head+"\n"+SanitiseBody(p.Text))
	}
	return strings.Join(blocks, "\n\n")
}

// ItemClock is the full local date-time of a post, the poster's clock.
func ItemClock(ts int64) string {
	if ts <= 0 {
		return "----------- --:--:--"
	}
	return time.UnixMilli(ts).Format("2006-01-02 15:04:05")
}

// ItemsReply rebuilds a QueryReply holding only the picked items, grouped
// back into their threads in thread order — the JSON a filtered
// `rysh board tail --json` prints, in the shape scripts already parse.
func ItemsReply(r *QueryReply, picked []Item) QueryReply {
	out := QueryReply{Threads: []Thread{}, Roster: []RosterEntry{}}
	if r == nil {
		return out
	}
	out.Roster, out.Stats, out.Filtered, out.Withheld, out.RosterReconciled =
		r.Roster, r.Stats, r.Filtered, r.Withheld, r.RosterReconciled
	byKey := map[string]*Thread{}
	var order []string
	for _, it := range picked {
		t := byKey[it.ThreadKey]
		if t == nil {
			t = &Thread{Key: it.ThreadKey, Replies: []*msg.MsgBoardPost{}}
			byKey[it.ThreadKey] = t
			order = append(order, it.ThreadKey)
		}
		if it.Reply {
			t.Replies = append(t.Replies, it.Post)
		} else {
			t.Root = it.Post
		}
	}
	// Thread order as the board has it, not as the picks happened to land.
	rank := map[string]int{}
	for i, t := range r.Threads {
		rank[t.Key] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return rank[order[i]] < rank[order[j]] })
	for _, k := range order {
		t := byKey[k]
		t.Provisional = t.Root == nil
		out.Threads = append(out.Threads, *t)
	}
	return out
}

// ---------------------------------------------------------------------------
// The shared argument grammar: [worker] [N] [html|pdf], in any order.
// ---------------------------------------------------------------------------

// Output formats.
const (
	FormatHTML = "html"
	FormatPDF  = "pdf"
)

// ItemArgs is a parsed `copy` / `output` request.
type ItemArgs struct {
	Worker string
	N      int
	Format string // output only: html | pdf
	Board  string // --board / --fleet; "" = the caller's own board
	Out    string // output only: --out <dir>
	NoOpen bool   // output only: --no-open
}

// CopyUsage / OutputUsage are the grammar lines, shown together whenever
// either command is given no or bad arguments.
const (
	CopyUsage   = "copy [worker] [N]              copy agent messages to the clipboard (one item = one message)"
	OutputUsage = "output [worker] [N] html|pdf   write messages to an HTML page or a PDF and open it"
)

// UsageLines are the examples every surface prints under the grammar.
var UsageLines = []string{
	"copy                         all messages on the board",
	"copy 3                       the last 3 messages",
	"copy issue-314               all of issue-314's messages",
	"copy issue-314 3             issue-314's last 3 messages",
	"output issue-314 html        all of issue-314's messages as HTML",
	"output 5 pdf                 the last 5 messages as PDF",
	"output issue-314 3 pdf       issue-314's last 3 messages as PDF",
	"flags: --board <id> (another board), --no-open and --out <dir> (output)",
}

// ErrNeedsFormat is `output` without html or pdf.
var ErrNeedsFormat = errors.New("output needs a format: html or pdf (e.g. ##board output 5 pdf)")

// ParseItemArgs parses `copy` (output=false) or `output` (output=true)
// arguments. Positionals may come in any order: a bare integer is N, html or
// pdf is the format (output only), anything else is the worker. A second
// worker, a second N, or an unknown flag is REFUSED — a silently dropped
// argument copies something other than what was asked for.
func ParseItemArgs(args []string, output bool) (ItemArgs, error) {
	var a ItemArgs
	haveN := false
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "" {
			continue
		}
		if strings.HasPrefix(arg, "--") {
			name, val, hasVal := strings.Cut(arg, "=")
			switch name {
			case "--no-open":
				if !output || hasVal {
					return a, fmt.Errorf("unexpected flag %q", arg)
				}
				a.NoOpen = true
				continue
			case "--board", "--fleet", "--out":
				if name == "--out" && !output {
					return a, fmt.Errorf("--out is for output only")
				}
			default:
				return a, fmt.Errorf("unknown flag %q", name)
			}
			if !hasVal {
				if i+1 >= len(args) {
					return a, fmt.Errorf("%s needs a value", name)
				}
				i++
				val = args[i]
			}
			val = strings.TrimSpace(val)
			if val == "" {
				return a, fmt.Errorf("%s needs a value", name)
			}
			if name == "--out" {
				a.Out = val
				continue
			}
			if err := msg.ValidateBoardID(val); err != nil {
				return a, err
			}
			a.Board = msg.NormalizeBoardID(val)
			continue
		}
		if v, err := strconv.Atoi(arg); err == nil && v >= 0 && arg[0] != '+' {
			if haveN {
				return a, fmt.Errorf("two counts given (%d and %d)", a.N, v)
			}
			a.N, haveN = v, true
			continue
		}
		if output {
			if f := strings.ToLower(arg); f == FormatHTML || f == FormatPDF {
				if a.Format != "" {
					return a, fmt.Errorf("two formats given (%s and %s)", a.Format, f)
				}
				a.Format = f
				continue
			}
		}
		if a.Worker != "" {
			if output {
				// `output issue-314 docx`: the second word is where a format
				// goes, so name it as one.
				return a, fmt.Errorf("unknown format %q: use html or pdf", arg)
			}
			return a, fmt.Errorf("two workers given (%q and %q)", a.Worker, arg)
		}
		a.Worker = arg
	}
	if output && a.Format == "" {
		return a, ErrNeedsFormat
	}
	return a, nil
}

// ---------------------------------------------------------------------------
// Copy: select, format, cap, confirm.
// ---------------------------------------------------------------------------

// ErrNoMessages means the recorder answered and nothing matched. Callers
// refuse on it rather than copying "" or writing an empty page.
type ErrNoMessages struct{ Worker string }

func (e ErrNoMessages) Error() string { return NoMessagesLine(e.Worker) }

// NoMessagesLine is the refusal for an empty selection.
func NoMessagesLine(worker string) string {
	if worker == "" {
		return "[board] no messages"
	}
	return "[board] no messages from " + worker
}

// CopyResult is a built copy.
type CopyResult struct {
	Text           string
	Picked         []Item
	Matched, Total int
	Worker         string
}

// BuildCopy runs the whole copy pipeline on a recorder answer. A nil answer
// is ErrNoRecorder — nobody answering is not an empty board.
func BuildCopy(r *QueryReply, worker string, n int) (CopyResult, error) {
	if r == nil {
		return CopyResult{}, ErrNoRecorder
	}
	return BuildCopyItems(Flatten(r), worker, n)
}

// BuildCopyItems is BuildCopy over items already in hand (the pane's y / Y).
func BuildCopyItems(items []Item, worker string, n int) (CopyResult, error) {
	picked, matched, total := Select(items, worker, n)
	res := CopyResult{Picked: picked, Matched: matched, Total: total, Worker: worker}
	if len(picked) == 0 {
		return res, ErrNoMessages{Worker: worker}
	}
	res.Text = FormatItems(picked)
	if len(res.Text) > CopyMaxBytes {
		return res, fmt.Errorf(
			"[board] copy refused: %d bytes is over the %d KiB clipboard limit — copy fewer messages with N (e.g. copy 20) or a worker",
			len(res.Text), CopyMaxBytes/1024)
	}
	return res, nil
}

// Summary is the confirmation every copy surface prints.
func (c CopyResult) Summary() string {
	return fmt.Sprintf("[board] copied %d of %d messages%s to the clipboard", len(c.Picked), c.Matched, fromWorker(c.Worker))
}

func fromWorker(w string) string {
	if w == "" {
		return ""
	}
	return " from " + w
}

// TruncationNotes says what a recorder window left out, or "".
func TruncationNotes(r *QueryReply) string {
	if r == nil {
		return ""
	}
	var notes []string
	if r.Withheld > 0 {
		notes = append(notes, fmt.Sprintf("%d older threads withheld (--limit)", r.Withheld))
	}
	if r.Filtered > 0 {
		notes = append(notes, fmt.Sprintf("%d threads filtered out (--since)", r.Filtered))
	}
	if r.Stats.Evicted > 0 {
		notes = append(notes, fmt.Sprintf("%d posts evicted from the recorder", r.Stats.Evicted))
	}
	return strings.Join(notes, ", ")
}

func sanitisedPost(p *msg.MsgBoardPost) *msg.MsgBoardPost {
	cp := *p
	cp.Text = SanitiseBody(p.Text)
	cp.Persona = SanitiseInline(p.Persona)
	cp.Kind = SanitiseInline(p.Kind)
	return &cp
}

// ---------------------------------------------------------------------------
// Sanitising agent-written text.
// ---------------------------------------------------------------------------

// SanitiseBody makes an agent-written body safe to copy, print or render
// while keeping it otherwise byte for byte: newlines and tabs stay, CRLF and
// lone CR become LF, and every terminal escape sequence is removed WHOLE — not
// just its ESC byte, which would leave `[31m` litter — along with the other
// C0/C1 controls and DEL.
func SanitiseBody(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return stripControls(s, true)
}

// SanitiseInline is SanitiseBody for a one-line value (persona, kind).
func SanitiseInline(s string) string {
	s = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ", "\t", " ").Replace(s)
	return stripControls(s, false)
}

func stripControls(s string, keepLayout bool) string {
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == 0x1b: // ESC
			i = skipEscape(rs, i)
		case r == 0x9b: // C1 CSI
			i = skipCSI(rs, i+1)
		case r == 0x9d || r == 0x90 || r == 0x98 || r == 0x9e || r == 0x9f: // C1 OSC/DCS/SOS/PM/APC
			i = skipString(rs, i+1)
		case keepLayout && (r == '\n' || r == '\t'):
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// skipEscape consumes the sequence starting at the ESC at i and returns the
// index of its last rune.
func skipEscape(rs []rune, i int) int {
	if i+1 >= len(rs) {
		return i
	}
	switch rs[i+1] {
	case '[':
		return skipCSI(rs, i+2)
	case ']', 'P', 'X', '^', '_':
		return skipString(rs, i+2)
	}
	// Two-byte escapes (ESC 7, ESC c, ESC ( B …): intermediates 0x20–0x2f then
	// one final byte.
	j := i + 1
	for j < len(rs) && rs[j] >= 0x20 && rs[j] <= 0x2f {
		j++
	}
	if j >= len(rs) {
		return len(rs) - 1
	}
	return j
}

// skipCSI consumes parameter and intermediate bytes up to a final byte
// (0x40–0x7e) starting at j, returning the index of the final byte.
func skipCSI(rs []rune, j int) int {
	for ; j < len(rs); j++ {
		if rs[j] >= 0x40 && rs[j] <= 0x7e {
			return j
		}
		if rs[j] < 0x20 {
			// Malformed: stop before the control so the caller handles it.
			return j - 1
		}
	}
	return len(rs) - 1
}

// skipString consumes an OSC/DCS-style string up to BEL, ST (ESC \) or C1 ST,
// starting at j, returning the index of the terminator's last rune.
func skipString(rs []rune, j int) int {
	for ; j < len(rs); j++ {
		switch rs[j] {
		case 0x07, 0x9c:
			return j
		case 0x1b:
			if j+1 < len(rs) && rs[j+1] == '\\' {
				return j + 1
			}
			// Cut short by a new escape: end here and let the caller
			// consume that escape itself.
			return j - 1
		}
	}
	return len(rs) - 1
}
