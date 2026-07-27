package replay

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// recEvent is one captured output append.
type recEvent struct {
	ts   int64 // unix millis
	text string
}

// Capture subscribes to the merged pane output subject and records timestamped
// output events per pane, for export as an asciicast. It runs on the NATS
// subscription goroutine; the mutex guards the event store (a documented
// exception to the no-mutex rule, at the NATS/actor boundary — same as the
// proxy's audit buffer).
type Capture struct {
	nc        *nats.Conn
	codecs    *msg.CodecRegistry
	session   string
	sub       *nats.Subscription
	resizeSub *nats.Subscription
	maxPer    int

	// Durable half (design 006 §3.1): set once by EnableDurable, before any
	// reader, and only touched from the owning actor's goroutine — so these
	// stay outside the mutex, which remains scoped to the event store. Both
	// zero when JetStream is unavailable (in-memory-only capture).
	js         nats.JetStreamContext
	streamName string

	mu     sync.Mutex
	byPane map[string][]recEvent
	// dims records the latest real terminal size (cols, rows) per pane, learned
	// from the pane's .resized events, so the asciicast header reflects the
	// actual pane geometry instead of a hardcoded 80x24. lastDims is the most
	// recent size across any pane, used for the merged ("" = all panes) export.
	dims     map[string]dim
	lastDims dim
	// resizes keeps the per-pane resize timeline (design 006): a single-pane
	// export replays mid-recording size changes as asciicast "r" events instead
	// of stretching the whole cast to the final size. The merged ("" = all
	// panes) export deliberately emits no "r" events — sizes from different
	// panes would fight over one virtual terminal.
	resizes map[string][]resizeRec
}

// resizeRec is one resize on a pane's timeline (ts unix millis).
type resizeRec struct {
	ts int64
	d  dim
}

// dim is a terminal size (cols x rows). A zero dim means "unknown" and lets the
// asciicast writer fall back to its 80x24 default.
type dim struct{ cols, rows int }

// NewCapture builds a capture bound to nc + codecs for the given session.
func NewCapture(nc *nats.Conn, codecs *msg.CodecRegistry, session string) *Capture {
	return &Capture{
		nc: nc, codecs: codecs, session: session, maxPer: 5000,
		byPane:  map[string][]recEvent{},
		dims:    map[string]dim{},
		resizes: map[string][]resizeRec{},
	}
}

// Start subscribes to the merged pane output subject (pane.*.output) and the
// pane resize subject (pane.*.resized) so the export knows the real pane size.
func (c *Capture) Start() error {
	sub, err := c.nc.Subscribe(msg.T("pane", "*", "output"), c.onMsg)
	if err != nil {
		return fmt.Errorf("replay: subscribe: %w", err)
	}
	c.sub = sub
	// Best-effort: a failed resize subscription only costs the real dimensions
	// (the writer falls back to 80x24), so it must not fail the whole capture.
	if rs, err := c.nc.Subscribe(msg.T("pane", "*", "resized"), c.onResized); err == nil {
		c.resizeSub = rs
	}
	return nil
}

// Stop unsubscribes.
func (c *Capture) Stop() {
	if c.sub != nil {
		_ = c.sub.Unsubscribe()
		c.sub = nil
	}
	if c.resizeSub != nil {
		_ = c.resizeSub.Unsubscribe()
		c.resizeSub = nil
	}
}

// onResized records the latest real terminal size for a pane from its .resized
// events.
func (c *Capture) onResized(m *nats.Msg) {
	var env msg.NATSEnvelope
	if err := json.Unmarshal(m.Data, &env); err != nil {
		return
	}
	decoded, err := c.codecs.Decode(env.TypeTag, env.Payload)
	if err != nil {
		return
	}
	rz, ok := decoded.(*msg.MsgPaneResized)
	if !ok || rz.Cols <= 0 || rz.Rows <= 0 {
		return
	}
	paneID := rz.PaneID
	if paneID == "" {
		paneID = paneFromSubject(m.Subject)
	}
	d := dim{cols: rz.Cols, rows: rz.Rows}
	now := time.Now().UnixMilli()
	c.mu.Lock()
	if paneID != "" {
		c.dims[paneID] = d
		// MsgPaneResized carries no timestamp, so the timeline uses receipt
		// time — the same wall clock ConversationMessage.TimestampMs uses.
		c.resizes[paneID] = append(c.resizes[paneID], resizeRec{ts: now, d: d})
	}
	c.lastDims = d
	c.mu.Unlock()
}

// dimsFor returns the recorded size for a pane ("" ⇒ the session's most recent),
// or a zero dim when unknown (the writer then falls back to 80x24).
func (c *Capture) dimsFor(paneID string) dim {
	c.mu.Lock()
	defer c.mu.Unlock()
	if paneID != "" {
		if d, ok := c.dims[paneID]; ok {
			return d
		}
	}
	return c.lastDims
}

