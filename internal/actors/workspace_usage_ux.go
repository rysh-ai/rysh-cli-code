package actors

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/usage"
)

// spendTTL bounds how often the status-bar spend is refreshed from the ledger.
const spendTTL = 2 * time.Second

// snapshotSpend returns today's session spend and the ceiling-warning flag for
// the status bar (design 003 §3.5), refreshing from the UsageActor at most once
// per spendTTL. A stale-but-recent figure is fine for a status line, so this
// stays cheap on the hot snapshot path.
func (w *WorkspaceActor) snapshotSpend() (int64, bool) {
	if w.pub == nil {
		return 0, false
	}
	if !w.spendAt.IsZero() && time.Since(w.spendAt) < spendTTL {
		return w.spendMicroUSD, w.spendWarn
	}
	if reply := w.queryUsage("today"); reply != nil {
		w.spendMicroUSD = reply.SessionCostMicroUSD
		w.spendWarn = ceilingWarn(reply.Ceilings)
	}
	w.spendAt = time.Now()
	// A reachable ledger query means the UsageActor is up; run the once-per-session
	// weekly-digest check now (it needs the separate "week" window).
	w.maybeWriteWeeklyDigest()
	return w.spendMicroUSD, w.spendWarn
}

// queryUsage asks the UsageActor for a window snapshot ("today"|"week"), or nil
// on failure.
func (w *WorkspaceActor) queryUsage(window string) *msg.MsgUsageSnapshotReply {
	if w.pub == nil {
		return nil
	}
	// Bounded: the status bar is best-effort, so never block a snapshot long on
	// an unreachable ledger. The in-process UsageActor answers in well under this.
	reply, err := w.pub.Request(msg.UsageInboxSubject(),
		&msg.MsgUsageSnapshotRequest{Window: window}, 1*time.Second)
	if err != nil {
		return nil
	}
	snap, _ := reply.(*msg.MsgUsageSnapshotReply)
	return snap
}

// ceilingWarn reports whether any active ceiling is at ≥80% of its limit
// (design 003 §3.5 — the status bar turns yellow).
func ceilingWarn(ceilings []msg.UsageCeiling) bool {
	for _, c := range ceilings {
		if c.CeilingTokens > 0 && c.SpentTokens*100 >= c.CeilingTokens*80 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Weekly digest (design 003 §3.5): on the first session of a new ISO week,
// write reports/usage-{week}.md and print one summary line.
// ---------------------------------------------------------------------------

// maybeWriteWeeklyDigest writes the weekly usage digest at most once per session,
// and only when the current ISO week has not yet been digested (a marker file
// under the reports dir records the last digested week). Project-local
// (RyshDir), not the design's ~/.local/state, to match rysh's state model.
func (w *WorkspaceActor) maybeWriteWeeklyDigest() {
	if w.digestChecked {
		return
	}
	w.digestChecked = true
	if w.cfg.RyshDir == "" {
		return
	}
	week := isoWeekKey(time.Now())
	reportsDir := filepath.Join(w.cfg.RyshDir, "reports")
	// Cheap pre-check to avoid a ledger query when this week is already digested.
	if digestedWeek(reportsDir) == week {
		return
	}
	reply := w.queryUsage("week")
	if reply == nil {
		w.digestChecked = false // ledger not reachable yet — retry on the next refresh
		return
	}
	path, wrote, err := writeWeeklyDigest(reportsDir, week, reply)
	if err != nil || !wrote {
		return
	}
	if w.activePaneID != "" {
		_ = w.pub.SendPaneRyshOutput(w.activePaneID, fmt.Sprintf(
			"[usage] weekly digest for %s: %s over %s tokens → %s\n",
			week, usage.FormatUSD(reply.SessionCostMicroUSD), formatTokens(reply.SessionTokens), path))
	}
}

// digestedWeek returns the ISO week recorded in the reports dir's .last-digest
// marker, or "" if none.
func digestedWeek(reportsDir string) string {
	data, err := os.ReadFile(filepath.Join(reportsDir, ".last-digest"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writeWeeklyDigest writes reports/usage-{week}.md and updates the .last-digest
// marker, unless the marker already names week (returns wrote=false then). Pure
// file I/O — no ledger — so it is unit-testable.
func writeWeeklyDigest(reportsDir, week string, r *msg.MsgUsageSnapshotReply) (path string, wrote bool, err error) {
	if digestedWeek(reportsDir) == week {
		return "", false, nil
	}
	if err := os.MkdirAll(reportsDir, 0o755); err != nil {
		return "", false, err
	}
	path = filepath.Join(reportsDir, "usage-"+week+".md")
	if err := os.WriteFile(path, []byte(renderUsageDigest(week, r)), 0o644); err != nil {
		return "", false, err
	}
	if err := os.WriteFile(filepath.Join(reportsDir, ".last-digest"), []byte(week+"\n"), 0o644); err != nil {
		return path, true, err
	}
	return path, true, nil
}

// isoWeekKey renders an ISO-8601 year-week key like "2026-W30".
func isoWeekKey(t time.Time) string {
	y, wk := t.ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, wk)
}

// renderUsageDigest builds the markdown digest for a week's usage snapshot.
func renderUsageDigest(week string, r *msg.MsgUsageSnapshotReply) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# rysh usage — %s\n\n", week)
	fmt.Fprintf(&b, "- **Total:** %s over %s tokens\n\n",
		usage.FormatUSD(r.SessionCostMicroUSD), formatTokens(r.SessionTokens))
	if len(r.ByAgent) > 0 {
		b.WriteString("## By agent\n\n")
		for _, a := range r.ByAgent {
			fmt.Fprintf(&b, "- `%s` — %s (%s tok)\n",
				a.Key, usage.FormatUSD(a.CostMicroUSD), formatTokens(a.InTokens+a.OutTokens+a.CacheRead+a.CacheWrite))
		}
		b.WriteString("\n")
	}
	if len(r.ByPane) > 0 {
		b.WriteString("## By pane\n\n")
		for _, p := range r.ByPane {
			fmt.Fprintf(&b, "- `%s` — %s (%s tok)\n",
				shortID(p.Key), usage.FormatUSD(p.CostMicroUSD), formatTokens(p.InTokens+p.OutTokens+p.CacheRead+p.CacheWrite))
		}
	}
	return b.String()
}
