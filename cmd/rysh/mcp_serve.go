// SPDX-License-Identifier: Apache-2.0

package main

// `rysh mcp serve` — a stdio MCP server for rysh itself (design 067).
//
// The company server (company_mcp.go) gives agents INSIDE a session the
// company's data. This one faces the other way: an outside client — Claude
// Code, Cursor, Claude Desktop — reads and drives one running session.
//
//   - Read tools are always on: panes and their state, a pane's screen, the
//     board, the channels.
//   - Write tools exist only with --allow-write, and tools/list shows exactly
//     what is enabled. Every write asks the daemon's policy gate first
//     (MsgCLIMCPGate): a gated call fails, it is never auto-approved, and the
//     attempt is audited with the caller marked "mcp".
//   - Writes reuse the doors a human's commands use: the pane inbox `rysh send`
//     writes (never ansa), the silent board post, the ## command table for
//     starting panes, and the humanoid's own governance for channel sends.
//
// Never exposed: secret values, ##secret/##var contents, pane env, histories,
// pane meta beyond the attention state, the channel-state store.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/actors"
	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

const (
	mcpReadDefaultLines = 50
	mcpReadMaxLines     = 500
	mcpBoardDefault     = 20
	mcpBoardMax         = 200
	mcpWaitDefault      = 60 * time.Second
	mcpWaitMax          = 600 * time.Second
	// mcpScreenReads bounds concurrent VT reads in list_panes.
	mcpScreenReads = 4

	stateWorking  = "working"
	stateIdle     = "idle"
	stateNeedsYou = "needs-you"
	stateExited   = "exited"
)

// sessionBackend is what the tools need from a running session. liveSession
// talks NATS through internal/cli; tests use a fake.
type sessionBackend interface {
	Snapshot(ctx context.Context) (*domain.WorkspaceSnapshot, error)
	PaneOutput(ctx context.Context, paneID string) (*domain.PaneSnapshot, error)
	PaneScreen(ctx context.Context, paneID string) (*msg.MsgPaneVTReply, error)
	Board(ctx context.Context, boardID string, limit int) (*board.QueryReply, error)
	Humanoids(ctx context.Context) ([]msg.HumanoidInfo, error)
	WatchProcess(ctx context.Context, paneID string) (<-chan msg.MsgPaneProcess, func(), error)

	Gate(ctx context.Context, req *msg.MsgCLIMCPGate) (*msg.MsgCLIResponse, error)
	SendInput(ctx context.Context, paneID, text, mode string) error
	BoardPost(ctx context.Context, asPane, text, threadID, boardID string) (*msg.MsgCLIResponse, error)
	ChannelSend(ctx context.Context, humanoid string, req *msg.MsgHumanoidMCPSend) (*msg.MsgHumanoidMCPSendReply, error)
	RyshCommand(ctx context.Context, paneID, command string) (*msg.MsgCLIResponse, error)
}

// sessionTools answers tools/list and tools/call for one session.
type sessionTools struct {
	b          sessionBackend
	allowWrite bool
	// poll is how often wait_for_pane_state re-reads the pane.
	poll time.Duration
}

func newSessionTools(b sessionBackend, allowWrite bool) *sessionTools {
	return &sessionTools{b: b, allowWrite: allowWrite, poll: time.Second}
}

type mcpToolSpec = companyToolSpec

