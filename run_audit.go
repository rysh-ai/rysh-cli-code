package main

// The --json-audit artifact for `rysh run` (design 009 §3.1): "every tool
// call, diff, SNAT hit count, token spend". Everything in the artifact is
// derived from what the headless session actually published on its bus or
// actually did on disk — nothing is fabricated, and facts that are NOT
// observable from the run client are absent with a documented reason in
// `limits`:
//
//   - Tool calls come from the step-event stream (tool_start/tool_end). Step
//     titles carry the tool name plus an input summary truncated to 80 chars;
//     the FULL tool-input JSON never crosses the session bus, so the audit
//     records the summary, not the raw input.
//   - Approval decisions: a headless run fails closed on the first approval
//     request, so every recorded approval has decision "fail_closed". When
//     policy-as-code forced the gate, the orchestrator cites the rule in the
//     request description ("[gated by policy rule <id>]") and the audit
//     extracts it into `policy_rule`. Policy AUTO-approvals are logged only
//     daemon-side (slog) and never published on the bus — they are therefore
//     absent here, and `limits` says so.
//   - SNAT: redaction counts are only observable via the governance-proxy
//     audit plane (pane.*.proxy.audit → MsgProxyRequestAudit.RedactionHits),
//     which fires for proxied third-party traffic. rysh's own direct provider
//     calls sanitize in-process without publishing counts, so a typical
//     `rysh run` reports snat.observed=false with the reason.
//   - Diffs: per-file `git diff --stat` plus the full patch (capped) for the
//     paths the run changed (the before/after porcelain delta). Untracked
//     files appear in the changed list but have no git patch; noted per file.
//   - Budget/usage: summed from the design-003 usage ledger records the
//     session emitted; the ceiling is the --budget flag. Absent records leave
//     zeros — never estimates.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"

	"github.com/rysh-ai/rysh-cli-code/internal/eval"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// runAuditSchemaVersion identifies the artifact schema.
const runAuditSchemaVersion = 1

// auditPatchCap bounds the embedded full patch (bytes). Diff --stat is always
// complete; only the patch body is truncated (flagged when it happens).
const auditPatchCap = 256 * 1024

// runAudit is the full machine-readable audit artifact for one headless run.
type runAudit struct {
	SchemaVersion int    `json:"schema_version"`
	Session       string `json:"session"`
	Skill         string `json:"skill,omitempty"`
	Provider      string `json:"provider"`
	Prompt        string `json:"prompt"`

	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	DurationMs int64     `json:"duration_ms"`

	Status   string `json:"status"`
	ExitCode int    `json:"exit_code"`
	Detail   string `json:"detail,omitempty"`

	ToolCalls []auditToolCall `json:"tool_calls"`
	Approvals []auditApproval `json:"approvals"`
	Budget    auditBudget     `json:"budget"`
	Usage     auditUsage      `json:"usage"`
	SNAT      auditSNAT       `json:"snat"`
	// ProxyRequests are the raw governance-proxy audit records observed
	// during the run (design 001 §4.5) — present only when traffic actually
	// routed through the proxy.
	ProxyRequests []msg.MsgProxyRequestAudit `json:"proxy_requests,omitempty"`
	Diff          auditDiff                  `json:"diff"`
	Result        eval.Result                `json:"result"`

	// Limits documents what this artifact structurally cannot contain and
	// why — honesty over completeness theater.
	Limits []string `json:"limits"`
}

// auditToolCall is one observed tool invocation, paired from
// tool_start/tool_end step events.
type auditToolCall struct {
	Tool string `json:"tool"`
	// InputSummary is the step title's input label — first line, ≤80 chars
	// (see limits). Empty when the tool exposes no concise label.
	InputSummary string `json:"input_summary,omitempty"`
	StartedAtMs  int64  `json:"started_at_ms"`
	// EndedAtMs is 0 when no matching tool_end was observed (run ended
	// mid-call: gate-blocked, budget, timeout).
	EndedAtMs     int64  `json:"ended_at_ms,omitempty"`
	DurationMs    int64  `json:"duration_ms,omitempty"`
	ResultSummary string `json:"result_summary,omitempty"`
	Iteration     int    `json:"iteration,omitempty"`
	// Depth is the sub-agent depth (0 = top-level orchestrator).
	Depth int `json:"depth,omitempty"`
}

