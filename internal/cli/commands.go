package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// defaultTimeout bounds a CLI request/reply against the daemon.
//
// 5s was too tight for scripted use. A structural command does real work
// before it can answer — `##new grid 2x2` spawns four PTYs, `##forge add`
// ingests a spec and generates artifacts — and on a loaded machine those
// exceed 5s and come back as "timeout waiting for reply", which reads like the
// daemon is broken rather than busy. A human typing into a pane never saw this
// because the TUI does not use this path.
//
// 30s is chosen to cover the slowest structural command comfortably while
// still failing fast enough that a genuinely wedged daemon does not hang a
// script for minutes. `rysh prompt` and `rysh run`, which wait for an agentic
// turn, have their own much longer budgets and do not use this.
const defaultTimeout = 30 * time.Second

// resolveSession finds a running or detached session by name (or default) and
// returns its record. The session's NATS server must be reachable.
func resolveSession(store *session.Store, name string) (session.Record, error) {
	if name == "" {
		name = "default"
	}
	rec, err := store.Get(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return session.Record{}, fmt.Errorf("session %q not found", name)
		}
		return session.Record{}, err
	}
	if rec.NATSPort <= 0 {
		return session.Record{}, fmt.Errorf("session %q has no NATS port recorded", name)
	}
	if rec.PID > 0 && !session.ProcessAlive(rec.PID) {
		return session.Record{}, fmt.Errorf("session %q process (PID %d) is not alive", name, rec.PID)
	}
	if rec.State != "running" && rec.State != "detached" {
		return session.Record{}, fmt.Errorf("session %q is not running (state: %s)", name, rec.State)
	}
	return rec, nil
}

// connect creates a Client to a running session.
func connect(store *session.Store, sessionName string) (*Client, error) {
	rec, err := resolveSession(store, sessionName)
	if err != nil {
		return nil, err
	}
	// Set the NATS topic prefix to match the running session.
	msg.SetSessionPrefix(rec.Name)
	return NewClient(rec.NATSPort, rec.Name)
}

// Connect opens a client against a running (or detached) session by name,
// setting the NATS subject prefix to match it.
//
// Exported for `rysh prompt`, which needs a long-lived connection it can
// subscribe on rather than the single request/reply the helpers here wrap.
func Connect(store *session.Store, sessionName string) (*Client, error) {
	return connect(store, sessionName)
}

// getSnapshot fetches the workspace snapshot from a running session.
func getSnapshot(c *Client) (*domain.WorkspaceSnapshot, error) {
	reply, err := c.RequestSnapshot(defaultTimeout)
	if err != nil {
		return nil, fmt.Errorf("fetch snapshot: %w", err)
	}
	sr, ok := reply.(*msg.MsgWorkspaceSnapshotReply)
	if !ok {
		return nil, fmt.Errorf("unexpected snapshot reply type: %T", reply)
	}
	return &sr.Snapshot, nil
}

// cliRequest sends a CLI request to workspace and parses the CLIResponse.
func cliRequest(c *Client, message interface{}) (*msg.MsgCLIResponse, error) {
	reply, err := c.Request(message, defaultTimeout)
	if err != nil {
		return nil, err
	}
	resp, ok := reply.(*msg.MsgCLIResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected response type: %T", reply)
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// Tab commands
// ---------------------------------------------------------------------------

// TabList lists all tabs in the running session.
func TabList(store *session.Store, sessionName string) error {
	c, err := connect(store, sessionName)
	if err != nil {
		return err
	}
	defer c.Close()

	snap, err := getSnapshot(c)
	if err != nil {
		return err
	}

	fmt.Printf("TABS (%d total)\n", len(snap.Tabs))
	fmt.Println(strings.Repeat("-", 80))
	for i, tab := range snap.Tabs {
		marker := "  "
		if tab.ID == snap.ActiveTabID {
			marker = "> "
		}
		totalPanes := domain.CountPanesInTab(&tab)
		pipeLabel := ""
		if tab.PipelineEnabled {
			pipeLabel = "  [pipeline]"
		}
		fmt.Printf("%s[%d] %-20s  id=%s  lanes=%d  panes=%d%s\n",
			marker, i+1, tab.Title, tab.ID, len(tab.Lanes), totalPanes, pipeLabel)
	}
	return nil
}

// TabCreate creates a new tab.
func TabCreate(store *session.Store, sessionName string) error {
	c, err := connect(store, sessionName)
	if err != nil {
		return err
	}
	defer c.Close()

	resp, err := cliRequest(c, &msg.MsgCLICreateTab{})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("create tab: %s", resp.Error)
	}
	fmt.Printf("created tab %s\n", resp.ID)
	return nil
}

// TabDelete deletes a tab by ID.
func TabDelete(store *session.Store, sessionName, tabID string) error {
	c, err := connect(store, sessionName)
	if err != nil {
		return err
	}
	defer c.Close()

	resp, err := cliRequest(c, &msg.MsgCLIDeleteTab{TabID: tabID})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("delete tab: %s", resp.Error)
	}
	fmt.Printf("deleted tab %s\n", tabID)
	return nil
}