var sessionReadToolSpecs = []mcpToolSpec{
	{
		Name:        "list_panes",
		Description: "Every tab and pane in the rysh session: id, title, given-name, agent kind (claude, codex, shell, rysh-agent, humanoid, program), and state — working, idle or needs-you (waiting on a human: an approval, a question or an agent CLI's prompt). Optional tab filter (id or title).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"tab":{"type":"string","description":"only this tab (id or title)"}},"additionalProperties":false}`),
	},
	{
		Name:        "read_pane",
		Description: "Read a pane: its live screen when an interactive program (claude, vim, …) owns it, otherwise the last N lines of its output. Plain text, escape codes stripped. pane is an id, given-name or title.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"pane":{"type":"string"},"lines":{"type":"integer","minimum":1,"maximum":500,"description":"last N lines (default 50)"},"source":{"type":"string","enum":["auto","screen","output"],"description":"default auto"}},"required":["pane"],"additionalProperties":false}`),
	},
	{
		Name:        "read_board",
		Description: "The most recent threads on the session's agents board (who posted, text, replies). Optional board id and thread limit.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"board":{"type":"string","description":"board id; default the session board"},"limit":{"type":"integer","minimum":1,"maximum":200,"description":"at most this many threads (default 20)"}},"additionalProperties":false}`),
	},
	{
		Name:        "list_channels",
		Description: "The session's humanoids and their external channels (slack, email, telegram, …): connected or not, and governance — ai (sends on its own) or human (every send is a draft the owner approves).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	},
	{
		Name:        "wait_for_pane_state",
		Description: "Block until a pane reaches a state — idle, needs-you, or exited (its foreground program ended) — or the timeout passes. Returns the state reached and whether it was the one asked for. Use it instead of polling list_panes.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"pane":{"type":"string"},"state":{"type":"string","enum":["idle","needs-you","exited"]},"timeout_seconds":{"type":"number","minimum":0,"maximum":600,"description":"default 60"}},"required":["pane","state"],"additionalProperties":false}`),
	},
}

var sessionWriteToolSpecs = []mcpToolSpec{
	{
		Name:        "send_input",
		Description: "Type one line into a pane and submit it, exactly as `rysh send` does: a shell command (default) or a prompt. Passes the session's policy gate first; a gated input is refused, never auto-approved.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"pane":{"type":"string"},"input":{"type":"string"},"mode":{"type":"string","enum":["shell","prompt"]}},"required":["pane","input"],"additionalProperties":false}`),
	},
	{
		Name:        "board_post",
		Description: "Post to the agents board as a named pane (as_pane is required: a post names its poster). Optional thread to reply under.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"as_pane":{"type":"string"},"text":{"type":"string"},"thread":{"type":"string"},"board":{"type":"string"}},"required":["as_pane","text"],"additionalProperties":false}`),
	},
	{
		Name:        "channel_send",
		Description: "Send a message on a humanoid's channel. On a human-governed channel this only creates a draft that the owner approves in rysh; it never sends. The result says sent or drafted.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"humanoid":{"type":"string"},"channel":{"type":"string"},"to":{"type":"string","description":"recipient id (channel, chat or address)"},"text":{"type":"string"},"thread":{"type":"string"}},"required":["humanoid","channel","to","text"],"additionalProperties":false}`),
	},
	{
		Name:        "start_pane",
		Description: "Start a pane: a shell, an interactive claude with an optional first prompt, or a named rysh agent (.rysh/agents/<name>). near_pane places it next to that pane.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string","enum":["shell","claude","agent"]},"prompt":{"type":"string"},"agent":{"type":"string"},"near_pane":{"type":"string"}},"required":["kind"],"additionalProperties":false}`),
	},
}

func isWriteTool(name string) bool {
	for _, s := range sessionWriteToolSpecs {
		if s.Name == name {
			return true
		}
	}
	return false
}

// specs is exactly what is enabled.
func (t *sessionTools) specs() []mcpToolSpec {
	out := append([]mcpToolSpec(nil), sessionReadToolSpecs...)
	if t.allowWrite {
		out = append(out, sessionWriteToolSpecs...)
	}
	return out
}

type sessionToolArgs struct {
	Tab      string   `json:"tab"`
	Pane     string   `json:"pane"`
	Lines    int      `json:"lines"`
	Source   string   `json:"source"`
	Board    string   `json:"board"`
	Limit    int      `json:"limit"`
	State    string   `json:"state"`
	Timeout  *float64 `json:"timeout_seconds"`
	Input    string   `json:"input"`
	Mode     string   `json:"mode"`
	AsPane   string   `json:"as_pane"`
	Text     string   `json:"text"`
	Thread   string   `json:"thread"`
	Humanoid string   `json:"humanoid"`
	Channel  string   `json:"channel"`
	To       string   `json:"to"`
	Kind     string   `json:"kind"`
	Prompt   string   `json:"prompt"`
	Agent    string   `json:"agent"`
	NearPane string   `json:"near_pane"`
}

// call runs one tool. The text is JSON on success; isErr marks a failure the
// client should read as such.
func (t *sessionTools) call(ctx context.Context, name string, raw json.RawMessage) (string, bool) {
	known := false
	for _, s := range append(append([]mcpToolSpec(nil), sessionReadToolSpecs...), sessionWriteToolSpecs...) {
		if s.Name == name {
			known = true
		}
	}
	if !known {
		return fmt.Sprintf("unknown tool %q", name), true
	}
	if isWriteTool(name) && !t.allowWrite {
		return fmt.Sprintf("%s is a write tool and this server is read-only; start it with `%s mcp serve --allow-write`", name, progname.Name()), true
	}
	var a sessionToolArgs
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "invalid arguments: " + err.Error(), true
		}
	}
	var out any
	var err error
	switch name {
	case "list_panes":
		out, err = t.listPanes(ctx, a)
	case "read_pane":
		out, err = t.readPane(ctx, a)
	case "read_board":
		out, err = t.readBoard(ctx, a)
	case "list_channels":
		out, err = t.listChannels(ctx)
	case "wait_for_pane_state":
		out, err = t.waitForPaneState(ctx, a)
	case "send_input":
		out, err = t.sendInput(ctx, raw, a)
	case "board_post":
		out, err = t.boardPost(ctx, raw, a)
	case "channel_send":
		out, err = t.channelSend(ctx, raw, a)
	case "start_pane":
		out, err = t.startPane(ctx, raw, a)
	}
	if err != nil {
		return err.Error(), true
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "could not encode the answer: " + err.Error(), true
	}
	return string(b), false
}

// --- panes ---------------------------------------------------------------------

type mcpPane struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	GivenName   string `json:"given_name,omitempty"`
	Kind        string `json:"kind"`
	Program     string `json:"program,omitempty"`
	Mode        string `json:"mode,omitempty"`
	State       string `json:"state"`
	StateReason string `json:"state_reason,omitempty"`
	Active      bool   `json:"active,omitempty"`
}

type mcpTab struct {
	ID    string    `json:"id"`
	Title string    `json:"title"`
	Panes []mcpPane `json:"panes"`
}

// agentCLIs are the agent programs a pane can run whose idle screen is a
// prompt, not work: an idle claude is idle, an idle `make` is not a thing.
var agentCLIs = map[string]bool{
	"claude": true, "codex": true, "gemini": true, "cursor-agent": true,
	"opencode": true, "aider": true, "goose": true, "amp": true, "qwen": true,
}

func paneKind(p domain.PaneSnapshot) string {
	switch {
	case p.RegisteredHumanoid != "":
		return "humanoid"
	case agentCLIs[p.Program]:
		return p.Program
	case p.Program != "":
		return "program"
	case p.Mode == "prompt":
		return "rysh-agent"
	default:
		return "shell"
	}
}

// paneState classifies a pane from its snapshot and, for a pane with a
// foreground program, its screen. needs-you comes from the pane's own
// attention state (root-order-044 #3); working for an agent CLI is the footer
// test the fleet watcher uses; any other foreground program is working.
func paneState(p domain.PaneSnapshot, screen []string) (state, reason string) {
	if why, ok := domain.PaneNeedsYou(p.Meta); ok {
		return stateNeedsYou, why
	}
	if p.Program != "" {
		if !agentCLIs[p.Program] {
			return stateWorking, "running " + p.Program
		}
		if actors.ScreenShowsWork(screen) {
			return stateWorking, ""
		}
		return stateIdle, ""
	}
	switch strings.TrimSpace(p.Status) {
	case "", "idle", "done", "error":
		return stateIdle, ""
	default:
		return stateWorking, p.Status
	}
}

func allPanes(snap *domain.WorkspaceSnapshot) []domain.PaneSnapshot {
	var out []domain.PaneSnapshot
	for i := range snap.Tabs {
		for p := range domain.PanesInTab(&snap.Tabs[i]) {
			out = append(out, *p)
		}
	}
	return out
}

// resolvePane finds a pane by id, then a unique given-name, then a unique
// title. An ambiguous name is an error, never a guess.
func resolvePane(snap *domain.WorkspaceSnapshot, ref string) (domain.PaneSnapshot, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return domain.PaneSnapshot{}, errors.New("a pane (id, given-name or title) is required")
	}
	panes := allPanes(snap)
	for _, p := range panes {
		if p.ID == ref {
			return p, nil
		}
	}
	for _, field := range []func(domain.PaneSnapshot) string{
		func(p domain.PaneSnapshot) string { return p.GivenName },
		func(p domain.PaneSnapshot) string { return p.Title },
	} {
		var hits []domain.PaneSnapshot
		for _, p := range panes {
			if field(p) == ref {
				hits = append(hits, p)
			}
		}
		if len(hits) == 1 {
			return hits[0], nil
		}
		if len(hits) > 1 {
			return domain.PaneSnapshot{}, fmt.Errorf("%q names %d panes; use the pane id", ref, len(hits))
		}
	}
	return domain.PaneSnapshot{}, fmt.Errorf("pane not found: %s", ref)
}

func (t *sessionTools) screenRows(ctx context.Context, paneID string) []string {
	vt, err := t.b.PaneScreen(ctx, paneID)
	if err != nil || vt == nil {
		return nil
	}
	return vt.Screen
}

func (t *sessionTools) classify(ctx context.Context, p domain.PaneSnapshot) (string, string) {
	var screen []string
	if p.Program != "" && agentCLIs[p.Program] {
		if _, needs := domain.PaneNeedsYou(p.Meta); !needs {
			screen = t.screenRows(ctx, p.ID)
		}
	}
	return paneState(p, screen)
}

func (t *sessionTools) listPanes(ctx context.Context, a sessionToolArgs) (any, error) {
	snap, err := t.b.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	type slot struct {
		tab, pane int
		p         domain.PaneSnapshot
	}
	var tabs []mcpTab
	var slots []slot
	for i := range snap.Tabs {
		tab := &snap.Tabs[i]
		if a.Tab != "" && a.Tab != tab.ID && a.Tab != tab.Title {
			continue
		}
		mt := mcpTab{ID: tab.ID, Title: tab.Title, Panes: []mcpPane{}}
		for p := range domain.PanesInTab(tab) {
			mt.Panes = append(mt.Panes, mcpPane{
				ID: p.ID, Title: p.Title, GivenName: p.GivenName, Kind: paneKind(*p),
				Program: p.Program, Mode: p.Mode, Active: p.ID == snap.ActivePaneID,
			})
			slots = append(slots, slot{tab: len(tabs), pane: len(mt.Panes) - 1, p: *p})
		}
		tabs = append(tabs, mt)
	}
	if a.Tab != "" && len(tabs) == 0 {
		return nil, fmt.Errorf("tab not found: %s", a.Tab)
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, mcpScreenReads)
	for _, s := range slots {
		wg.Add(1)
		sem <- struct{}{}
		go func(s slot) {
			defer wg.Done()
			defer func() { <-sem }()
			st, why := t.classify(ctx, s.p)
			tabs[s.tab].Panes[s.pane].State = st
			tabs[s.tab].Panes[s.pane].StateReason = why
		}(s)
	}
	wg.Wait()
	return map[string]any{"tabs": tabs}, nil
}

func clampInt(v, def, max int) int {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

func plainLines(rows []string) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, strings.TrimRight(actors.StripANSI(r), " "))
	}
	return out
}

func lastLines(lines []string, n int) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func (t *sessionTools) readPane(ctx context.Context, a sessionToolArgs) (any, error) {
	snap, err := t.b.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	p, err := resolvePane(snap, a.Pane)
	if err != nil {
		return nil, err
	}
	n := clampInt(a.Lines, mcpReadDefaultLines, mcpReadMaxLines)
	source := a.Source
	if source == "" {
		source = "auto"
	}
	if source != "auto" && source != "screen" && source != "output" {
		return nil, fmt.Errorf("source must be auto, screen or output")
	}
	if source != "output" {
		vt, err := t.b.PaneScreen(ctx, p.ID)
		if err == nil && vt != nil && (vt.Interactive || source == "screen") {
			return map[string]any{"pane": p.ID, "source": "screen", "lines": lastLines(plainLines(vt.Screen), n)}, nil
		}
		if source == "screen" {
			return nil, fmt.Errorf("could not read the screen of pane %s: %v", p.ID, err)
		}
	}
	ps, err := t.b.PaneOutput(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	text := ps.Output
	if text == "" {
		text = ps.ShellOutput
	}
	return map[string]any{"pane": p.ID, "source": "output", "lines": lastLines(plainLines(strings.Split(text, "\n")), n)}, nil
}

// --- board and channels ------------------------------------------------------------

type mcpPost struct {
	Pane    string `json:"pane"`
	Persona string `json:"persona,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Text    string `json:"text"`
	TS      int64  `json:"ts"`
}

