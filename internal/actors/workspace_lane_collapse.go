// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// cmdLaneCollapse implements `##lane collapse|expand|toggle [<lane>]`.
//
// A collapsed lane is drawn as a one-column strip with its name running down
// it, and its width is shared out among the lanes still open: three lanes at a
// third each, fold the middle one, and the outer two take half each. The lane
// keeps its panes, processes and flex weight — opening it restores the layout
// it had.
//
// <lane> is what ##lane info takes: an id (or unique prefix), a 1-based index
// as ##lane list numbers them, or a name; empty means the active lane.
// `##lane expand all` opens every folded lane in the tab.
func (w *WorkspaceActor) cmdLaneCollapse(out *strings.Builder, verb string, args []string) {
	report, errText := w.laneCollapse(verb, strings.Join(args, " "))
	if errText != "" {
		fmt.Fprintf(out, "\n[rysh] %s\n", errText)
		w.failRysh("%s", errText)
		return
	}
	fmt.Fprintf(out, "\n[rysh] %s\n", report)
}

// laneCollapse applies verb ("collapse", "expand", "toggle") to the lane ref
// names in the active tab and returns a one-line report, or an error text.
// Shared by the ##lane command and the TUI's MsgLaneCollapse.
func (w *WorkspaceActor) laneCollapse(verb, ref string) (report, errText string) {
	switch verb {
	case "collapse", "expand", "toggle":
	default:
		return "", fmt.Sprintf("unknown lane verb %q", verb)
	}
	tab := w.currentTab()
	if tab == nil {
		return "", "no active tab"
	}
	snap := w.queryTabSnapshot(tab.id)
	if snap == nil {
		return "", "tab snapshot unavailable"
	}

	// "auto" is layout-mode `o`: open the focused lane if it is a strip,
	// otherwise open every folded lane.
	if verb == "expand" && ref == "auto" {
		ref = "all"
		if l := domain.ResolveLane(snap, ""); l != nil && l.Collapsed {
			ref = ""
		}
	}

	if verb == "expand" && ref == "all" {
		n := 0
		for i := range snap.Lanes {
			if !snap.Lanes[i].Collapsed {
				continue
			}
			if e := w.setLaneCollapsed(tab.id, snap.Lanes[i].ID, false); e != "" {
				w.afterLaneCollapse()
				return "", e
			}
			n++
		}
		w.afterLaneCollapse()
		return fmt.Sprintf("%d lane(s) expanded", n), ""
	}

	lane := domain.ResolveLane(snap, ref)
	if lane == nil {
		return "", fmt.Sprintf("no lane matches %q (see ##lane list)", ref)
	}
	collapsed := verb == "collapse" || (verb == "toggle" && !lane.Collapsed)
	if e := w.setLaneCollapsed(tab.id, lane.ID, collapsed); e != "" {
		return "", e
	}
	w.afterLaneCollapse()
	state := "expanded"
	if collapsed {
		state = "collapsed"
	}
	return fmt.Sprintf("lane %d (%s) %s", laneIndexInTab(snap, lane.ID)+1, laneLabel(lane), state), ""
}

// setLaneCollapsed asks the tab, and waits: the tab owns the rule (the last
// open lane cannot fold) and may move focus, and the active pane must be
// re-read only after it has.
func (w *WorkspaceActor) setLaneCollapsed(tabID, laneID string, collapsed bool) string {
	if w.pub == nil {
		return "no message bus"
	}
	reply, err := w.pub.Request(
		msg.T("tab", tabID, "inbox"),
		&msg.MsgSetLaneCollapsed{LaneID: laneID, Collapsed: collapsed},
		2*time.Second,
	)
	if err != nil {
		return fmt.Sprintf("tab did not answer: %v", err)
	}
	r, ok := reply.(*msg.MsgSetLaneCollapsedReply)
	if !ok {
		return "unexpected reply from tab"
	}
	return r.Error
}

func (w *WorkspaceActor) afterLaneCollapse() {
	w.syncActivePane()
	w.persistToKV()
}