// ---------------------------------------------------------------------------
// Lane commands
// ---------------------------------------------------------------------------

// LaneList lists all lanes in a tab.
func LaneList(store *session.Store, sessionName, tabID string) error {
	c, err := connect(store, sessionName)
	if err != nil {
		return err
	}
	defer c.Close()

	snap, err := getSnapshot(c)
	if err != nil {
		return err
	}

	// Find the target tab.
	var tab *domain.TabSnapshot
	for i := range snap.Tabs {
		if tabID == "" {
			if snap.Tabs[i].ID == snap.ActiveTabID {
				tab = &snap.Tabs[i]
				break
			}
		} else if snap.Tabs[i].ID == tabID {
			tab = &snap.Tabs[i]
			break
		}
	}
	if tab == nil {
		if tabID == "" {
			return fmt.Errorf("no active tab found")
		}
		return fmt.Errorf("tab %q not found", tabID)
	}

	fmt.Printf("LANES in tab %q (%d lanes)\n", tab.Title, len(tab.Lanes))
	fmt.Println(strings.Repeat("-", 80))
	for i, lane := range tab.Lanes {
		totalPanes := domain.CountPanesInLane(&lane)
		marker := "  "
		if lane.ActivePaneID == tab.ActivePaneID {
			marker = "> "
		}
		fmt.Printf("%s[%d] id=%s  flex=%d  groups=%d  panes=%d\n",
			marker, i+1, lane.ID, lane.Flex, len(lane.PaneGroups), totalPanes)
	}
	return nil
}

// LaneCreate creates a new lane in the specified tab.
func LaneCreate(store *session.Store, sessionName, tabID string) error {
	c, err := connect(store, sessionName)
	if err != nil {
		return err
	}
	defer c.Close()

	resp, err := cliRequest(c, &msg.MsgCLICreateLane{TabID: tabID})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("create lane: %s", resp.Error)
	}
	fmt.Printf("created lane in tab %s\n", tabID)
	return nil
}

// LaneDelete deletes a lane by ID.
func LaneDelete(store *session.Store, sessionName, tabID, laneID string) error {
	c, err := connect(store, sessionName)
	if err != nil {
		return err
	}
	defer c.Close()

	resp, err := cliRequest(c, &msg.MsgCLIDeleteLane{TabID: tabID, LaneID: laneID})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("delete lane: %s", resp.Error)
	}
	fmt.Printf("deleted lane %s\n", laneID)
	return nil
}

// ---------------------------------------------------------------------------
// PaneGroup commands
// ---------------------------------------------------------------------------

// PaneGroupList lists all pane groups.
func PaneGroupList(store *session.Store, sessionName, tabID, laneID string) error {
	c, err := connect(store, sessionName)
	if err != nil {
		return err
	}
	defer c.Close()

	snap, err := getSnapshot(c)
	if err != nil {
		return err
	}

	// Find the target tab.
	var tab *domain.TabSnapshot
	for i := range snap.Tabs {
		if tabID == "" {
			if snap.Tabs[i].ID == snap.ActiveTabID {
				tab = &snap.Tabs[i]
				break
			}
		} else if snap.Tabs[i].ID == tabID {
			tab = &snap.Tabs[i]
			break
		}
	}
	if tab == nil {
		return fmt.Errorf("tab not found")
	}

	fmt.Printf("PANE GROUPS in tab %q\n", tab.Title)
	fmt.Println(strings.Repeat("-", 80))
	for li, lane := range tab.Lanes {
		if laneID != "" && lane.ID != laneID {
			continue
		}
		fmt.Printf("  lane %d (id=%s  flex=%d)\n", li+1, lane.ID, lane.Flex)
		for gi, g := range lane.PaneGroups {
			fmt.Printf("    [group %d] id=%s  panes=%d\n", gi+1, g.ID, len(g.Panes))
			for pi, p := range g.Panes {
				marker := "        "
				if p.ID == tab.ActivePaneID {
					marker = "      > "
				}
				fmt.Printf("%s[%d] %-16s  id=%s\n", marker, pi+1, p.Title, p.ID)
			}
		}
	}
	return nil
}

