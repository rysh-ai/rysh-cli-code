package actors

import (
	"fmt"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ---------------------------------------------------------------------------
// ##hop commands
// ---------------------------------------------------------------------------

// handleHopCommand processes ##hop commands.
func (w *WorkspaceActor) handleHopCommand(out *strings.Builder, paneID string, args []string) {
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}

	if arg == "" {
		ryshWriter(out).Usage(
			"##hop <pane-name|pane-id>   hop this pane's output + agent memory to another pane (fork)",
			"##hop resume                resume the target's AI with the hopped session",
			"##hop status                show hop state",
			"##hop clear                 clear hopped content",
		)
		return
	}

	switch arg {
	case "resume":
		_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneHopResume{})

	case "status":
		w.cmdHopStatus(out, paneID)

	case "clear":
		_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneHopClear{})
		fmt.Fprintf(out, "\n[hop] cleared hopped content\n")

	default:
		w.cmdHopTo(out, paneID, arg)
	}
}

func (w *WorkspaceActor) cmdHopTo(out *strings.Builder, sourcePaneID, targetArg string) {
	targetID := w.resolvePaneID(targetArg)
	if targetID == "" {
		fmt.Fprintf(out, "\n[hop] pane not found: %s\n", targetArg)
		w.failRysh("pane not found: %s", targetArg)
		return
	}
	if targetID == sourcePaneID {
		fmt.Fprintf(out, "\n[hop] cannot hop to self\n")
		w.failRysh("cannot hop to self")
		return
	}

	tab := w.currentTab()
	if tab == nil {
		fmt.Fprintf(out, "\n[hop] no active tab\n")
		w.failRysh("no active tab")
		return
	}
	content := tab.actor.PanePrivateOutput(sourcePaneID)
	chatContent := tab.actor.PaneChatOutput(sourcePaneID)
	trimmedContent := strings.TrimSpace(content)
	trimmedChat := strings.TrimSpace(chatContent)

	// Fork the source's LLM session memory into the target with REPLACE
	// semantics: full-fidelity export (tool calls/results, thinking blocks,
	// categories, pause checkpoint) fetched live from the source's
	// LLM-execution actor, then installed atomically on the target's. Both
	// panes hold identical agent memory at this instant and diverge
	// independently afterwards — a fork, not shared memory. An empty source
	// session skips the fork so hopping never wipes the target's memory
	// with nothing.
	memoryTurns := 0
	memPaused := false
	srcLLMInbox := msg.T("pane", sourcePaneID, "llm_prompt_execution", "inbox")
	tgtLLMInbox := msg.T("pane", targetID, "llm_prompt_execution", "inbox")
	if res, err := w.pub.Request(srcLLMInbox, &msg.MsgGetSessionMemory{}, 3*time.Second); err == nil {
		if mem, ok := res.(*msg.MsgSessionMemoryReply); ok && len(mem.Turns) > 0 {
			_ = w.pub.Send(tgtLLMInbox, &msg.MsgSessionMemoryReplace{
				Turns:        mem.Turns,
				Paused:       mem.Paused,
				PausedReason: mem.PausedReason,
				SourcePaneID: sourcePaneID,
				SourceAlias:  w.resolvePaneAlias(sourcePaneID),
			})
			memoryTurns = len(mem.Turns)
			memPaused = mem.Paused
		}
	}

	if trimmedContent == "" && trimmedChat == "" && memoryTurns == 0 {
		fmt.Fprintf(out, "\n[hop] source pane output and agent memory are empty — nothing to hop\n")
		w.failRysh("source pane output and agent memory are empty — nothing to hop")
		return
	}

	sourceAlias := w.resolvePaneAlias(sourcePaneID)

	_ = w.pub.Send(msg.T("pane", targetID, "inbox"), &msg.MsgPaneHopContent{
		SourcePaneID: sourcePaneID,
		SourceAlias:  sourceAlias,
		Content:      trimmedContent,
		ChatContent:  trimmedChat,
		MemoryTurns:  memoryTurns,
	})

	lines := 0
	if trimmedContent != "" {
		lines = strings.Count(trimmedContent, "\n") + 1
	}
	summary := fmt.Sprintf("\n[hop] hopped %d lines (%d bytes)", lines, len(trimmedContent))
	if trimmedChat != "" {
		chatLines := strings.Count(trimmedChat, "\n") + 1
		summary += fmt.Sprintf(" + %d chat lines (%d bytes)", chatLines, len(trimmedChat))
	}
	if memoryTurns > 0 {
		summary += fmt.Sprintf(" + agent memory (%d turns, replaced)", memoryTurns)
		if memPaused {
			summary += " [paused checkpoint carried]"
		}
	} else {
		summary += " (source agent memory empty — target memory untouched)"
	}
	summary += fmt.Sprintf(" to pane %s\n", targetArg)
	fmt.Fprint(out, summary)
}

func (w *WorkspaceActor) cmdHopStatus(out *strings.Builder, paneID string) {
	tab := w.currentTab()
	if tab == nil {
		fmt.Fprintf(out, "\n[hop] no active tab\n")
		w.failRysh("no active tab")
		return
	}
	hoppedInfo := tab.actor.PaneHoppedInfo(paneID)
	if hoppedInfo == nil || (hoppedInfo.Content == "" && hoppedInfo.ChatContent == "" && hoppedInfo.MemoryTurns == 0) {
		fmt.Fprintf(out, "\n[hop] no hopped content in this pane\n")
		return
	}
	lines := 0
	if hoppedInfo.Content != "" {
		lines = strings.Count(hoppedInfo.Content, "\n") + 1
	}
	fmt.Fprintf(out, "\n[hop] status\n")
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
	fmt.Fprintf(out, "  source pane : %s\n", hoppedInfo.Alias)
	fmt.Fprintf(out, "  source id   : %.8s\n", hoppedInfo.ID)
	fmt.Fprintf(out, "  content     : %d lines, %d bytes\n", lines, len(hoppedInfo.Content))
	if hoppedInfo.ChatContent != "" {
		chatLines := strings.Count(hoppedInfo.ChatContent, "\n") + 1
		fmt.Fprintf(out, "  chat        : %d lines, %d bytes\n", chatLines, len(hoppedInfo.ChatContent))
	}
	if hoppedInfo.MemoryTurns > 0 {
		fmt.Fprintf(out, "  agent memory: %d turns (session forked — replace)\n", hoppedInfo.MemoryTurns)
	} else {
		fmt.Fprintf(out, "  agent memory: not forked (text-only hop)\n")
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
	if hoppedInfo.MemoryTurns > 0 {
		fmt.Fprintf(out, "  use ##hop resume to continue the forked session\n\n")
	} else {
		fmt.Fprintf(out, "  use ##hop resume to send to AI\n\n")
	}
}

func (w *WorkspaceActor) resolvePaneAlias(paneID string) string {
	for _, info := range w.tabs {
		tabSnap := w.queryTabSnapshot(info.id)
		if tabSnap == nil {
			continue
		}
		if p := domain.FindPaneInTab(tabSnap, paneID); p != nil {
			return p.Title
		}
	}
	if len(paneID) > 8 {
		return paneID[:8]
	}
	return paneID
}
