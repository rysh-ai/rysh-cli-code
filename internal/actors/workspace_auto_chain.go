package actors

import (
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/webauto"
)

// ---------------------------------------------------------------------------
// ##auto chaining & fan-out — items 10 (on_success) and 11 (--each)
//
// The AutoLoopActor supervises every run that needs completion handling
// (loops, chain-watch for plain runs, --each queues, notify). When the run
// ends it posts an autoRunDoneMsg (in-process, proto.actor Send — never NATS)
// back to the workspace, which decides ONE next step in its own mailbox:
// advance the --each queue, else fire the recipe's on_success chain.
// ---------------------------------------------------------------------------

// ryshOut prints to a pane's rysh stream, tolerating a nil publisher (unit
// tests drive the chain decision layer without NATS).
func (w *WorkspaceActor) ryshOut(paneID, text string) {
	if w.pub != nil {
		_ = w.pub.SendPaneRyshOutput(paneID, text)
	}
}

// autoRunDoneMsg is the supervisor's completion notification to the workspace.
type autoRunDoneMsg struct {
	Label  string // kind (web/task/agent/humanoid/code)
	Recipe string
	ExecID string // pane ID or agent/humanoid name
	PaneID string // source pane for output + re-dispatch
	Cause  string // "fulfilled" | "done" | "unfulfilled: …" | "aborted: …" | "judge error: …" | "stopped by user"
}

// autoQueue is one pending --each fan-out: the remaining arg items for a
// recipe on one exec target, plus everything needed to re-dispatch.
type autoQueue struct {
	label    string
	recipe   string
	paneID   string
	items    []string // remaining {{args}} items, in order
	headless bool     // web kind: keep the original run's executor
	ov       webAutoRunOverrides
	total    int // original item count, for progress lines
}

// autoChainNext decides what a completion cause allows. The queue tolerates
// budget-exhausted items (matrix runs keep going); the chain only fires on a
// genuine success; user stops and errors halt everything.
func autoChainNext(cause string) (queueContinue, chainFire bool) {
	switch {
	case cause == "fulfilled" || cause == "done":
		return true, true
	case strings.HasPrefix(cause, "unfulfilled"):
		return true, false // pass cap / outer ceiling — not a success, but the matrix moves on
	}
	return false, false // stopped by user, aborted, judge error
}

// maxChainDepth caps on_success chains (cycle guard).
const maxChainDepth = 8

// parseChainTarget splits an on_success value into (kind, recipe): a bare
// name stays in the current kind; "<kind>:<name>" crosses kinds.
func parseChainTarget(current, value string) (label, recipe string, ok bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", false
	}
	if i := strings.IndexByte(value, ':'); i >= 0 {
		label, recipe = value[:i], value[i+1:] // empty label/recipe rejected below
	} else {
		label, recipe = current, value
	}
	switch label {
	case "web", "task", "agent", "humanoid", "code":
		return label, webauto.SanitizeName(recipe), recipe != ""
	}
	return "", "", false
}

// specForLabel returns the kind spec for a label (web included — its spec is
// only used by the shared machinery, never by web's own run handler).
func specForLabel(label string) (autoKindSpec, bool) {
	switch label {
	case "web":
		return webKindSpec(), true
	case "task":
		return taskAutoSpec(), true
	case "agent":
		return agentAutoSpec(), true
	case "humanoid":
		return humanoidAutoSpec(), true
	case "code":
		return codeAutoSpec(), true
	}
	return autoKindSpec{}, false
}

// runAutoByLabel dispatches `##auto <label> run <name> [args…]` from inside
// the workspace mailbox (queue advancement and chain firing), routing web to
// its own run handler and everything else through the generic engine. Output
// lands on the source pane's rysh stream.
func (w *WorkspaceActor) runAutoByLabel(label, paneID, name string, runArgs []string, headless bool, ov webAutoRunOverrides) {
	var out strings.Builder
	if label == "web" {
		w.cmdWebAutoRun(&out, paneID, name, runArgs, headless, ov, "")
	} else if spec, ok := specForLabel(label); ok {
		w.cmdAutoRun(&out, paneID, spec, name, runArgs, "", ov, "")
	}
	if out.Len() > 0 {
		_ = w.pub.SendPaneRyshOutput(paneID, out.String())
	}
}

