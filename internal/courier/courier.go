// SPDX-License-Identifier: Apache-2.0

// Package courier delivers agents-board commands into the recipient agent's own
// session — beside its keyboard, never through it (design 050, polling-stream).
//
// A command (msg.BoardKindCmd) is a board post addressed to one pane. The
// courier hears it on the board, works out how that pane's agent takes
// messages from outside, and hands it over:
//
//   - claude-inbox: one JSON line on the claude session's own inbox socket
//     (Claude Code cross-session messaging). An idle claude starts a turn; a
//     busy one reads it between tool calls.
//   - codex-queue:  `codex queue --thread <id> --message <text>`. The running
//     codex TUI runs it at its next idle point.
//   - rysh-native:  a prompt on the pane's inbox subject, for a pane whose
//     agent IS rysh (no foreground program).
//
// THE ONE RULE: there is no fallback to typing into the pane. The pane's PTY
// belongs to the human, and the input-stream path that types into it erases
// whatever the human was writing (design 050 §1). A command the courier cannot
// deliver is reported on the board as undelivered, where the human sees it.
package courier

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Pane meta keys the courier reads.
const (
	// MetaClaudeInbox is a claude's inbox socket path, stamped by a SessionStart
	// hook fleetctl installs (`$CLAUDE_CODE_MESSAGING_SOCKET`). Preferred over
	// guessing from the pid because claude can bind elsewhere on a collision.
	MetaClaudeInbox = "claude.inbox"
	// MetaCodexSession is the codex thread id the daemon discovers (design 029).
	MetaCodexSession = "codex.session_id"
	// MetaFleetComm is the fleet's comm mode (design 050): "input-stream" panes
	// are typed into by ANSA and never receive courier deliveries.
	MetaFleetComm = "fleet.comm"
	// MetaFleetAgent is what fleetctl launched in the pane: claude|codex|rysh.
	MetaFleetAgent = "fleet.agent"
)

// Route names.
const (
	RouteClaudeInbox = "claude-inbox"
	RouteCodexQueue  = "codex-queue"
	RouteRyshNative  = "rysh-native"
)

// Fleet comm modes (design 050/051), as stamped in pane meta fleet.comm.
//
//   - queue: the default since design 051. No central push: every agent runs
//     its OWN listener (`rysh board listen`) that watches the board and takes
//     the prompts addressed to it. The courier stays out of these panes.
//   - grid: point-to-point. The daemon's courier writes each prompt straight
//     into the recipient's inbox. "polling-stream" is its pre-051 name.
//   - input-stream: ANSA types into the pane. Neither courier nor listener.
const (
	CommQueue       = "queue"
	CommGrid        = "grid"
	CommPolling     = "polling-stream" // = grid
	CommInputStream = "input-stream"
)

// UndeliveredKind is the kind of the board post the courier files on a
// command's thread when it gives up.
const UndeliveredKind = "undelivered"

// Persona/PaneID the courier posts under. Not a pane uuid on purpose: the board
// store files a post whose PaneID does not own the thread as a reply, so the
// notice lands under the command, and no agent's inbox counts it as an answer.
const Identity = "courier"

// Target is what the courier knows about a recipient pane at delivery time.
type Target struct {
	PaneID  string
	Program string // foreground executable, "" at the shell prompt
	PGID    int    // foreground process group, 0 when Program is ""
	Meta    map[string]string
}

// Route is a planned delivery: which adapter, and its address.
type Route struct {
	Adapter string
	Address string // socket path | codex thread id | pane id
}

// ErrPermanent wraps a planning failure that waiting will not fix.
var ErrPermanent = errors.New("permanent")

// SocketDirs are where Claude Code binds inbox sockets, in the order it tries
// them: the shared dir, then the per-user fallback it uses when it cannot accept
// the shared one.
func SocketDirs() []string {
	return []string{"/tmp/cc-socks", "/tmp/cc-socks-" + strconv.Itoa(os.Getuid())}
}

