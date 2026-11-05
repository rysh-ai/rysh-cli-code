// SPDX-License-Identifier: Apache-2.0

package familyserve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Config is everything serve needs. NC must be connected to the session's
// NATS with the session prefix already set (msg.SetSessionPrefix).
type Config struct {
	NC      *nats.Conn
	Codecs  *msg.CodecRegistry
	Session string
	Version string
	In      io.Reader
	Out     io.Writer
	Log     *slog.Logger

	// QueueMax is how many prompts may wait behind a pane's running turn
	// (DefaultQueueMax).
	QueueMax int
	// TurnTimeout bounds one turn once it is sent to the pane (15 min).
	TurnTimeout time.Duration
	// IdleWait bounds how long a turn waits for a turn the pane is already
	// running for someone else (a courier delivery) before it is sent anyway
	// (5 min, the courier's own bound).
	IdleWait time.Duration
	// IdlePoll is how often the pane's run status is asked while waiting.
	IdlePoll time.Duration
	// CancelGrace bounds the wait for the run to stop after a cancel (10 s);
	// the turn is then reported cancelled regardless.
	CancelGrace time.Duration
	// Refresh is how often profiled panes and their boards are re-read (10 s).
	Refresh time.Duration
	// RequestTimeout bounds one request to the daemon (3 s).
	RequestTimeout time.Duration
}

