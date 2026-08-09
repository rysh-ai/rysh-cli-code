package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/bus"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The agents-board view (design 025 §6): a threaded, push-based stream of what
// every agent in the session is doing, rendered inside a pane.
//
// SHAPE: agents-board is a PaneType, not an input mode. That is a founder
// ruling (design 025 §2.3 / §8.1) made against this unit's own recommendation;
// there is deliberately NO mode variant and no seam for one. A pane of this
// type never starts a shell (domain.IsShelllessPaneType), so nothing else ever
// writes into its buffer — everything on screen is built here.
//
// The rendering is modelled on internal/tui/email_view.go, which is the only
// other live-NATS-fed view in this TUI: a subscription drained by a tea.Cmd, a
// per-pane view state, and a build function called from buildPanePanel. The
// KEYMAP is modelled on internal/tui/replay_pane.go instead, because that is
// the precedent for a PaneType rather than a mode: claim a small set of keys
// while focused and let everything else through, so the user is never trapped.
//
// The store is NOT here. internal/board owns threading, orphan re-parenting,
// dedup, eviction and KV persistence; this file only reads Threads() and draws
// them. That boundary is why the view needs no bookkeeping of its own for
// re-parenting: a re-parented thread is simply a different Threads() result on
// the next frame.

// boardViewState is the per-pane view state.
//
// The BOARD ITSELF is session-wide and lives on Model.board — two agents-board
// panes show the same stream, and each keeps only its own scroll position.
type boardViewState struct {
	// scrollOffset is how many rendered rows the view is scrolled UP from the
	// newest post. 0 means pinned to the live tail, which is the default and
	// the point of the thing: the board answers "what is happening now".
	scrollOffset int

	// lastRows / lastVisible are what the previous render measured. Scroll
	// clamping needs the row count and the pane's height, and neither is known
	// to the key handler — the pane rect is resolved during layout. Recording
	// them at render time and clamping against them is how the raw-scroll path
	// solves the same problem.
	lastRows    int
	lastVisible int
}

// boardEventMsg carries one decoded board message from the subscription
// goroutine into Update.
type boardEventMsg struct{ ev board.Event }

// setupBoardSubscriptions builds this TUI's READ-ONLY view of the session board.
//
// THE TUI IS A READER, NOT THE COLLECTOR. Collection and persistence belong to
// ABLA — actors.AgentBoardListenerActor, one per session, always on — because
// this function used to be the only subscriber and the only KV writer in the
// system, which meant the board heard nothing and recorded nothing whenever no
// TUI had a board open. That was proved live: two panes registered their
// personas and the board showed one agent, because the announcements were
// published before any board existed.
//
// So there are exactly two reads here and no writes:
//
//  1. a RESTORE from the board KV, to load history (Keys/Get only);
//  2. a subscription with a NIL persister, for the live tail.
//
// The nil is the load-bearing part of this function. Handing a *Persistence to
// board.Subscribe here would put the session's memory back inside a process
// that may not be running, and would double-write every post while ABLA is also
// recording. TestTUISubscriptionDoesNotPersist pins it.
//
// It still returns a usable Store in every failure path: agents-board is a
// monitoring view, and a monitoring view that takes the session down with it is
// worse than no view.
func setupBoardSubscriptions(b *bus.Bus) (*board.Store, <-chan board.Event, *board.Subscriber) {
	store := board.New(0)

	// A never-delivering channel, so listenBoardEventCmd parks instead of
	// receiving from nil (which is the same park, but reads like a bug).
	dead := make(chan board.Event)

	if b == nil || b.Conn() == nil {
		return store, dead, nil
	}

	// Read-only restore. board.NewPersistence(nil) is a no-op Persistence, so a
	// session without JetStream gets an empty history rather than a refusal to
	// start. Restore before subscribing: history first, then the live tail —
	// the other order interleaves restored posts among live ones and puts
	// yesterday's milestones in the middle of today's stream.
	persist := board.NewPersistence(b.BoardKV())
	_, _, _ = persist.Restore(store)

	// nil persister: ABLA writes, this view does not.
	sub, err := board.Subscribe(b.Conn(), b.Codecs(), board.DefaultBufferSize, nil)
	if err != nil {
		return store, dead, nil
	}
	return store, sub.Events(), sub
}