func toMCPPost(p *msg.MsgBoardPost) *mcpPost {
	if p == nil {
		return nil
	}
	return &mcpPost{Pane: p.PaneID, Persona: p.Persona, Kind: p.Kind, Text: p.Text, TS: p.TS}
}

func (t *sessionTools) readBoard(ctx context.Context, a sessionToolArgs) (any, error) {
	reply, err := t.b.Board(ctx, a.Board, clampInt(a.Limit, mcpBoardDefault, mcpBoardMax))
	if errors.Is(err, board.ErrNoRecorder) {
		return nil, errors.New("the board recorder did not answer — this is not an empty board")
	}
	if err != nil {
		return nil, err
	}
	type thread struct {
		Thread  string     `json:"thread"`
		Root    *mcpPost   `json:"root,omitempty"`
		Replies []*mcpPost `json:"replies,omitempty"`
	}
	threads := make([]thread, 0, len(reply.Threads))
	for _, th := range reply.Threads {
		out := thread{Thread: th.Key, Root: toMCPPost(th.Root)}
		for _, r := range th.Replies {
			out.Replies = append(out.Replies, toMCPPost(r))
		}
		threads = append(threads, out)
	}
	return map[string]any{"threads": threads, "withheld": reply.Withheld, "total_threads": reply.Stats.Threads}, nil
}

