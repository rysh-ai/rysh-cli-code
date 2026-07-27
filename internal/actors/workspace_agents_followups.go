package actors

import (
	"fmt"
	"strings"

	sharedagentic "github.com/rysh-ai/rysh-cli-shared/agentic"

	"github.com/rysh-ai/rysh-cli-code/internal/metrics"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Follow-ups 2b + 3b — extra ##agent subcommands.

// handleAgentReloadPrompts reloads the layered prompt store and broadcasts
// MsgReloadPrompts to every pane's LLM-execution actor so the next user
// prompt picks up the new content. Currently-running orchestrators keep
// their captured snapshot — by design.
//
// Follow-up 2b.
func (w *WorkspaceActor) handleAgentReloadPrompts(out *strings.Builder, paneID string) {
	if w.agSetup == nil {
		fmt.Fprintf(out, "\n[reload] no agentic setup configured\n")
		return
	}
	if w.agSetup.PromptReloader == nil {
		fmt.Fprintf(out, "\n[reload] no prompt reloader installed (host main.go did not wire one up)\n")
		return
	}
	prompts := w.agSetup.Reload()
	if prompts == nil {
		fmt.Fprintf(out, "\n[reload] reload returned no prompts\n")
		return
	}

	// Broadcast to every pane's LLM-execution actor we know about. Walk
	// every tab snapshot to enumerate paneIDs (active + background).
	paneIDs := w.collectAllPaneIDs()
	if len(paneIDs) == 0 {
		fmt.Fprintf(out, "\n[reload] no active panes to notify\n")
		return
	}
	for _, pid := range paneIDs {
		_ = w.pub.Send(
			msg.T("pane", pid, "llm_prompt_execution", "inbox"),
			&msg.MsgReloadPrompts{
				SystemPrompt:    w.agSetup.SystemPrompt,
				EmailGovernance: prompts.EmailGovernance,
			},
		)
	}
	fmt.Fprintf(out, "\n[reload] prompts reloaded; broadcast to %d pane(s); effective on next prompt\n", len(paneIDs))
}

// collectAllPaneIDs walks every tab and returns the IDs of every pane
// it finds. Used by the reload broadcast above.
func (w *WorkspaceActor) collectAllPaneIDs() []string {
	var ids []string
	for _, t := range w.tabs {
		snap := w.queryTabSnapshot(t.id)
		if snap == nil {
			continue
		}
		for _, lane := range snap.Lanes {
			for _, g := range lane.PaneGroups {
				for _, p := range g.Panes {
					if p.ID != "" {
						ids = append(ids, p.ID)
					}
				}
			}
		}
	}
	return ids
}

// handleAgentMetrics dumps the in-process metrics sink's accumulated
// totals to the active pane. The sink is owned by agSetup so the same
// counters span every orchestrator created through this Setup.
//
// Follow-up 3b.
func (w *WorkspaceActor) handleAgentMetrics(out *strings.Builder) {
	if w.agSetup == nil || w.agSetup.Metrics == nil {
		fmt.Fprintf(out, "\n[metrics] no metrics sink installed\n")
		return
	}
	// We exported the concrete in-process type so the workspace can dump
	// it; the interface alone doesn't expose Dump(). Type-assert; fall
	// back to a polite "not supported" message for other implementations
	// (e.g. a future Prometheus sink that exposes nothing in-process).
	if dumpable, ok := w.agSetup.Metrics.(interface{ Dump() string }); ok {
		fmt.Fprintf(out, "\n%s\n", dumpable.Dump())
		return
	}
	// Type-assert to the known in-process sink type (concrete).
	if s, ok := w.agSetup.Metrics.(*metrics.Sink); ok {
		fmt.Fprintf(out, "\n%s\n", s.Dump())
		return
	}
	_ = sharedagentic.NoopMetricsSink{} // keep the import referenced for callers that swap to noop
	fmt.Fprintf(out, "\n[metrics] sink installed but does not support Dump()\n")
}