// boardNATSConn hands the view the connection it asks liveness on. Nil when
// there is no bus, which renders as UNKNOWN rather than as either answer.
func boardNATSConn(b *bus.Bus) *nats.Conn {
	if b == nil {
		return nil
	}
	return b.Conn()
}

// RecorderState is what a view needs in order to be honest about an empty board.
type RecorderState int

const (
	// RecorderUnknown — we cannot even ask. Say so; do not imply either answer.
	RecorderUnknown RecorderState = iota
	// RecorderLive — the recorder answered. An empty board really does mean
	// nobody posted.
	RecorderLive
	// RecorderStale — nobody answered. The board is missing messages.
	RecorderStale
)

// recorderAliveTimeout bounds the liveness question. Short, because it is asked
// on a timer and never on the render path.
const recorderAliveTimeout = 400 * time.Millisecond

// boardRecorderMsg carries a liveness answer back into Update.
type boardRecorderMsg struct{ state RecorderState }

// askRecorderCmd asks the recorder whether it is recording.
//
// A REQUEST, not a heartbeat, and never on the render path: it runs as a
// tea.Cmd, the view renders from the last known answer, and the reply updates
// it. A liveness check that can hang the TUI is worse than the bug it reports.
//
// A timeout tests the ACTUAL path rather than an artifact's freshness, which is
// why this replaced a KV heartbeat: the heartbeat needed a staleness threshold
// that could be wrong under load, and it wrote into the board's bucket, whose
// single-writer detector depends on nothing else writing there.
func (m Model) askRecorderCmd() tea.Cmd {
	nc := m.boardConn
	return func() tea.Msg {
		if nc == nil {
			return boardRecorderMsg{state: RecorderUnknown}
		}
		if _, err := nc.Request(msgpkg.BoardAliveSubject(), nil, recorderAliveTimeout); err != nil {
			return boardRecorderMsg{state: RecorderStale}
		}
		return boardRecorderMsg{state: RecorderLive}
	}
}

// recorderNotice returns the line a board must show when it cannot honestly
// render an empty stream, and "" when it can.
//
// An empty board is a CLAIM that nothing was posted. When the recorder is dead
// that claim is false, and false in the most confident-looking way: a clean,
// empty view. This is the roster defect with the sign flipped — a roster that
// overcounts looks authoritative and is wrong; a board that should not be empty
// is the same thing.
//
// "Cannot tell" is kept distinct from "not recording" on purpose. Collapsing
// them would either cry wolf on every session without JetStream, which trains
// people to ignore the warning, or claim health we cannot verify.
func (m Model) recorderNotice() string {
	switch m.boardRecorder {
	case RecorderStale:
		return "  ⚠ NOT RECORDING — the board listener did not answer. Messages are being missed."
	case RecorderUnknown:
		return "  ⚠ recording state unknown — this board cannot confirm it is receiving messages."
	default:
		return ""
	}
}

// listenBoardEventCmd blocks for the next board event and yields it as a
// tea.Msg, re-armed by Update after each delivery — the email_view.go idiom.
func (m Model) listenBoardEventCmd() tea.Cmd {
	ch := m.boardEventCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg { return boardEventMsg{ev: <-ch} }
}

// applyBoardEvent files one event into the session store.
func (m *Model) applyBoardEvent(ev board.Event) {
	if m.board == nil {
		return
	}
	m.board.ApplyEvent(ev)
}

// boardViewFor returns (creating if needed) the view state for a pane.
//
// The value receiver is deliberate and safe: boardViews is a map, so the
// assignment mutates the Model's map through the copy. A nil map means a Model
// built without the board wiring (a unit test of some other surface); it gets a
// throwaway state rather than a panic.
func (m Model) boardViewFor(paneID string) *boardViewState {
	if m.boardViews == nil {
		return &boardViewState{}
	}
	st := m.boardViews[paneID]
	if st == nil {
		st = &boardViewState{}
		m.boardViews[paneID] = st
	}
	return st
}

