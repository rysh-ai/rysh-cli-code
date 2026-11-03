// SPDX-License-Identifier: Apache-2.0

package session

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bustoken"
)

// ReapAction records what the reaper did to a single session, for logging.
type ReapAction struct {
	Name   string
	Killed bool   // a live-but-wedged daemon was terminated
	Reason string // human-readable explanation
}

// ReapOptions tunes orphan detection. The zero value is sensible — every field
// falls back to a default.
type ReapOptions struct {
	// NATSProbeTimeout bounds a single NATS reachability probe.
	NATSProbeTimeout time.Duration
	// NATSProbeAttempts is how many times to probe before declaring a live
	// daemon unreachable, guarding against a transient blip killing a healthy
	// daemon.
	NATSProbeAttempts int
	// StartupGrace skips records updated within this window, so a daemon that is
	// still coming up (PID alive, NATS not yet listening) is never mistaken for a
	// wedged orphan and killed.
	StartupGrace time.Duration

	// now and probe are injection points for tests; nil selects the real impl.
	now   func() time.Time
	probe func(port int, timeout time.Duration) probeResult
}

// probeResult is what one reachability probe learned about a NATS port.
type probeResult int

const (
	// probeServing: a NATS server answered (or refused our token, which still
	// proves a live server).
	probeServing probeResult = iota
	// probeRefused: nothing listens on the port; the connection was refused.
	probeRefused
	// probeInconclusive: anything else, most often a timeout. A daemon on a
	// busy machine can accept the connection and still miss the handshake
	// deadline; that is a slow daemon, not a dead one (QA-21: a CLI start
	// SIGTERMed a live session daemon this way, 2026-11-03 09:32Z).
	probeInconclusive
)

func (o *ReapOptions) applyDefaults() {
	if o.NATSProbeTimeout <= 0 {
		o.NATSProbeTimeout = 750 * time.Millisecond
	}
	if o.NATSProbeAttempts <= 0 {
		o.NATSProbeAttempts = 3
	}
	if o.StartupGrace <= 0 {
		o.StartupGrace = 20 * time.Second
	}
	if o.now == nil {
		o.now = time.Now
	}
	if o.probe == nil {
		o.probe = natsProbe
	}
}

// Reap scans the registry and cleans up orphaned daemons:
//
//   - A record whose daemon PID is dead is reconciled to stopped (this happens
//     as a side effect of List; nothing is killed).
//   - A daemon that is alive but has NO LISTENER on its recorded NATS port
//     (every probe refused) — a zombie no longer serving — is gracefully
//     terminated (SIGTERM→SIGKILL via Terminate) and its record marked stopped.
//     A probe that times out proves nothing: the daemon may only be slow, and
//     killing a live session daemon takes every pane with it, so a timeout
//     never leads to a kill (QA-21).
//
// Healthy daemons (PID alive AND NATS reachable) are deliberately left running,
// even when detached and idle: a long-lived detached session that you can
// re-attach to is an intended feature, so the reaper never kills a daemon that
// is still serving. To stop such a session, use "rysh stop" / "rysh
// delete-session".
//
// Reaping is best-effort: a probe or terminate failure is recorded in the
// returned actions but never aborts the sweep. The returned slice contains only
// sessions the reaper acted on (or tried to).
func (s *Store) Reap(opts ReapOptions) ([]ReapAction, error) {
	opts.applyDefaults()

	// List reconciles dead-PID records to stopped and self-heals them to disk.
	records, err := s.List()
	if err != nil {
		return nil, err
	}

	var actions []ReapAction
	for _, rec := range records {
		// Dead daemons were already reconciled by List — nothing to terminate.
		if !rec.DaemonAlive() {
			continue
		}
		// A daemon that just wrote its record may still be starting up (NATS not
		// listening yet). Don't risk killing it.
		if opts.now().Sub(rec.UpdatedAt) < opts.StartupGrace {
			continue
		}
		// Without a recorded port we cannot prove the daemon is wedged, so we
		// leave it alone rather than risk killing a healthy process.
		if rec.NATSPort <= 0 {
			continue
		}
		// Only a port that refuses every probe proves the daemon stopped
		// serving; a serving or merely slow daemon is left running.
		if !refusedEveryProbe(opts, rec.NATSPort) {
			continue
		}

		// Live PID, past startup grace, nothing listening on its port ⇒ orphan.
		reason := fmt.Sprintf("daemon PID %d alive but nothing listens on NATS port %d", rec.PID, rec.NATSPort)
		action := ReapAction{Name: rec.Name, Reason: reason}
		if err := TerminateDaemon(rec, 3*time.Second); err != nil {
			if errors.Is(err, ErrNotOurDaemon) {
				// The PID was recycled: the daemon is gone and the process now
				// holding its PID is someone else's. Reconcile, signal nothing.
				action.Reason = fmt.Sprintf("daemon PID %d was recycled; record marked stopped, nothing signalled", rec.PID)
				rec.State = "stopped"
				rec.PID = 0
				rec.TUIPIDs = nil
				_, _ = s.Upsert(rec)
			} else {
				action.Reason = fmt.Sprintf("%s; not terminated: %v", reason, err)
			}
			actions = append(actions, action)
			continue
		}
		action.Killed = true
		rec.State = "stopped"
		rec.PID = 0
		rec.TUIPIDs = nil
		_, _ = s.Upsert(rec)
		actions = append(actions, action)
	}
	return actions, nil
}

// refusedEveryProbe reports whether every one of the configured attempts found
// no listener on port. Any answer, and any inconclusive result such as a
// timeout, stops the sweep from touching the daemon. A small pause between
// attempts rides out a listener that is momentarily being re-bound.
func refusedEveryProbe(opts ReapOptions, port int) bool {
	for i := 0; i < opts.NATSProbeAttempts; i++ {
		if opts.probe(port, opts.NATSProbeTimeout) != probeRefused {
			return false
		}
		if i < opts.NATSProbeAttempts-1 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return true
}

// natsProbe classifies the NATS port on 127.0.0.1. It first asks whether
// anything listens at all (a refused TCP connect is the only result that
// counts against the daemon), then whether that listener speaks NATS with the
// session bus token.
func natsProbe(port int, timeout time.Duration) probeResult {
	if port <= 0 {
		return probeInconclusive
	}
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), timeout)
	if err != nil {
		if isConnRefused(err) {
			return probeRefused
		}
		return probeInconclusive
	}
	c.Close()
	nc, err := bustoken.Dial(port,
		nats.MaxReconnects(0),
		nats.Timeout(timeout),
	)
	if bustoken.IsAuthError(err) {
		return probeServing // a server refusing our token is still a live server
	}
	if err != nil {
		return probeInconclusive
	}
	nc.Close()
	return probeServing
}