func (c *Config) defaults() {
	if c.Codecs == nil {
		c.Codecs = msg.DefaultCodecRegistry()
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.QueueMax <= 0 {
		c.QueueMax = DefaultQueueMax
	}
	if c.TurnTimeout <= 0 {
		c.TurnTimeout = 15 * time.Minute
	}
	if c.IdleWait <= 0 {
		c.IdleWait = 5 * time.Minute
	}
	if c.IdlePoll <= 0 {
		c.IdlePoll = 250 * time.Millisecond
	}
	if c.CancelGrace <= 0 {
		c.CancelGrace = 10 * time.Second
	}
	if c.Refresh <= 0 {
		c.Refresh = 10 * time.Second
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 3 * time.Second
	}
}

// Run serves the protocol until In reaches EOF (nil) or ctx ends (ctx.Err()).
// On either, running turns are cancelled in their panes and queued ones are
// dropped: the server fails every turn whose done it did not see.
func Run(ctx context.Context, cfg Config) error {
	if cfg.NC == nil || cfg.In == nil || cfg.Out == nil {
		return errors.New("familyserve: NC, In and Out are required")
	}
	cfg.defaults()
	ctx, stop := context.WithCancel(ctx)
	s := &server{
		cfg:      cfg,
		ctx:      ctx,
		pub:      msg.NewNATSPublisher(cfg.NC, cfg.Codecs),
		out:      &lineWriter{w: cfg.Out},
		log:      cfg.Log,
		turns:    map[string]*turn{},
		workers:  map[string]*paneWorker{},
		profiled: map[string]bool{},
		boards:   map[string]*nats.Subscription{},
		status:   map[string]string{},
	}
	defer func() {
		stop()
		s.close()
	}()

	if err := s.subscribeFleet(); err != nil {
		return fmt.Errorf("familyserve: subscribe fleet events: %w", err)
	}
	if err := s.ensureBoard(msg.DefaultBoardID); err != nil {
		return fmt.Errorf("familyserve: subscribe board: %w", err)
	}
	// ready goes out after ONE bounded request (the layout snapshot): the
	// server waits at most 15 s for it before falling back to one-shot
	// prompts. The per-pane board lookups follow, in parallel.
	s.loadProfiled()
	_ = cfg.NC.Flush()
	s.out.emit(readyEvent{Ev: EvReady, Session: cfg.Session, Version: cfg.Version, Panes: s.profiledList()})
	s.subscribeBoards()

	go s.refreshLoop()

	readDone := make(chan error, 1)
	go func() { readDone <- s.readLoop(cfg.In) }()
	select {
	case err := <-readDone:
		if err != nil {
			s.log.Warn("familyserve: stdin read failed", "err", err)
		}
		return nil // stdin closed: exit 0
	case <-ctx.Done():
		return ctx.Err()
	}
}

type server struct {
	cfg Config
	ctx context.Context
	pub *msg.NATSPublisher
	out *lineWriter
	log *slog.Logger

	mu       sync.Mutex
	turns    map[string]*turn // queued or running, by id
	workers  map[string]*paneWorker
	profiled map[string]bool
	boards   map[string]*nats.Subscription
	fleetSub *nats.Subscription
	status   map[string]string // last status signature per pane
	wg       sync.WaitGroup
}

// close stops the workers (each cancels its running turn in the pane) and
// unsubscribes everything.
func (s *server) close() {
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.workers {
		w.unsubscribe()
	}
	for _, sub := range s.boards {
		_ = sub.Unsubscribe()
	}
	if s.fleetSub != nil {
		_ = s.fleetSub.Unsubscribe()
	}
	_ = s.cfg.NC.Flush()
}

// ---------------------------------------------------------------------------
// stdin
// ---------------------------------------------------------------------------

func (s *server) readLoop(in io.Reader) error {
	r := bufio.NewReaderSize(in, MaxLineBytes+2)
	for {
		line, tooLong, err := readLine(r)
		if s.ctx.Err() != nil {
			return nil
		}
		if tooLong {
			s.errorf("line exceeds %d bytes; ignored", MaxLineBytes)
		} else if len(line) > 0 {
			s.handleLine(line)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// readLine reads one '\n'-terminated line. A line longer than MaxLineBytes is
// read to its end and discarded (tooLong).
func readLine(r *bufio.Reader) (line []byte, tooLong bool, err error) {
	for {
		frag, e := r.ReadSlice('\n')
		if !tooLong {
			line = append(line, frag...)
			if len(bytes.TrimRight(line, "\r\n")) > MaxLineBytes {
				tooLong, line = true, nil
			}
		}
		if e == bufio.ErrBufferFull {
			continue
		}
		return bytes.TrimSpace(line), tooLong, e
	}
}

func (s *server) handleLine(line []byte) {
	var l inLine
	if err := json.Unmarshal(line, &l); err != nil {
		s.errorf("malformed line: %v", err)
		return
	}
	switch l.Op {
	case OpPrompt:
		s.handlePrompt(l)
	case OpCancel:
		s.handleCancel(l)
	case OpPing:
		s.out.emit(simpleEvent{Ev: EvPong})
	case "":
		s.errorf("line has no op")
	default:
		s.errorf("unknown op %q", truncUTF8(l.Op, 64))
	}
}

func (s *server) errorf(format string, a ...any) {
	m := fmt.Sprintf(format, a...)
	s.out.emitFit(m, func(t string, _ bool) any { return errorEvent{Ev: EvError, Message: t} })
}

// ---------------------------------------------------------------------------
// turns
// ---------------------------------------------------------------------------

type turn struct {
	id, pane, text string

	cancelOnce sync.Once
	cancelCh   chan struct{}
	doneOnce   sync.Once
}

func (t *turn) requestCancel() { t.cancelOnce.Do(func() { close(t.cancelCh) }) }

func (t *turn) cancelRequested() bool {
	select {
	case <-t.cancelCh:
		return true
	default:
		return false
	}
}

// finish emits the turn's one done and forgets it.
func (s *server) finish(t *turn, status, text, errMsg string) {
	t.doneOnce.Do(func() {
		s.mu.Lock()
		if s.turns[t.id] == t {
			delete(s.turns, t.id)
		}
		s.mu.Unlock()
		s.out.emitFit(text, func(txt string, cut bool) any {
			return doneEvent{Ev: EvDone, ID: t.id, Pane: t.pane, Status: status, Text: txt, Error: errMsg, Truncated: cut}
		})
	})
}

func (s *server) handlePrompt(l inLine) {
	if !ValidID(l.ID) {
		s.errorf("prompt: invalid id %q (1-64 characters of A-Z a-z 0-9 . _ : -)", truncUTF8(l.ID, 80))
		return
	}
	s.mu.Lock()
	_, dup := s.turns[l.ID]
	s.mu.Unlock()
	if dup {
		// Not accepted, so no done: a done would end the original turn's wait.
		s.errorf("prompt %s: a turn with this id is already queued or running", l.ID)
		return
	}
	t := &turn{id: l.ID, pane: l.Pane, text: l.Text, cancelCh: make(chan struct{})}
	switch {
	case !ValidPane(l.Pane):
		s.finish(t, StatusError, "", "pane must be a pane uuid")
		return
	case strings.TrimSpace(l.Text) == "":
		s.finish(t, StatusError, "", "text is empty")
		return
	case len(l.Text) > MaxPromptBytes:
		s.finish(t, StatusError, "", fmt.Sprintf("text exceeds %d bytes", MaxPromptBytes))
		return
	}
	// e20 N1: decided from the pane's meta as it is NOW, never a cached list.
	if why := s.refuseReason(l.Pane); why != "" {
		s.finish(t, StatusError, "", why)
		return
	}

	s.mu.Lock()
	if _, dup := s.turns[l.ID]; dup {
		s.mu.Unlock()
		s.errorf("prompt %s: a turn with this id is already queued or running", l.ID)
		return
	}
	w := s.workerLocked(l.Pane)
	if w == nil {
		s.mu.Unlock()
		s.finish(t, StatusError, "", "cannot listen to the pane's agent")
		return
	}
	if !w.enqueue(t, s.cfg.QueueMax) {
		s.mu.Unlock()
		s.finish(t, StatusBusy, "", fmt.Sprintf("the pane already has %d prompts queued", s.cfg.QueueMax))
		return
	}
	s.turns[l.ID] = t
	s.mu.Unlock()
}

func (s *server) handleCancel(l inLine) {
	s.mu.Lock()
	t := s.turns[l.ID]
	var w *paneWorker
	if t != nil {
		w = s.workers[t.pane]
	}
	s.mu.Unlock()
	if t == nil {
		s.errorf("cancel: no queued or running turn %q", truncUTF8(l.ID, 80))
		return
	}
	if w != nil && w.removeQueued(t) {
		s.finish(t, StatusCancelled, "", "")
		return
	}
	t.requestCancel()
}

// ---------------------------------------------------------------------------
// pane workers: one FIFO per pane, one turn at a time
// ---------------------------------------------------------------------------

type paneEventKind int

const (
	evStatus paneEventKind = iota
	evOutput
	evApproval
)

type paneEvent struct {
	kind    paneEventKind
	orch    string
	phase   string // evStatus
	typ     string // evOutput
	content string // evOutput text, evApproval description
}

type paneWorker struct {
	s    *server
	pane string

	mu      sync.Mutex
	queue   []*turn
	running *turn
	wake    chan struct{}
	events  chan paneEvent
	subs    []*nats.Subscription
	stop    chan struct{} // ends the subscription reader
}

// workerLocked returns the pane's worker, starting it (and its exact-subject
// subscriptions) on first use. s.mu must be held.
func (s *server) workerLocked(pane string) *paneWorker {
	if w := s.workers[pane]; w != nil {
		return w
	}
	w := &paneWorker{s: s, pane: pane, wake: make(chan struct{}, 1), events: make(chan paneEvent, 4096)}
	if err := w.subscribe(); err != nil {
		s.log.Warn("familyserve: subscribe pane", "pane", pane, "err", err)
		w.unsubscribe()
		return nil
	}
	_ = s.cfg.NC.Flush()
	s.workers[pane] = w
	s.wg.Add(1)
	go w.loop()
	return w
}

func (w *paneWorker) push(ev paneEvent) {
	select {
	case w.events <- ev:
	default:
		w.s.log.Warn("familyserve: pane event buffer full; event dropped", "pane", w.pane)
	}
}

func (w *paneWorker) subscribe() error {
	decode := w.s.decoder()
	handlers := map[string]func(interface{}){
		msg.T("pane", w.pane, "llm_prompt_execution", "status"): func(d interface{}) {
			if st, ok := d.(*msg.MsgAgenticStatus); ok {
				w.push(paneEvent{kind: evStatus, orch: st.OrchestratorID, phase: st.Phase})
			}
		},
		msg.T("pane", w.pane, "llm_prompt_execution", "output"): func(d interface{}) {
			if o, ok := d.(*msg.MsgAgenticOutput); ok {
				w.push(paneEvent{kind: evOutput, orch: o.OrchestratorID, typ: o.Type, content: o.Content})
			}
		},
		msg.T("pane", w.pane, "approval", "request"): func(d interface{}) {
			if r, ok := d.(*msg.MsgApprovalRequest); ok {
				w.push(paneEvent{kind: evApproval, orch: r.OrchestratorID, content: r.Description})
			}
		},
	}
	// One channel for all three subjects: the connection's read loop fills it
	// in wire order. Separate async subscriptions each deliver on their own
	// goroutine, so a turn's done status could overtake its text (QA-32).
	ch := make(chan *nats.Msg, cap(w.events))
	for subject := range handlers {
		sub, err := w.s.cfg.NC.ChanSubscribe(subject, ch)
		if err != nil {
			return err
		}
		w.subs = append(w.subs, sub)
	}
	stop := make(chan struct{})
	w.stop = stop
	go func() {
		for {
			select {
			case <-stop:
				return
			case m := <-ch:
				if fn := handlers[m.Subject]; fn != nil {
					if d, ok := decode(m.Data); ok {
						fn(d)
					}
				}
			}
		}
	}()
	return nil
}

func (w *paneWorker) unsubscribe() {
	for _, sub := range w.subs {
		_ = sub.Unsubscribe()
	}
	w.subs = nil
	if w.stop != nil {
		close(w.stop)
		w.stop = nil
	}
}

// enqueue appends t unless max prompts already wait (the running one is not
// counted).
func (w *paneWorker) enqueue(t *turn, max int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.queue) >= max {
		return false
	}
	w.queue = append(w.queue, t)
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return true
}

func (w *paneWorker) removeQueued(t *turn) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, q := range w.queue {
		if q == t {
			w.queue = append(w.queue[:i], w.queue[i+1:]...)
			return true
		}
	}
	return false
}

func (w *paneWorker) loop() {
	defer w.s.wg.Done()
	for {
		w.mu.Lock()
		if len(w.queue) > 0 {
			t := w.queue[0]
			w.queue = w.queue[1:]
			w.running = t
			w.mu.Unlock()
			w.s.runTurn(w, t)
			w.mu.Lock()
			w.running = nil
			w.mu.Unlock()
			continue
		}
		w.mu.Unlock()
		select {
		case <-w.s.ctx.Done():
			return
		case <-w.wake:
		}
	}
}

func (s *server) llmInbox(pane string) string {
	return msg.T("pane", pane, "llm_prompt_execution", "inbox")
}

func (s *server) sendCancel(pane string) {
	_ = s.pub.Send(s.llmInbox(pane), &msg.MsgAgenticCancel{})
}

// waitIdle waits until the pane's agent has no run in flight — a courier
// delivery, say — so this turn's prompt never cancels it. It gives up waiting
// after IdleWait and the turn goes ahead. It returns false when the turn has
// already been finished (cancelled, unreachable agent, shutdown).
func (s *server) waitIdle(t *turn) bool {
	deadline := time.Now().Add(s.cfg.IdleWait)
	fails := 0
	for {
		reply, err := s.pub.Request(s.llmInbox(t.pane), &msg.MsgGetRunStatus{}, s.cfg.RequestTimeout)
		if err == nil {
			fails = 0
			st, ok := reply.(*msg.MsgRunStatusReply)
			if !ok || !st.InFlight {
				return true
			}
			if time.Now().After(deadline) {
				s.log.Warn("familyserve: pane still busy; sending the turn anyway", "pane", t.pane, "turn", t.id)
				return true
			}
		} else {
			fails++
			if fails >= 3 {
				s.finish(t, StatusError, "", "the pane's agent did not answer")
				return false
			}
		}
		select {
		case <-s.ctx.Done():
			return false
		case <-t.cancelCh:
			s.finish(t, StatusCancelled, "", "")
			return false
		case <-time.After(s.cfg.IdlePoll):
		}
	}
}

// runTurn sends one turn to its pane and streams it to its done.
func (s *server) runTurn(w *paneWorker, t *turn) {
	if t.cancelRequested() {
		s.finish(t, StatusCancelled, "", "")
		return
	}
	if !s.waitIdle(t) {
		return
	}
	// Checked again right before the send: the profile may have changed while
	// the turn waited in the queue.
	if why := s.refuseReason(t.pane); why != "" {
		s.finish(t, StatusError, "", why)
		return
	}
	// Events from before this turn belong to nobody.
	for drained := false; !drained; {
		select {
		case <-w.events:
		default:
			drained = true
		}
	}
	if err := s.pub.Send(msg.T("pane", t.pane, "inbox"), &msg.MsgPaneExecPrompt{Prompt: t.text, Literal: true}); err != nil {
		s.finish(t, StatusError, "", "could not deliver the prompt: "+err.Error())
		return
	}

	var (
		orch       string
		pending    []paneEvent
		text       strings.Builder
		errText    strings.Builder
		reason     string // "cancel" | "timeout" | "approval"
		approval   string
		cancelSent bool
		grace      <-chan time.Time
	)
	cancelCh := t.cancelCh
	timeout := time.NewTimer(s.cfg.TurnTimeout)
	defer timeout.Stop()

	requestCancel := func(r string) {
		if reason == "" {
			reason = r
		}
		cancelCh = nil
		s.sendCancel(t.pane)
		cancelSent = true
		if grace == nil {
			grace = time.After(s.cfg.CancelGrace)
		}
	}
	finalize := func(phase string) {
		switch {
		case reason == "cancel":
			s.finish(t, StatusCancelled, text.String(), "")
		case reason == "timeout":
			s.finish(t, StatusError, text.String(), fmt.Sprintf("the turn did not finish within %s", s.cfg.TurnTimeout))
		case reason == "approval":
			// Not an error status: the server shows a failed turn as "could
			// not answer", while this is a known stop the user should read.
			why := "this turn stopped: it needed an approval, and hosted avatars are not given approvals"
			if approval != "" {
				why += " (" + approval + ")"
			}
			reply := strings.TrimRight(text.String(), "\n")
			if reply != "" {
				reply += "\n\n"
			}
			s.finish(t, StatusDone, reply+"["+why+"]", why)
		case phase == "error":
			e := strings.TrimSpace(errText.String())
			if e == "" {
				e = "the agent's run failed"
			}
			s.finish(t, StatusError, text.String(), e)
		case phase == "cancelled":
			s.finish(t, StatusCancelled, text.String(), "")
		default: // done, or paused (the reply so far is the answer)
			s.finish(t, StatusDone, text.String(), "")
		}
	}
	// handle applies one event of this turn's run; true when the turn ended.
	handle := func(ev paneEvent) bool {
		switch ev.kind {
		case evOutput:
			switch ev.typ {
			case "text":
				text.WriteString(ev.content)
				for _, piece := range splitText(ev.content, chunkTextMax) {
					s.out.emitFit(piece, func(p string, _ bool) any {
						return chunkEvent{Ev: EvChunk, ID: t.id, Pane: t.pane, Text: p}
					})
				}
			case "error":
				errText.WriteString(ev.content)
			}
		case evApproval:
			approval = strings.TrimSpace(ev.content)
			requestCancel("approval")
		case evStatus:
			switch ev.phase {
			case "done", "error", "paused", "cancelled":
				finalize(ev.phase)
				return true
			}
		}
		return false
	}

	for {
		select {
		case <-s.ctx.Done():
			s.sendCancel(t.pane)
			return
		case <-cancelCh:
			requestCancel("cancel")
		case <-timeout.C:
			requestCancel("timeout")
		case <-grace:
			finalize("")
			return
		case ev := <-w.events:
			if orch == "" {
				// The run is ours from its "planning" status, which the
				// LLM actor emits when it starts the prompt. Events of the
				// run that raced ahead of it are replayed once it is known.
				if ev.kind != evStatus || ev.phase != "planning" {
					if len(pending) < 4096 {
						pending = append(pending, ev)
					}
					continue
				}
				orch = ev.orch
				if cancelSent {
					s.sendCancel(t.pane) // the first cancel may have beaten the run
				}
				for _, p := range pending {
					if p.orch == orch && handle(p) {
						return
					}
				}
				pending = nil
				continue
			}
			if ev.orch == orch && handle(ev) {
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// discovery: profiled panes and their boards
// ---------------------------------------------------------------------------

// FamilyTools is the only allow-list serve drives: a pane whose agent
// profile allows anything else (bash, files, the web …) is never prompted
// from outside the container (e20 N1).
var FamilyTools = map[string]bool{"family_ask": true, "family_reply": true, "family_status": true}

// FamilyOnly reports whether pane meta carries an agent profile whose
// allow-list is non-empty and holds only family tools.
func FamilyOnly(meta map[string]string) bool {
	allow := domain.ParseAgentTools(meta[domain.PaneMetaAgentTools])
	if len(allow) == 0 {
		return false
	}
	for _, t := range allow {
		if !FamilyTools[t] {
			return false
		}
	}
	return true
}

// refuseReason reads the pane's meta from the pane itself and returns why a
// prompt must not drive it, or "" when it may.
func (s *server) refuseReason(pane string) string {
	reply, err := s.pub.Request(msg.T("pane", pane, "snapshot"),
		&msg.MsgGetPaneSnapshot{LayoutOnly: true, NoHistories: true}, s.cfg.RequestTimeout)
	if err != nil {
		return "pane did not answer (no such pane in this session?)"
	}
	snap, ok := reply.(*msg.MsgPaneSnapshotReply)
	if !ok {
		return "pane did not answer"
	}
	meta := snap.Snapshot.Meta
	if len(domain.ParseAgentTools(meta[domain.PaneMetaAgentTools])) == 0 {
		return "pane has no agent profile in this session"
	}
	if !FamilyOnly(meta) {
		return "pane's agent profile allows tools beyond family_ask, family_reply and family_status; serve drives family-only panes"
	}
	return ""
}

func (s *server) profiledList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.profiled))
	for p := range s.profiled {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// discover re-reads which panes carry an agent profile and subscribes to the
// board each of them posts to (exact subjects; never a wildcard).
func (s *server) discover() {
	s.loadProfiled()
	s.subscribeBoards()
}

// loadProfiled re-reads which panes carry an agent profile (one request).
func (s *server) loadProfiled() {
	reply, err := s.pub.Request(msg.T("ws", "snapshot"),
		&msg.MsgGetWorkspaceSnapshot{LayoutOnly: true, NoHistories: true}, s.cfg.RequestTimeout)
	if err != nil {
		s.log.Warn("familyserve: workspace snapshot", "err", err)
		return
	}
	snap, ok := reply.(*msg.MsgWorkspaceSnapshotReply)
	if !ok {
		s.log.Warn("familyserve: unexpected snapshot reply", "type", fmt.Sprintf("%T", reply))
		return
	}
	found := map[string]bool{}
	for p := range domain.PanesInWorkspace(&snap.Snapshot) {
		if FamilyOnly(p.Meta) {
			found[p.ID] = true
		}
	}
	s.mu.Lock()
	s.profiled = found
	s.mu.Unlock()
}

// subscribeBoards resolves every profiled pane's board, in parallel, and
// subscribes to each board once.
func (s *server) subscribeBoards() {
	var wg sync.WaitGroup
	for _, id := range s.profiledList() {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if b := s.boardOf(id); b != "" {
				if err := s.ensureBoard(b); err != nil {
					s.log.Warn("familyserve: subscribe board", "board", b, "err", err)
				}
			}
		}(id)
	}
	wg.Wait()
	_ = s.cfg.NC.Flush()
}

// boardOf asks the workspace which board a pane posts to — the same door the
// family tools use (MsgCLIBoardResolve).
func (s *server) boardOf(pane string) string {
	reply, err := s.pub.Request(msg.T("ws", "inbox"), &msg.MsgCLIBoardResolve{AsPaneID: pane}, s.cfg.RequestTimeout)
	if err != nil {
		return ""
	}
	resp, ok := reply.(*msg.MsgCLIResponse)
	if !ok || !resp.OK {
		return ""
	}
	b := strings.TrimSpace(strings.SplitN(resp.Output, "\n", 2)[0])
	if b == "" {
		b = msg.DefaultBoardID
	}
	return msg.NormalizeBoardID(b)
}

func (s *server) refreshLoop() {
	t := time.NewTicker(s.cfg.Refresh)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.discover()
		}
	}
}

func (s *server) decoder() func([]byte) (interface{}, bool) {
	codecs := s.cfg.Codecs
	return func(data []byte) (interface{}, bool) {
		var env msg.NATSEnvelope
		if json.Unmarshal(data, &env) != nil {
			return nil, false
		}
		d, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return nil, false
		}
		return d, true
	}
}

// ensureBoard subscribes to one board's post subject, once.
func (s *server) ensureBoard(board string) error {
	board = msg.NormalizeBoardID(board)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return nil
	}
	if _, ok := s.boards[board]; ok {
		return nil
	}
	decode := s.decoder()
	sub, err := s.cfg.NC.Subscribe(msg.BoardPostSubject(board), func(m *nats.Msg) {
		d, ok := decode(m.Data)
		if !ok {
			return
		}
		if p, ok := d.(*msg.MsgBoardPost); ok {
			s.emitBoard(p)
		}
	})
	if err != nil {
		return err
	}
	s.boards[board] = sub
	return nil
}

