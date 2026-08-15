// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// durable.go — the JetStream half of design 006 §3.1: a per-session stream
// "RYSH_REPLAY_<session>" configured directly on the merged pane output
// subject ("<session>.pane.*.output") and the pane resize subject
// ("<session>.pane.*.resized"), so the broker captures every publish itself —
// no double-publishing, no mailbox hop — and both the output and the
// dimension timeline survive a daemon restart. Export walks this stream when
// the in-memory buffer is empty (the post-restart case); the in-memory
// capture stays the primary path (no per-sequence fetch loop on the hot
// path).

// Durable stream retention defaults (design 006 §3.1: 7d / 1GB, whichever
// first). Overridable via `[replay] retention` / `max_bytes` in
// rysh.config.yaml.
const (
	DefaultRetention = 7 * 24 * time.Hour
	DefaultMaxBytes  = 1 << 30 // 1 GiB
)

// DurableOptions bounds the durable stream. Zero values mean the defaults.
type DurableOptions struct {
	Retention time.Duration // stream MaxAge; 0 ⇒ DefaultRetention
	MaxBytes  int64         // stream MaxBytes; 0 ⇒ DefaultMaxBytes
}

// StreamName returns the per-session replay stream name (design 006 §3.1),
// with the session mapped into JetStream's allowed name characters.
func StreamName(session string) string {
	return "RYSH_REPLAY_" + sanitizeStreamName(session)
}

// EnableDurable ensures the session's replay stream exists on the merged pane
// output and pane resize subjects — the same subjects Start subscribes to, so
// the durable and in-memory captures always record the same traffic (an
// existing stream's subject list is refreshed by the UpdateStream below, so
// streams created before resize capture existed pick it up). Idempotent: on restart
// the existing stream is reused and its limits refreshed to the current
// options. js/streamName are written once here and only read from the owning
// actor's goroutine, so the mutex stays scoped to the event store.
func (c *Capture) EnableDurable(opts DurableOptions) error {
	if c.nc == nil {
		return fmt.Errorf("replay: durable capture needs a NATS connection")
	}
	js, err := c.nc.JetStream()
	if err != nil {
		return fmt.Errorf("replay: jetstream unavailable: %w", err)
	}
	if opts.Retention <= 0 {
		opts.Retention = DefaultRetention
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	name := StreamName(c.session)
	cfg := &nats.StreamConfig{
		Name:      name,
		Subjects:  []string{msg.T("pane", "*", "output"), msg.T("pane", "*", "resized")},
		Storage:   nats.FileStorage,
		Retention: nats.LimitsPolicy,
		Discard:   nats.DiscardOld,
		MaxAge:    opts.Retention,
		MaxBytes:  opts.MaxBytes,
	}
	// Create if missing; reuse if present (AddStream errors on a name clash,
	// so check first — same pattern as the proxy-audit stream).
	if _, err := js.StreamInfo(name); err != nil {
		if _, err := js.AddStream(cfg); err != nil {
			return fmt.Errorf("replay: create stream %s: %w", name, err)
		}
	} else {
		// Refreshing limits on an existing stream is best-effort: the stream
		// keeps capturing under its previous limits, which is strictly better
		// than failing durable capture over a config tweak.
		_, _ = js.UpdateStream(cfg)
	}
	c.js = js
	c.streamName = name
	return nil
}

// DeleteDurable removes the session's replay stream if present. Called when
// capture is configured OFF: the stream captures publishes at the broker, so
// a leftover from a previously-enabled run would keep storing output while
// ##replay reports capture off — exactly the storage surprise design 006's
// opt-in exists to prevent. Recorded output is local-only and deletable with
// the session (design 006 §5).
func DeleteDurable(nc *nats.Conn, session string) {
	if nc == nil {
		return
	}
	js, err := nc.JetStream()
	if err != nil {
		return
	}
	name := StreamName(session)
	if _, err := js.StreamInfo(name); err == nil {
		_ = js.DeleteStream(name)
	}
}

// DurableInfo reports whether durable capture is active and the stream's
// stored message count. False covers both "never enabled" (JetStream absent,
// capture configured off) and "stream lost since enable".
func (c *Capture) DurableInfo() (bool, uint64) {
	if c.js == nil || c.streamName == "" {
		return false, 0
	}
	info, err := c.js.StreamInfo(c.streamName)
	if err != nil {
		return false, 0
	}
	return true, info.State.Msgs
}

// streamEvents rebuilds recorded events for a pane ("" = all panes) from the
// durable stream, decoding the same MsgConversationAppend envelopes onMsg
// records live. The walk is a per-sequence fetch loop against the embedded
// (in-process) broker; sequence holes from retention expiry are skipped, and
// non-append payloads on the subject are ignored rather than failing the
// export.
func (c *Capture) streamEvents(paneID string) []recEvent {
	if c.js == nil || c.streamName == "" || c.codecs == nil {
		return nil
	}
	info, err := c.js.StreamInfo(c.streamName)
	if err != nil || info.State.Msgs == 0 {
		return nil
	}
	var evs []recEvent
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq; seq++ {
		rm, err := c.js.GetMsg(c.streamName, seq)
		if err != nil {
			continue
		}
		if strings.HasSuffix(rm.Subject, ".resized") {
			continue // dimension timeline; streamResizes reads these
		}
		if paneID != "" && paneFromSubject(rm.Subject) != paneID {
			continue
		}
		var env msg.NATSEnvelope
		if err := json.Unmarshal(rm.Data, &env); err != nil {
			continue
		}
		decoded, err := c.codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			continue
		}
		ca, ok := decoded.(*msg.MsgConversationAppend)
		if !ok || ca.Message == nil || ca.Message.Content == "" {
			continue
		}
		if ca.Message.Role == RoleReplay {
			continue // replayed output stored by the broker; not part of the recording
		}
		evs = append(evs, recEvent{ts: ca.Message.TimestampMs, text: ca.Message.Content})
	}
	// Stream order is publish order; export order is timestamp order, same as
	// the in-memory path.
	sort.Slice(evs, func(i, j int) bool { return evs[i].ts < evs[j].ts })
	return evs
}

