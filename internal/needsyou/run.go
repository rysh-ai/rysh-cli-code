// SPDX-License-Identifier: Apache-2.0

package needsyou

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// SessionClient is the part of cli.Client the runner uses (the same shape as
// remoteserve.SessionClient).
type SessionClient interface {
	RequestFromSubject(subject string, m interface{}, timeout time.Duration) (interface{}, error)
	// SubscribeExact subscribes to one exact subject (never a wildcard).
	SubscribeExact(subject string, fn func(interface{})) (unsubscribe func(), err error)
}

// Options configure Run.
type Options struct {
	Session  string
	Host     string // this machine's name, so a phone can tell hosts apart
	After    time.Duration
	Cooldown time.Duration
	// DoneAfter turns on "task done" alerts for runs at least this long
	// (0 = off). They share Cooldown and the sinks.
	DoneAfter time.Duration
	Sinks     []Sink
	// Tick is how often a pending raise is looked at again (default 2s).
	Tick   time.Duration
	Logger *slog.Logger
}

const (
	defaultTick     = 2 * time.Second
	snapshotTimeout = 5 * time.Second
	// sendTimeout bounds one alert on one sink.
	sendTimeout = 30 * time.Second
	// idleLook is how often the layout is read when nothing is pending and
	// nothing changed: a safety net for a missed layoutDirty.
	idleLook = 30 * time.Second
	// doneLook is how often the layout is read while "done" alerts are on: a
	// foreground program starting or ending does not signal layoutDirty, so
	// a run is timed to within this much.
	doneLook = 5 * time.Second
)

// Run watches the session until ctx ends. It reads the layout when it changes
// (exact subject ws.layoutDirty), while a raise is pending (every Tick), and
// every idleLook regardless.
func Run(ctx context.Context, c SessionClient, o Options) error {
	if len(o.Sinks) == 0 {
		return errors.New("needs-you notifier: no sink configured")
	}
	if o.Tick <= 0 {
		o.Tick = defaultTick
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	dirty := make(chan struct{}, 1)
	unsub, err := c.SubscribeExact(msg.T("ws", "layoutDirty"), func(interface{}) {
		select {
		case dirty <- struct{}{}:
		default:
		}
	})
	if err != nil {
		return fmt.Errorf("needs-you notifier: subscribe layout: %w", err)
	}
	defer unsub()

	w := NewWatcher(o.After, o.Cooldown)
	dw := NewDoneWatcher(o.DoneAfter, o.Cooldown)
	tick := time.NewTicker(o.Tick)
	defer tick.Stop()
	lastLook := time.Time{}
	look := func() {
		lastLook = time.Now()
		reply, err := c.RequestFromSubject(msg.T("ws", "snapshot"),
			&msg.MsgGetWorkspaceSnapshot{LayoutOnly: true, NoHistories: true}, snapshotTimeout)
		if err != nil {
			log.Debug("needs-you notifier: snapshot failed", "err", err)
			return
		}
		r, ok := reply.(*msg.MsgWorkspaceSnapshotReply)
		if !ok {
			return
		}
		now := time.Now()
		var alerts []Alert
		for _, d := range dw.Observe(now, StatesFromTabs(r.Snapshot.Tabs)) {
			alerts = append(alerts, Alert{Event: EventDone, Session: o.Session, Host: o.Host, PaneID: d.ID, PaneName: d.Name,
				Reason: d.Reason, Since: now.UnixMilli(), Ran: d.Ran})
		}
		for _, p := range w.Observe(now, PanesFromTabs(r.Snapshot.Tabs)) {
			alerts = append(alerts, Alert{Session: o.Session, Host: o.Host, PaneID: p.ID, PaneName: p.Name, Reason: p.Reason, Since: p.Since})
		}
		for _, a := range alerts {
			for _, s := range o.Sinks {
				go deliver(ctx, log, s, a)
			}
		}
	}
	look()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-dirty:
			look()
		case <-tick.C:
			since := time.Since(lastLook)
			if w.Pending() || since >= idleLook || (o.DoneAfter > 0 && since >= min(doneLook, o.DoneAfter)) {
				look()
			}
		}
	}
}

// deliver sends one alert on one sink: one try, logged, never retried (a
// retry loop toward a broken endpoint is how a notifier becomes a flood).
func deliver(ctx context.Context, log *slog.Logger, s Sink, a Alert) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := s.Send(ctx, a); err != nil {
		log.Warn("needs-you notifier: alert not delivered", "sink", s.Name(), "pane", a.PaneID, "err", err)
		return
	}
	log.Info("needs-you notifier: alert sent", "sink", s.Name(), "event", a.event(), "pane", a.PaneID, "reason", a.Reason)
}
