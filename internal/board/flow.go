// SPDX-License-Identifier: Apache-2.0

package board

// The flow model (design 052): a board's traffic as a graph and a sequence —
// who orders whom, what is open, what bounced, how long acks take, and in
// which order it happened.
//
// A VIEW, not a channel: pure over (board id, threads, panes), no NATS, no
// clock. The TUI's board pane (view f), `rysh board tail --view flow` and the
// desktop app's flow.ts port all build the same model, and the golden fixture
// in testdata/flow-*.json (copied from new_roadmap/designs/052-fixtures/) is
// what keeps the two implementations byte for byte equal. A model change
// updates that fixture first.

import (
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Event types (design 052 §3.2).
const (
	FlowPrompt      = "prompt"
	FlowAck         = "ack"
	FlowUndelivered = "undelivered"
	FlowReply       = "reply"
	FlowStatus      = "status"
	FlowChain       = "chain" // an edge type only
	// FlowBlocks is an edge type only (design 054): a blocked member → each
	// member it is blocked by, from the fleet status ledger mirrored on pane
	// meta. A dependency, not traffic: it has no events and no count beyond 1.
	FlowBlocks = "blocks"
)

// Node kinds.
const (
	FlowNodeBoard    = "board"
	FlowNodePane     = "pane"
	FlowNodeCourier  = "courier"
	FlowNodeListener = "listener"
	// FlowNodeFleet is the fleet actor (design 054): it posts check-ins and
	// alerts as the board identity "fleet".
	FlowNodeFleet = "fleet"
)

// Pane meta keys the fleet actor mirrors its status ledger onto (design 054;
// written by internal/actors/fleet_watch.go, read here).
const (
	flowMetaWork      = "fleet.work"
	flowMetaTask      = "fleet.task"
	flowMetaBlockedBy = "fleet.blocked_by"
	flowMetaActivity  = "fleet.activity"
	flowMetaHealth    = "fleet.health"

	// The goal executor's verdict (design 055), mirrored by it.
	flowMetaGoalState = "fleet.goal_state"
	flowMetaGoalPct   = "fleet.goal_pct"
	flowMetaGoalEta   = "fleet.goal_eta"
)

// flowUndeliveredKind is the post kind the courier and a pane's listener file
// when a cmd could not be delivered (internal/courier.UndeliveredKind; not
// imported, because the courier imports this package).
const flowUndeliveredKind = "undelivered"

// FlowEventCap is how many events the model keeps: the newest ones.
const FlowEventCap = 200

// flowSummaryRunes is the longest summary kept whole.
const flowSummaryRunes = 80

// FlowPane is one session pane as the model needs it: id, display name, meta.
type FlowPane struct {
	ID   string            `json:"id"`
	Name string            `json:"name"`
	Meta map[string]string `json:"meta"`
}

// Flow is the model (design 052 §3.7).
type Flow struct {
	Board  string      `json:"board"`
	Nodes  []FlowNode  `json:"nodes"`
	Edges  []FlowEdge  `json:"edges"`
	Events []FlowEvent `json:"events"`
	Stats  FlowStats   `json:"stats"`
}

// FlowNode is a board, a pane, the courier or a listener.
type FlowNode struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Role   string `json:"role,omitempty"`
	Parent string `json:"parent,omitempty"`
	Comm   string `json:"comm,omitempty"`
	Posts  int    `json:"posts"`

	// The fleet status ledger (design 054), from pane meta; omitted for a
	// pane in no watched fleet. Work is declared, Activity observed, Health
	// derived; BlockedBy are pane ids (or "human" / "ext:<what>").
	Work      string   `json:"work,omitempty"`
	Task      string   `json:"task,omitempty"`
	Activity  string   `json:"activity,omitempty"`
	Health    string   `json:"health,omitempty"`
	BlockedBy []string `json:"blocked_by,omitempty"`

	// The goal executor's verdict (design 055), from pane meta: the primary
	// condition (progressing, blocker, stressed, strayed, …), declared
	// progress and ETA (unix millis). Omitted with no executor, so a board
	// without one renders byte-identically to before.
	GoalState string `json:"goal_state,omitempty"`
	GoalPct   *int   `json:"goal_pct,omitempty"`
	GoalEta   int64  `json:"goal_eta,omitempty"`
}

