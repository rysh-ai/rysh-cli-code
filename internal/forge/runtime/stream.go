package runtime

// Streaming sessions (design 015 §2.1/§2.2): server-streaming gRPC methods and
// GraphQL subscriptions are exposed to agents as BACKGROUND SESSIONS, mirroring
// the bash_background / bash_session contract — a start call returns a session
// ID, a poll call returns the frames received since the last poll, and a stop
// call cancels the underlying request. Frames land in a bounded ring buffer so
// a chatty stream can never grow memory or flood the model's context.
//
// Lifecycle defaults (see DefaultStreamOptions):
//   - idle expiry: a session not polled for 5 minutes is cancelled;
//   - max lifetime: a session is cancelled 30 minutes after start regardless;
//   - expired/finished sessions linger (with their reason) until one idle
//     period after their last poll, then are removed by the sweeper;
//   - at most 8 live sessions per manager (per integration instance).
//
// The clock is injectable (Options.Now + explicit Sweep) so expiry is testable
// without sleeping, mirroring the `after` seam in internal/replay.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Stream lifecycle defaults.
const (
	DefaultStreamIdleTTL     = 5 * time.Minute  // cancelled when not polled for this long
	DefaultStreamMaxLifetime = 30 * time.Minute // cancelled this long after start, regardless
	DefaultStreamMaxSessions = 8                // live sessions per manager
	DefaultStreamMaxFrames   = 256              // ring capacity, in frames
	DefaultStreamMaxBytes    = 4 << 20          // ring capacity, in bytes (matches Options.MaxBody default)
	DefaultPollMaxFrames     = 50               // frames returned per poll
	DefaultPollMaxBytes      = 256 << 10        // bytes returned per poll
	sweepInterval            = 30 * time.Second // background sweeper cadence
)

// StreamOptions configure a StreamManager. Zero values take the defaults above.
type StreamOptions struct {
	IdleTTL       time.Duration
	MaxLifetime   time.Duration
	MaxSessions   int
	MaxFrames     int   // ring capacity in frames
	MaxBytes      int64 // ring capacity in bytes
	MaxFrameBytes int64 // per-frame cap; oversize frames are truncated with a marker (default: executor MaxBody default)
	Now           func() time.Time
	// NoSweeper disables the background sweeper goroutine (tests drive expiry
	// via Sweep with a fake clock instead).
	NoSweeper bool
}

func (o StreamOptions) withDefaults() StreamOptions {
	if o.IdleTTL <= 0 {
		o.IdleTTL = DefaultStreamIdleTTL
	}
	if o.MaxLifetime <= 0 {
		o.MaxLifetime = DefaultStreamMaxLifetime
	}
	if o.MaxSessions <= 0 {
		o.MaxSessions = DefaultStreamMaxSessions
	}
	if o.MaxFrames <= 0 {
		o.MaxFrames = DefaultStreamMaxFrames
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultStreamMaxBytes
	}
	if o.MaxFrameBytes <= 0 {
		o.MaxFrameBytes = DefaultStreamMaxBytes
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// streamFrame is one received frame with its monotonic sequence number.
type streamFrame struct {
	seq  uint64
	text string
}

// StreamSession is one live (or recently finished) stream.
type StreamSession struct {
	ID    string
	Kind  string // "grpc-stream" | "graphql-subscription"
	Label string // method id / subscription summary, for listings

	mu        sync.Mutex
	frames    []streamFrame // ring: oldest first, bounded by MaxFrames/MaxBytes
	bytes     int64         // current ring size in bytes
	nextSeq   uint64        // seq assigned to the next pushed frame
	cursor    uint64        // first seq not yet returned by Poll
	dropped   int           // frames evicted before they were polled
	done      bool
	endReason string // why the session ended ("" while live)
	started   time.Time
	lastPoll  time.Time
	cancel    context.CancelFunc
}

// StreamInfo is a snapshot of one session for listings.
type StreamInfo struct {
	ID        string
	Kind      string
	Label     string
	StartedAt time.Time
	Frames    uint64 // total frames received so far
	Pending   uint64 // frames buffered but not yet polled
	Done      bool
	EndReason string
}

// PollResult is what one Poll call returns.
type PollResult struct {
	Frames  []string
	Dropped int    // frames lost to ring eviction since the previous poll
	Pending int    // frames still buffered after this poll (poll again now)
	Done    bool   // the stream has ended (after draining, stop the session)
	Reason  string // why it ended ("" while live)
}

// StreamManager owns the streaming sessions of one integration instance.
type StreamManager struct {
	opts StreamOptions

	mu       sync.Mutex
	sessions map[string]*StreamSession
	closed   bool
	stop     chan struct{} // closes the sweeper
}

// NewStreamManager builds a manager and (unless opts.NoSweeper) starts its
// background sweeper, which enforces idle/lifetime expiry every 30s.
func NewStreamManager(opts StreamOptions) *StreamManager {
	m := &StreamManager{
		opts:     opts.withDefaults(),
		sessions: map[string]*StreamSession{},
		stop:     make(chan struct{}),
	}
	if !m.opts.NoSweeper {
		go func() {
			t := time.NewTicker(sweepInterval)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					m.Sweep()
				case <-m.stop:
					return
				}
			}
		}()
	}
	return m
}

