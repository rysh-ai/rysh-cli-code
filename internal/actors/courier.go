// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"log/slog"
	"os"
	"sync"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/courier"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// BoardCourierActor runs the design-050 courier inside the daemon: it listens
// to every board for commands and delivers each into its recipient agent's own
// session (internal/courier). It lives beside ABLA and shares its lifecycle, so
// a command is never posted to a board nobody is delivering from.
//
// It READS the board and never writes it: its subscriber has no persistence,
// which keeps ABLA the bucket's single writer (abla.go, Start). Its only board
// output is an "undelivered" post, published like any other post and recorded
// by ABLA.
//
// It listens LIVE only. History restored at start is not redelivered: a
// command that was on the board before this daemon started was either
// delivered by the previous one or is reported by nobody — the board shows it
// unanswered, which is the honest state.
type BoardCourierActor struct {
	nc     *nats.Conn
	codecs *msg.CodecRegistry
	pub    *msg.NATSPublisher

	mu      sync.Mutex
	started bool
	sub     *board.Subscriber
	c       *courier.Courier
	done    chan struct{}
}

func NewBoardCourierActor(nc *nats.Conn, codecs *msg.CodecRegistry, pub *msg.NATSPublisher) *BoardCourierActor {
	return &BoardCourierActor{nc: nc, codecs: codecs, pub: pub, done: make(chan struct{})}
}

func (a *BoardCourierActor) Receive(ctx actor.Context) {
	switch ctx.Message().(type) {
	case *actor.Started:
		if err := a.Start(); err != nil {
			slog.Warn("rysh: board courier not started; board commands will not be delivered", "err", err)
		}
	case *actor.Stopping:
		a.Stop()
	}
}

// Start subscribes and pumps commands into the courier. Idempotent.
func (a *BoardCourierActor) Start() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started || a.nc == nil {
		return nil
	}
	sub, err := board.Subscribe(a.nc, a.codecs, 0, nil)
	if err != nil {
		return err
	}
	a.started, a.sub = true, sub
	bin, _ := os.Executable()
	a.c = courier.New(courier.Deps{
		Bin:      bin,
		Session:  msg.SessionPrefix(),
		Snapshot: a.snapshot,
		Deliver:  a.deliver,
		Notify: func(boardID string, p *msg.MsgBoardPost) {
			if err := msg.SendBoardPost(a.pub, boardID, p); err != nil {
				slog.Warn("courier: could not post undelivered notice", "err", err)
			}
		},
	})
	go func() {
		defer close(a.done)
		for ev := range sub.Events() {
			a.c.Handle(ev)
		}
	}()
	return nil
}

// Stop unsubscribes and waits for in-flight deliveries.
func (a *BoardCourierActor) Stop() {
	a.mu.Lock()
	sub, c := a.sub, a.c
	a.sub, a.c = nil, nil
	a.mu.Unlock()
	if sub != nil {
		sub.Close()
		<-a.done
	}
	if c != nil {
		c.Close()
	}
}

// snapshot reads the recipient pane: its foreground program, pgid and meta.
func (a *BoardCourierActor) snapshot(paneID string) (courier.Target, error) {
	return courier.SnapshotVia(a.pub, paneID)
}

// deliver performs one delivery (courier.Dispatch: never rawinput).
func (a *BoardCourierActor) deliver(ctx context.Context, r courier.Route, t courier.Target, text string) error {
	return courier.Dispatch(ctx, a.pub, r, t, text)
}
