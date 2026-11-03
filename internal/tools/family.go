// SPDX-License-Identifier: Apache-2.0

package tools

// Native family coordination tools (design 093 §12.3 H1).
//
// Hosted family avatars are native rysh panes with a strict tool allow-list and
// no shell. Before H1 they did the family protocol with bash
// (`rysh_local board send …`, `rysh fleet status …`); these three tools do the
// same in-process, over the same daemon doors those commands use — the
// workspace's agent board door (MsgCLIBoardPost / MsgCLIBoardResolve on the
// workspace inbox), the board recorder's query subject and the fleet
// registry's update subject. Nothing is shelled out.
//
// IDENTITY: every tool acts as the pane it was built for. The pane id is fixed
// at construction (the orchestrator's pane), and no parameter can name another
// poster: the schemas have no identity field and unknown fields — "as",
// "as_pane_id", "pane_id" — are REFUSED, so a model cannot even try to pass an
// --as.
//
// They are registered as OPT-IN tools (agentic.Setup.CreateLLMPromptExecutionActor):
// a pane sees them only when its agent profile names them, so a pane without a
// profile keeps exactly the tool set it had.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Family tool names.
const (
	FamilyAskTool    = "family_ask"
	FamilyReplyTool  = "family_reply"
	FamilyStatusTool = "family_status"
)

// FamilyToolNames lists the family tools, in a stable order.
var FamilyToolNames = []string{FamilyAskTool, FamilyReplyTool, FamilyStatusTool}

// DefaultFamilyAskTimeout bounds how long family_ask waits for the answer.
const DefaultFamilyAskTimeout = 150 * time.Second

// familyBusTimeout bounds one request to the workspace, recorder or registry.
// They answer from memory; anything near this is not coming.
const familyBusTimeout = 5 * time.Second

// familyBus is what the family tools need from the session bus.
type familyBus struct {
	nc     *nats.Conn
	pub    *msg.NATSPublisher
	paneID string
}

func (b *familyBus) ready() *ToolOutput {
	if b.nc == nil || b.pub == nil {
		return ErrOutput(ErrKindMissing, "the session bus is not available")
	}
	if strings.TrimSpace(b.paneID) == "" {
		return ErrOutput(ErrKindInternal, "family tool has no pane identity")
	}
	return nil
}

// cli sends one agent-door request to the workspace and returns its reply.
func (b *familyBus) cli(m interface{}) (*msg.MsgCLIResponse, error) {
	reply, err := b.pub.Request(msg.T("ws", "inbox"), m, familyBusTimeout)
	if err != nil {
		return nil, fmt.Errorf("the workspace did not answer: %v", err)
	}
	resp, ok := reply.(*msg.MsgCLIResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected workspace reply %T", reply)
	}
	if !resp.OK {
		e := strings.TrimSpace(resp.Error)
		if e == "" {
			e = "the workspace refused the request"
		}
		return resp, errors.New(e)
	}
	return resp, nil
}

// myBoard resolves this pane's full uuid and the board it posts to.
func (b *familyBus) myBoard() (me, boardID string, err error) {
	resp, err := b.cli(&msg.MsgCLIBoardResolve{AsPaneID: b.paneID})
	if err != nil {
		return "", "", err
	}
	boardID = strings.TrimSpace(strings.SplitN(resp.Output, "\n", 2)[0])
	if boardID == "" {
		boardID = msg.DefaultBoardID
	}
	return resp.ID, boardID, nil
}