// updateBoardPaneInput handles keystrokes while the focused pane is an
// agents-board pane. Returns (true, cmd) when the key was claimed, (false, nil)
// otherwise.
//
// Only scrolling keys and enter are claimed. esc, [ / ] tab switching, alt+←/→
// and every multiplexer chord (ctrl+o, ctrl+p, ctrl+t, ctrl+space, …) fall
// through ON PURPOSE, exactly as replay_pane.go does: a board you cannot leave
// is a bug, and it is the one bug a read-only full-pane view is most likely to
// ship with.
func (m *Model) updateBoardPaneInput(msg tea.KeyMsg) (bool, tea.Cmd) {
	snap := m.focusedPaneSnapshot()
	if snap == nil || snap.PaneType != domain.PaneTypeAgentsBoard {
		return false, nil
	}
	st := m.boardViewFor(snap.ID)
	page := max(1, st.lastVisible-1)

	switch msg.String() {
	case "up", "k":
		st.scrollOffset = clampBoardScroll(st.scrollOffset+1, st)
		return true, nil
	case "down", "j":
		st.scrollOffset = clampBoardScroll(st.scrollOffset-1, st)
		return true, nil
	case "pgup":
		st.scrollOffset = clampBoardScroll(st.scrollOffset+page, st)
		return true, nil
	case "pgdown":
		st.scrollOffset = clampBoardScroll(st.scrollOffset-page, st)
		return true, nil
	case "home", "g":
		st.scrollOffset = maxBoardScroll(st.lastRows, st.lastVisible)
		return true, nil
	case "end", "G":
		st.scrollOffset = 0
		return true, nil
	case "enter":
		// Swallowed for replay_pane.go's reason: an agents-board pane is
		// shell-less by construction, so letting enter through would print a
		// "shell is not available" error into a read-only view.
		return true, nil
	}
	return false, nil
}

func maxBoardScroll(rows, visible int) int {
	if visible <= 0 || rows <= visible {
		return 0
	}
	return rows - visible
}

func clampBoardScroll(want int, st *boardViewState) int {
	maxOff := maxBoardScroll(st.lastRows, st.lastVisible)
	if want < 0 {
		return 0
	}
	if want > maxOff {
		return maxOff
	}
	return want
}

// buildBoardPanel renders the whole pane: a header line of honest counters and
// the threaded stream beneath it, windowed to the pane's height.
func (m Model) buildBoardPanel(pane domain.PaneSnapshot, paneWidth, height int) string {
	// Seed before rendering: the roster must reflect the panes that exist now,
	// not only those that announced while this board happened to be attached.
	m.seedRosterFromSnapshot()

	contentWidth := max(8, paneWidth-4)
	maxLines := height - 1 // the header line is the only overhead
	if maxLines < 0 {
		maxLines = 0
	}

	st := m.boardViewFor(pane.ID)
	rows := m.boardRows(contentWidth)

	// Record what this frame measured so the key handler can clamp, and clamp
	// the current offset against it: the board grows under the user, and an
	// offset that was valid two posts ago must not scroll past the top.
	st.lastRows = len(rows)
	st.lastVisible = maxLines
	st.scrollOffset = clampBoardScroll(st.scrollOffset, st)

	win := visibleRowWindow(rows, maxLines, st.scrollOffset)
	out := make([]string, 0, maxLines)
	for _, r := range win {
		out = append(out, truncBoardLine(r, contentWidth))
	}
	for len(out) < maxLines {
		out = append(out, "")
	}
	return fmt.Sprintf("%s\n%s",
		metaStyle.Render(truncBoardLine(m.boardHeaderText(st), contentWidth)),
		strings.Join(out, "\n"))
}

