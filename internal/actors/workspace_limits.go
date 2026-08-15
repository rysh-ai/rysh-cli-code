// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"log/slog"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/limits"
)

// ---------------------------------------------------------------------------
// Subscription limit enforcement
// ---------------------------------------------------------------------------

// resourceUsage returns the current resource counts for limit checking.
// initLimitChecker builds this workspace's subscription limit checker from its
// own resolved upstream config and fetches the plan limits in the background.
// Called once from Started. When this workspace's upstream is disabled (or
// misconfigured) the checker stays nil and no limits are enforced.
func (w *WorkspaceActor) initLimitChecker(ctx actor.Context) {
	if !w.cfg.Upstream.Enabled {
		return
	}
	up := w.cfg.Upstream
	switch {
	case up.URL == "":
		slog.Warn("upstream enabled but no URL configured", "workspace", w.workspaceName)
		w.reportUpstreamError("upstream enabled but no URL configured")
		return
	case up.APIKey == "":
		slog.Warn("upstream enabled but no API key configured", "workspace", w.workspaceName)
		w.reportUpstreamError("upstream enabled but no API key configured")
		return
	}
	w.limitChecker = limits.NewChecker(up.URL, up.APIKey)

	// Fetch limits off the mailbox so startup is not blocked. The Checker is a
	// self-contained, mutex-protected object, so the goroutine touches only it
	// (never actor state). On failure it sends a message back so the actor can
	// surface the error from within its mailbox.
	checker := w.limitChecker
	self := ctx.Self()
	system := ctx.ActorSystem()
	wsName := w.workspaceName
	go func() {
		if err := checker.FetchLimits(); err != nil {
			slog.Warn("subscription limits fetch failed", "workspace", wsName, "err", err)
			system.Root.Send(self, &limitsFetchErrorMsg{err: err.Error()})
		}
	}()
}

// reportUpstreamError surfaces an upstream/limits problem to this workspace's
// active pane (visible when the user is viewing this workspace).
func (w *WorkspaceActor) reportUpstreamError(detail string) {
	w.syncActivePane()
	if w.activePaneID == "" {
		return
	}
	_ = w.pub.SendPaneRyshOutput(w.activePaneID,
		fmt.Sprintf("\n[rysh] workspace %q: %s\n"+
			"  sharing/limits will not work until a valid [workspace.upstream]/[upstream] api_key is configured\n\n",
			w.workspaceName, detail))
}

func (w *WorkspaceActor) resourceUsage() limits.ResourceUsage {
	return limits.ResourceUsage{
		Panes: w.resCounts.panes,
	}
}

// checkLimits validates whether creating the specified resources is allowed.
// Returns nil if allowed or an error describing the exceeded limit.
func (w *WorkspaceActor) checkLimits(addPanes int) error {
	if w.limitChecker == nil {
		return nil
	}
	return w.limitChecker.CheckCreate(w.resourceUsage(), addPanes)
}

// emitLimitError sends a subscription limit error message to the active pane's output.
func (w *WorkspaceActor) emitLimitError(err error) {
	if w.activePaneID == "" {
		return
	}
	errMsg := fmt.Sprintf("\r\n\x1b[31m[subscription] %s\x1b[0m\r\n", err.Error())
	_ = w.pub.SendPaneRyshOutput(w.activePaneID, errMsg)
}

// initResourceCounts calculates the initial resource counts from the current
// actor state. Called after restore from KV or bootstrap.
func (w *WorkspaceActor) initResourceCounts() {
	w.resCounts.panes = 0

	for _, info := range w.tabs {
		tabSnap := w.queryTabSnapshot(info.id)
		if tabSnap == nil {
			continue
		}
		w.resCounts.panes += domain.CountPanesInTab(tabSnap)
	}
}

// decrementTabResources decrements resource counters for a tab that's being closed.
func (w *WorkspaceActor) decrementTabResources(tabSnap *domain.TabSnapshot) {
	for _, lane := range tabSnap.Lanes {
		for _, g := range lane.PaneGroups {
			w.resCounts.panes -= len(g.Panes)
		}
	}
	if w.resCounts.panes < 0 {
		w.resCounts.panes = 0
	}
}