// familyMember resolves ref (a pane id, given-name or title) to a full pane id
// and checks that the pane is a family member: its agent profile's allow-list
// includes family_reply. Anything else is refused.
func (b *familyBus) familyMember(ref, me string) (string, error) {
	resp, err := b.cli(&msg.MsgCLIBoardResolve{AsPaneID: ref})
	if err != nil {
		return "", fmt.Errorf("%s is not a pane in this session (%v)", ref, err)
	}
	id := strings.TrimSpace(resp.ID)
	if id == "" {
		return "", fmt.Errorf("%s is not a pane in this session", ref)
	}
	if id == me {
		return "", fmt.Errorf("you cannot ask yourself")
	}
	reply, err := b.pub.Request(msg.T("pane", id, "snapshot"), &msg.MsgGetPaneSnapshot{LayoutOnly: true, NoHistories: true}, familyBusTimeout)
	if err != nil {
		return "", fmt.Errorf("%s did not answer (%v)", ref, err)
	}
	snap, ok := reply.(*msg.MsgPaneSnapshotReply)
	if !ok {
		return "", fmt.Errorf("unexpected reply from %s: %T", ref, reply)
	}
	if !domain.AgentToolsAllow(snap.Snapshot.Meta, FamilyReplyTool) {
		return "", fmt.Errorf("%s is not a family member (its agent profile does not allow %s); you can only ask family members", ref, FamilyReplyTool)
	}
	return id, nil
}

// query asks the board recorder for a board.
func (b *familyBus) query(boardID string) (*board.QueryReply, error) {
	q := boardID
	if q == msg.DefaultBoardID {
		q = ""
	}
	return board.Ask(b.nc, q, board.Query{}, familyBusTimeout)
}

// decodeStrict refuses unknown fields, so an identity override ("as",
// "as_pane_id", …) is an error rather than silently ignored.
func decodeStrict(params json.RawMessage, v interface{}) error {
	if len(bytes.TrimSpace(params)) == 0 {
		params = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(params))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid parameters: %v (these tools always act as your own pane; there is no identity parameter)", err)
	}
	return nil
}

// NewFamilyTools builds the three family tools for one pane.
func NewFamilyTools(nc *nats.Conn, pub *msg.NATSPublisher, paneID string) []ToolExecutor {
	bus := &familyBus{nc: nc, pub: pub, paneID: paneID}
	return []ToolExecutor{
		&FamilyAsk{bus: bus, Timeout: DefaultFamilyAskTimeout},
		&FamilyReply{bus: bus},
		&FamilyStatus{bus: bus},
	}
}

// ---------------------------------------------------------------------------
// family_ask
// ---------------------------------------------------------------------------

// FamilyAsk posts a board command to another pane and waits for the answer on
// its thread.
type FamilyAsk struct {
	bus *familyBus
	// Timeout bounds the wait for the reply (DefaultFamilyAskTimeout).
	Timeout time.Duration
}

type familyAskParams struct {
	ToPane   string `json:"to_pane"`
	Question string `json:"question"`
}

