// SPDX-License-Identifier: Apache-2.0

package tui

// The board pane's flow view, f (design 052 §4): the board's traffic as a
// chain tree with each agent's prompt edges, over a sequence diagram — one
// lifeline per pane, one row per event, newest at the bottom.
//
// Pure rendering of board.BuildFlow, rebuilt from the store every frame, so it
// is live exactly as the list views are. Like those, the rows are plain text:
// the only text from agents is the event summary, already sanitised by the
// model, and the labels, sanitised again here.

import (
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
)

// boardFlowFrame renders the flow view for a pane window of maxLines rows.
// fixed rows (the tree and the lifeline header) stay put; scroll rows (the
// events) scroll like the other views' rows, pinned to the newest by default.
func (m Model) boardFlowFrame(boardID string, width, maxLines int) (fixed, scroll []string) {
	f := m.boardFlow(boardID)
	tree := boardFlowTree(f)
	// The tree gets at most half the window: the sequence is the live part.
	if limit := max(1, maxLines/2); len(tree) > limit {
		more := len(tree) - (limit - 1)
		tree = append(tree[:limit-1:limit-1], fmt.Sprintf("  … %d more row(s) — widen the pane or see `rysh board tail --view flow`", more))
	}
	fixed = append(fixed, tree...)

	// The dependency tree (design 054 §8a): who waits on whom, rooted at the
	// bottlenecks. Only when someone is blocked, and capped so the live
	// sequence below keeps most of the window.
	if deps := board.RenderFlowDepsFit(board.BuildFlowDeps(f), max(3, maxLines/3)); len(deps) > 0 {
		fixed = append(fixed, "")
		for _, l := range deps {
			fixed = append(fixed, "  "+sanitiseBoardInline(l))
		}
	}

	if len(f.Events) == 0 {
		fixed = append(fixed, "")
		if notice := m.recorderNotice(); notice != "" {
			return append(fixed, notice, "  An empty board here does NOT mean nobody posted."), nil
		}
		return append(fixed, "  No board traffic yet — the sequence fills as agents post, send and ack."), nil
	}
	cols := boardFlowColumns(f)
	head, rows := boardFlowSequence(f, cols, width)
	fixed = append(fixed, "", head)
	return fixed, rows
}

// boardFlow builds the model from what the TUI already holds: the board store
// and the snapshot's panes.
func (m Model) boardFlow(boardID string) board.Flow {
	return board.BuildFlow(boardID, m.boardStore(boardID).Threads(), board.FlowPanesFromSnapshot(&m.snapshot))
}

// boardFlowHeader is the view part of the header line in flow view.
func boardFlowHeader(f board.Flow) string {
	s := f.Stats
	blocked := ""
	if s.Blocked > 0 {
		blocked = fmt.Sprintf(" · %d blocked", s.Blocked)
	}
	return fmt.Sprintf("showing flow · %s%s · %s (%d answered · %d undelivered · %d open) · %d status · keys s p a f t",
		plural(s.Panes, "pane"), blocked, plural(s.Prompts, "prompt"), s.Answered, s.Undelivered, s.Open, s.Status)
}

// boardFlowStatusBadge renders a node's fleet status ledger (design 054):
// declared work, then what the fleet actor observed or derived when it says
// more than the work does — "{progress·stalled}", "{done·available}",
// "{blocked}", "{progress·working}". Empty for a pane in no watched fleet.
func boardFlowStatusBadge(n board.FlowNode) string {
	if n.Work == "" && n.Health == "" {
		return ""
	}
	parts := []string{}
	if n.Work != "" {
		parts = append(parts, n.Work)
	}
	switch {
	case n.Health != "" && n.Health != "ok":
		parts = append(parts, strings.ToUpper(n.Health))
	case n.Activity != "" && n.Activity != "unknown":
		parts = append(parts, n.Activity)
	}
	return "{" + sanitiseBoardInline(strings.Join(parts, "·")) + "}"
}

// boardFlowCommBadge renders a node's comm mode.
func boardFlowCommBadge(comm string) string {
	switch comm {
	case "":
		return ""
	case "input-stream":
		return "[typed]"
	}
	return "[" + comm + "]"
}

// boardFlowLatency renders an ack latency: 850ms, 4.1s, 3m05s.
func boardFlowLatency(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	return fmt.Sprintf("%dm%02ds", ms/60_000, (ms%60_000)/1000)
}