// Start registers a new session and runs `run` on its own goroutine. `run`
// receives the session context (cancelled by Stop / expiry / CloseAll) and a
// push callback for frames; when it returns, the session is marked done with
// the returned error (nil ⇒ "stream ended"). The returned id is what the model
// polls/stops with.
func (m *StreamManager) Start(kind, label string, run func(ctx context.Context, push func(frame string)) error) (string, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", fmt.Errorf("stream manager is closed")
	}
	live := 0
	for _, s := range m.sessions {
		s.mu.Lock()
		if !s.done {
			live++
		}
		s.mu.Unlock()
	}
	if live >= m.opts.MaxSessions {
		m.mu.Unlock()
		return "", fmt.Errorf("too many live streaming sessions (%d); stop one first", live)
	}

	ctx, cancel := context.WithCancel(context.Background())
	now := m.opts.Now()
	sess := &StreamSession{
		ID:       uuid.New().String()[:8],
		Kind:     kind,
		Label:    label,
		started:  now,
		lastPoll: now,
		cancel:   cancel,
	}
	m.sessions[sess.ID] = sess
	m.mu.Unlock()

	go func() {
		err := run(ctx, func(frame string) { sess.push(frame, m.opts) })
		reason := "stream ended"
		if err != nil && ctx.Err() == nil {
			reason = err.Error()
		} else if ctx.Err() != nil {
			reason = "cancelled"
		}
		sess.finish(reason)
	}()
	return sess.ID, nil
}

// push appends a frame to the ring, evicting oldest frames past the bounds.
func (s *StreamSession) push(frame string, opts StreamOptions) {
	if int64(len(frame)) > opts.MaxFrameBytes {
		frame = frame[:opts.MaxFrameBytes] + "\n…[frame truncated]"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	s.frames = append(s.frames, streamFrame{seq: s.nextSeq, text: frame})
	s.nextSeq++
	s.bytes += int64(len(frame))
	for len(s.frames) > opts.MaxFrames || (s.bytes > opts.MaxBytes && len(s.frames) > 1) {
		old := s.frames[0]
		s.frames = s.frames[1:]
		s.bytes -= int64(len(old.text))
		if old.seq >= s.cursor {
			s.dropped++ // evicted before it was ever polled
		}
	}
}

// finish marks the session done (idempotent; the first reason wins).
func (s *StreamSession) finish(reason string) {
	s.mu.Lock()
	if !s.done {
		s.done = true
		s.endReason = reason
	}
	s.mu.Unlock()
}

// Poll returns the frames received since the previous poll, bounded to
// maxFrames (default DefaultPollMaxFrames) and DefaultPollMaxBytes per call.
// It refreshes the session's idle timer.
func (m *StreamManager) Poll(id string, maxFrames int) (*PollResult, error) {
	m.mu.Lock()
	sess, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("streaming session %q not found (it may have expired)", id)
	}
	if maxFrames <= 0 || maxFrames > DefaultPollMaxFrames {
		maxFrames = DefaultPollMaxFrames
	}

	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.lastPoll = m.opts.Now()

	res := &PollResult{Dropped: sess.dropped, Done: sess.done, Reason: sess.endReason}
	sess.dropped = 0

	var budget int64
	for _, f := range sess.frames {
		if f.seq < sess.cursor {
			continue
		}
		if len(res.Frames) >= maxFrames || budget+int64(len(f.text)) > DefaultPollMaxBytes {
			break
		}
		res.Frames = append(res.Frames, f.text)
		budget += int64(len(f.text))
		sess.cursor = f.seq + 1
	}
	res.Pending = int(sess.nextSeq - sess.cursor)
	return res, nil
}

