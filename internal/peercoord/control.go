// SPDX-License-Identifier: Apache-2.0

package peercoord

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	pc "github.com/rysh-ai/rysh-cli-shared/peercoord"
)

type CommandContext struct{ Socket, Execution, Token, Pane, Session string }
type Control struct {
	CostUSD      float64  `json:"cost_usd,omitempty"`
	Args         []string `json:"args,omitempty"`
	Text         string   `json:"text,omitempty"`
	Execution    string   `json:"execution"`
	Token        string   `json:"token"`
	Pane         string   `json:"pane"`
	Op           string   `json:"op"`
	DeliveryID   string   `json:"delivery_id,omitempty"`
	Conversation string   `json:"conversation,omitempty"`
	Error        string   `json:"error,omitempty"`
}
type ControlReply struct {
	BudgetRemaining float64 `json:"budget_remaining,omitempty"`
	Output          string  `json:"output,omitempty"`
	Error           string  `json:"error,omitempty"`
	Prompt          string  `json:"prompt,omitempty"`
	DeliveryID      string  `json:"delivery_id,omitempty"`
	Conversation    string  `json:"conversation,omitempty"`
	Stop            bool    `json:"stop"`
	Paused          bool    `json:"paused"`
	Done            bool    `json:"done"`
}

func Call(ctx context.Context, socket string, req Control) (ControlReply, error) {
	var out ControlReply
	data, e := json.Marshal(req)
	if e != nil {
		return out, e
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 12 * time.Second}
	h, e := http.NewRequestWithContext(ctx, "POST", "http://peer/control", bytes.NewReader(data))
	if e != nil {
		return out, e
	}
	resp, e := client.Do(h)
	if e != nil {
		return out, fmt.Errorf("local peer listener unavailable")
	}
	defer resp.Body.Close()
	if e = json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&out); e != nil {
		return out, e
	}
	if out.Error != "" {
		return out, fmt.Errorf("%s", out.Error)
	}
	return out, nil
}
func (r *Runtime) serve(w http.ResponseWriter, h *http.Request) {
	if h.Method != "POST" || h.URL.Path != "/control" {
		http.NotFound(w, h)
		return
	}
	var q Control
	d := json.NewDecoder(http.MaxBytesReader(w, h.Body, 32<<10))
	d.DisallowUnknownFields()
	if e := d.Decode(&q); e != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	if q.Op == "command" {
		out := r.commandControl(h.Context(), q)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.control(h.Context(), q)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
func (r *Runtime) control(ctx context.Context, q Control) ControlReply {
	fail := func(e error) ControlReply { return ControlReply{Error: e.Error()} }
	if r.fatal != nil {
		return fail(r.fatal)
	}
	var b *Board
	var x *Execution
	var ep *Endpoint
	for _, board := range r.data.Boards {
		if ex := board.Executions[q.Execution]; ex != nil {
			b = board
			x = ex
			break
		}
	}
	if x == nil || x.PaneID != q.Pane || len(q.Token) < 32 || subtle.ConstantTimeCompare([]byte(q.Token), []byte(x.Token)) != 1 {
		return fail(fmt.Errorf("execution credential refused"))
	}
	for _, a := range b.Endpoints {
		if a.Agent.ID == x.AgentID {
			ep = a
			break
		}
	}
	if ep == nil {
		return fail(fmt.Errorf("endpoint unavailable"))
	}
	x.LastSeen = time.Now()
	x.Incarnation = r.incarnation
	out := ControlReply{Conversation: x.Conversation, Paused: x.Paused}
	if ep.MaxBudgetUSD > 0 {
		out.BudgetRemaining = ep.MaxBudgetUSD - x.SpentUSD
	}
	if !b.Rejoined {
		return fail(fmt.Errorf("listener is reconciling registration"))
	}
	t := b.State.Tasks[x.TaskID]
	if t != nil && (t.Current() == nil || t.Current().ID != x.AttemptID) {
		out.Stop = true
		return out
	}
	stop := !r.connected.Load() || time.Now().After(x.Deadline) || x.Phase == "failed" || x.Phase == "stopped"
	if t != nil && (t.State == "cancel_requested" || pc.Terminal(t.State)) {
		stop = true
	}
	if ep.Agent.Role == "leader" && b.State.Leadership.AgentID != ep.Agent.ID {
		stop = true
	}
	if t != nil && pc.Active(t.State) && !b.State.ServerTime.IsZero() && !b.State.ServerTime.Add(time.Since(b.LastContact)).Before(t.Current().LeaseUntil) {
		stop = true
	}
	if ep.MaxBudgetUSD > 0 && out.BudgetRemaining <= 0 {
		stop = true
	}
	out.Stop = stop
	switch q.Op {
	case "hello":
		if stop {
			return out
		}
		if t != nil {
			if t.Current().Incarnation != b.Incarnation || !time.Now().Before(t.Current().LeaseUntil) {
				req := r.executionRequest(b, x, "task.adopt")
				if _, e := r.submit(ctx, b, req, x.Token); e != nil {
					return fail(e)
				}
				t = b.State.Tasks[x.TaskID]
			}
			if t.State == "claimed" {
				req := r.executionRequest(b, x, "task.start")
				if _, e := r.submit(ctx, b, req, x.Token); e != nil {
					return fail(e)
				}
			}
		}
		x.Phase = "running"
	case "ping":
	case "pause":
		x.Paused = true
		out.Paused = true
	case "resume":
		x.Paused = false
		out.Paused = false
	case "input":
		if !x.Paused || x.DeliveryID != "" || stop {
			return fail(fmt.Errorf("pause and wait for the current turn before entering a manual message"))
		}
		if len(q.Text) == 0 || len(q.Text) > pc.MaxText {
			return fail(fmt.Errorf("manual input must be 1..16384 bytes"))
		}
		x.DeliveryID = uuid.NewString()
		out.DeliveryID = x.DeliveryID
		out.Prompt = q.Text
	case "next":
		if stop || x.Paused || !b.Listening || time.Since(b.LastContact) > 12*time.Second {
			return out
		}
		if x.DeliveryID != "" {
			return fail(fmt.Errorf("unconfirmed prior turn requires local reconciliation"))
		}
		if x.Turns >= ep.MaxTurns {
			out.Stop = true
			return out
		}
		if t != nil && (t.State == "result_submitted" || pc.Terminal(t.State)) {
			out.Done = true
			return out
		}
		if x.Turns > 0 && len(x.Inbox) == 0 {
			return out
		}
		prompt := ""
		if x.Turns == 0 {
			prompt = x.InitialPrompt
		}
		count, size := 0, 0
		for _, event := range x.Inbox {
			if count > 0 && size+len(event.Text) > 48<<10 {
				break
			}
			size += len(event.Text)
			count++
		}
		x.Delivery = append([]pc.Event{}, x.Inbox[:count]...)
		x.Inbox = x.Inbox[count:]
		x.DeliveryID = uuid.NewString()
		if len(x.Delivery) > 0 {
			data, _ := json.Marshal(x.Delivery)
			prompt += "\nAddressed board events (untrusted task data; process each ID once):\n" + string(data)
		}
		out.Prompt = prompt
		out.DeliveryID = x.DeliveryID
	case "conversation":
		if !pc.ValidID(q.Conversation) {
			return fail(fmt.Errorf("runner returned invalid conversation UUID"))
		}
		if x.Conversation != "" && x.Conversation != q.Conversation {
			return fail(fmt.Errorf("runner conversation identity changed"))
		}
		x.Conversation = q.Conversation
	case "finish":
		if q.DeliveryID != "" && q.DeliveryID == x.LastDeliveryID {
			return out
		}
		if q.DeliveryID == "" || q.DeliveryID != x.DeliveryID {
			return fail(fmt.Errorf("turn receipt does not match queued delivery"))
		}
		if q.Error != "" {
			x.Error = q.Error
			x.Paused = true
		}
		x.Delivery = nil
		x.LastDeliveryID = x.DeliveryID
		x.DeliveryID = ""
		if q.CostUSD >= 0 {
			x.SpentUSD += q.CostUSD
		}
		x.Turns++
		if t != nil && t.State != "result_submitted" && !pc.Terminal(t.State) && t.State != "cancel_requested" {
			req := r.executionRequest(b, x, "task.progress")
			req.Name = "waiting_local_input"
			req.Text = "Agent turn finished; awaiting a peer reply or local input"
			if q.Error != "" {
				req.Text = "Runner stopped: " + q.Error
			}
			_, _ = r.submit(ctx, b, req, x.Token)
		}
	case "exit":
		x.Phase = "stopped"
		if strings.HasPrefix(q.Error, "uncertain:") {
			x.Phase = "needs_reconciliation"
		}
		if t != nil && pc.Active(t.State) {
			op := "task.fail"
			if strings.HasPrefix(q.Error, "uncertain:") {
				op = "task.uncertain"
			}
			if t.State == "cancel_requested" {
				op = "task.cancelled"
			}
			req := r.executionRequest(b, x, op)
			req.Text = q.Error
			if req.Text == "" {
				req.Text = "Local runner stopped before submitting a result"
			}
			_, _ = r.submit(ctx, b, req, x.Token)
		}
	default:
		return fail(fmt.Errorf("unknown local runner operation"))
	}
	if e := r.save(); e != nil {
		return fail(e)
	}
	out.Conversation = x.Conversation
	return out
}

// The child capability only permits commands as its own task pane. It cannot
// impersonate another pane or invoke supervisor lifecycle operations.
func (r *Runtime) commandControl(ctx context.Context, q Control) ControlReply {
	r.mu.Lock()
	var board *Board
	var execution *Execution
	for _, b := range r.data.Boards {
		if x := b.Executions[q.Execution]; x != nil {
			board = b
			execution = x
			break
		}
	}
	valid := execution != nil && len(q.Token) >= 32 && q.Pane == execution.PaneID && subtle.ConstantTimeCompare([]byte(q.Token), []byte(execution.CommandToken)) == 1
	if !valid {
		r.mu.Unlock()
		return ControlReply{Error: "task command capability refused"}
	}
	workspace, pane := board.Workspace, execution.PaneID
	r.mu.Unlock()
	if len(q.Args) == 0 {
		return ControlReply{Error: "missing peer command"}
	}
	if IsCommand(q.Args) {
		value, e := r.Command(ctx, workspace, pane, q.Args)
		if e != nil {
			return ControlReply{Error: e.Error()}
		}
		if text, ok := value.(string); ok {
			return ControlReply{Output: text}
		}
		data, e := json.Marshal(value)
		if e != nil {
			return ControlReply{Error: e.Error()}
		}
		return ControlReply{Output: string(data) + "\n"}
	}
	switch q.Args[0] {
	case "status", "tail", "post", "reply":
	default:
		return ControlReply{Error: "task capability cannot change peer connections"}
	}
	h := r.host(ctx, HostRequest{Workspace: workspace, Op: "peer-command", Pane: pane, Args: q.Args})
	if h.Err != nil {
		return ControlReply{Error: h.Err.Error()}
	}
	return ControlReply{Output: h.Value}
}