// auditApproval is one observed approval request. Headless runs have no
// approver: the first request ends the run, decision "fail_closed".
type auditApproval struct {
	RequestID   string `json:"request_id"`
	ToolCallID  string `json:"tool_call_id,omitempty"`
	Type        string `json:"type"`
	Description string `json:"description"`
	// PolicyRule is the policy-as-code rule that forced this gate, parsed
	// from the orchestrator's "[gated by policy rule <id>]" citation. Empty
	// when the gate came from the tool's own classifier.
	PolicyRule   string `json:"policy_rule,omitempty"`
	Decision     string `json:"decision"` // always "fail_closed" in a headless run
	ObservedAtMs int64  `json:"observed_at_ms"`
}

// auditBudget reports the --budget ceiling against observed spend.
type auditBudget struct {
	CeilingTokens   int64 `json:"ceiling_tokens,omitempty"`
	CeilingMicroUSD int64 `json:"ceiling_micro_usd,omitempty"`
	SpentTokens     int64 `json:"spent_tokens"`
	SpentMicroUSD   int64 `json:"spent_micro_usd"`
	Exhausted       bool  `json:"exhausted"`
}

// auditUsage is the usage-ledger split (design 003 records, summed).
type auditUsage struct {
	Records      int   `json:"records"`
	InTokens     int64 `json:"in_tokens"`
	OutTokens    int64 `json:"out_tokens"`
	CacheRead    int64 `json:"cache_read"`
	CacheWrite   int64 `json:"cache_write"`
	TotalTokens  int64 `json:"total_tokens"`
	CostMicroUSD int64 `json:"cost_micro_usd"`
}

// auditSNAT reports SNAT/redaction observability for this run.
type auditSNAT struct {
	// Observed is true only when at least one proxy audit record was seen —
	// the only surface that carries redaction counts.
	Observed      bool   `json:"observed"`
	RedactionHits int    `json:"redaction_hits"`
	Note          string `json:"note"`
}

// auditDiff carries the run's file-change evidence.
type auditDiff struct {
	GitAvailable bool `json:"git_available"`
	// ChangedPaths is the attributable delta (same derivation as the
	// Result's files_changed).
	ChangedPaths []string        `json:"changed_paths"`
	Files        []auditFileDiff `json:"files,omitempty"`
	// Stat is `git diff --stat` over the changed tracked paths.
	Stat string `json:"stat,omitempty"`
	// Patch is the full unified diff over the changed tracked paths, capped
	// at auditPatchCap bytes.
	Patch          string `json:"patch,omitempty"`
	PatchTruncated bool   `json:"patch_truncated,omitempty"`
	Note           string `json:"note,omitempty"`
}

// auditFileDiff is one changed path with what git could say about it.
type auditFileDiff struct {
	Path string `json:"path"`
	// Note flags paths git diff cannot cover (e.g. untracked files).
	Note string `json:"note,omitempty"`
}

// policyRuleRe extracts the policy-as-code citation the orchestrator appends
// to gated approval descriptions (rysh-shared/agentic withPolicyRule). The
// citation is always the description's TAIL and rule IDs themselves contain
// brackets ("bash.deny[1]"), so the match is greedy and end-anchored.
var policyRuleRe = regexp.MustCompile(`\[gated by policy rule (.+)\]\s*$`)

// parsePolicyRule returns the cited rule ID, or "" when the description
// carries none.
func parsePolicyRule(description string) string {
	m := policyRuleRe.FindStringSubmatch(description)
	if m == nil {
		return ""
	}
	return m[1]
}

