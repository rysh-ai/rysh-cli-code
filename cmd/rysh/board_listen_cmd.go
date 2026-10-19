// SPDX-License-Identifier: Apache-2.0

package main

// `rysh board listen` — one agent's queue listener (design 051, queue-messaging).
//
// In queue-messaging there is no central deliverer. Every agent has its own
// listener on the board; each one takes only the prompts addressed to its own
// agent and lets everything else pass. The board is the queue: a prompt posted
// while its agent is down stays open there, and the listener catches it up when
// it (re)starts.
//
//	rysh board listen [--as <pane-id>] [--board <id>] [--session <name>] [--state-dir <dir>] [--once]
//
// One listener per pane (a lock in the state dir); a second one exits 0. It
// stops by itself when its pane is gone or the pane leaves queue messaging
// (fleet.comm set to anything else), so a listener never outlives its agent's
// place in a fleet. --once catches up the backlog and exits.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/courier"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

const boardListenUsage = "usage: rysh board listen [--as <pane-id>] [--board <id>] [--session <name>] [--state-dir <dir>] [--once]"

// listenCheckEvery is how often a listener confirms its pane still exists and
// is still in queue mode. listenGoneAfter misses in a row mean the pane is gone.
const (
	listenCheckEvery = 20 * time.Second
	listenGoneAfter  = 3
)

type boardListenArgs struct {
	as, board, sess, stateDir string
	once                      bool
}

func parseBoardListenArgs(rest []string) (boardListenArgs, error) {
	var a boardListenArgs
	rest, a.sess = extractStringFlag(rest, "--session")
	rest, a.as = extractStringFlag(rest, "--as")
	rest, a.board = extractStringFlag(rest, "--board")
	if a.board == "" {
		rest, a.board = extractStringFlag(rest, "--fleet")
	}
	rest, a.stateDir = extractStringFlag(rest, "--state-dir")
	rest, a.once = extractBoolFlag(rest, "--once")
	if len(rest) > 0 {
		return a, fmt.Errorf("rysh board listen takes no positional arguments, got %q\n%s",
			strings.Join(rest, " "), progname.Rewrite(boardListenUsage))
	}
	if a.board != "" {
		if err := msg.ValidateBoardID(a.board); err != nil {
			return a, err
		}
		a.board = msg.NormalizeBoardID(a.board)
	}
	return a, nil
}