// PaneGroupCreate creates a new pane group (split down) in the specified lane.
func PaneGroupCreate(store *session.Store, sessionName, tabID, laneID string) error {
	c, err := connect(store, sessionName)
	if err != nil {
		return err
	}
	defer c.Close()

	resp, err := cliRequest(c, &msg.MsgCLICreatePaneGroup{TabID: tabID, LaneID: laneID})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("create pane-group: %s", resp.Error)
	}
	fmt.Printf("created pane group\n")
	return nil
}

// PaneGroupDelete deletes a pane group by ID.
func PaneGroupDelete(store *session.Store, sessionName, tabID, laneID, paneGroupID string) error {
	c, err := connect(store, sessionName)
	if err != nil {
		return err
	}
	defer c.Close()

	resp, err := cliRequest(c, &msg.MsgCLIDeletePaneGroup{TabID: tabID, LaneID: laneID, PaneGroupID: paneGroupID})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("delete pane-group: %s", resp.Error)
	}
	fmt.Printf("deleted pane group %s\n", paneGroupID)
	return nil
}

// ---------------------------------------------------------------------------
// Pane commands
// ---------------------------------------------------------------------------

// PaneList lists panes in the specified tab and lane.
func PaneList(store *session.Store, sessionName, tabID, laneID string) error {
	c, err := connect(store, sessionName)
	if err != nil {
		return err
	}
	defer c.Close()

	snap, err := getSnapshot(c)
	if err != nil {
		return err
	}

	// Find the target tab.
	var tab *domain.TabSnapshot
	for i := range snap.Tabs {
		if snap.Tabs[i].ID == tabID {
			tab = &snap.Tabs[i]
			break
		}
	}
	if tab == nil {
		return fmt.Errorf("tab %q not found", tabID)
	}

	// Find the target lane.
	var lane *domain.LaneSnapshot
	for i := range tab.Lanes {
		if tab.Lanes[i].ID == laneID {
			lane = &tab.Lanes[i]
			break
		}
	}
	if lane == nil {
		return fmt.Errorf("lane %q not found in tab %q", laneID, tabID)
	}

	totalPanes := domain.CountPanesInLane(lane)

	fmt.Printf("PANES in lane %s, tab %s (%d total)\n", laneID, tab.Title, totalPanes)
	fmt.Println(strings.Repeat("-", 80))
	for _, g := range lane.PaneGroups {
		for pi, p := range g.Panes {
			marker := "  "
			if p.ID == snap.ActivePaneID {
				marker = "> "
			}
			groupLabel := ""
			if len(g.Panes) > 1 {
				groupLabel = fmt.Sprintf("  [stack %d/%d]", pi+1, len(g.Panes))
			}
			fmt.Printf("%s%-16s  mode=%-6s  status=%-10s  id=%s  group=%s%s\n",
				marker, p.Title, p.Mode, p.Status, p.ID, g.ID, groupLabel)
		}
	}
	return nil
}