func (t *FamilyAsk) Spec() ToolSpec {
	return ToolSpec{
		Name: FamilyAskTool,
		Description: "Ask another family member (another agent pane) a question and wait for the answer. " +
			"The question is delivered to them as a board command; their family_reply on its thread is returned to you. " +
			"Waits up to about " + fmt.Sprintf("%d", int(DefaultFamilyAskTimeout.Seconds())) + " seconds.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "to_pane": {"type": "string", "description": "The member to ask: their pane name or pane id"},
    "question": {"type": "string", "description": "The question, complete and self-contained"}
  },
  "required": ["to_pane", "question"],
  "additionalProperties": false
}`),
	}
}

// RequiresApproval is false: the tool only posts as its own pane, to its own
// board, and the recipient is a peer agent that answers in its own turn.
func (t *FamilyAsk) RequiresApproval(json.RawMessage) bool { return false }

func (t *FamilyAsk) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p familyAskParams
	if err := decodeStrict(params, &p); err != nil {
		return ErrOutput(ErrKindValidation, err.Error()), nil
	}
	p.ToPane, p.Question = strings.TrimSpace(p.ToPane), strings.TrimSpace(p.Question)
	if p.ToPane == "" || p.Question == "" {
		return ErrOutput(ErrKindValidation, "to_pane and question are required"), nil
	}
	if o := t.bus.ready(); o != nil {
		return o, nil
	}
	me, boardID, err := t.bus.myBoard()
	if err != nil {
		return ErrOutputf(ErrKindMissing, "family_ask: %v", err), nil
	}

	// e20 W3: only a family member may be asked — a pane whose agent profile
	// allows family_reply. A command to any other pane (a claude pane with a
	// shell, say) would let a prompt-injected avatar instruct it. The target is
	// resolved to its full pane id once, its meta read from its own snapshot,
	// and the command addressed to that id (no second name resolution).
	toID, err := t.bus.familyMember(p.ToPane, me)
	if err != nil {
		return ErrOutputf(ErrKindPermissionDenied, "family_ask: %v", err), nil
	}

	// Mint the command id here so the thread is known BEFORE the post, and
	// listen BEFORE posting so a fast answer cannot be missed.
	cmdID := msg.MintCmdID(time.Now().UnixMilli())
	thread := msg.CmdThreadID(me, cmdID)
	w, err := newThreadWatch(t.bus, boardID, thread, me)
	if err != nil {
		return ErrOutputf(ErrKindTransient, "family_ask: cannot listen on the board: %v", err), nil
	}
	defer w.close()

	resp, err := t.bus.cli(&msg.MsgCLIBoardPost{
		AsPaneID: me, Kind: msg.BoardKindCmd, Text: p.Question,
		Board: boardID, To: toID, CmdID: cmdID,
	})
	if err != nil {
		return ErrOutputf(ErrKindValidation, "family_ask: the question was not sent: %v", err), nil
	}
	if id := strings.TrimSpace(resp.ID); id != "" && id != thread {
		w.setThread(id)
		thread = id
	}

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = DefaultFamilyAskTimeout
	}
	res := w.wait(ctx, timeout)
	switch {
	case res.answer != nil:
		return &ToolOutput{
			Content:  fmt.Sprintf("%s answered:\n%s", res.answer.Persona, strings.TrimSpace(res.answer.Text)),
			Metadata: map[string]string{"thread_id": thread, "cmd_id": cmdID},
		}, nil
	case res.undelivered != "":
		return ErrOutputf(ErrKindMissing, "family_ask: %s could not be reached: %s", p.ToPane, res.undelivered), nil
	case ctx.Err() != nil:
		return ErrOutput(ErrKindCancelled, "family_ask: cancelled while waiting for the answer"), nil
	}
	msgTxt := fmt.Sprintf("family_ask: no answer from %s within %d seconds (thread %s). "+
		"They may still answer later; do not ask again right away.", p.ToPane, int(timeout.Seconds()), thread)
	if res.ackNote != "" {
		msgTxt += " They acknowledged it: " + res.ackNote
	}
	return ErrOutput(ErrKindTimeout, msgTxt), nil
}

// threadWatch listens to one board's posts for the answer on one thread.
type threadWatch struct {
	sub *nats.Subscription
	me  string

	mu     sync.Mutex
	thread string
	ch     chan *msg.MsgBoardPost
}

func newThreadWatch(bus *familyBus, boardID, thread, me string) (*threadWatch, error) {
	w := &threadWatch{me: me, thread: thread, ch: make(chan *msg.MsgBoardPost, 64)}
	codecs := bus.pub.Codecs()
	sub, err := bus.nc.Subscribe(msg.BoardPostSubject(boardID), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		d, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		p, ok := d.(*msg.MsgBoardPost)
		if !ok || p == nil {
			return
		}
		select {
		case w.ch <- p:
		default: // a flood on the board: drop rather than block the bus
		}
	})
	if err != nil {
		return nil, err
	}
	if err := bus.nc.Flush(); err != nil {
		_ = sub.Unsubscribe()
		return nil, err
	}
	w.sub = sub
	return w, nil
}

func (w *threadWatch) setThread(t string) {
	w.mu.Lock()
	w.thread = t
	w.mu.Unlock()
}

func (w *threadWatch) close() { _ = w.sub.Unsubscribe() }

type watchResult struct {
	answer      *msg.MsgBoardPost
	undelivered string
	ackNote     string
}

// wait returns the first answer on the thread: any post on it by another pane
// that is not the command itself or an ack. An ack is remembered (and waited
// past — the answer is the reply); a courier "undelivered" notice ends the wait.
func (w *threadWatch) wait(ctx context.Context, timeout time.Duration) watchResult {
	var res watchResult
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return res
		case <-timer.C:
			return res
		case p := <-w.ch:
			w.mu.Lock()
			thread := w.thread
			w.mu.Unlock()
			if p.ThreadID != thread || p.PaneID == w.me || p.Kind == msg.BoardKindCmd {
				continue
			}
			switch p.Kind {
			case msg.BoardKindAck:
				res.ackNote = strings.TrimSpace(p.Text)
				continue
			case "undelivered":
				res.undelivered = strings.TrimSpace(p.Text)
				return res
			}
			res.answer = p
			return res
		}
	}
}

// ---------------------------------------------------------------------------
// family_reply
// ---------------------------------------------------------------------------

// FamilyReply answers a delivered command: a reply on its thread, then an ack.
type FamilyReply struct{ bus *familyBus }

type familyReplyParams struct {
	ThreadID string `json:"thread_id"`
	Answer   string `json:"answer"`
}

func (t *FamilyReply) Spec() ToolSpec {
	return ToolSpec{
		Name: FamilyReplyTool,
		Description: "Answer a question another family member asked you. Use the thread id from the delivered " +
			"command's header ([rysh board … | command … | thread <id>]). The answer goes back to the asker.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "thread_id": {"type": "string", "description": "The thread id of the command you are answering"},
    "answer": {"type": "string", "description": "Your answer"}
  },
  "required": ["thread_id", "answer"],
  "additionalProperties": false
}`),
	}
}