// streamResizes rebuilds the resize timeline for a pane ("" = all panes) from
// the durable stream's captured .resized messages. MsgPaneResized carries no
// timestamp of its own, so the entry timestamp is the message's JetStream
// receipt time — the broker-side equivalent of the wall-clock receipt time the
// in-memory capture records (design 006 §3.1: stored messages keep their
// original NATS timestamp).
func (c *Capture) streamResizes(paneID string) []resizeRec {
	if c.js == nil || c.streamName == "" || c.codecs == nil {
		return nil
	}
	info, err := c.js.StreamInfo(c.streamName)
	if err != nil || info.State.Msgs == 0 {
		return nil
	}
	var out []resizeRec
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq; seq++ {
		rm, err := c.js.GetMsg(c.streamName, seq)
		if err != nil {
			continue
		}
		if !strings.HasSuffix(rm.Subject, ".resized") {
			continue
		}
		var env msg.NATSEnvelope
		if err := json.Unmarshal(rm.Data, &env); err != nil {
			continue
		}
		decoded, err := c.codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			continue
		}
		rz, ok := decoded.(*msg.MsgPaneResized)
		if !ok || rz.Cols <= 0 || rz.Rows <= 0 {
			continue
		}
		pane := rz.PaneID
		if pane == "" {
			pane = paneFromSubject(rm.Subject)
		}
		if paneID != "" && pane != paneID {
			continue
		}
		out = append(out, resizeRec{ts: rm.Time.UnixMilli(), d: dim{cols: rz.Cols, rows: rz.Rows}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ts < out[j].ts })
	return out
}

// sanitizeStreamName keeps a session name to the character set JetStream
// allows in a stream name (no '.', '*', '>', whitespace).
func sanitizeStreamName(s string) string {
	if s == "" {
		return "default"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}