// handleAutoRunDone is the single decision point after a supervised run ends:
// (1) advance this exec's --each queue; else (2) fire the recipe's
// on_success chain. Both re-enter the normal run path (budget, loop, notify,
// supervision all re-apply per run).
func (w *WorkspaceActor) handleAutoRunDone(m *autoRunDoneMsg) {
	// Close out this run's recording first: a supervised recorder spans every
	// pass of the loop, so this completion is its only stop signal. Doing it
	// before the queue/chain re-entry below means the next run's recorder
	// starts against an already-encoded predecessor.
	w.stopWebRecorder(m.PaneID, "run finished: "+m.Cause)

	queueOK, chainOK := autoChainNext(m.Cause)

	// (1) --each queue advancement.
	if q, ok := w.autoQueues[m.ExecID]; ok && q.recipe == m.Recipe && q.label == m.Label {
		if !queueOK || len(q.items) == 0 {
			delete(w.autoQueues, m.ExecID)
			if !queueOK {
				w.ryshOut(m.PaneID, fmt.Sprintf(
					"\n[%s] --each queue for %q abandoned (%s) — %d item(s) not run\n",
					m.Label, m.Recipe, m.Cause, len(q.items)))
			} else if q.total > 1 {
				w.ryshOut(m.PaneID, fmt.Sprintf(
					"\n[%s] --each queue for %q complete (%d item(s))\n", m.Label, m.Recipe, q.total))
			}
			// fall through to the chain only when the WHOLE queue finished well
			if !queueOK {
				return
			}
		} else {
			item := q.items[0]
			q.items = q.items[1:]
			done := q.total - len(q.items) - 1
			w.ryshOut(m.PaneID, fmt.Sprintf(
				"\n[%s] --each: item %d/%d done (%s) — next: %q\n",
				m.Label, done, q.total, m.Cause, item))
			w.runAutoByLabel(q.label, q.paneID, q.recipe, []string{item}, q.headless, q.ov)
			return
		}
	}

	// (2) on_success chain.
	if !chainOK {
		return
	}
	spec, ok := specForLabel(m.Label)
	if !ok {
		return
	}
	a, err := w.autoStore(spec.kind).Load(m.Recipe)
	if err != nil || strings.TrimSpace(a.OnSuccess) == "" {
		delete(w.autoChainDepth, m.ExecID) // chain ends here — reset the depth
		return
	}
	nextLabel, nextRecipe, ok := parseChainTarget(m.Label, a.OnSuccess)
	if !ok {
		w.ryshOut(m.PaneID, fmt.Sprintf(
			"\n[%s] on_success %q is not a valid target (use <name> or <kind>:<name>)\n", m.Label, a.OnSuccess))
		return
	}
	// Depth guard rides the overrides into the next run.
	depth := w.autoChainDepth[m.ExecID] + 1
	if depth > maxChainDepth {
		w.ryshOut(m.PaneID, fmt.Sprintf(
			"\n[%s] chain depth cap (%d) reached at %q — not running %s:%s\n",
			m.Label, maxChainDepth, m.Recipe, nextLabel, nextRecipe))
		delete(w.autoChainDepth, m.ExecID)
		return
	}
	if w.autoChainDepth == nil {
		w.autoChainDepth = map[string]int{}
	}
	w.autoChainDepth[m.ExecID] = depth
	w.ryshOut(m.PaneID, fmt.Sprintf(
		"\n[%s] %q completed (%s) → chaining to %s:%s (depth %d/%d)\n",
		m.Label, m.Recipe, m.Cause, nextLabel, nextRecipe, depth, maxChainDepth))
	w.runAutoByLabel(nextLabel, m.PaneID, nextRecipe, nil, false, webAutoRunOverrides{})
}

// whileFlagOverrides renders the while-loop run flags as the synthetic
// highest-precedence WhileConfig tier (nil when no loop flag was given).
// --no-loop is an explicit enabled:false; token totals are expressed in
// pages (rounded up) since WhileConfig.Budget is a p/b/s size string.
func whileFlagOverrides(ov webAutoRunOverrides) *webauto.WhileConfig {
	if !ov.noLoop && ov.passes == 0 && ov.whileDuration == 0 && ov.whileBudgetTok == 0 {
		return nil
	}
	fc := &webauto.WhileConfig{MaxIterations: ov.passes}
	if ov.noLoop {
		f := false
		fc.Enabled = &f
	}
	if ov.whileDuration > 0 {
		fc.MaxDuration = ov.whileDuration.String()
	}
	if ov.whileBudgetTok > 0 {
		fc.Budget = fmt.Sprintf("%dp", (ov.whileBudgetTok+webauto.TokensPerPage-1)/webauto.TokensPerPage)
	}
	return fc
}

// registerEachQueue stores the --each fan-out queue (items after the first,
// which the caller runs immediately). The queued overrides drop `each` and
// `dryRun` so re-dispatches don't re-queue or dry-run. Returns whether a
// queue was registered.
func (w *WorkspaceActor) registerEachQueue(out *strings.Builder, label, recipe, execID, paneID string, headless bool, ov webAutoRunOverrides) bool {
	if len(ov.each) == 0 {
		return false
	}
	qov := ov
	qov.each = nil
	qov.dryRun = false
	qov.fromQueue = true // re-dispatches must not clobber this queue
	if w.autoQueues == nil {
		w.autoQueues = map[string]*autoQueue{}
	}
	rest := append([]string{}, ov.each[1:]...)
	w.autoQueues[execID] = &autoQueue{
		label: label, recipe: recipe, paneID: paneID,
		items: rest, headless: headless, ov: qov, total: len(ov.each),
	}
	fmt.Fprintf(out, "[%s] --each: %d item(s) — running %q now; queued next: %s\n",
		label, len(ov.each), ov.each[0], strings.Join(rest, ", "))
	return true
}
