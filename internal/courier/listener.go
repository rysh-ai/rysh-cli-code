// SPDX-License-Identifier: Apache-2.0

package courier

// The per-agent queue listener (design 051, queue-messaging).
//
// grid-messaging has ONE deliverer: the daemon's courier pushes every prompt
// into its recipient's inbox. queue-messaging has none at the centre: every
// agent runs its own listener on the board, and each listener takes only the
// prompts addressed to its own agent — agent x posts "x → y", and x, y and z
// all see it on the board; y's listener takes it, x's and z's let it pass.
//
// The board is the queue. A prompt posted while its agent is down is not lost
// and not refused: it stays on the board, open, until that agent's listener
// starts and catches up. Delivered prompts are recorded per agent so a restarted
// listener does not hand the same order over twice.

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ListenerFor reports whether a board event is a prompt for this listener's
// agent on this listener's board. Everything else — status posts, prompts for
// other agents, other boards — is somebody else's, and passes.
func ListenerFor(ev board.Event, me, boardID string) bool {
	p := ev.Post
	return ev.Kind == board.EventPost && p != nil && p.Kind == msg.BoardKindCmd &&
		p.ToPaneID == me && p.CmdID != "" &&
		msg.NormalizeBoardID(ev.Board) == msg.NormalizeBoardID(boardID)
}

// DeliveredKey identifies one prompt on one board: (board, sender, cmd id).
func DeliveredKey(boardID string, p *msg.MsgBoardPost) string {
	return msg.NormalizeBoardID(boardID) + "\t" + p.PaneID + "\t" + p.CmdID
}

// CatchUp returns the prompts still OPEN for me on a board — never acked or
// answered by me, and not already delivered by an earlier listener — oldest
// first, so a listener that starts late hands over the backlog in order.
func CatchUp(r *board.QueryReply, me, boardID string, delivered map[string]bool) []*msg.MsgBoardPost {
	if r == nil {
		return nil
	}
	open := map[string]bool{}
	for _, it := range board.Inbox(r, me) {
		if !it.Answered {
			open[it.FromPaneID+"\t"+it.CmdID] = true
		}
	}
	var out []*msg.MsgBoardPost
	seen := map[string]bool{}
	for _, t := range r.Threads {
		posts := append([]*msg.MsgBoardPost{t.Root}, t.Replies...)
		for _, p := range posts {
			if p == nil || p.Kind != msg.BoardKindCmd || p.ToPaneID != me || p.CmdID == "" {
				continue
			}
			k := DeliveredKey(boardID, p)
			if seen[k] || delivered[k] || !open[p.PaneID+"\t"+p.CmdID] {
				continue
			}
			seen[k] = true
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out
}

// DeliveredLog is the listener's record of what it has handed over, one key
// per line, append-only. It is per agent pane, in the listener's state dir.
type DeliveredLog struct {
	mu   sync.Mutex
	path string
}

func NewDeliveredLog(path string) *DeliveredLog { return &DeliveredLog{path: path} }

// Load reads every recorded key. A missing file is an empty record.
func (d *DeliveredLog) Load() map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(d.path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out[line] = true
		}
	}
	return out
}

// Record appends one key. Best effort: a failure costs a possible duplicate
// after a restart, never a lost prompt.
func (d *DeliveredLog) Record(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = os.MkdirAll(filepath.Dir(d.path), 0o755)
	f, err := os.OpenFile(d.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(key + "\n")
}

// ListenerBackoff is longer than the courier's: a queue listener's agent may
// simply be restarting, and the prompt is safe on the board meanwhile. After
// it runs out the listener files `undelivered`; a restarted listener still
// catches the prompt up, because it remains open on the board.
var ListenerBackoff = []time.Duration{
	time.Second, 2 * time.Second, 5 * time.Second, 15 * time.Second,
	30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute,
}