// boardHeaderText states what the board is NOT showing as well as what it is.
//
// Design 025 §7 is explicit that a documented limit is a feature and an implied
// one is a defect: a burst can drop posts at the subscriber (§7.2) and the cap
// evicts old threads (§7.1a). Both are counted rather than hidden, so the board
// says "N dropped" instead of silently lying about being complete.
func (m Model) boardHeaderText(st *boardViewState) string {
	if m.board == nil {
		return "AGENTS-BOARD | no feed on this session"
	}
	s := m.board.Stats()
	parts := []string{fmt.Sprintf("AGENTS-BOARD | %s · %s",
		plural(s.Threads, "thread"), plural(s.Posts, "post"))}
	if n := len(m.board.Roster()); n > 0 {
		parts = append(parts, plural(n, "agent"))
	}
	if s.Provisional > 0 {
		parts = append(parts, fmt.Sprintf("%d awaiting root", s.Provisional))
	}
	if s.Evicted > 0 {
		parts = append(parts, fmt.Sprintf("%d evicted", s.Evicted))
	}
	if m.boardSub != nil {
		if d := m.boardSub.Dropped(); d > 0 {
			parts = append(parts, fmt.Sprintf("%d DROPPED", d))
		}
	}
	if st.scrollOffset > 0 {
		parts = append(parts, fmt.Sprintf("↑%d  G=live", st.scrollOffset))
	}
	return strings.Join(parts, "  ·  ")
}

// boardKeyHelp is the footer for a focused agents-board pane, in the shape
// model_view.go:keyHelp uses per mode.
func (m Model) boardKeyHelp() string {
	return "pane: agents-board (read-only) | j/k ↑/↓ scroll  pgup/pgdn page  g top  G/end live  " +
		"[/] tabs  ctrl+space navigate  ctrl+p pane-mgmt  ctrl+o prefix"
}

const (
	boardRootGlyph        = "●"
	boardProvisionalGlyph = "◌"
	boardReplyGlyph       = "↳"

	boardRootIndent  = 2
	boardReplyIndent = 4
)

// boardRows renders every thread to plain rows, newest thread last.
//
// The rows carry NO styling on purpose. Two reasons, and the second is the real
// one: (a) width maths stays exact, since an ANSI escape has display width 0
// but string length > 0, and this view hard-truncates to the pane width; (b)
// the content is agent-supplied text, and the moment a renderer emits escapes
// of its own it becomes much harder to be sure an agent's escapes are not
// getting through with them. See sanitiseBoardText.
func (m Model) boardRows(width int) []string {
	if m.board == nil {
		return []string{"", "  No board feed on this session.", ""}
	}
	threads := m.board.Threads()
	if len(threads) == 0 {
		// "Nothing posted yet" is only sayable when the recorder is known live.
		// Otherwise the honest answer is that we do not know, and saying so is
		// the whole point of this branch.
		if notice := m.recorderNotice(); notice != "" {
			return []string{"", notice, "  An empty board here does NOT mean nobody posted.", ""}
		}
		return []string{
			"",
			"  Nothing posted yet.",
			"  Agents post with `rysh board post <text>` and reply with `rysh board reply <thread> <text>`.",
			"",
		}
	}
	rows := make([]string, 0, len(threads)*4)
	for i, t := range threads {
		if i > 0 {
			rows = append(rows, "")
		}
		rows = append(rows, boardThreadRows(t, width)...)
	}
	return rows
}