// assembleRunAudit builds the audit artifact from the collector's verbatim
// observations plus the git evidence captured before teardown. Pure over its
// inputs (no I/O) so the whole derivation is unit-testable from scripted
// observations.
func assembleRunAudit(c *runCollector, opts runOptions, providerName, sessionName string,
	outcome runOutcome, res eval.Result, diff auditDiff, startedAt, finishedAt time.Time) *runAudit {

	c.mu.Lock()
	steps := append([]msg.MsgAgenticStep(nil), c.steps...)
	approvals := append([]msg.MsgApprovalRequest(nil), c.approvals...)
	approvalTS := append([]int64(nil), c.approvalTS...)
	proxy := append([]msg.MsgProxyRequestAudit(nil), c.proxyAudits...)
	usage := auditUsage{
		Records:      c.usageRecords,
		InTokens:     c.inTokens,
		OutTokens:    c.outTokens,
		CacheRead:    c.cacheRead,
		CacheWrite:   c.cacheWrite,
		TotalTokens:  c.tokens,
		CostMicroUSD: c.costMuSD,
	}
	breached := c.breached
	c.mu.Unlock()

	a := &runAudit{
		SchemaVersion: runAuditSchemaVersion,
		Session:       sessionName,
		Skill:         opts.SkillName,
		Provider:      providerName,
		Prompt:        opts.Prompt,
		StartedAt:     startedAt,
		FinishedAt:    finishedAt,
		DurationMs:    finishedAt.Sub(startedAt).Milliseconds(),
		Status:        outcome.Status,
		ExitCode:      outcome.ExitCode,
		Detail:        outcome.Detail,
		ToolCalls:     pairToolCalls(steps),
		Budget: auditBudget{
			CeilingTokens:   opts.Budget.Tokens,
			CeilingMicroUSD: opts.Budget.MicroUSD,
			SpentTokens:     usage.TotalTokens,
			SpentMicroUSD:   usage.CostMicroUSD,
			Exhausted:       breached,
		},
		Usage:         usage,
		ProxyRequests: proxy,
		Diff:          diff,
		Result:        res,
	}

	for i, req := range approvals {
		ts := int64(0)
		if i < len(approvalTS) {
			ts = approvalTS[i]
		}
		a.Approvals = append(a.Approvals, auditApproval{
			RequestID:    req.RequestID,
			ToolCallID:   req.ToolCallID,
			Type:         string(req.Type),
			Description:  req.Description,
			PolicyRule:   parsePolicyRule(req.Description),
			Decision:     "fail_closed",
			ObservedAtMs: ts,
		})
	}
	if a.Approvals == nil {
		a.Approvals = []auditApproval{}
	}

	hits := 0
	for _, p := range proxy {
		hits += p.RedactionHits
	}
	if len(proxy) > 0 {
		a.SNAT = auditSNAT{Observed: true, RedactionHits: hits,
			Note: fmt.Sprintf("summed from %d governance-proxy audit record(s)", len(proxy))}
	} else {
		a.SNAT = auditSNAT{Observed: false,
			Note: "no governance-proxy audit records observed; SNAT redaction counts are only published on the " +
				"proxy audit plane (proxied third-party traffic) — direct provider calls sanitize in-process " +
				"without publishing counts"}
	}

	a.Limits = []string{
		"tool input_summary is the step-event title (first line, ≤80 chars); full tool-input JSON is not published on the session bus",
		"policy auto-approvals are logged daemon-side only and are not observable on the bus; only gated approvals carry a policy rule ID",
		"bash_background invocations emit no command text in their step title and appear without an input summary",
	}
	if !diff.GitAvailable {
		a.Limits = append(a.Limits, "run directory is not a git repository — file diffs cannot be derived")
	}
	return a
}

// pairToolCalls turns the step-event stream into per-call audit entries,
// matching each tool_end to the earliest unfinished tool_start with the same
// origin and depth (the orchestrator runs calls sequentially per depth).
func pairToolCalls(steps []msg.MsgAgenticStep) []auditToolCall {
	calls := []auditToolCall{}
	open := map[string][]int{} // origin/depth key → indices of unfinished calls
	key := func(origin string, depth int) string { return fmt.Sprintf("%s/%d", origin, depth) }
	for _, st := range steps {
		switch st.Kind {
		case sharedmsg.StepToolStart:
			c := auditToolCall{
				Tool:         st.Origin,
				InputSummary: stripTitlePrefix(st.Title, st.Origin),
				StartedAtMs:  st.TimestampMs,
				Iteration:    st.Iteration,
				Depth:        st.Depth,
			}
			if c.Tool == "" {
				c.Tool = st.Title // defensive: origin should always be the tool name
			}
			k := key(st.Origin, st.Depth)
			open[k] = append(open[k], len(calls))
			calls = append(calls, c)
		case sharedmsg.StepToolEnd:
			k := key(st.Origin, st.Depth)
			idxs := open[k]
			if len(idxs) == 0 {
				continue // end without observed start (subscription raced the first event)
			}
			i := idxs[0]
			open[k] = idxs[1:]
			calls[i].EndedAtMs = st.TimestampMs
			if calls[i].StartedAtMs > 0 && st.TimestampMs >= calls[i].StartedAtMs {
				calls[i].DurationMs = st.TimestampMs - calls[i].StartedAtMs
			}
			calls[i].ResultSummary = extractResultSummary(st.Title)
		}
	}
	return calls
}

