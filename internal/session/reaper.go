package session

import (
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
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
	probe func(port int, timeout time.Duration) bool
}

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
		o.probe = natsReachable
	}
}

// Reap scans the registry and cleans up orphaned daemons:
//
//   - A record whose daemon PID is dead is reconciled to stopped (this happens
//     as a side effect of List; nothing is killed).
//   - A daemon that is alive but unreachable on its recorded NATS port — i.e.
//     wedged or zombied, no longer serving — is gracefully terminated
//     (SIGTERM→SIGKILL via Terminate) and its record marked stopped.
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
		// A daemon that answers on its NATS port is healthy — leave it running.
		if probeWithRetries(opts, rec.NATSPort) {
			continue
		}

		// Live PID, past startup grace, not serving NATS ⇒ wedged orphan.
		reason := fmt.Sprintf("daemon PID %d alive but NATS port %d unreachable", rec.PID, rec.NATSPort)
		action := ReapAction{Name: rec.Name, Reason: reason}
		if err := Terminate(rec.PID, 3*time.Second); err != nil {
			action.Reason = fmt.Sprintf("%s; terminate failed: %v", reason, err)
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

// probeWithRetries reports whether the NATS server on port answers within the
// configured number of attempts. A small pause between attempts avoids treating
// a momentary blip as a dead server.
func probeWithRetries(opts ReapOptions, port int) bool {
	for i := 0; i < opts.NATSProbeAttempts; i++ {
		if opts.probe(port, opts.NATSProbeTimeout) {
			return true
		}
		if i < opts.NATSProbeAttempts-1 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return false
}

// natsReachable reports whether a NATS server answers on 127.0.0.1:port. It uses
// the same anonymous local connection the rest of the store uses (see
// deleteNATSBucketsViaAPI).
func natsReachable(port int, timeout time.Duration) bool {
	if port <= 0 {
		return false
	}
	nc, err := nats.Connect(
		fmt.Sprintf("nats://127.0.0.1:%d", port),
		nats.MaxReconnects(0),
		nats.Timeout(timeout),
	)
	if err != nil {
		return false
	}
	nc.Close()
	return true
}