// boardThreadRows renders one root and its replies, the replies UNDER the root.
//
// A provisional thread — replies that arrived before their root, which design
// 025 §4.3 makes an expected ordering and not an error, because thread ids are
// minted agent-side with no round trip — renders a placeholder root so its
// replies are visible immediately. When the real root lands, the store
// re-parents into the SAME thread and re-positions it to the root's arrival, so
// the next frame renders one thread with a real root. The view holds no state
// across frames, which is precisely why it cannot duplicate the root.
func boardThreadRows(t board.Thread, width int) []string {
	rows := make([]string, 0, 2+len(t.Replies)*2)

	if t.Root != nil {
		rows = append(rows, boardPostHeader(boardRootGlyph, t.Root, 0, width))
		rows = append(rows, boardTextRows(t.Root.Text, boardRootIndent, width)...)
	} else {
		rows = append(rows, fmt.Sprintf("%s awaiting root  ·  thread %s",
			boardProvisionalGlyph, shortThreadKey(t.Key)))
	}

	for _, r := range t.Replies {
		rows = append(rows, boardPostHeader(boardReplyGlyph, r, boardRootIndent, width))
		rows = append(rows, boardTextRows(r.Text, boardReplyIndent, width)...)
	}
	return rows
}

// boardPostHeader is the "who spoke" line: glyph, persona, kind, clock.
func boardPostHeader(glyph string, p *msgpkg.MsgBoardPost, indent, width int) string {
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", indent))
	b.WriteString(glyph)
	b.WriteString(" ")
	b.WriteString(boardDisplayPersona(p))
	if k := sanitiseBoardInline(p.Kind); k != "" {
		b.WriteString("  [")
		b.WriteString(k)
		b.WriteString("]")
	}
	b.WriteString("  ")
	b.WriteString(boardClock(p.TS))
	return truncBoardLine(b.String(), width)
}

// boardDisplayPersona is the ONE place this view turns a post into a name.
//
// It defers to msg.BoardPersona rather than re-deriving the fallback chain.
// The trap it guards is specific and verified: approval panes OVERLOAD
// PaneSnapshot.GivenName to carry "requestID\x1FresponseSubject"
// (actors/approval_pane.go:76-78, and the warning on domain.PaneTypeApproval),
// so a view that prints Persona raw will one day print a NATS subject as
// somebody's name.
//
// The store also sanitises on ingest (board.sanitisePersona). This is the
// second guard rather than a redundant one: a post can reach a renderer without
// passing through Store.Apply, and the identity rule must not depend on which
// door the post came in. Note also that Persona is a LABEL, never a key —
// given-names are unique per lane, not per session (design 025 §5), so two
// panes may legitimately display the same name; PaneID is the identity.
func boardDisplayPersona(p *msgpkg.MsgBoardPost) string {
	return msgpkg.BoardPersona(p.Persona, "", p.PaneID)
}

// boardClock renders a post's own clock. TS is the POSTER's clock, so the board
// shows arrival order and stamps sender time — design 025 §7.8 — and does not
// pretend to a causal ordering it does not have.
func boardClock(ts int64) string {
	if ts <= 0 {
		return "--:--:--"
	}
	return time.UnixMilli(ts).Format("15:04:05")
}

// boardTextRows renders a post's body, wrapped and indented under its header.
func boardTextRows(text string, indent, width int) []string {
	avail := max(1, width-indent)
	pad := strings.Repeat(" ", indent)
	out := make([]string, 0, 2)
	for _, line := range sanitiseBoardText(text) {
		for _, w := range wrapBoardLine(line, avail) {
			out = append(out, pad+w)
		}
	}
	if len(out) == 0 {
		out = append(out, pad+"(no text)")
	}
	return out
}

// sanitiseBoardText turns agent-supplied text into safe display lines.
//
// Post text arrives from any process that can reach the session's NATS — there
// is no auth on the board (design 025 §7.5) — so it is untrusted bytes, not a
// string this TUI produced. Control characters are dropped and ESC with them:
// without this, one post containing escape sequences could repaint the pane,
// move the cursor, or hide every other agent's messages. This is the same class
// of defect as the \x1f persona overload, and it is guarded in the same place.
func sanitiseBoardText(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, sanitiseBoardInline(line))
	}
	return out
}