// IsSocket reports whether path is a live-looking unix socket.
func IsSocket(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

// Plan picks the route for a target. Pure apart from isSocket, which tests
// replace. The order is evidence first: an explicit stamp beats a pid guess,
// and a program name is never trusted on its own (claude's executable is named
// after its version, e.g. "2.1.296").
func Plan(t Target, isSocket func(string) bool) (Route, error) {
	meta := t.Meta
	if meta == nil {
		meta = map[string]string{}
	}
	if strings.TrimSpace(meta[MetaFleetComm]) == CommInputStream {
		return Route{}, fmt.Errorf("%w: the recipient's fleet uses input-stream; its messages go through the pane, not the board", ErrPermanent)
	}

	if t.Program != "" {
		if p := strings.TrimSpace(meta[MetaClaudeInbox]); p != "" && isSocket(p) {
			return Route{RouteClaudeInbox, p}, nil
		}
		if t.PGID > 0 {
			for _, dir := range SocketDirs() {
				if p := filepath.Join(dir, strconv.Itoa(t.PGID)+".sock"); isSocket(p) {
					return Route{RouteClaudeInbox, p}, nil
				}
			}
		}
		if id := strings.TrimSpace(meta[MetaCodexSession]); id != "" && isCodex(t) {
			return Route{RouteCodexQueue, id}, nil
		}
		// A program is running but none of its doors is known yet. Not
		// permanent: a claude's hook or a codex's session discovery may simply
		// not have landed, so this is retried.
		return Route{}, fmt.Errorf("no inbox found for %q in pane %s yet (no %s stamp, no inbox socket for pid %d, no %s)",
			t.Program, short(t.PaneID), MetaClaudeInbox, t.PGID, MetaCodexSession)
	}

	if strings.TrimSpace(meta[MetaFleetAgent]) == "rysh" {
		return Route{RouteRyshNative, t.PaneID}, nil
	}
	return Route{}, fmt.Errorf("no agent is running in pane %s (it is at its shell prompt)", short(t.PaneID))
}

func isCodex(t Target) bool {
	return strings.Contains(strings.ToLower(t.Program), "codex") ||
		strings.TrimSpace(t.Meta[MetaFleetAgent]) == "codex"
}

// Compose is the text the agent receives: the command, who sent it on which
// board, and how to answer. The answer instruction is what turns a delivered
// message into a closed loop — the sender learns it was done from the ack.
//
// The ack instruction is COMPLETE — binary, session, board and identity are
// all spelled out — because the agent's own shell cannot be trusted to know
// them. Found live (design 050 P2): codex runs commands in an environment that
// does not carry $RYSH_PANE/$RYSH_SESSION, and spent a minute rediscovering who
// and where it was before it could ack. bin is the daemon's own absolute
// executable: a bare "rysh" resolves through the agent's PATH, which may hold
// an older build with no `board` verb.
func Compose(boardID string, p *msg.MsgBoardPost, bin, session string) string {
	if bin == "" {
		bin = "rysh"
	}
	ack := fmt.Sprintf("%s board ack %s --as %s --board %s", shellQuote(bin), p.CmdID, p.ToPaneID, boardID)
	if session != "" {
		ack += " --session " + shellQuote(session)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[rysh board %s | command %s | from %s (%s)]\n",
		boardID, p.CmdID, p.Persona, short(p.PaneID))
	b.WriteString(strings.TrimSpace(p.Text))
	b.WriteString("\n")
	if p.File != "" {
		fmt.Fprintf(&b, "File: %s (read it in full).\n", p.File)
	}
	fmt.Fprintf(&b, "Answer on the board when you have taken it: %s --note '<status>'. "+
		"A reply on its thread (fleetctl report) also counts.", ack)
	return b.String()
}

// shellQuote quotes s for a POSIX shell only when it needs it.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r == '/' || r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Deps is everything the courier needs from the daemon. Injected so the queue,
// retry and refusal logic is testable without a daemon, a socket or codex.
type Deps struct {
	// Snapshot reads the recipient pane now.
	Snapshot func(paneID string) (Target, error)
	// Deliver performs one delivery over a planned route.
	Deliver func(ctx context.Context, r Route, t Target, text string) error
	// Notify publishes a courier post (undelivered) on a board.
	Notify func(boardID string, p *msg.MsgBoardPost)
	// IsSocket defaults to IsSocket.
	IsSocket func(string) bool
	// Bin and Session are named in the answer instruction (Compose).
	Bin     string
	Session string
	// AsListener marks a per-agent queue listener (design 051) rather than the
	// daemon's central courier. The courier skips queue panes, because their
	// own listener takes their prompts; a listener delivers to them.
	AsListener bool
	// Identity is who files undelivered notices; defaults to Identity.
	Identity string
	// Delivered, when set, is told about every successful delivery — the
	// listener records it so a restart does not deliver the same order twice.
	Delivered func(boardID string, p *msg.MsgBoardPost, r Route)
	// Backoff is the wait before each retry; len(Backoff)+1 attempts in all.
	Backoff []time.Duration
	// Log defaults to slog.Default().
	Log *slog.Logger
}

// DefaultBackoff is design 050 §5.2's schedule.
var DefaultBackoff = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 15 * time.Second, 60 * time.Second}

// Courier holds one FIFO queue per recipient: commands to one agent are
// delivered in the order they were posted, one at a time, so a burst cannot
// trip the recipient's inbox limits and a slow delivery to one agent never
// delays another.
type Courier struct {
	deps   Deps
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	queues map[string]chan job
	seen   map[string]struct{}
	order  []string
	wg     sync.WaitGroup
	// pending counts commands taken but not yet finished (delivered, given
	// up on, or skipped), for Drain.
	pending sync.WaitGroup
}

type job struct {
	board string
	post  *msg.MsgBoardPost
}