// boardFlowNodeIndex maps node id → index.
func boardFlowNodeIndex(f board.Flow) map[string]int {
	idx := make(map[string]int, len(f.Nodes))
	for i, n := range f.Nodes {
		idx[n.ID] = i
	}
	return idx
}

func boardFlowLabel(f board.Flow, idx map[string]int, id string) string {
	if i, ok := idx[id]; ok {
		return sanitiseBoardInline(f.Nodes[i].Label)
	}
	return sanitiseBoardInline(id)
}

// boardFlowTreeOrder is the pane nodes in chain order: each root (a pane
// whose parent is not a node), then its children depth first, node order
// breaking ties. depth runs parallel.
func boardFlowTreeOrder(f board.Flow) (order []int, depth []int) {
	idx := boardFlowNodeIndex(f)
	children := map[int][]int{}
	hasParent := map[int]bool{}
	for _, e := range f.Edges {
		if e.Type != board.FlowChain {
			continue
		}
		p, c := idx[e.From], idx[e.To]
		children[p] = append(children[p], c)
		hasParent[c] = true
	}
	seen := map[int]bool{}
	var walk func(i, d int)
	walk = func(i, d int) {
		if seen[i] {
			return // a meta cycle must not hang the render
		}
		seen[i] = true
		order, depth = append(order, i), append(depth, d)
		for _, c := range children[i] {
			walk(c, d+1)
		}
	}
	for i, n := range f.Nodes {
		if n.Kind == board.FlowNodePane && !hasParent[i] {
			walk(i, 0)
		}
	}
	for i, n := range f.Nodes { // anything a cycle left out
		if n.Kind == board.FlowNodePane && !seen[i] {
			walk(i, 0)
		}
	}
	return order, depth
}

// boardFlowTree renders the chain tree: each pane with its badge and role,
// and under it the prompt edges it sends; then the courier / listener with
// what bounced.
func boardFlowTree(f board.Flow) []string {
	idx := boardFlowNodeIndex(f)
	out := map[string][]board.FlowEdge{}
	for _, e := range f.Edges {
		if e.Type == board.FlowPrompt || e.Type == board.FlowUndelivered || e.Type == board.FlowBlocks {
			out[e.From] = append(out[e.From], e)
		}
	}
	nodeLine := func(n board.FlowNode, indent string) string {
		parts := []string{indent + sanitiseBoardInline(n.Label)}
		if b := boardFlowCommBadge(n.Comm); b != "" {
			parts = append(parts, b)
		}
		if b := boardFlowStatusBadge(n); b != "" {
			parts = append(parts, b)
		}
		var info []string
		if n.Role != "" {
			info = append(info, sanitiseBoardInline(n.Role))
		}
		info = append(info, plural(n.Posts, "post"))
		if n.Task != "" {
			info = append(info, sanitiseBoardInline(n.Task))
		}
		return strings.Join(parts, " ") + "  " + strings.Join(info, " · ")
	}
	edgeLine := func(e board.FlowEdge, indent string) string {
		to := boardFlowLabel(f, idx, e.To)
		if e.Type == board.FlowBlocks {
			return fmt.Sprintf("%s⧗ blocked by %s", indent, to)
		}
		if e.Type == board.FlowUndelivered {
			return fmt.Sprintf("%s╳ %s  %s undelivered", indent, to, fmt.Sprint(e.Count))
		}
		parts := []string{plural(e.Count, "prompt")}
		if e.Answered != nil && *e.Answered > 0 {
			parts = append(parts, fmt.Sprintf("%d ✓", *e.Answered))
		}
		if e.Undelivered != nil && *e.Undelivered > 0 {
			parts = append(parts, fmt.Sprintf("%d ✗", *e.Undelivered))
		}
		if e.Open != nil && *e.Open > 0 {
			parts = append(parts, fmt.Sprintf("%d open", *e.Open))
		}
		if e.LatencyMS != nil {
			parts = append(parts, "ack "+boardFlowLatency(*e.LatencyMS))
		}
		return fmt.Sprintf("%s──▶ %s  %s", indent, to, strings.Join(parts, " · "))
	}

	var rows []string
	order, depth := boardFlowTreeOrder(f)
	for k, i := range order {
		n := f.Nodes[i]
		indent := strings.Repeat("   ", depth[k])
		branch := ""
		if depth[k] > 0 {
			indent = strings.Repeat("   ", depth[k]-1)
			branch = "└─ "
		}
		rows = append(rows, "  "+nodeLine(n, indent+branch))
		for _, e := range out[n.ID] {
			rows = append(rows, "  "+edgeLine(e, indent+strings.Repeat(" ", len([]rune(branch)))+"    "))
		}
		// Blockers outside the board's nodes ("human", "ext:ci") have no
		// edge to draw; they still belong under the agent they block.
		for _, b := range n.BlockedBy {
			if _, isNode := idx[b]; !isNode {
				rows = append(rows, "  "+indent+strings.Repeat(" ", len([]rune(branch)))+"    ⧗ blocked by "+sanitiseBoardInline(b))
			}
		}
	}
	for _, n := range f.Nodes {
		if n.Kind != board.FlowNodeCourier && n.Kind != board.FlowNodeListener && n.Kind != board.FlowNodeFleet {
			continue
		}
		rows = append(rows, "  "+nodeLine(n, ""))
		for _, e := range out[n.ID] {
			rows = append(rows, "  "+edgeLine(e, "    "))
		}
	}
	if len(rows) == 0 {
		rows = append(rows, "  No agents on this board yet.")
	}
	return rows
}