func (t *sessionTools) listChannels(ctx context.Context) (any, error) {
	hs, err := t.b.Humanoids(ctx)
	if err != nil {
		return nil, err
	}
	type channel struct {
		Type       string `json:"type"`
		Connected  bool   `json:"connected"`
		Governance string `json:"governance"`
		Details    string `json:"details,omitempty"`
		Error      string `json:"error,omitempty"`
	}
	type humanoid struct {
		Name     string    `json:"name"`
		Active   bool      `json:"active"`
		Channels []channel `json:"channels"`
	}
	out := make([]humanoid, 0, len(hs))
	for _, h := range hs {
		hh := humanoid{Name: h.Name, Active: h.Active, Channels: []channel{}}
		for _, c := range h.Channels {
			gov := c.Governance
			if gov == "" {
				gov = "ai"
			}
			hh.Channels = append(hh.Channels, channel{Type: c.Type, Connected: c.Connected, Governance: gov, Details: c.Details, Error: c.Error})
		}
		sort.Slice(hh.Channels, func(i, j int) bool { return hh.Channels[i].Type < hh.Channels[j].Type })
		out = append(out, hh)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return map[string]any{"humanoids": out}, nil
}

// --- wait ------------------------------------------------------------------------------

func (t *sessionTools) waitForPaneState(ctx context.Context, a sessionToolArgs) (any, error) {
	want := a.State
	if want != stateIdle && want != stateNeedsYou && want != stateExited {
		return nil, fmt.Errorf("state must be idle, needs-you or exited")
	}
	timeout := mcpWaitDefault
	if a.Timeout != nil {
		timeout = time.Duration(*a.Timeout * float64(time.Second))
		if timeout < 0 {
			timeout = 0
		}
		if timeout > mcpWaitMax {
			timeout = mcpWaitMax
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()

	snap, err := t.b.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	p, err := resolvePane(snap, a.Pane)
	if err != nil {
		return nil, err
	}
	result := func(state, reason string, reached bool) any {
		r := map[string]any{"pane": p.ID, "state": state, "reached": reached, "waited_ms": time.Since(start).Milliseconds()}
		if reason != "" {
			r["reason"] = reason
		}
		return r
	}

	var exits <-chan msg.MsgPaneProcess
	if want == stateExited {
		if p.Program == "" {
			return result(stateExited, "no foreground program", true), nil
		}
		ch, stop, err := t.b.WatchProcess(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		defer stop()
		exits = ch
	}

	program := p.Program
	tick := time.NewTicker(t.poll)
	defer tick.Stop()
	for {
		// Check now, then on every tick or event.
		snap, err := t.b.Snapshot(ctx)
		if err == nil {
			cur, rerr := resolvePane(snap, p.ID)
			if rerr != nil {
				return result(stateExited, "pane closed", want == stateExited), nil
			}
			if want == stateExited && program != "" && cur.Program == "" {
				return result(stateExited, "", true), nil
			}
			st, why := t.classify(ctx, cur)
			if st == want {
				return result(st, why, true), nil
			}
			if ctx.Err() != nil {
				return result(st, why, false), nil
			}
		}
		select {
		case <-ctx.Done():
			snap, err := t.b.Snapshot(context.Background())
			if err == nil {
				if cur, rerr := resolvePane(snap, p.ID); rerr == nil {
					st, why := t.classify(context.Background(), cur)
					return result(st, why, st == want), nil
				}
			}
			return result("unknown", "timed out", false), nil
		case ev := <-exits:
			if ev.Event == "exit" {
				return result(stateExited, "", true), nil
			}
		case <-tick.C:
		}
	}
}

// --- writes ----------------------------------------------------------------------------

// gate asks the daemon before any write. A refusal, or no answer at all, stops
// the call: the daemon is the one that holds the policy.
func (t *sessionTools) gate(ctx context.Context, tool string, raw json.RawMessage, target, shell string) error {
	resp, err := t.b.Gate(ctx, &msg.MsgCLIMCPGate{Tool: tool, Args: raw, Target: target, ShellCommand: shell, Caller: "mcp"})
	if err != nil {
		return fmt.Errorf("the session did not answer its policy gate (%v); refusing — a daemon older than design 067 cannot take MCP writes", err)
	}
	if !resp.OK {
		if resp.Error == "" {
			return errors.New("refused by the session's policy gate")
		}
		return errors.New(resp.Error)
	}
	return nil
}

func (t *sessionTools) sendInput(ctx context.Context, raw json.RawMessage, a sessionToolArgs) (any, error) {
	if strings.TrimSpace(a.Input) == "" {
		return nil, errors.New("input is required")
	}
	mode := a.Mode
	if mode == "" {
		mode = "shell"
	}
	if mode != "shell" && mode != "prompt" {
		return nil, errors.New("mode must be shell or prompt")
	}
	snap, err := t.b.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	p, err := resolvePane(snap, a.Pane)
	if err != nil {
		return nil, err
	}
	shell := ""
	if mode == "shell" {
		shell = a.Input
	}
	if err := t.gate(ctx, "send_input", raw, p.ID, shell); err != nil {
		return nil, err
	}
	if err := t.b.SendInput(ctx, p.ID, a.Input, mode); err != nil {
		return nil, err
	}
	return map[string]any{"sent": true, "pane": p.ID, "mode": mode}, nil
}

func (t *sessionTools) boardPost(ctx context.Context, raw json.RawMessage, a sessionToolArgs) (any, error) {
	if strings.TrimSpace(a.AsPane) == "" {
		return nil, errors.New("as_pane is required: a board post names its poster")
	}
	if strings.TrimSpace(a.Text) == "" {
		return nil, errors.New("text is required")
	}
	if err := t.gate(ctx, "board_post", raw, a.AsPane, ""); err != nil {
		return nil, err
	}
	resp, err := t.b.BoardPost(ctx, a.AsPane, a.Text, a.Thread, a.Board)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, errors.New(resp.Error)
	}
	return map[string]any{"posted": true, "thread": resp.ID, "output": strings.TrimSpace(resp.Output)}, nil
}

func (t *sessionTools) channelSend(ctx context.Context, raw json.RawMessage, a sessionToolArgs) (any, error) {
	for k, v := range map[string]string{"humanoid": a.Humanoid, "channel": a.Channel, "to": a.To, "text": a.Text} {
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("%s is required", k)
		}
	}
	if err := t.gate(ctx, "channel_send", raw, a.Humanoid+"/"+a.Channel, ""); err != nil {
		return nil, err
	}
	reply, err := t.b.ChannelSend(ctx, a.Humanoid, &msg.MsgHumanoidMCPSend{
		ChannelType: a.Channel, RecipientID: a.To, Content: a.Text, ThreadID: a.Thread,
	})
	if err != nil {
		return nil, err
	}
	if reply.Error != "" {
		return nil, errors.New(reply.Error)
	}
	out := map[string]any{"outcome": reply.Outcome, "governance": reply.Governance}
	if reply.Outcome == "drafted" {
		out["queued"] = reply.Queued
		out["note"] = "human-governed channel: nothing was sent; the owner approves the draft in the humanoid's pane"
	}
	return out, nil
}

var agentNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// startPane builds one ## command and runs it through the same table `rysh
// exec` reaches. Free text is refused when it could change what that command
// line means: no newlines, no "##".
func (t *sessionTools) startPane(ctx context.Context, raw json.RawMessage, a sessionToolArgs) (any, error) {
	prompt := strings.TrimSpace(a.Prompt)
	if strings.ContainsAny(prompt, "\r\n") || strings.Contains(prompt, "##") {
		return nil, errors.New("prompt must be one line and must not contain ##")
	}
	var command string
	switch a.Kind {
	case "shell":
		command = "pane new"
	case "claude":
		command = strings.TrimSpace("pane new --claude " + prompt)
	case "agent":
		if !agentNameRe.MatchString(a.Agent) {
			return nil, errors.New("agent must be a name from .rysh/agents (letters, digits, . _ -)")
		}
		command = strings.TrimSpace("agent spawn " + a.Agent + " " + prompt)
	default:
		return nil, errors.New("kind must be shell, claude or agent")
	}
	near := ""
	if strings.TrimSpace(a.NearPane) != "" {
		snap, err := t.b.Snapshot(ctx)
		if err != nil {
			return nil, err
		}
		p, err := resolvePane(snap, a.NearPane)
		if err != nil {
			return nil, err
		}
		near = p.ID
	}
	if err := t.gate(ctx, "start_pane", raw, near, ""); err != nil {
		return nil, err
	}
	resp, err := t.b.RyshCommand(ctx, near, command)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		msgText := resp.Error
		if msgText == "" {
			msgText = strings.TrimSpace(resp.Output)
		}
		return nil, errors.New(msgText)
	}
	return map[string]any{"started": true, "id": resp.ID, "output": strings.TrimSpace(resp.Output)}, nil
}

// --- live backend ------------------------------------------------------------------

type liveSession struct {
	store *session.Store
	name  string
}

func (s liveSession) Snapshot(context.Context) (*domain.WorkspaceSnapshot, error) {
	return cli.Snapshot(s.store, s.name)
}
func (s liveSession) PaneOutput(_ context.Context, id string) (*domain.PaneSnapshot, error) {
	return cli.PaneOutput(s.store, s.name, id)
}
func (s liveSession) PaneScreen(_ context.Context, id string) (*msg.MsgPaneVTReply, error) {
	return cli.PaneScreen(s.store, s.name, id)
}
func (s liveSession) Board(_ context.Context, boardID string, limit int) (*board.QueryReply, error) {
	return cli.BoardTail(s.store, s.name, boardID, board.Query{Limit: limit})
}
func (s liveSession) Humanoids(context.Context) ([]msg.HumanoidInfo, error) {
	return cli.Humanoids(s.store, s.name)
}
func (s liveSession) WatchProcess(_ context.Context, id string) (<-chan msg.MsgPaneProcess, func(), error) {
	return cli.WatchPaneProcess(s.store, s.name, id)
}
func (s liveSession) Gate(_ context.Context, req *msg.MsgCLIMCPGate) (*msg.MsgCLIResponse, error) {
	return cli.MCPGate(s.store, s.name, req)
}
func (s liveSession) SendInput(_ context.Context, id, text, mode string) error {
	return cli.SendPaneInput(s.store, s.name, id, text, mode)
}
func (s liveSession) BoardPost(_ context.Context, as, text, thread, boardID string) (*msg.MsgCLIResponse, error) {
	return cli.BoardPost(s.store, s.name, as, "", thread, boardID, text)
}
func (s liveSession) ChannelSend(_ context.Context, humanoid string, req *msg.MsgHumanoidMCPSend) (*msg.MsgHumanoidMCPSendReply, error) {
	return cli.HumanoidMCPSend(s.store, s.name, humanoid, req)
}
func (s liveSession) RyshCommand(_ context.Context, id, command string) (*msg.MsgCLIResponse, error) {
	return cli.RyshCommandResult(s.store, s.name, "", id, command)
}

// runMCPCmd implements `rysh mcp serve [--session <name>] [--allow-write]`.
// stdout is the protocol: nothing else may be printed there.
func runMCPCmd(cfg config.Config, args []string) error {
	rest := args[1:]
	usage := errors.New(progname.Rewrite("usage: rysh mcp serve [--session <name>] [--allow-write]"))
	if len(rest) == 0 || rest[0] != "serve" {
		return usage
	}
	rest = rest[1:]
	rest, sess := extractStringFlag(rest, "--session")
	rest, allowWrite := extractBoolFlag(rest, "--allow-write")
	if len(rest) != 0 {
		return usage
	}
	if sess == "" {
		sess = cfg.SessionName
	}
	store, err := session.NewStore(cfg)
	if err != nil {
		return err
	}
	tools := newSessionTools(liveSession{store: store, name: sess}, allowWrite)
	return serveStdioMCP(os.Stdin, os.Stdout, stdioMCPServer{
		name:        "rysh",
		tools:       func() any { return tools.specs() },
		call:        tools.call,
		callTimeout: mcpWaitMax + 30*time.Second,
	})
}