// seenCap bounds the duplicate filter. A retried `board send` re-posts the same
// (sender, CmdID); it must be delivered once.
const seenCap = 10000

// queueCap bounds one recipient's backlog.
const queueCap = 256

// New builds a courier. Close stops it.
func New(d Deps) *Courier {
	if d.IsSocket == nil {
		d.IsSocket = IsSocket
	}
	if d.Backoff == nil {
		d.Backoff = DefaultBackoff
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Courier{deps: d, ctx: ctx, cancel: cancel, queues: map[string]chan job{}, seen: map[string]struct{}{}}
}

// Drain waits until every command taken so far has finished — delivered,
// reported undelivered or skipped — or until timeout. It does not stop the
// courier; Close does.
func (c *Courier) Drain(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() { c.pending.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Close stops every queue and waits for in-flight deliveries to return.
func (c *Courier) Close() {
	c.cancel()
	c.wg.Wait()
}

// Handle takes one board event. Everything but a well-formed command is
// ignored: status posts are never work.
func (c *Courier) Handle(ev board.Event) {
	p := ev.Post
	if ev.Kind != board.EventPost || p == nil || p.Kind != msg.BoardKindCmd ||
		p.ToPaneID == "" || p.CmdID == "" {
		return
	}
	key := ev.Board + "\x00" + p.PaneID + "\x00" + p.CmdID

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx.Err() != nil {
		return
	}
	if _, dup := c.seen[key]; dup {
		return
	}
	c.seen[key] = struct{}{}
	c.order = append(c.order, key)
	if len(c.order) > seenCap {
		delete(c.seen, c.order[0])
		c.order = c.order[1:]
	}

	q := c.queues[p.ToPaneID]
	if q == nil {
		q = make(chan job, queueCap)
		c.queues[p.ToPaneID] = q
		c.wg.Add(1)
		go c.run(q)
	}
	c.pending.Add(1)
	select {
	case q <- job{board: ev.Board, post: p}:
	default:
		c.pending.Done()
		go c.undelivered(ev.Board, p, fmt.Sprintf("the recipient's queue is full (%d commands waiting)", queueCap))
	}
}

func (c *Courier) run(q chan job) {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case j := <-q:
			c.deliver(j)
			c.pending.Done()
		}
	}
}

// deliver tries a command until it lands, the schedule runs out, or a
// permanent refusal makes waiting pointless.
func (c *Courier) deliver(j job) {
	p := j.post
	text := Compose(j.board, p, c.deps.Bin, c.deps.Session)
	var last error
	for attempt := 0; attempt <= len(c.deps.Backoff); attempt++ {
		if attempt > 0 {
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(c.deps.Backoff[attempt-1]):
			}
		}
		t, err := c.deps.Snapshot(p.ToPaneID)
		if err != nil {
			last = fmt.Errorf("recipient pane %s did not answer: %v", short(p.ToPaneID), err)
			continue
		}
		if !c.deps.AsListener && strings.TrimSpace(t.Meta[MetaFleetComm]) == CommQueue {
			// queue messaging: the recipient's own listener takes this one.
			c.deps.Log.Debug("courier: queue pane, left to its listener", "cmd", p.CmdID, "pane", short(p.ToPaneID))
			return
		}
		r, err := Plan(t, c.deps.IsSocket)
		if err != nil {
			last = err
			if errors.Is(err, ErrPermanent) {
				break
			}
			continue
		}
		ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
		err = c.deps.Deliver(ctx, r, t, text)
		cancel()
		if err == nil {
			c.deps.Log.Info("courier: delivered", "cmd", p.CmdID, "to", p.ToPersona,
				"pane", short(p.ToPaneID), "route", r.Adapter, "board", j.board, "attempt", attempt+1)
			if c.deps.Delivered != nil {
				c.deps.Delivered(j.board, p, r)
			}
			return
		}
		last = fmt.Errorf("%s: %v", r.Adapter, err)
	}
	if c.ctx.Err() != nil {
		return
	}
	c.undelivered(j.board, p, strings.TrimPrefix(last.Error(), ErrPermanent.Error()+": "))
}

// undelivered files the failure on the command's own thread, where the sender
// and the human will look for the answer.
func (c *Courier) undelivered(boardID string, p *msg.MsgBoardPost, reason string) {
	c.deps.Log.Warn("courier: undelivered", "cmd", p.CmdID, "to", p.ToPersona, "reason", reason)
	if c.deps.Notify == nil {
		return
	}
	who := c.deps.Identity
	if who == "" {
		who = Identity
	}
	n := msg.NewBoardPost(who, who, UndeliveredKind,
		fmt.Sprintf("undelivered %s → %s: %s", p.CmdID, p.ToPersona, reason), time.Now().UnixMilli())
	n.ThreadID, n.CmdID = p.ThreadID, p.CmdID
	c.deps.Notify(boardID, n)
}