func (t *FamilyReply) RequiresApproval(json.RawMessage) bool { return false }

func (t *FamilyReply) Execute(_ context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p familyReplyParams
	if err := decodeStrict(params, &p); err != nil {
		return ErrOutput(ErrKindValidation, err.Error()), nil
	}
	p.ThreadID, p.Answer = strings.TrimSpace(p.ThreadID), strings.TrimSpace(p.Answer)
	if p.ThreadID == "" || p.Answer == "" {
		return ErrOutput(ErrKindValidation, "thread_id and answer are required"), nil
	}
	if o := t.bus.ready(); o != nil {
		return o, nil
	}
	me, boardID, err := t.bus.myBoard()
	if err != nil {
		return ErrOutputf(ErrKindMissing, "family_reply: %v", err), nil
	}
	reply, err := t.bus.query(boardID)
	if err != nil {
		return ErrOutputf(ErrKindTransient, "family_reply: %v", err), nil
	}
	// Only a command addressed to THIS pane can be answered: a reply on some
	// other thread would put words in a conversation this pane was not part of.
	var item *board.InboxItem
	for _, it := range board.Inbox(reply, me) {
		if it.ThreadID == p.ThreadID {
			it := it
			item = &it
			break
		}
	}
	if item == nil {
		return ErrOutputf(ErrKindMissing, "family_reply: no command addressed to you on thread %s", p.ThreadID), nil
	}
	alreadyAnswered := item.Answered
	if _, err := t.bus.cli(&msg.MsgCLIBoardPost{
		AsPaneID: me, Kind: msg.BoardKindReply, Text: p.Answer,
		ThreadID: item.ThreadID, Board: boardID,
	}); err != nil {
		return ErrOutputf(ErrKindTransient, "family_reply: the answer was not posted: %v", err), nil
	}
	if !alreadyAnswered {
		// Close the command: the asker's tooling and the board count the ack.
		if _, err := t.bus.cli(&msg.MsgCLIBoardPost{
			AsPaneID: me, Kind: msg.BoardKindAck, Text: "answered",
			ThreadID: item.ThreadID, Board: boardID, CmdID: item.CmdID,
		}); err != nil {
			return &ToolOutput{Content: fmt.Sprintf("answer sent to %s (the ack failed: %v)", item.From, err)}, nil
		}
	}
	return &ToolOutput{
		Content:  fmt.Sprintf("answer sent to %s", item.From),
		Metadata: map[string]string{"thread_id": item.ThreadID, "cmd_id": item.CmdID},
	}, nil
}

// ---------------------------------------------------------------------------
// family_status
// ---------------------------------------------------------------------------

// FamilyStatus declares this pane's status in the fleet ledger.
type FamilyStatus struct{ bus *familyBus }

