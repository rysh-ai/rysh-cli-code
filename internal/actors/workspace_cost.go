package actors

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/usage"
)

// handleCostCommand implements ##cost (design 003):
//
//	##cost              spend today, by pane
//	##cost week         7-day rollup, by pane
//	##cost budget <n>   set/clear this pane's token ceiling (0 clears; k/m suffix ok)
//
// It queries the session's UsageActor over the usage.inbox request/reply
// endpoint and renders the reply. Output goes to the caller's rysh buffer.
func (w *WorkspaceActor) handleCostCommand(out *strings.Builder, paneID string, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = strings.ToLower(strings.TrimSpace(args[0]))
	}

	switch sub {
	case "budget":
		w.handleCostBudget(out, paneID, args[1:])
		return
	case "", "today", "day":
		w.renderCostWindow(out, "today")
	case "week", "7d":
		w.renderCostWindow(out, "week")
	default:
		fmt.Fprintf(out, "usage: ##cost [today|week] | ##cost budget <tokens>\n")
		w.failRyshUsage("unknown ##cost subcommand: %q", sub)
	}
}

func (w *WorkspaceActor) renderCostWindow(out *strings.Builder, window string) {
	reply, err := w.pub.Request(msg.UsageInboxSubject(), &msg.MsgUsageSnapshotRequest{Window: window}, 3*time.Second)
	if err != nil {
		fmt.Fprintf(out, "cost: usage ledger unavailable (%v)\n", err)
		w.failRysh("usage ledger unavailable")
		return
	}
	snap, ok := reply.(*msg.MsgUsageSnapshotReply)
	if !ok || snap == nil {
		fmt.Fprintf(out, "cost: unexpected reply from usage ledger\n")
		return
	}

	title := "Today"
	if window == "week" {
		title = "Last 7 days"
	}
	fmt.Fprintf(out, "%s · session %s\n", title, w.sessionName)

	if len(snap.ByPane) == 0 {
		fmt.Fprintf(out, "  (no spend recorded yet)\n")
		return
	}
	for _, a := range snap.ByPane {
		mark := ""
		if a.UnknownCost {
			mark = " ~"
		}
		fmt.Fprintf(out, "  pane %-10s  %8s in  %8s out  %s%s\n",
			shortID(a.Key),
			formatTokens(a.InTokens),
			formatTokens(a.OutTokens),
			usage.FormatUSD(a.CostMicroUSD),
			mark,
		)
	}
	fmt.Fprintf(out, "  %s\n", strings.Repeat("─", 46))
	fmt.Fprintf(out, "  session %-8s  %8s tok  %s\n",
		strings.ToLower(title),
		formatTokens(snap.SessionTokens),
		usage.FormatUSD(snap.SessionCostMicroUSD),
	)

	// By-agent breakdown (design 003): spend attributed to named autonomous
	// agents and humanoids. Only shown when there is any — a session with no
	// @agents/humanoids stays pane-only. Agent Keys are readable names, not UUIDs.
	if len(snap.ByAgent) > 0 {
		fmt.Fprintf(out, "  by agent:\n")
		for _, a := range snap.ByAgent {
			mark := ""
			if a.UnknownCost {
				mark = " ~"
			}
			fmt.Fprintf(out, "  @%-14s %8s in  %8s out  %s%s\n",
				truncateStr(a.Key, 14),
				formatTokens(a.InTokens),
				formatTokens(a.OutTokens),
				usage.FormatUSD(a.CostMicroUSD),
				mark,
			)
		}
	}

	for _, c := range snap.Ceilings {
		pct := 0
		if c.CeilingTokens > 0 {
			pct = int(c.SpentTokens * 100 / c.CeilingTokens)
		}
		fmt.Fprintf(out, "  budget pane %-10s %s / %s (%d%%)\n",
			shortID(c.PaneID), formatTokens(c.SpentTokens), formatTokens(c.CeilingTokens), pct)
	}
}

func (w *WorkspaceActor) handleCostBudget(out *strings.Builder, paneID string, args []string) {
	if len(args) == 0 {
		fmt.Fprintf(out, "usage: ##cost budget <tokens>   (0 clears; k/m suffix ok, e.g. 500k)\n")
		return
	}
	n, err := parseTokenCount(args[0])
	if err != nil {
		fmt.Fprintf(out, "cost: invalid token count %q: %v\n", args[0], err)
		w.failRysh("cost: invalid token count %q: %v", args[0], err)
		return
	}
	if err := w.pub.Send(msg.UsageInboxSubject(), &msg.MsgUsageBudgetSet{PaneID: paneID, CeilingTokens: n}); err != nil {
		fmt.Fprintf(out, "cost: failed to set budget: %v\n", err)
		w.failRysh("cost: failed to set budget: %v", err)
		return
	}
	if n == 0 {
		fmt.Fprintf(out, "cost: budget cleared for pane %s\n", shortID(paneID))
	} else {
		fmt.Fprintf(out, "cost: budget set to %s tokens for pane %s\n", formatTokens(n), shortID(paneID))
	}
}

// ---------------------------------------------------------------------------
// Formatting helpers (shortID lives in upstream_share.go)
// ---------------------------------------------------------------------------

// formatTokens renders a token count compactly (e.g. 412k, 2.9M).
func formatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64) + "M"
	case n >= 1_000:
		return strconv.FormatInt(n/1_000, 10) + "k"
	default:
		return strconv.FormatInt(n, 10)
	}
}

// parseTokenCount parses "500k", "2m", "100000" into a token count.
func parseTokenCount(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'k':
		mult, s = 1_000, s[:len(s)-1]
	case 'm':
		mult, s = 1_000_000, s[:len(s)-1]
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, err
	}
	if f < 0 {
		return 0, fmt.Errorf("negative")
	}
	return int64(f * float64(mult)), nil
}