func boardKind(k string) string {
	switch k {
	case msg.BoardKindCmd, msg.BoardKindReply, msg.BoardKindAck:
		return k
	}
	return "post"
}

func (s *server) emitBoard(p *msg.MsgBoardPost) {
	thread := p.ThreadID
	if thread == "" && p.Kind == msg.BoardKindCmd && p.CmdID != "" {
		thread = msg.CmdThreadID(p.PaneID, p.CmdID)
	}
	ev := boardEvent{Ev: EvBoard, Kind: boardKind(p.Kind), Thread: thread, From: p.PaneID, To: p.ToPaneID, At: boardTime(p.TS)}
	s.out.emitFit(p.Text, func(t string, _ bool) any { e := ev; e.Text = t; return e })
}

// subscribeFleet listens to the fleet actor's ledger change events.
func (s *server) subscribeFleet() error {
	sub, err := s.cfg.NC.Subscribe(msg.FleetEventSubject(), func(m *nats.Msg) {
		var ev fleet.LedgerEvent
		if json.Unmarshal(m.Data, &ev) != nil || ev.PaneID == "" {
			return
		}
		s.emitStatus(ev)
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.fleetSub = sub
	s.mu.Unlock()
	return nil
}

// emitStatus forwards a ledger change, once per distinct (state, note, pct)
// per pane: the fleet event also changes on task and health, which the
// protocol's status event does not carry.
func (s *server) emitStatus(ev fleet.LedgerEvent) {
	pct := "-"
	if ev.Pct != nil {
		pct = strconv.Itoa(*ev.Pct)
	}
	sig := ev.State + "\x00" + ev.Note + "\x00" + pct
	s.mu.Lock()
	if s.status[ev.PaneID] == sig {
		s.mu.Unlock()
		return
	}
	s.status[ev.PaneID] = sig
	s.mu.Unlock()
	out := statusEvent{Ev: EvStatus, Pane: ev.PaneID, State: ev.State, Pct: ev.Pct}
	s.out.emitFit(ev.Note, func(t string, _ bool) any { e := out; e.Note = t; return e })
}
