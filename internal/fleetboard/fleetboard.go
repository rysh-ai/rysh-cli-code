// SPDX-License-Identifier: Apache-2.0

// Package fleetboard keeps the session-wide fleet board's rows: one row per
// registered fleet (design 030 §9.2, founder ruling D-25, wave W2).
//
// It is pure: no NATS, no KV handle, no clock. The FleetBoardActor
// (internal/actors/fleet_board_actor.go) feeds it registry answers and board
// posts, persists its rows and serves them.
//
// THE SUMMARY IS DETERMINISTIC. A row's summary is its latest status post's
// first line plus how many posts came after it. No LLM: a summariser would be
// a new spend line and a new way to be wrong (design §9.2).
package fleetboard

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// QuietAfter is how long a fleet's own board may stay silent before the row
// reads quiet.
const QuietAfter = 30 * time.Minute

// textClip bounds a stored post: its first line, at most this many runes.
const textClip = 200

// Health values, in the order they win.
const (
	HealthDown    = "down"
	HealthBlocked = "blocked"
	HealthQuiet   = "quiet"
	HealthActive  = "active"
)

// statusKinds are the post kinds a row tracks, normalised to lower case.
var statusKinds = map[string]bool{"start": true, "milestone": true, "blocked": true, "done": true, "report": true}

// Post is a stored, clipped board post.
type Post struct {
	PaneID  string `json:"pane_id"`
	Persona string `json:"persona,omitempty"`
	Kind    string `json:"kind,omitempty"` // normalised status kind, or ""
	Text    string `json:"text"`
	TS      int64  `json:"ts"`
}

// Row is one fleet on the fleet board.
type Row struct {
	Fleet   string `json:"fleet"`
	BoardID string `json:"board_id"`
	State   string `json:"state"`
	Members int    `json:"members"`

	// HeadPane is the fleet's head (the member with no parent). MemberPanes
	// lets a post on the reserved fleet board be attributed by who posted it.
	HeadPane    string   `json:"head_pane,omitempty"`
	MemberPanes []string `json:"member_panes,omitempty"`

	Head         *Post           `json:"head,omitempty"`         // the head's last post
	LastByKind   map[string]Post `json:"last_by_kind,omitempty"` // last post per status kind
	LastStatus   *Post           `json:"last_status,omitempty"`  // latest status post
	SinceStatus  int             `json:"since_status"`           // own-board posts after it
	LastActivity int64           `json:"last_activity,omitempty"`

	Updated int64 `json:"updated"`
}

// Health derives the row's health at now (unix millis).
func (r Row) Health(now int64) string {
	switch {
	case r.State == fleet.StateDown:
		return HealthDown
	case r.LastStatus != nil && r.LastStatus.Kind == "blocked":
		return HealthBlocked
	case r.LastActivity == 0 || now-r.LastActivity > QuietAfter.Milliseconds():
		return HealthQuiet
	default:
		return HealthActive
	}
}

// Summary is the latest status post's first line, plus the posts since.
func (r Row) Summary() string {
	if r.LastStatus == nil {
		return "no status post yet"
	}
	s := strings.ToUpper(r.LastStatus.Kind) + ": " + r.LastStatus.Text
	if r.SinceStatus > 0 {
		s += fmt.Sprintf(" (+%d posts since)", r.SinceStatus)
	}
	return s
}

// NormalizeKind maps a post to a status kind ("start", "milestone",
// "blocked", "done", "report") or "". The Kind field wins; otherwise the text
// may lead with it, after an optional "[fleet]" tag ("[epic-a] DONE: …").
func NormalizeKind(kind, text string) string {
	if k := strings.ToLower(strings.TrimSpace(kind)); statusKinds[k] {
		return k
	}
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "[") {
		if i := strings.Index(t, "]"); i >= 0 {
			t = strings.TrimSpace(t[i+1:])
		}
	}
	word := strings.FieldsFunc(t, func(r rune) bool { return r == ':' || r == ' ' || r == '\t' || r == '\n' })
	if len(word) > 0 {
		if k := strings.ToLower(word[0]); statusKinds[k] {
			return k
		}
	}
	return ""
}

// Board is the set of rows. Not safe for concurrent use; the actor holds a
// lock around it.
type Board struct {
	rows map[string]*Row
}

// New returns an empty Board.
func New() *Board { return &Board{rows: map[string]*Row{}} }

// Load replaces the rows with ones read back from storage.
func (b *Board) Load(rows []Row) {
	b.rows = map[string]*Row{}
	for i := range rows {
		r := cloneRow(rows[i])
		if r.Fleet == "" {
			continue
		}
		b.rows[r.Fleet] = &r
	}
}