// sanitiseBoardInline is sanitiseBoardText for a value that must stay one line
// (a Kind, a title): newlines are dropped rather than split, so a crafted Kind
// cannot inject rows into the stream.
func sanitiseBoardInline(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteString("    ")
		case r < 0x20 || r == 0x7f:
			// Dropped: C0 controls, ESC (0x1b) and DEL.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// wrapBoardLine wraps on word boundaries, hard-splitting a token longer than
// the width so nothing is ever dropped.
func wrapBoardLine(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	if s == "" {
		return []string{""}
	}
	var out []string
	var cur []rune
	flush := func() {
		out = append(out, string(cur))
		cur = cur[:0]
	}
	for _, word := range strings.Fields(s) {
		w := []rune(word)
		// A token wider than the pane: hard-split it rather than overflow.
		for len(w) > width {
			if len(cur) > 0 {
				flush()
			}
			out = append(out, string(w[:width]))
			w = w[width:]
		}
		switch {
		case len(cur) == 0:
			cur = append(cur, w...)
		case len(cur)+1+len(w) <= width:
			cur = append(cur, ' ')
			cur = append(cur, w...)
		default:
			flush()
			cur = append(cur, w...)
		}
	}
	if len(cur) > 0 || len(out) == 0 {
		flush()
	}
	return out
}

// truncBoardLine clamps a plain (unstyled) row to the content width by runes.
func truncBoardLine(s string, width int) string {
	if width < 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}

// shortThreadKey renders a thread key for a human. Keys are agent-minted as
// "<full-pane-uuid>/<n>" (msg.MintThreadID), which is far too wide for a header
// line and whose informative half is the ordinal.
func shortThreadKey(key string) string {
	if i := strings.LastIndex(key, "/"); i > 0 {
		head := key[:i]
		if len(head) > 8 {
			head = head[:8]
		}
		return head + key[i:]
	}
	return truncBoardLine(key, 20)
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// seedRosterFromSnapshot fills the roster from the panes the TUI can already
// see, and it is what makes the roster TRUE rather than merely non-empty.
//
// Why a producer alone is not enough. The only board subscriber lives here, in
// the TUI (setupBoardSubscriptions). A registration published while no board is
// attached is heard by nobody and persisted by nobody — and panes announce
// themselves at startup, which is almost always BEFORE any board exists. Wiring
// the pane-side producer therefore covers panes born while a board is watching,
// and loses every pane that predates it. Demonstrated live: two named panes,
// one agent in the roster.
//
// The workspace snapshot the TUI already renders is authoritative and always
// present, so seeding from it needs no message and cannot be missed. The
// pane-side announcement still earns its place: it carries panes created later
// and re-announces on rename, which a snapshot seed would only pick up on the
// next render anyway.
//
// Idempotent by construction — Store.Register keys on pane id and last
// announcement wins, so re-seeding replaces rather than duplicates.
func (m Model) seedRosterFromSnapshot() {
	if m.board == nil {
		return
	}
	now := time.Now().UnixMilli()
	live := map[string]bool{}
	for _, tab := range m.snapshot.Tabs {
		for _, lane := range tab.Lanes {
			for _, g := range lane.PaneGroups {
				for i := range g.Panes {
					p := &g.Panes[i]
					if !domain.PaneCanHostAnAgent(p.PaneType) {
						continue
					}
					live[p.ID] = true
					m.board.Register(&msgpkg.MsgBoardRegister{
						V:      msgpkg.BoardSchemaVersion,
						PaneID: p.ID,
						// Same persona rule as every other surface.
						Persona: msgpkg.BoardPersona(p.GivenName, p.Title, p.ID),
						TS:      now,
					})
				}
			}
		}
	}
	// Registrations outlive the panes that made them (gate 2 persists them), so
	// seeding alone would only ever ADD. Without this the roster accumulates
	// ghosts — observed live as "3 agents" in a two-agent session, the third a
	// pane id from an earlier run.
	m.board.RetainRoster(live)
}

// recorderAskInterval is how often the view re-asks whether the recorder is
// alive. Unlike the heartbeat staleness threshold it replaced, this number
// cannot be WRONG — it only decides how quickly a dead recorder is noticed, not
// whether one is detected at all.
const recorderAskInterval = 5 * time.Second

// recorderAskTick re-arms the liveness question.
type recorderAskTick struct{}