func (c *Capture) onMsg(m *nats.Msg) {
	var env msg.NATSEnvelope
	if err := json.Unmarshal(m.Data, &env); err != nil {
		return
	}
	decoded, err := c.codecs.Decode(env.TypeTag, env.Payload)
	if err != nil {
		return
	}
	ca, ok := decoded.(*msg.MsgConversationAppend)
	if !ok || ca.Message == nil || ca.Message.Content == "" {
		return
	}
	if ca.Message.Role == RoleReplay {
		// ##replay play re-emits recorded output onto this same subject;
		// recording it again would append a copy of the recording to itself.
		return
	}
	paneID := paneFromSubject(m.Subject)
	if paneID == "" {
		return
	}
	c.mu.Lock()
	evs := append(c.byPane[paneID], recEvent{ts: ca.Message.TimestampMs, text: ca.Message.Content})
	if len(evs) > c.maxPer {
		evs = evs[len(evs)-c.maxPer:]
	}
	c.byPane[paneID] = evs
	c.mu.Unlock()
}

// Panes returns the pane IDs with captured output.
func (c *Capture) Panes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.byPane))
	for k := range c.byPane {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Count returns the number of captured events for a pane ("" = all panes).
func (c *Capture) Count(paneID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if paneID != "" {
		return len(c.byPane[paneID])
	}
	n := 0
	for _, evs := range c.byPane {
		n += len(evs)
	}
	return n
}

// events returns recorded events for a pane ("" = all panes merged), sorted by ts.
func (c *Capture) events(paneID string) []recEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var evs []recEvent
	if paneID != "" {
		evs = append(evs, c.byPane[paneID]...)
	} else {
		for _, e := range c.byPane {
			evs = append(evs, e...)
		}
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].ts < evs[j].ts })
	return evs
}

// ExportToFile writes the captured output for paneID ("" = all) as an
// asciicast at path. Returns the event count written. A single-pane export
// carries the pane's resize timeline: the header is the size in effect at the
// first output, and later size changes become asciicast "r" events (design
// 006). The merged export keeps its latest-size header and no "r" events.
func (c *Capture) ExportToFile(paneID, path, title string) (int, error) {
	evs := c.events(paneID)
	fromStream := false
	if len(evs) == 0 {
		// Post-restart path (design 006 §3.3): the in-memory buffer died with
		// the previous process, but the durable stream still holds the
		// recording — rebuild the events from it.
		evs = c.streamEvents(paneID)
		fromStream = true
	}
	if len(evs) == 0 {
		return 0, fmt.Errorf("no captured output%s", paneSuffix(paneID))
	}
	first := evs[0].ts
	events := make([]Event, len(evs))
	for i, e := range evs {
		events[i] = Event{Time: float64(e.ts-first) / 1000.0, Data: e.text}
	}

	d := c.dimsFor(paneID)
	var rz []resizeRec
	if fromStream {
		// The RAM ring died with the process, so the dims/resizes maps are as
		// empty as the event buffer — rebuild the timeline from the stream's
		// captured .resized messages too, and take the latest recorded size as
		// the lastDims equivalent (used directly by the merged export's
		// header).
		rz = c.streamResizes(paneID)
		if len(rz) > 0 {
			d = rz[len(rz)-1].d
		}
	} else {
		c.mu.Lock()
		rz = append(rz, c.resizes[paneID]...)
		c.mu.Unlock()
	}
	if paneID != "" {
		// Header = the last size known at (or before) the first output; a pane
		// whose only resizes were recorded later still uses the earliest one —
		// a resize normally precedes the output it reshapes.
		cur := dim{}
		for _, r := range rz {
			if r.ts <= first {
				cur = r.d
			}
		}
		if cur == (dim{}) && len(rz) > 0 {
			cur = rz[0].d
		}
		if cur != (dim{}) {
			d = cur
		}
		// Mid-recording changes become "r" events, deduped against the size
		// already in effect.
		for _, r := range rz {
			if r.ts <= first || r.d == cur {
				continue
			}
			cur = r.d
			events = append(events, Event{
				Time: float64(r.ts-first) / 1000.0,
				Kind: "r",
				Data: fmt.Sprintf("%dx%d", r.d.cols, r.d.rows),
			})
		}
		sort.SliceStable(events, func(i, j int) bool { return events[i].Time < events[j].Time })
	}

	h := Header{Timestamp: first / 1000, Title: title, Width: d.cols, Height: d.rows}
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	if err := WriteCast(f, h, events); err != nil {
		return 0, err
	}
	return len(events), nil
}

func paneSuffix(paneID string) string {
	if paneID == "" {
		return ""
	}
	return " for pane " + paneID
}

// paneFromSubject extracts the pane id from "{session}.pane.{id}.output".
func paneFromSubject(subject string) string {
	parts := strings.Split(subject, ".")
	for i, p := range parts {
		if p == "pane" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}