// FlowEdge aggregates one (type, from, to). Answered/Undelivered/Open and
// LatencyMS are meaningful on prompt edges only, but answered/undelivered/open
// are always present on those (the fixture says so), hence pointers.
type FlowEdge struct {
	Type        string `json:"type"`
	From        string `json:"from"`
	To          string `json:"to"`
	Count       int    `json:"count"`
	Answered    *int   `json:"answered,omitempty"`
	Undelivered *int   `json:"undelivered,omitempty"`
	Open        *int   `json:"open,omitempty"`
	LatencyMS   *int64 `json:"latency_ms,omitempty"`
	LastTS      int64  `json:"last_ts,omitempty"`
}

// FlowEvent is one classified post.
type FlowEvent struct {
	TS      int64  `json:"ts"`
	Type    string `json:"type"`
	From    string `json:"from"`
	To      string `json:"to"`
	Kind    string `json:"kind"`
	Thread  string `json:"thread"`
	CmdID   string `json:"cmd_id,omitempty"`
	Summary string `json:"summary"`
}

// FlowStats are the header counts.
type FlowStats struct {
	Panes       int `json:"panes"`
	Prompts     int `json:"prompts"`
	Answered    int `json:"answered"`
	Undelivered int `json:"undelivered"`
	Open        int `json:"open"`
	Status      int `json:"status"`
	// Blocked is how many pane nodes declare blocked (design 054).
	Blocked int `json:"blocked,omitempty"`
}

// FlowBoardNodeID is the node id of board b.
func FlowBoardNodeID(b string) string { return "board:" + b }

// NormaliseComm maps a fleet.comm meta value to queue | grid | input-stream:
// polling-stream is grid's old name (design 051).
func NormaliseComm(c string) string {
	c = strings.TrimSpace(c)
	if c == "polling-stream" {
		return "grid"
	}
	return c
}

// flowPost is a post with the thread it was filed on.
type flowPost struct {
	p      *msg.MsgBoardPost
	thread *Thread
	key    string // the thread's id as an event names it
}