// Rows returns copies of every row, ordered by fleet name.
func (b *Board) Rows() []Row {
	out := make([]Row, 0, len(b.rows))
	for _, r := range b.rows {
		out = append(out, cloneRow(*r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fleet < out[j].Fleet })
	return out
}

// ApplyRegistry reconciles the rows with the registry's fleets. It returns the
// rows that changed (new, or a different state, board, head or roster) and the
// names of fleets that are gone.
func (b *Board) ApplyRegistry(fleets []fleet.Fleet, now int64) (changed []Row, removed []string) {
	seen := map[string]bool{}
	for _, f := range fleets {
		name := msg.NormalizeBoardID(f.Name)
		seen[name] = true
		boardID := msg.NormalizeBoardID(f.BoardID)
		if strings.TrimSpace(f.BoardID) == "" {
			boardID = name
		}
		panes := make([]string, 0, len(f.Members))
		for _, m := range f.Members {
			if m.PaneID != "" {
				panes = append(panes, m.PaneID)
			}
		}
		sort.Strings(panes)
		head := headOf(f.Members)

		r := b.rows[name]
		if r == nil {
			r = &Row{Fleet: name}
			b.rows[name] = r
		} else if r.BoardID == boardID && r.State == f.State && r.Members == len(f.Members) &&
			r.HeadPane == head && equalStrings(r.MemberPanes, panes) {
			continue
		}
		r.BoardID, r.State, r.Members, r.HeadPane, r.MemberPanes = boardID, f.State, len(f.Members), head, panes
		r.Updated = now
		changed = append(changed, cloneRow(*r))
	}
	for name := range b.rows {
		if !seen[name] {
			delete(b.rows, name)
			removed = append(removed, name)
		}
	}
	sort.Strings(removed)
	return changed, removed
}

// ApplyPost folds one post into the row it belongs to and returns that row,
// or nil when the post belongs to no fleet.
//
//   - On a fleet's OWN board: own-board activity, the head's last post, and
//     the status kinds.
//   - On the reserved fleet board: the fleet whose MEMBER pane posted it
//     (never the "[fleet]" text prefix, which anyone can type); status kinds
//     and the head's post only, not own-board activity. A non-member's post
//     is attributed to nobody (W3 tags those).
func (b *Board) ApplyPost(boardID string, p *msg.MsgBoardPost) *Row {
	if p == nil {
		return nil
	}
	id := msg.NormalizeBoardID(boardID)
	var r *Row
	own := false
	if id == msg.FleetBoardID {
		// W3: a post the recorder could not verify (no door stamp, or not a
		// head) stays in the raw stream and sets no fleet's status.
		if p.Unverified {
			return nil
		}
		r = b.rowOfMember(p.PaneID)
	} else {
		r = b.rowOfBoard(id)
		own = r != nil
	}
	if r == nil {
		return nil
	}

	kind := NormalizeKind(p.Kind, p.Text)
	sp := Post{PaneID: p.PaneID, Persona: p.Persona, Kind: kind, Text: clip(p.Text), TS: p.TS}

	if own {
		if p.TS > r.LastActivity {
			r.LastActivity = p.TS
		}
		if r.LastStatus != nil && p.TS > r.LastStatus.TS && kind == "" {
			r.SinceStatus++
		}
	}
	if p.PaneID != "" && p.PaneID == r.HeadPane && (r.Head == nil || p.TS >= r.Head.TS) {
		h := sp
		r.Head = &h
	}
	if kind != "" {
		if r.LastByKind == nil {
			r.LastByKind = map[string]Post{}
		}
		if prev, ok := r.LastByKind[kind]; !ok || p.TS >= prev.TS {
			r.LastByKind[kind] = sp
		}
		if r.LastStatus == nil || p.TS >= r.LastStatus.TS {
			s := sp
			r.LastStatus = &s
			r.SinceStatus = 0
		}
	}
	r.Updated = maxInt64(r.Updated, p.TS)
	out := cloneRow(*r)
	return &out
}

func (b *Board) rowOfBoard(id string) *Row {
	for _, r := range b.rows {
		if r.BoardID == id {
			return r
		}
	}
	return nil
}

func (b *Board) rowOfMember(pane string) *Row {
	if pane == "" {
		return nil
	}
	for _, r := range b.rows {
		i := sort.SearchStrings(r.MemberPanes, pane)
		if i < len(r.MemberPanes) && r.MemberPanes[i] == pane {
			return r
		}
	}
	return nil
}

// headOf picks the fleet's head: the member with no parent; among several,
// a top role (fleet.TopRoles) first, then the lowest pane id.
func headOf(members []fleet.Member) string {
	var roots []fleet.Member
	for _, m := range members {
		if m.PaneID != "" && strings.TrimSpace(m.Parent) == "" {
			roots = append(roots, m)
		}
	}
	if len(roots) == 0 {
		return ""
	}
	rank := func(m fleet.Member) int {
		if fleet.IsTopRole(m.Role) {
			return 0
		}
		return 1
	}
	sort.Slice(roots, func(i, j int) bool {
		if rank(roots[i]) != rank(roots[j]) {
			return rank(roots[i]) < rank(roots[j])
		}
		return roots[i].PaneID < roots[j].PaneID
	})
	return roots[0].PaneID
}

func clip(text string) string {
	line := strings.TrimSpace(text)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	r := []rune(line)
	if len(r) > textClip {
		return string(r[:textClip-1]) + "…"
	}
	return line
}

func cloneRow(r Row) Row {
	out := r
	if r.MemberPanes != nil {
		out.MemberPanes = append([]string(nil), r.MemberPanes...)
	}
	if r.LastByKind != nil {
		out.LastByKind = make(map[string]Post, len(r.LastByKind))
		for k, v := range r.LastByKind {
			out.LastByKind[k] = v
		}
	}
	if r.Head != nil {
		h := *r.Head
		out.Head = &h
	}
	if r.LastStatus != nil {
		s := *r.LastStatus
		out.LastStatus = &s
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