// PaneCreate creates a new pane in the specified tab and lane.
// If paneGroupID is empty, a new pane-group is auto-created first.
// If paneGroupID is provided, a stacked pane is added to the existing group.
func PaneCreate(store *session.Store, sessionName, tabID, laneID, paneGroupID string) error {
	c, err := connect(store, sessionName)
	if err != nil {
		return err
	}
	defer c.Close()

	if paneGroupID != "" {
		// Create a stacked pane inside an existing pane-group.
		resp, err := cliRequest(c, &msg.MsgCLICreateStackedPane{
			TabID:       tabID,
			LaneID:      laneID,
			PaneGroupID: paneGroupID,
		})
		if err != nil {
			return err
		}
		if !resp.OK {
			return fmt.Errorf("create pane in group %s: %s", paneGroupID, resp.Error)
		}
		fmt.Printf("created pane in pane-group %s\n", paneGroupID)
	} else {
		// No pane-group specified: auto-create a new pane-group (with its
		// initial pane) in the specified lane.
		resp, err := cliRequest(c, &msg.MsgCLICreatePaneGroup{TabID: tabID, LaneID: laneID})
		if err != nil {
			return err
		}
		if !resp.OK {
			return fmt.Errorf("create pane: %s", resp.Error)
		}
		fmt.Printf("created pane (new pane-group in lane %s)\n", laneID)
	}
	return nil
}

// PaneDelete deletes a pane by ID.
func PaneDelete(store *session.Store, sessionName, paneID string) error {
	c, err := connect(store, sessionName)
	if err != nil {
		return err
	}
	defer c.Close()

	resp, err := cliRequest(c, &msg.MsgCLIDeletePane{PaneID: paneID})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("delete pane: %s", resp.Error)
	}
	fmt.Printf("deleted pane %s\n", paneID)
	return nil
}

// ---------------------------------------------------------------------------
// Stacked pane commands
// ---------------------------------------------------------------------------

// StackedPaneList lists stacked panes in a pane group.
func StackedPaneList(store *session.Store, sessionName, paneGroupID string) error {
	c, err := connect(store, sessionName)
	if err != nil {
		return err
	}
	defer c.Close()

	snap, err := getSnapshot(c)
	if err != nil {
		return err
	}

	found := false
	for _, tab := range snap.Tabs {
		for _, lane := range tab.Lanes {
			for _, g := range lane.PaneGroups {
				if paneGroupID != "" && g.ID != paneGroupID {
					continue
				}
				if len(g.Panes) <= 1 && paneGroupID == "" {
					continue
				}
				found = true
				fmt.Printf("STACKED PANES in group %s (%d panes)\n", g.ID, len(g.Panes))
				fmt.Println(strings.Repeat("-", 80))
				for pi, p := range g.Panes {
					marker := "  "
					if pi == 0 {
						marker = "> "
					}
					fmt.Printf("%s[%d] %-16s  mode=%-6s  status=%-10s  id=%s\n",
						marker, pi+1, p.Title, p.Mode, p.Status, p.ID)
				}
			}
		}
	}
	if !found {
		if paneGroupID != "" {
			return fmt.Errorf("pane group %q not found or has no stacked panes", paneGroupID)
		}
		fmt.Println("no stacked pane groups found")
	}
	return nil
}

// StackedPaneCreate creates a new stacked pane in the specified pane group.
func StackedPaneCreate(store *session.Store, sessionName, tabID, laneID, paneGroupID string) error {
	c, err := connect(store, sessionName)
	if err != nil {
		return err
	}
	defer c.Close()

	resp, err := cliRequest(c, &msg.MsgCLICreateStackedPane{
		TabID:       tabID,
		LaneID:      laneID,
		PaneGroupID: paneGroupID,
	})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("create stacked pane: %s", resp.Error)
	}
	fmt.Printf("created stacked pane\n")
	return nil
}

// ---------------------------------------------------------------------------
// Rysh ## system command
// ---------------------------------------------------------------------------

// RyshCommand runs a "##" system command on a running/detached session and
// prints its output. It is the command-line equivalent of typing a "##" command
// in a pane. command is the rysh command body WITHOUT the leading "##" (e.g.
// "cmd echo hailo", "pane info", "tab list"). tabID/paneID select the target
// pane; empty values fall back to the tab's / workspace's active pane.
func RyshCommand(store *session.Store, sessionName, tabID, paneID, command string) error {
	resp, err := RyshCommandResult(store, sessionName, tabID, paneID, command)
	if err != nil {
		return err
	}
	if resp.Output != "" {
		out := strings.TrimRight(resp.Output, "\n")
		if out != "" {
			fmt.Println(out)
		}
	}
	if !resp.OK {
		if resp.Error != "" {
			return fmt.Errorf("%s", resp.Error)
		}
		return fmt.Errorf("rysh command failed")
	}
	return nil
}