type familyStatusParams struct {
	State     string `json:"state"`
	Note      string `json:"note,omitempty"`
	Pct       *int   `json:"pct,omitempty"`
	BlockedBy string `json:"blocked_by,omitempty"`
}

// familyBlockedDefault is who blocks an avatar that says blocked without
// naming anyone: the person it talks to (the fleet ledger's "human").
const familyBlockedDefault = fleet.BlockerHuman

func (t *FamilyStatus) Spec() ToolSpec {
	return ToolSpec{
		Name:        FamilyStatusTool,
		Description: "Set your status for the family: idle, progress (working on something), blocked, or done. Add a short note and, while working, a percent.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "state": {"type": "string", "enum": ["idle", "progress", "blocked", "done"], "description": "Your state"},
    "note": {"type": "string", "description": "A short note: what you are doing, or what blocks you"},
    "pct": {"type": "integer", "minimum": 0, "maximum": 100, "description": "Progress percent (optional)"},
    "blocked_by": {"type": "string", "description": "Only with state blocked: who you are waiting for — a family member's name or pane id, \"human\" for the person you talk to (the default), or \"ext:<what>\" for something outside the family"}
  },
  "required": ["state"],
  "additionalProperties": false
}`),
	}
}

func (t *FamilyStatus) RequiresApproval(json.RawMessage) bool { return false }

func (t *FamilyStatus) Execute(_ context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p familyStatusParams
	if err := decodeStrict(params, &p); err != nil {
		return ErrOutput(ErrKindValidation, err.Error()), nil
	}
	work := strings.ToLower(strings.TrimSpace(p.State))
	if !fleet.ValidWork(work) {
		return ErrOutputf(ErrKindValidation, "state must be idle, progress, blocked or done, got %q", p.State), nil
	}
	if p.Pct != nil && (*p.Pct < 0 || *p.Pct > 100) {
		return ErrOutput(ErrKindValidation, "pct must be 0-100"), nil
	}
	if t.bus.nc == nil {
		return ErrOutput(ErrKindMissing, "the session bus is not available"), nil
	}
	if strings.TrimSpace(t.bus.paneID) == "" {
		return ErrOutput(ErrKindInternal, "family tool has no pane identity"), nil
	}
	blockedBy := strings.TrimSpace(p.BlockedBy)
	if blockedBy != "" && work != fleet.WorkBlocked {
		return ErrOutput(ErrKindValidation, "blocked_by is only for state blocked"), nil
	}
	u := fleet.StatusUpdate{Work: work, Note: strings.TrimSpace(p.Note), Pct: p.Pct}
	if work == fleet.WorkBlocked {
		// The ledger refuses blocked with no blocker; an avatar that does not
		// say who is waiting on the person it talks to.
		if blockedBy == "" {
			blockedBy = familyBlockedDefault
		}
		u.BlockedBy = []string{blockedBy}
	}
	// As `rysh fleet status`: blocked/done with no task keep the task they are about.
	u.KeepTask = work == fleet.WorkBlocked || work == fleet.WorkDone
	reply, err := fleet.Send(t.bus.nc, fleet.Update{Op: fleet.OpStatus, PaneID: t.bus.paneID, Status: u}, familyBusTimeout)
	if err != nil {
		return ErrOutputf(ErrKindTransient, "family_status: %v", err), nil
	}
	out := "status recorded: " + work
	if reply != nil && reply.Member != nil && reply.Member.Label != "" {
		out = fmt.Sprintf("status recorded: %s is %s", reply.Member.Label, work)
	}
	return &ToolOutput{Content: out}, nil
}

// NewFamilyToolRegistry is NewFamilyTools in a registry, keyed by tool name —
// the opt-in registry a pane's LLM actor is given (SetOptInTools).
func NewFamilyToolRegistry(nc *nats.Conn, pub *msg.NATSPublisher, paneID string) *ToolRegistry {
	r := NewToolRegistry()
	for _, e := range NewFamilyTools(nc, pub, paneID) {
		r.Register(e.Spec().Name, e)
	}
	return r
}