// boardFlowColumns orders the lifelines: panes in chain order, then the
// courier / listener, the board last.
func boardFlowColumns(f board.Flow) []int {
	order, _ := boardFlowTreeOrder(f)
	cols := append([]int(nil), order...)
	for i, n := range f.Nodes {
		if n.Kind == board.FlowNodeCourier || n.Kind == board.FlowNodeListener || n.Kind == board.FlowNodeFleet {
			cols = append(cols, i)
		}
	}
	for i, n := range f.Nodes {
		if n.Kind == board.FlowNodeBoard {
			cols = append(cols, i)
		}
	}
	return cols
}

const boardFlowClockWidth = 9 // "15:04:05 "

// boardFlowSequence renders the lifeline header and one row per event.
func boardFlowSequence(f board.Flow, cols []int, width int) (head string, rows []string) {
	idx := boardFlowNodeIndex(f)
	colOf := make(map[string]int, len(cols))
	for c, i := range cols {
		colOf[f.Nodes[i].ID] = c
	}
	// Column width: room for a short label, never less than 3 so an arrow
	// stays an arrow, and never so wide the event text loses its 2/5 of the
	// row — a diagram without its summaries says who, not what.
	colW := 12
	if avail := width - boardFlowClockWidth - max(20, width*2/5); len(cols) > 0 && avail/len(cols) < colW {
		colW = max(3, avail/len(cols))
	}
	diagW := colW * len(cols)
	center := func(c int) int { return c*colW + colW/2 }

	var h strings.Builder
	h.WriteString(strings.Repeat(" ", boardFlowClockWidth))
	for _, i := range cols {
		label := []rune(sanitiseBoardInline(f.Nodes[i].Label))
		if len(label) > colW-1 {
			label = append(label[:max(0, colW-2)], '…')
		}
		cell := make([]rune, colW)
		for k := range cell {
			cell[k] = ' '
		}
		start := max(0, colW/2-len(label)/2)
		for k, r := range label {
			if start+k < colW {
				cell[start+k] = r
			}
		}
		h.WriteString(string(cell))
	}
	head = h.String()

	for _, ev := range f.Events {
		line := make([]rune, diagW)
		for k := range line {
			line[k] = ' '
		}
		for c := range cols {
			line[center(c)] = '│'
		}
		from, okF := colOf[ev.From]
		to, okT := colOf[ev.To]
		var note string
		switch ev.Type {
		case board.FlowStatus:
			if okF {
				line[center(from)] = '○'
			}
			if okT {
				line[center(to)] = '●'
			}
			note = fmt.Sprintf("%s [%s] %s", boardFlowLabel(f, idx, ev.From), sanitiseBoardInline(ev.Kind), ev.Summary)
		default:
			if okF && okT && from != to {
				body := '─'
				if ev.Type != board.FlowPrompt {
					body = '╌'
				}
				a, b := center(from), center(to)
				step := 1
				if b < a {
					step = -1
				}
				for k := a + step; k != b; k += step {
					line[k] = body
				}
				line[a] = '├'
				switch {
				case ev.Type == board.FlowUndelivered:
					line[b] = '╳'
				case step > 0:
					line[b] = '▶'
				default:
					line[b] = '◀'
				}
			}
			note = fmt.Sprintf("%s %s", ev.Type, ev.CmdID)
			if ev.Summary != "" {
				note += " · " + ev.Summary
			}
		}
		rows = append(rows, boardClock(ev.TS)+" "+string(line)+"  "+sanitiseBoardInline(note))
	}
	return head, rows
}