// RyshCommandResult is RyshCommand without the printing: it runs a "##" system
// command and hands back the daemon's raw response so the caller decides what
// to do with the output, the OK flag and the error text.
//
// `rysh exec` needs this to render --json, and to keep "the command failed"
// (resp.OK == false) distinct from "we could not ask" (a transport error) —
// the two mean different things to a script and want different exit codes.
func RyshCommandResult(store *session.Store, sessionName, tabID, paneID, command string) (*msg.MsgCLIResponse, error) {
	c, err := connect(store, sessionName)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	return cliRequest(c, &msg.MsgCLIRyshCommand{
		TabID:   tabID,
		PaneID:  paneID,
		Command: command,
	})
}

// ---------------------------------------------------------------------------
// Send input command
// ---------------------------------------------------------------------------

// PaneSendInput sends input (shell command or AI prompt) to a pane in a
// running or detached session. If paneID is empty, the input is sent to the
// active pane via the workspace inbox. If paneID is specified, the input is
// delivered directly to that pane's inbox as a MsgPaneExecShell or
// MsgPaneExecPrompt (the PaneActor does not handle MsgSubmitInput directly;
// that conversion is done by PaneGroupActor in the normal routing chain).
func PaneSendInput(store *session.Store, sessionName, paneID, text, mode string) error {
	c, err := connect(store, sessionName)
	if err != nil {
		return err
	}
	defer c.Close()

	if paneID != "" {
		// Send directly to the specified pane's inbox.
		// PaneActor only understands MsgPaneExecShell / MsgPaneExecPrompt,
		// so we must send the correct typed message.
		subject := msg.T("pane", paneID, "inbox")
		var paneMsg interface{}
		switch mode {
		case "prompt":
			paneMsg = &msg.MsgPaneExecPrompt{Prompt: text}
		default:
			// Default to shell mode when targeting a specific pane.
			paneMsg = &msg.MsgPaneExecShell{Command: text}
		}
		err = c.SendToSubject(subject, paneMsg)
	} else {
		// Send to workspace inbox → routes through the actor hierarchy
		// (Workspace → Tab → Lane → PaneGroup → Pane) which handles
		// mode detection and message conversion.
		err = c.Send(&msg.MsgSubmitInput{Text: text, Mode: mode})
	}
	if err != nil {
		return fmt.Errorf("send input: %w", err)
	}

	if paneID != "" {
		fmt.Printf("sent to pane %s\n", paneID)
	} else {
		fmt.Println("sent to active pane")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Pipeline commands
// ---------------------------------------------------------------------------

// PipelineList lists pipeline state for all tabs.
func PipelineList(store *session.Store, sessionName string) error {
	c, err := connect(store, sessionName)
	if err != nil {
		return err
	}
	defer c.Close()

	snap, err := getSnapshot(c)
	if err != nil {
		return err
	}

	fmt.Println("PIPELINES")
	fmt.Println(strings.Repeat("-", 80))
	for _, tab := range snap.Tabs {
		status := "disabled"
		if tab.PipelineEnabled {
			status = "enabled"
			if tab.PipelineActive {
				status = "active"
			}
		}
		name := tab.PipelineName
		if name == "" {
			name = "no-pipeline"
		}
		fmt.Printf("  tab %-16s  id=%s  pipeline=%s  status=%s\n",
			tab.Title, tab.ID, name, status)
	}
	return nil
}

// PipelineEnable enables pipeline mode for a tab.
func PipelineEnable(store *session.Store, sessionName, tabID string) error {
	c, err := connect(store, sessionName)
	if err != nil {
		return err
	}
	defer c.Close()

	resp, err := cliRequest(c, &msg.MsgCLIPipelineEnable{TabID: tabID})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("enable pipeline: %s", resp.Error)
	}
	fmt.Println("pipeline enabled")
	return nil
}

// PipelineDisable disables pipeline mode for a tab.
func PipelineDisable(store *session.Store, sessionName, tabID string) error {
	c, err := connect(store, sessionName)
	if err != nil {
		return err
	}
	defer c.Close()

	resp, err := cliRequest(c, &msg.MsgCLIPipelineDisable{TabID: tabID})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("disable pipeline: %s", resp.Error)
	}
	fmt.Println("pipeline disabled")
	return nil
}