func runBoardListen(cfg config.Config, rest []string) error {
	a, err := parseBoardListenArgs(rest)
	if err != nil {
		return err
	}
	if a.as == "" {
		a.as = os.Getenv("RYSH_PANE")
	}
	if a.as == "" {
		return errors.New(progname.Rewrite("rysh board listen: whose listener? pass --as <pane-id>, or run inside a pane"))
	}
	store, sess, err := resolveBoardSession(cfg, a.sess)
	if err != nil {
		return err
	}
	resp, err := cli.BoardResolve(store, sess, a.as)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	me, boardID := resp.ID, strings.TrimSpace(strings.SplitN(resp.Output, "\n", 2)[0])
	if a.board != "" {
		boardID = a.board
	}
	if a.stateDir == "" {
		a.stateDir = defaultListenStateDir(store, sess)
	}

	// ONE listener per agent. A second start is not an error: it is the normal
	// shape of "make sure it is listening" (fleetctl listen, a resume).
	lock, ok, err := courier.LockPane(filepath.Join(a.stateDir, me+".lock"))
	if err != nil {
		return fmt.Errorf("listener lock: %w", err)
	}
	if !ok {
		fmt.Printf("already listening for %s on board %s\n", me, boardID)
		return nil
	}
	defer lock.Close()
	pidPath := filepath.Join(a.stateDir, me+".pid")
	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
	defer os.Remove(pidPath)

	c, err := cli.ConnectPersistent(store, sess)
	if err != nil {
		return err
	}
	defer c.Close()
	pub := c.Publisher()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("listener", me[:min(8, len(me))], "board", boardID)

	dlog := courier.NewDeliveredLog(filepath.Join(a.stateDir, me+".delivered"))
	delivered := dlog.Load()
	bin, _ := os.Executable()
	cour := courier.New(courier.Deps{
		AsListener: true,
		Identity:   "listener",
		Bin:        bin,
		Session:    sess,
		Backoff:    courier.ListenerBackoff,
		Log:        log,
		Snapshot:   func(id string) (courier.Target, error) { return courier.SnapshotVia(pub, id) },
		Deliver: func(ctx context.Context, r courier.Route, t courier.Target, text string) error {
			return courier.Dispatch(ctx, pub, r, t, text)
		},
		Notify: func(b string, p *msg.MsgBoardPost) {
			if err := msg.SendBoardPost(pub, b, p); err != nil {
				log.Warn("could not post undelivered notice", "err", err)
			}
		},
		Delivered: func(b string, p *msg.MsgBoardPost, _ courier.Route) {
			dlog.Record(courier.DeliveredKey(b, p))
		},
	})
	defer cour.Close()

	// SUBSCRIBE BEFORE CATCHING UP: a prompt posted between the two is then
	// seen twice rather than never, and the courier delivers it once.
	sub, err := board.Subscribe(c.Conn(), c.Codecs(), 0, nil)
	if err != nil {
		return err
	}
	defer sub.Close()

	q := boardID
	if q == msg.DefaultBoardID {
		q = ""
	}
	if reply, err := cli.BoardTail(store, sess, q, board.Query{}); err != nil {
		log.Warn("catch-up skipped: the recorder did not answer", "err", err)
	} else {
		backlog := courier.CatchUp(reply, me, boardID, delivered)
		if len(backlog) > 0 {
			log.Info("catching up open prompts", "count", len(backlog))
		}
		for _, p := range backlog {
			cour.Handle(board.Event{Kind: board.EventPost, Board: boardID, Post: p})
		}
	}
	fmt.Printf("listening for %s on board %s (state %s)\n", me, boardID, a.stateDir)

	if a.once {
		// Wait for the backlog to be handed over (or given up on), then stop.
		if !cour.Drain(10 * time.Minute) {
			log.Warn("--once: backlog still delivering after 10m; exiting")
		}
		return nil
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	tick := time.NewTicker(listenCheckEvery)
	defer tick.Stop()
	misses := 0
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return nil
			}
			if courier.ListenerFor(ev, me, boardID) && !delivered[courier.DeliveredKey(boardID, ev.Post)] {
				cour.Handle(ev)
			}
		case <-tick.C:
			t, err := courier.SnapshotVia(pub, me)
			if err != nil {
				if misses++; misses >= listenGoneAfter {
					log.Info("pane is gone; listener exits")
					return nil
				}
				continue
			}
			misses = 0
			if comm := strings.TrimSpace(t.Meta[courier.MetaFleetComm]); comm != "" && comm != courier.CommQueue {
				log.Info("pane left queue messaging; listener exits", "comm", comm)
				return nil
			}
		case <-sigs:
			return nil
		}
	}
}

// runBoardCaps prints the RUNNING daemon's board capabilities, one per line
// (`rysh board caps [--as <pane>] [--session <name>]`). It asks the daemon, not
// this binary: a session keeps its old daemon however new the file on disk is.
func runBoardCaps(cfg config.Config, rest []string) error {
	rest, sess := extractStringFlag(rest, "--session")
	rest, as := extractStringFlag(rest, "--as")
	if len(rest) > 0 {
		return fmt.Errorf("usage: rysh board caps [--as <pane-id>] [--session <name>]")
	}
	if as == "" {
		as = os.Getenv("RYSH_PANE")
	}
	store, sess, err := resolveBoardSession(cfg, sess)
	if err != nil {
		return err
	}
	caps, err := cli.BoardCaps(store, sess, as)
	if err != nil {
		return err
	}
	for _, c := range caps {
		fmt.Println(c)
	}
	return nil
}

// defaultListenStateDir is <rysh_dir>/listen of the session, so fleetctl and a
// human find the same lock, pid and delivered files.
func defaultListenStateDir(store *session.Store, sess string) string {
	if rec, err := store.Get(sess); err == nil && strings.TrimSpace(rec.RyshDir) != "" {
		return filepath.Join(rec.RyshDir, "listen")
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "rysh", "listen", sess)
	}
	return filepath.Join(os.TempDir(), "rysh-listen-"+sess)
}
