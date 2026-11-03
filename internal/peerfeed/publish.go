// SPDX-License-Identifier: Apache-2.0

package peerfeed

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/peer"
)

// The publisher (design 044 amendment 2026-10-20, N3): the ONLY path by which
// a local board's posts reach a peer board, and only for boards the user
// published with `peer publish`. The contract it keeps:
//
//   - exact subjects: one subscription per published board, on that board's
//     post subject; never a wildcard;
//   - forward-only: posts stamped before the board was published are skipped;
//   - text only: persona, kind and text — never the attachment path, pane
//     output or history; status kinds only unless the user chose --all;
//   - revocable at once: the saved list is re-read before every send, and a
//     queued post of a board that is no longer published is dropped;
//   - idempotent: a post's peer id is derived from the post itself, so a retry
//     or a restart never stores it twice;
//   - bounded: an outage keeps at most Max posts, dropping the oldest.

// publishedNamespace scopes PublishedMessageID.
var publishedNamespace = uuid.MustParse("6f1d7a52-4a4e-4d0b-9a55-1d8e2b0a7c31")

// maxPublishedText caps one published post (the server takes up to 16 KiB).
const maxPublishedText = 4000

type pendingPost struct {
	board string
	post  *msg.MsgBoardPost
}

// Publisher sends posts of explicitly published local boards to the peer board.
type Publisher struct {
	Root, Session string
	NC            *nats.Conn
	Codecs        *msg.CodecRegistry
	Key           func(name string) string
	Max           int

	mu      sync.Mutex
	subs    map[string]*nats.Subscription
	pending []pendingPost
	dropped int
}

func (p *Publisher) bound() int {
	if p.Max <= 0 {
		return 256
	}
	return p.Max
}

// Reconcile subscribes to every published board and unsubscribes from every
// board that is no longer published.
func (p *Publisher) Reconcile() error {
	pubs, err := peer.LoadPublications(p.Root, p.Session)
	if err != nil {
		return err
	}
	c, err := peer.Load(p.Root, p.Session)
	if err != nil {
		return err
	}
	if !c.Enabled {
		pubs.Boards = nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.subs == nil {
		p.subs = map[string]*nats.Subscription{}
	}
	for board, sub := range p.subs {
		if _, ok := pubs.Boards[board]; !ok {
			_ = sub.Unsubscribe()
			delete(p.subs, board)
		}
	}
	if p.NC == nil {
		return nil
	}
	for board := range pubs.Boards {
		if _, ok := p.subs[board]; ok || !peer.ValidLocalBoard(board) {
			continue
		}
		name := board
		sub, err := p.NC.Subscribe(msg.BoardPostSubject(name), func(m *nats.Msg) { p.receive(name, m) })
		if err != nil {
			return err
		}
		p.subs[name] = sub
	}
	return nil
}

// receive decodes one local post and queues it. It never blocks the NATS
// callback on the network.
func (p *Publisher) receive(board string, m *nats.Msg) {
	var env msg.NATSEnvelope
	if json.Unmarshal(m.Data, &env) != nil || p.Codecs == nil {
		return
	}
	decoded, err := p.Codecs.Decode(env.TypeTag, env.Payload)
	if err != nil {
		return
	}
	post, ok := decoded.(*msg.MsgBoardPost)
	if !ok || post == nil {
		return
	}
	pub, ok := p.publication(board)
	if !ok || !wanted(pub, post) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending = append(p.pending, pendingPost{board: board, post: post})
	if over := len(p.pending) - p.bound(); over > 0 {
		p.pending = append([]pendingPost(nil), p.pending[over:]...)
		p.dropped += over
	}
}

// publication re-reads the saved list: the user's latest word wins.
func (p *Publisher) publication(board string) (peer.Publication, bool) {
	pubs, err := peer.LoadPublications(p.Root, p.Session)
	if err != nil {
		return peer.Publication{}, false
	}
	pub, ok := pubs.Boards[board]
	return pub, ok
}

// wanted applies the forward-only and kind rules.
func wanted(pub peer.Publication, post *msg.MsgBoardPost) bool {
	if post.TS < pub.Since {
		return false
	}
	if !pub.All && (post.Kind == msg.BoardKindCmd || post.Kind == msg.BoardKindAck) {
		return false
	}
	return true
}

// Flush sends what is queued. A post whose board is no longer published is
// dropped; a failed send stays queued and is retried with the same id.
func (p *Publisher) Flush(ctx context.Context) {
	p.mu.Lock()
	queue := p.pending
	p.pending = nil
	p.mu.Unlock()
	if len(queue) == 0 {
		return
	}
	c, err := peer.Load(p.Root, p.Session)
	var keep []pendingPost
	for i, item := range queue {
		pub, ok := p.publication(item.board)
		if err != nil || !c.Enabled || !ok || !wanted(pub, item.post) {
			continue
		}
		client := peer.Client{Connection: c, Key: p.Key(c.KeySecret)}
		post := peer.Post{
			ID:          PublishedMessageID(c.SessionID, item.board, item.post),
			SessionID:   c.SessionID,
			SessionName: p.Session,
			AgentID:     item.post.PaneID,
			AgentName:   item.post.Persona,
			Text:        publishedText(item.board, item.post),
		}
		if _, e := client.Send(ctx, post); e != nil {
			keep = append(keep, queue[i:]...)
			break
		}
	}
	if len(keep) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending = append(keep, p.pending...)
	if over := len(p.pending) - p.bound(); over > 0 {
		p.pending = append([]pendingPost(nil), p.pending[over:]...)
		p.dropped += over
	}
}

// publishedText is the post as it appears on the peer board: board, kind and
// text. Nothing else of the post leaves the session.
func publishedText(board string, post *msg.MsgBoardPost) string {
	kind := post.Kind
	if kind == "" {
		kind = "post"
	}
	text := fmt.Sprintf("[board %s] %s: %s", board, kind, post.Text)
	if r := []rune(text); len(r) > maxPublishedText {
		text = string(r[:maxPublishedText-1]) + "…"
	}
	return text
}

// Pending is how many posts wait to be sent.
func (p *Publisher) Pending() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pending)
}

// Subjects lists the subjects the publisher listens on, sorted.
func (p *Publisher) Subjects() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.subs))
	for _, sub := range p.subs {
		out = append(out, sub.Subject)
	}
	sort.Strings(out)
	return out
}

// Close unsubscribes from everything.
func (p *Publisher) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for board, sub := range p.subs {
		_ = sub.Unsubscribe()
		delete(p.subs, board)
	}
	p.pending = nil
}

// Run reconciles and flushes until stop is closed.
func (p *Publisher) Run(stop <-chan struct{}, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	defer p.Close()
	for {
		_ = p.Reconcile()
		ctx, cancel := context.WithTimeout(context.Background(), every+10*time.Second)
		p.Flush(ctx)
		cancel()
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

// PublishedMessageID is the peer message id of a published local post: the
// same post always gets the same id, so the server stores it once.
func PublishedMessageID(sessionID, board string, post *msg.MsgBoardPost) string {
	if post == nil {
		return ""
	}
	key := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s\x00%s", sessionID, board, post.PaneID, post.TS, post.Kind, post.Text)
	return uuid.NewSHA1(publishedNamespace, []byte(key)).String()
}
