// SPDX-License-Identifier: Apache-2.0

package peercoord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	pc "github.com/rysh-ai/rysh-cli-shared/peercoord"
	"github.com/rysh-ai/rysh-cli-code/internal/peer"
)

func validName(s string) bool { return pc.ValidName(s) }
func (r *Runtime) client(ctx context.Context, b *Board) (peer.CoordinationClient, error) {
	h := r.host(ctx, HostRequest{Workspace: b.Workspace, Op: "secret", Secret: b.Connection.KeySecret})
	if h.Err != nil {
		return peer.CoordinationClient{}, h.Err
	}
	if h.Value == "" {
		return peer.CoordinationClient{}, fmt.Errorf("peer API key secret unavailable")
	}
	return peer.CoordinationClient{Client: peer.Client{Connection: b.Connection, Key: h.Value}, PeerToken: b.Token}, nil
}
func (r *Runtime) request(b *Board, op string) pc.Request {
	return pc.Request{SchemaVersion: pc.Version, ID: uuid.NewString(), Op: op, PeerID: b.PeerID, Incarnation: b.Incarnation}
}
func (r *Runtime) transmit(ctx context.Context, b *Board, req pc.Request, token string) (pc.Response, error) {
	c, err := r.client(ctx, b)
	if err != nil {
		return pc.Response{}, err
	}
	resp, err := c.Apply(ctx, req, token)
	if err != nil {
		return resp, err
	}
	b.LastContact = time.Now()
	b.LastError = ""
	if resp.Peer != nil {
		b.PeerID = resp.Peer.ID
	}
	if resp.Agent != nil {
		ep := b.Endpoints[resp.Agent.Name]
		if ep == nil {
			ep = &Endpoint{Mode: "manual", Permission: "review", MaxRuntime: 900, MaxTurns: 20}
			b.Endpoints[resp.Agent.Name] = ep
		}
		ep.Agent = *resp.Agent
	}
	if resp.Leadership != nil {
		b.State.Leadership = *resp.Leadership
	}
	if resp.Task != nil {
		if b.State.Tasks == nil {
			b.State.Tasks = map[string]*pc.Task{}
		}
		b.State.Tasks[resp.Task.ID] = resp.Task
	}
	return resp, nil
}
func (r *Runtime) submit(ctx context.Context, b *Board, req pc.Request, token string) (pc.Response, error) {
	// The exact request and its UUID are durable before the first network send.
	for _, o := range b.Outbox {
		if o.Request.ID == req.ID {
			a, _ := json.Marshal(o.Request)
			z, _ := json.Marshal(req)
			if string(a) != string(z) || o.Token != token {
				return pc.Response{}, fmt.Errorf("request UUID already queued with other content")
			}
			return r.flushOne(ctx, b, req.ID)
		}
	}
	if len(b.Outbox) >= 1000 {
		return pc.Response{}, fmt.Errorf("outgoing queue full")
	}
	b.Outbox = append(b.Outbox, Outgoing{Request: req, Token: token})
	if err := r.save(); err != nil {
		return pc.Response{}, err
	}
	return r.flushOne(ctx, b, req.ID)
}
func (r *Runtime) flushOne(ctx context.Context, b *Board, id string) (pc.Response, error) {
	for i, o := range b.Outbox {
		if o.Request.ID != id {
			continue
		}
		resp, err := r.transmit(ctx, b, o.Request, o.Token)
		var known *pc.Error
		permanent := errors.As(err, &known) && known.Code != "internal" && known.Code != "http"
		if err == nil || permanent {
			b.Outbox = append(b.Outbox[:i], b.Outbox[i+1:]...)
			if saveErr := r.save(); saveErr != nil {
				return resp, saveErr
			}
		}
		return resp, err
	}
	return pc.Response{}, fmt.Errorf("outgoing request not found")
}
func (r *Runtime) refresh(ctx context.Context, b *Board) error {
	c, e := r.client(ctx, b)
	if e != nil {
		return e
	}
	s, e := c.State(ctx)
	if e == nil {
		b.State = s
		b.LastContact = time.Now()
	}
	return e
}
func (r *Runtime) endpoint(b *Board, pane, ref string) (*Endpoint, *Execution, error) {
	for _, x := range b.Executions {
		if x.PaneID == pane {
			for _, ep := range b.Endpoints {
				if ep.Agent.ID == x.AgentID {
					return ep, x, nil
				}
			}
		}
	}
	var found *Endpoint
	for name, ep := range b.Endpoints {
		if ep.Origin == pane && (ref == "" || ref == name || ref == ep.Agent.ID) {
			if found != nil {
				return nil, nil, fmt.Errorf("several endpoints use this registration pane; specify --agent NAME")
			}
			found = ep
		}
	}
	if found != nil {
		return found, nil, nil
	}
	return nil, nil, fmt.Errorf("this pane has no registered endpoint; register here or use its --as pane")
}
func (r *Runtime) Command(ctx context.Context, workspace, pane string, args []string) (any, error) {
	a, err := Parse(args)
	if err != nil {
		return nil, err
	}
	if a.Verb == "help" {
		return Usage, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fatal != nil {
		return nil, r.fatal
	}
	conn, err := peer.Load(r.root, r.session)
	if err != nil {
		return nil, err
	}
	if !conn.Enabled {
		return nil, fmt.Errorf("peer disconnected; use peer connect first")
	}
	if a.Flags["board"] != "" {
		conn.Board = a.Flags["board"]
	}
	key := BoardKey(conn)
	b := r.data.Boards[key]
	if b == nil && (a.Verb == "init" || a.Verb == "join") {
		b = &Board{Connection: conn, Workspace: workspace, Token: uuid.NewString() + uuid.NewString(), Incarnation: r.incarnation, Projects: map[string]Project{}, Endpoints: map[string]*Endpoint{}, Executions: map[string]*Execution{}}
		r.data.Boards[key] = b
	}
	if b == nil {
		return nil, fmt.Errorf("peer coordination not joined; use peer init or peer join")
	}
	// A managed task can report and inspect, but cannot enroll machines or raise
	// its local permissions. Role and leader authority are checked server-side.
	ep, x, endpointErr := r.endpoint(b, pane, a.Flags["agent"])
	if x != nil {
		switch a.Verb {
		case "state", "summary", "members", "agents", "events", "send", "task":
		default:
			return nil, fmt.Errorf("managed agents cannot change local enrollment")
		}
	}
	if a.Verb == "init" || a.Verb == "join" || a.Verb == "register" {
		if x != nil {
			return nil, fmt.Errorf("managed execution cannot register")
		}
		h := r.host(ctx, HostRequest{Workspace: workspace, Op: "inspect", Pane: pane})
		if h.Err != nil || !h.Exists {
			return nil, fmt.Errorf("registration requires an existing local --as pane")
		}
		if b.Workspace != workspace {
			return nil, fmt.Errorf("board is owned by local workspace %s", b.Workspace)
		}
		name := a.Flags["name"]
		if !validName(name) {
			return nil, fmt.Errorf("--name required (lowercase name)")
		}
		role := a.Flags["role"]
		if role == "" {
			role = "worker"
		}
		if a.Verb == "init" {
			role = "leader"
		}
		runner := a.Flags["runner"]
		if runner == "" {
			runner = "codex"
		}
		max, e := a.int("max-active", 1)
		if e != nil {
			return nil, e
		}
		req := r.request(b, a.Verb)
		req.Name = name
		req.Role = role
		req.Runner = runner
		req.Projects = csv(a.Flags["project"])
		req.Capabilities = csv(a.Flags["capabilities"])
		req.MaxActive = max
		req.SessionID = conn.SessionID
		req.SessionName = r.session
		if id := a.Flags["id"]; id != "" {
			req.ID = id
		}
		if old := b.Endpoints[name]; old != nil && old.Origin != pane {
			return nil, fmt.Errorf("endpoint is already bound to another local pane")
		}
		if b.Endpoints[name] == nil {
			b.Endpoints[name] = &Endpoint{Origin: pane, Mode: "manual", Permission: "review", MaxRuntime: 900, MaxTurns: 20}
		}
		resp, e := r.submit(ctx, b, req, "")
		if e == nil {
			b.Rejoined = true
			e = r.save()
		}
		return resp, e
	}
	if a.Verb == "project" {
		if x != nil {
			return nil, fmt.Errorf("managed task cannot map projects")
		}
		if a.Sub == "list" {
			return b.Projects, nil
		}
		if a.Sub != "add" || len(a.Pos) != 2 || !validName(a.Pos[0]) || !filepath.IsAbs(a.Pos[1]) {
			return nil, fmt.Errorf("peer project add ALIAS /absolute/git/repository [--base REF]")
		}
		path, e := filepath.EvalSymlinks(a.Pos[1])
		if e != nil {
			return nil, e
		}
		base := a.Flags["base"]
		if base == "" {
			base = "HEAD"
		}
		if _, e = git(ctx, path, "rev-parse", "--verify", base+"^{commit}"); e != nil {
			return nil, fmt.Errorf("project/base unavailable: %w", e)
		}
		b.Projects[a.Pos[0]] = Project{Path: path, Base: base}
		return b.Projects, r.save()
	}
	if a.Verb == "enroll" {
		name, e := a.one()
		if e != nil {
			return nil, e
		}
		ep = b.Endpoints[name]
		if ep == nil || ep.Agent.ID == "" {
			return nil, fmt.Errorf("register this endpoint first")
		}
		if ep.Origin != pane {
			return nil, fmt.Errorf("enroll from the endpoint's registration pane")
		}
		if e = r.refresh(ctx, b); e != nil {
			return nil, e
		}
		accepted := []string{}
		for _, ref := range csv(a.Flags["accept-leader"]) {
			ag, e := b.State.ResolveAgent(ref)
			if e != nil {
				return nil, e
			}
			if ag.Role != "leader" {
				return nil, fmt.Errorf("accepted endpoint must be a leader candidate")
			}
			accepted = append(accepted, ag.ID)
		}
		if ep.Agent.Role == "worker" && len(accepted) == 0 {
			return nil, fmt.Errorf("--accept-leader registered-ID or session/leader required")
		}
		permission := a.Flags["permission"]
		if permission != "review" && permission != "workspace" {
			return nil, fmt.Errorf("--permission review or workspace required")
		}
		mode := a.Flags["mode"]
		if mode == "" {
			mode = "manual"
		}
		if mode != "auto" && mode != "manual" {
			return nil, fmt.Errorf("--mode auto or manual")
		}
		max, e := a.int("max-runtime", 900)
		if e != nil || max < 30 || max > 86400 {
			return nil, fmt.Errorf("max-runtime must be 30..86400 seconds")
		}
		turns, e := a.int("max-turns", 20)
		if e != nil || turns < 1 || turns > 100 {
			return nil, fmt.Errorf("max-turns must be 1..100")
		}
		budget := 0.0
		if s := a.Flags["max-budget-usd"]; s != "" {
			budget, e = strconv.ParseFloat(s, 64)
			if e != nil || budget <= 0 {
				return nil, fmt.Errorf("invalid max-budget-usd")
			}
			if ep.Agent.Runner != "claude" {
				return nil, fmt.Errorf("native USD budget is currently supported for Claude; use max-runtime/max-turns for Codex")
			}
		}
		if e = r.probe(ctx, ep.Agent.Runner); e != nil {
			return nil, e
		}
		ep.LastProbe = time.Now()
		ep.SetupError = ""
		for _, p := range ep.Agent.Projects {
			if b.Projects[p].Path == "" {
				return nil, fmt.Errorf("project %s is not mapped locally", p)
			}
		}
		if len(ep.Agent.Projects) == 0 {
			return nil, fmt.Errorf("register at least one --project for a managed endpoint")
		}
		ep.Enrolled = true
		ep.AcceptedLeaders = accepted
		ep.Permission = permission
		ep.Mode = mode
		ep.Model = a.Flags["model"]
		ep.MaxRuntime = max
		ep.MaxTurns = turns
		ep.MaxBudgetUSD = budget
		return map[string]any{"agent": ep.Agent.Label, "mode": mode, "permission": permission, "accepted_leaders": accepted}, r.save()
	}
	if a.Verb == "listen" {
		switch a.Sub {
		case "start":
			b.Listening = true
			b.Draining = false
		case "pause", "stop":
			b.Listening = false
		case "drain":
			b.Draining = true
		case "status":
		default:
			return nil, fmt.Errorf("listen start|pause|drain|stop|status")
		}
		return r.localView(b), r.save()
	}
	if a.Verb == "local" {
		id, e := a.one()
		if e != nil {
			return nil, e
		}
		ex := b.Executions[id]
		if ex == nil {
			return nil, fmt.Errorf("local execution not found")
		}
		if a.Sub != "pause" && a.Sub != "resume" {
			return nil, fmt.Errorf("local pause|resume EXECUTION")
		}
		ex.Paused = a.Sub == "pause"
		return map[string]any{"execution": id, "paused": ex.Paused}, r.save()
	}
	if a.Verb == "leader" && a.Sub == "restart" {
		if ep == nil || x != nil || ep.Agent.Role != "leader" {
			return nil, fmt.Errorf("restart from the leader registration pane")
		}
		if old := b.Executions[ep.LeaderExecution]; old != nil {
			if old.Phase != "stopped" && old.Phase != "failed" {
				if a.Flags["confirm-stopped"] != "true" {
					return nil, fmt.Errorf("confirm the previous leader process stopped before --confirm-stopped")
				}
			}
			b.Inbox = append(b.Inbox, old.Delivery...)
			b.Inbox = append(b.Inbox, old.Inbox...)
			old.Inbox = nil
			old.Delivery = nil
		}
		ep.LeaderExecution = ""
		return map[string]string{"leader": ep.Agent.Label, "status": "ready for the next objective"}, r.save()
	}
	if err = r.refresh(ctx, b); err != nil {
		return nil, err
	}
	switch a.Verb {
	case "state":
		return r.boardView(b), nil
	case "summary":
		return summarizeBoard(b), nil
	case "members":
		return b.State.Peers, nil
	case "agents":
		return b.State.Agents, nil
	case "events":
		after, e := a.int("after", 0)
		if e != nil {
			return nil, e
		}
		c, e := r.client(ctx, b)
		if e != nil {
			return nil, e
		}
		return c.Events(ctx, int64(after))
	case "board":
		if a.Sub == "show" || a.Sub == "watch" {
			return r.boardView(b), nil
		}
		if a.Sub == "open" {
			return r.openBoard(ctx, b, pane)
		}
		return nil, fmt.Errorf("board open|show|watch")
	case "task":
		if a.Sub == "list" {
			return b.State.Tasks, nil
		}
		if a.Sub == "show" || a.Sub == "open" {
			ref, e := a.one()
			if e != nil {
				return nil, e
			}
			task, e := b.State.ResolveTask(ref)
			if e != nil {
				return nil, e
			}
			if a.Sub == "open" {
				if x != nil {
					return nil, fmt.Errorf("only a local human opens task panes")
				}
				for _, execution := range b.Executions {
					if execution.TaskID == task.ID && execution.AttemptID == task.Current().ID {
						h := r.host(ctx, HostRequest{Workspace: b.Workspace, Op: "focus", Pane: execution.PaneID, Execution: execution.ID})
						return map[string]string{"pane_id": execution.PaneID}, h.Err
					}
				}
				return nil, fmt.Errorf("task is remote or has no local pane; inspect its messages and result")
			}
			return task, nil
		}
	}
	if ep == nil {
		return nil, endpointErr
	}
	req := r.request(b, a.Verb)
	req.AgentID = ep.Agent.ID
	req.Term = b.State.Leadership.Term
	req.Text = a.Text
	if id := a.Flags["id"]; id != "" {
		req.ID = id
	}
	switch a.Verb {
	case "leave":
		b.Listening = false
	case "send":
		target, e := b.State.ResolveAgent(a.Flags["to"])
		if e != nil {
			return nil, e
		}
		req.Target = target.ID
		if ref := a.Flags["task"]; ref != "" {
			task, e := b.State.ResolveTask(ref)
			if e != nil {
				return nil, e
			}
			req.TaskID = task.ID
			req.AttemptID = task.Current().ID
		}
		if req.TaskID == "" && ep.Agent.ID == b.State.Leadership.AgentID && target.Role == "worker" {
			for _, task := range b.State.Tasks {
				if task.AgentID == target.ID && pc.Active(task.State) {
					if req.TaskID != "" {
						return nil, fmt.Errorf("worker has several active tasks; specify --task")
					}
					req.TaskID = task.ID
					req.AttemptID = task.Current().ID
				}
			}
		}
		if x != nil && x.TaskID != "" {
			req.TaskID = x.TaskID
			req.AttemptID = x.AttemptID
		}
	case "leader":
		if x != nil && ep.Agent.Role != "leader" {
			return nil, fmt.Errorf("worker cannot change leadership")
		}
		req.Op = "leader." + a.Sub
		if a.Sub == "objective" {
			req.Op = "objective"
			req.Target = b.State.Leadership.AgentID
		}
		if a.Sub == "handover" {
			ref, e := a.one()
			if e != nil {
				return nil, e
			}
			target, e := b.State.ResolveAgent(ref)
			if e != nil {
				return nil, e
			}
			req.Target = target.ID
		}
		if a.Sub == "standbys" {
			req.AutoFailover = a.Flags["auto"] == "true"
			for _, ref := range csv(a.Flags["agents"]) {
				target, e := b.State.ResolveAgent(ref)
				if e != nil {
					return nil, e
				}
				req.Standbys = append(req.Standbys, target.ID)
			}
		}
	case "task":
		req.Op = "task." + a.Sub
		if a.Sub == "assign" {
			if x != nil && ep.Agent.Role != "leader" {
				return nil, fmt.Errorf("nested worker delegation is disabled")
			}
			target, e := b.State.ResolveAgent(a.Flags["to"])
			if e != nil {
				return nil, e
			}
			req.Target = target.ID
			req.Project = a.Flags["project"]
			req.Runner = target.Runner
			req.Criteria = a.Flags["criteria"]
			req.Base = a.Flags["base"]
			req.MaxRuntimeSeconds, e = a.int("max-runtime", 900)
			if e != nil {
				return nil, e
			}
			for _, ref := range csv(a.Flags["depends"]) {
				t, e := b.State.ResolveTask(ref)
				if e != nil {
					return nil, e
				}
				req.Dependencies = append(req.Dependencies, t.ID)
			}
		} else {
			ref, e := a.one()
			if e != nil {
				return nil, e
			}
			t, e := b.State.ResolveTask(ref)
			if e != nil {
				return nil, e
			}
			req.TaskID = t.ID
			req.TaskVersion = t.Version
			req.AttemptID = t.Current().ID
			if x != nil && ep.Agent.Role == "worker" && (x.TaskID != t.ID || x.AttemptID != req.AttemptID) {
				return nil, fmt.Errorf("task command is outside this execution")
			}
			switch a.Sub {
			case "checkpoint":
				return checkpointTask(ctx, b, ep, x, t, a.Text)
			case "admit":
				if x != nil {
					return nil, fmt.Errorf("only the local owner can admit tasks")
				}
				target := b.State.Agents[t.AgentID]
				if target == nil || target.PeerID != b.PeerID {
					return nil, fmt.Errorf("task is not local")
				}
				return r.admit(ctx, b, t, true)
			case "progress", "result":
				if x == nil || x.TaskID != t.ID {
					return nil, fmt.Errorf("report from the assigned task pane")
				}
				req.ExecutionID = x.ID
				req.Name = a.Flags["state"]
				if a.Sub == "result" {
					if raw := a.Flags["result-json"]; raw != "" {
						req.Result = json.RawMessage(raw)
					} else {
						req.Result, _ = json.Marshal(map[string]string{"summary": a.Text})
					}
				}
			case "review":
				req.Ready = a.Flags["accept"] == "true"
			case "retry":
				target, e := b.State.ResolveAgent(a.Flags["to"])
				if e != nil {
					return nil, e
				}
				req.Target = target.ID
			case "resolve":
				req.ConfirmStopped = a.Flags["confirm-stopped"] == "true"
			case "cancel":
			default:
				return nil, fmt.Errorf("unsupported task command")
			}
		}
	default:
		return nil, fmt.Errorf("unsupported coordination command")
	}
	token := ""
	if x != nil {
		token = x.Token
	}
	resp, err := r.submit(ctx, b, req, token)
	if err == nil && a.Verb == "leave" {
		b.PeerID = ""
		b.Rejoined = false
		_ = r.save()
	}
	return resp, err
}
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	b, e := cmd.CombinedOutput()
	if e != nil {
		return "", fmt.Errorf("git: %s", strings.TrimSpace(string(b)))
	}
	return strings.TrimSpace(string(b)), nil
}
func (r *Runtime) localView(b *Board) map[string]any {
	execs := map[string]any{}
	for id, x := range b.Executions {
		execs[id] = map[string]any{"agent_id": x.AgentID, "task_id": x.TaskID, "attempt_id": x.AttemptID, "pane_id": x.PaneID, "phase": x.Phase, "directory": x.Directory, "branch": x.Branch, "conversation": x.Conversation, "paused": x.Paused, "error": x.Error, "pending": len(x.Inbox), "turns": x.Turns, "spent_usd": x.SpentUSD}
	}
	return map[string]any{"listening": b.Listening, "draining": b.Draining, "peer_id": b.PeerID, "last_contact": b.LastContact, "last_error": b.LastError, "cursor": b.Cursor, "outbox": len(b.Outbox), "executions": execs, "admission_errors": b.AdmissionErrors, "endpoints": b.Endpoints}
}
func (r *Runtime) boardView(b *Board) map[string]any {
	return map[string]any{"board": b.Connection.Board, "transport": "messages-only", "state": b.State, "local": r.localView(b), "events": b.Recent}
}
func leaderAccepted(ep *Endpoint, t *pc.Task) bool {
	return slices.Contains(ep.AcceptedLeaders, t.LeaderID)
}