// BuildFlow builds the flow model of board from its threads and the session's
// panes (design 052 §3).
func BuildFlow(boardID string, threads []Thread, panes []FlowPane) Flow {
	boardNode := FlowBoardNodeID(boardID)

	var posts []flowPost
	for i := range threads {
		t := &threads[i]
		all := make([]*msg.MsgBoardPost, 0, 1+len(t.Replies))
		if t.Root != nil {
			all = append(all, t.Root)
		}
		all = append(all, t.Replies...)
		for _, p := range all {
			if p == nil {
				continue
			}
			key := p.ThreadID // §3.8: the post's thread_id, else its thread's key
			if key == "" {
				key = t.Key
			}
			posts = append(posts, flowPost{p: p, thread: t, key: key})
		}
	}
	sort.SliceStable(posts, func(i, j int) bool { return posts[i].p.TS < posts[j].p.TS })

	// --- classify (§3.2) ---
	events := make([]FlowEvent, 0, len(posts))
	for _, fp := range posts {
		p := fp.p
		ev := FlowEvent{TS: p.TS, From: p.PaneID, Kind: p.Kind, Thread: fp.key, Summary: flowSummary(p.Text)}
		// A cmd with no recipient is malformed, not a prompt (§3.8): it is
		// status, and the thread it roots has no cmd for replies to name.
		var cmd *msg.MsgBoardPost
		if r := fp.thread.Root; r != nil && r.Kind == msg.BoardKindCmd && r.ToPaneID != "" {
			cmd = r
		}
		switch {
		case p.Kind == msg.BoardKindCmd && p.ToPaneID != "":
			ev.Type, ev.To, ev.CmdID = FlowPrompt, p.ToPaneID, p.CmdID
		case cmd == nil:
			ev.Type, ev.To = FlowStatus, boardNode
		case p.Kind == msg.BoardKindAck:
			ev.Type, ev.To, ev.CmdID = FlowAck, cmd.PaneID, cmd.CmdID
		case p.Kind == flowUndeliveredKind:
			ev.Type, ev.To, ev.CmdID = FlowUndelivered, cmd.ToPaneID, cmd.CmdID
		default:
			ev.Type, ev.CmdID = FlowReply, cmd.CmdID
			ev.To = cmd.ToPaneID
			if p.PaneID == cmd.ToPaneID {
				ev.To = cmd.PaneID
			}
		}
		events = append(events, ev)
	}

	// --- prompts (§3.3) ---
	type promptState struct {
		answered, undelivered bool
		latency               int64
		hasLatency            bool
	}
	byThread := map[*Thread][]*msg.MsgBoardPost{}
	acks := map[string][]*msg.MsgBoardPost{} // by cmd id
	for _, fp := range posts {
		byThread[fp.thread] = append(byThread[fp.thread], fp.p)
		if fp.p.Kind == msg.BoardKindAck && fp.p.CmdID != "" {
			acks[fp.p.CmdID] = append(acks[fp.p.CmdID], fp.p)
		}
	}
	// A cmd is (sender, cmd_id): a retried send is one prompt, the first
	// seen; the retry is still a prompt event but counts nowhere (§3.3).
	type cmdKey struct{ from, id string }
	firstSeen := map[cmdKey]bool{}
	isPrompt := make([]bool, len(posts))
	states := make([]promptState, len(posts))
	for i, fp := range posts {
		c := fp.p
		if c.Kind != msg.BoardKindCmd || c.ToPaneID == "" {
			continue
		}
		if c.CmdID != "" {
			k := cmdKey{c.PaneID, c.CmdID}
			if firstSeen[k] {
				continue
			}
			firstSeen[k] = true
		}
		isPrompt[i] = true
		var st promptState
		var first int64
		answer := func(r *msg.MsgBoardPost) {
			if !st.answered || r.TS < first {
				st.answered, first = true, r.TS
			}
		}
		// The board inbox rule (inbox.go): the recipient posted anything on
		// the cmd's thread, or acked it by id (filed on its thread or none).
		for _, r := range byThread[fp.thread] {
			if r == c || r.Kind == msg.BoardKindCmd {
				continue
			}
			if r.Kind == flowUndeliveredKind {
				st.undelivered = true
			}
			if c.ToPaneID != "" && r.PaneID == c.ToPaneID {
				answer(r)
			}
		}
		for _, r := range acks[c.CmdID] {
			if c.ToPaneID != "" && r.PaneID == c.ToPaneID && (r.ThreadID == "" || r.ThreadID == c.ThreadID) {
				answer(r)
			}
		}
		if st.answered && first-c.TS >= 0 {
			st.latency, st.hasLatency = first-c.TS, true
		}
		states[i] = st
	}

	// --- nodes (§3.4) ---
	paneByID := make(map[string]FlowPane, len(panes))
	for _, p := range panes {
		paneByID[p.ID] = p
	}
	nodeIdx := map[string]int{}
	var nodes []FlowNode
	addNode := func(id string) {
		if id == "" {
			return
		}
		if _, ok := nodeIdx[id]; ok {
			return
		}
		nodeIdx[id] = len(nodes)
		nodes = append(nodes, FlowNode{ID: id})
	}
	addNode(boardNode)
	for _, ev := range events {
		addNode(ev.From)
		addNode(ev.To)
	}
	var extra []string
	for _, p := range panes {
		if _, seen := nodeIdx[p.ID]; !seen && p.ID != "" && p.Meta["board.id"] == boardID {
			extra = append(extra, p.ID)
		}
	}
	sort.Strings(extra)
	for _, id := range extra {
		addNode(id)
	}

	persona := map[string]string{} // newest post's persona per poster
	for _, fp := range posts {
		if s := SanitiseInline(fp.p.Persona); strings.TrimSpace(s) != "" {
			persona[fp.p.PaneID] = s
		}
		if i, ok := nodeIdx[fp.p.PaneID]; ok {
			nodes[i].Posts++
		}
	}
	for i := range nodes {
		n := &nodes[i]
		switch {
		case n.ID == boardNode:
			n.Kind, n.Label = FlowNodeBoard, boardID
			continue
		case n.ID == FlowNodeCourier:
			n.Kind = FlowNodeCourier
		case n.ID == FlowNodeListener:
			n.Kind = FlowNodeListener
		case n.ID == FlowNodeFleet:
			n.Kind = FlowNodeFleet
		default:
			n.Kind = FlowNodePane
		}
		pane, known := paneByID[n.ID]
		switch {
		case persona[n.ID] != "":
			n.Label = persona[n.ID]
		case known && pane.Name != "":
			n.Label = pane.Name
		default:
			n.Label = msg.BoardPersona("", "", n.ID)
		}
		if known && n.Kind == FlowNodePane {
			n.Role = pane.Meta["fleet.role"]
			n.Parent = pane.Meta["fleet.parent"]
			n.Comm = NormaliseComm(pane.Meta["fleet.comm"])
			n.Work = pane.Meta[flowMetaWork]
			n.Task = SanitiseInline(pane.Meta[flowMetaTask])
			n.Activity = pane.Meta[flowMetaActivity]
			n.Health = pane.Meta[flowMetaHealth]
			for _, b := range strings.Split(pane.Meta[flowMetaBlockedBy], ",") {
				if b = strings.TrimSpace(b); b != "" {
					n.BlockedBy = append(n.BlockedBy, b)
				}
			}
			n.GoalState = SanitiseInline(pane.Meta[flowMetaGoalState])
			if v, err := strconv.Atoi(strings.TrimSpace(pane.Meta[flowMetaGoalPct])); err == nil && v >= 0 && v <= 100 {
				n.GoalPct = &v
			}
			if v, err := strconv.ParseInt(strings.TrimSpace(pane.Meta[flowMetaGoalEta]), 10, 64); err == nil && v > 0 {
				n.GoalEta = v
			}
		}
	}

	// --- edges (§3.5) ---
	type edgeKey struct{ typ, from, to string }
	edgeAt := map[edgeKey]int{}
	var edges []FlowEdge
	latencies := map[int][]int64{}
	edge := func(typ, from, to string) *FlowEdge {
		k := edgeKey{typ, from, to}
		if i, ok := edgeAt[k]; ok {
			return &edges[i]
		}
		edgeAt[k] = len(edges)
		e := FlowEdge{Type: typ, From: from, To: to}
		if typ == FlowPrompt {
			e.Answered, e.Undelivered, e.Open = new(int), new(int), new(int)
		}
		edges = append(edges, e)
		return &edges[len(edges)-1]
	}
	for _, n := range nodes {
		if n.Kind != FlowNodePane || n.Parent == "" {
			continue
		}
		if _, ok := nodeIdx[n.Parent]; ok {
			edge(FlowChain, n.Parent, n.ID).Count = 1
		}
	}
	for _, n := range nodes {
		if n.Kind != FlowNodePane {
			continue
		}
		for _, b := range n.BlockedBy {
			if _, ok := nodeIdx[b]; ok && b != n.ID {
				edge(FlowBlocks, n.ID, b).Count = 1
			}
		}
	}

	var stats FlowStats
	for i, ev := range events {
		switch ev.Type {
		case FlowPrompt:
			if !isPrompt[i] {
				break // a retry
			}
			e := edge(FlowPrompt, ev.From, ev.To)
			e.Count++
			e.LastTS = max(e.LastTS, ev.TS)
			st := states[i]
			stats.Prompts++
			switch {
			case st.answered:
				*e.Answered++
				stats.Answered++
				if st.hasLatency {
					k := edgeAt[edgeKey{FlowPrompt, ev.From, ev.To}]
					latencies[k] = append(latencies[k], st.latency)
				}
			case st.undelivered:
				*e.Undelivered++
				stats.Undelivered++
			default:
				*e.Open++
				stats.Open++
			}
		case FlowUndelivered, FlowStatus:
			if ev.To == "" {
				break
			}
			if ev.Type == FlowStatus {
				stats.Status++
			}
			e := edge(ev.Type, ev.From, ev.To)
			e.Count++
			e.LastTS = max(e.LastTS, ev.TS)
		}
	}
	for i, ls := range latencies {
		m := medianFloor(ls)
		edges[i].LatencyMS = &m
	}
	typeRank := map[string]int{FlowChain: 0, FlowBlocks: 1, FlowPrompt: 2, FlowUndelivered: 3, FlowStatus: 4}
	sort.SliceStable(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if typeRank[a.Type] != typeRank[b.Type] {
			return typeRank[a.Type] < typeRank[b.Type]
		}
		if nodeIdx[a.From] != nodeIdx[b.From] {
			return nodeIdx[a.From] < nodeIdx[b.From]
		}
		return nodeIdx[a.To] < nodeIdx[b.To]
	})

	for _, n := range nodes {
		if n.Kind == FlowNodePane {
			stats.Panes++
			if n.Work == "blocked" {
				stats.Blocked++
			}
		}
	}

	if len(events) > FlowEventCap {
		events = events[len(events)-FlowEventCap:]
	}
	if edges == nil {
		edges = []FlowEdge{}
	}
	return Flow{Board: boardID, Nodes: nodes, Edges: edges, Events: events, Stats: stats}
}

// medianFloor is the median of xs; for an even count, the floored mean of the
// two middle values.
func medianFloor(xs []int64) int64 {
	s := append([]int64(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// flowSummary is the first non-empty line of the sanitised body, trimmed, and
// clipped to 79 runes + "…" when over 80.
func flowSummary(text string) string {
	for _, line := range strings.Split(SanitiseBody(text), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if utf8.RuneCountInString(line) > flowSummaryRunes {
			rs := []rune(line)
			return string(rs[:flowSummaryRunes-1]) + "…"
		}
		return line
	}
	return ""
}