// Stop cancels a session's underlying request and removes it, returning any
// frames that had not been polled yet (bounded like a poll).
func (m *StreamManager) Stop(id string) (*PollResult, error) {
	m.mu.Lock()
	sess, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("streaming session %q not found (it may have expired)", id)
	}
	sess.cancel()
	sess.finish("stopped")

	sess.mu.Lock()
	defer sess.mu.Unlock()
	res := &PollResult{Dropped: sess.dropped, Done: true, Reason: sess.endReason}
	var budget int64
	for _, f := range sess.frames {
		if f.seq < sess.cursor {
			continue
		}
		if len(res.Frames) >= DefaultPollMaxFrames || budget+int64(len(f.text)) > DefaultPollMaxBytes {
			break
		}
		res.Frames = append(res.Frames, f.text)
		budget += int64(len(f.text))
	}
	return res, nil
}

// List returns a snapshot of all sessions, oldest first.
func (m *StreamManager) List() []StreamInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]StreamInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		s.mu.Lock()
		out = append(out, StreamInfo{
			ID:        s.ID,
			Kind:      s.Kind,
			Label:     s.Label,
			StartedAt: s.started,
			Frames:    s.nextSeq,
			Pending:   s.nextSeq - s.cursor,
			Done:      s.done,
			EndReason: s.endReason,
		})
		s.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Sweep enforces expiry: live sessions past their idle TTL or max lifetime are
// cancelled (kept, with the reason, for one more idle period so a poll can
// report why); done sessions idle past the TTL are removed.
func (m *StreamManager) Sweep() {
	now := m.opts.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		s.mu.Lock()
		idle := now.Sub(s.lastPoll)
		age := now.Sub(s.started)
		switch {
		case !s.done && idle > m.opts.IdleTTL:
			s.done = true
			s.endReason = fmt.Sprintf("expired: not polled for %s", m.opts.IdleTTL)
			s.cancel()
		case !s.done && age > m.opts.MaxLifetime:
			s.done = true
			s.endReason = fmt.Sprintf("expired: exceeded max lifetime %s", m.opts.MaxLifetime)
			s.cancel()
		case s.done && idle > m.opts.IdleTTL:
			delete(m.sessions, id)
		}
		s.mu.Unlock()
	}
}

// CloseAll cancels every session and stops the sweeper. Called when the owning
// integration is disabled / its scope is torn down / the workspace shuts down.
func (m *StreamManager) CloseAll() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	sessions := m.sessions
	m.sessions = map[string]*StreamSession{}
	close(m.stop)
	m.mu.Unlock()
	for _, s := range sessions {
		s.cancel()
		s.finish("closed")
	}
}

// RenderPoll formats a PollResult the way the session tools present it — one
// frame per block, with status/drop/pending annotations — so gRPC streams and
// GraphQL subscriptions read identically to the model.
func RenderPoll(id string, res *PollResult) string {
	var sb strings.Builder
	status := "running"
	if res.Done {
		status = "ended"
		if res.Reason != "" {
			status = "ended: " + res.Reason
		}
	}
	fmt.Fprintf(&sb, "[session: %s, status: %s, new frames: %d]\n", id, status, len(res.Frames))
	if res.Dropped > 0 {
		fmt.Fprintf(&sb, "[%d frame(s) dropped by the ring buffer before this poll]\n", res.Dropped)
	}
	for _, f := range res.Frames {
		sb.WriteString(f)
		if !strings.HasSuffix(f, "\n") {
			sb.WriteString("\n")
		}
	}
	if res.Pending > 0 {
		fmt.Fprintf(&sb, "[%d more frame(s) pending — poll again]\n", res.Pending)
	}
	return sb.String()
}