// stripTitlePrefix removes the "<tool>: " prefix from a step title, leaving
// the input summary ("" when the title is just the tool name).
func stripTitlePrefix(title, origin string) string {
	if origin == "" || title == origin {
		return ""
	}
	if len(title) > len(origin)+2 && title[:len(origin)+2] == origin+": " {
		return title[len(origin)+2:]
	}
	return ""
}

// extractResultSummary pulls the result digest from a tool_end title, which
// is "<stepTitle> — <resultSummary>" (rysh-shared/agentic emitStep call). The
// last separator wins so a " — " inside the input label cannot truncate the
// digest (a digest containing one is the residual ambiguity).
func extractResultSummary(title string) string {
	const sep = " — "
	if i := strings.LastIndex(title, sep); i >= 0 {
		return title[i+len(sep):]
	}
	return ""
}

// captureAuditDiff gathers the git evidence for the audit: the attributable
// changed paths (before/after porcelain delta, same as the Result), per-file
// notes, `git diff --stat`, and the full patch (capped). Must run BEFORE the
// worktree is removed. Tracked paths get stat+patch from `git diff HEAD`
// (staged and unstaged); when HEAD is unborn (fresh repo, no commits) it
// falls back to `git diff` and says so. Untracked paths are listed with a
// note — git diff cannot render them.
func captureAuditDiff(dir string, changed []string, after map[string]string, inRepo bool) auditDiff {
	d := auditDiff{GitAvailable: inRepo, ChangedPaths: changed}
	if d.ChangedPaths == nil {
		d.ChangedPaths = []string{}
	}
	if !inRepo {
		d.Note = "not a git repository — no diff derivable"
		return d
	}
	if len(changed) == 0 {
		d.Note = "no attributable working-tree changes"
		return d
	}

	var tracked []string
	for _, p := range changed {
		f := auditFileDiff{Path: p}
		// Porcelain "??" = untracked; a path absent from the after snapshot
		// was dirt that disappeared (e.g. an untracked file the run deleted).
		st, still := after[p]
		switch {
		case !still:
			f.Note = "no longer dirty after the run (deleted or reverted); not coverable by git diff"
		case st == "??":
			f.Note = "untracked — content not representable in git diff; see the worktree (or --keep)"
		default:
			tracked = append(tracked, p)
		}
		d.Files = append(d.Files, f)
	}
	if len(tracked) == 0 {
		d.Note = "no tracked changes — nothing for git diff to render"
		return d
	}

	base := []string{"-C", dir, "diff", "HEAD"}
	if !gitHasHead(dir) {
		base = []string{"-C", dir, "diff"}
		d.Note = "repository has no commits (unborn HEAD): diff covers unstaged tracked changes only"
	}
	statArgs := append(append([]string{}, base...), "--stat", "--")
	statArgs = append(statArgs, tracked...)
	if out, err := exec.Command("git", statArgs...).Output(); err == nil {
		d.Stat = string(out)
	}
	patchArgs := append(append([]string{}, base...), "--")
	patchArgs = append(patchArgs, tracked...)
	if out, err := exec.Command("git", patchArgs...).Output(); err == nil {
		if len(out) > auditPatchCap {
			out = out[:auditPatchCap]
			d.PatchTruncated = true
		}
		d.Patch = string(out)
	}
	return d
}

// gitHasHead reports whether the repo has any commit to diff against.
func gitHasHead(dir string) bool {
	return exec.Command("git", "-C", dir, "rev-parse", "--verify", "-q", "HEAD").Run() == nil
}

// writeRunAudit writes the audit artifact as indented JSON.
func writeRunAudit(path string, a *runAudit) error {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
