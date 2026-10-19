// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// workspace_native_agent.go — design 029. `##claude` and `##codex`: start an
// agent in the CURRENT pane, record enough about it that a stop/start can bring
// it back, and do the bringing back.
//
// The contrast worth holding onto is with `##pane new --claude`, which creates
// a NEW pane and therefore has to wait for it to exist before it can launch
// anything (workspace_pane_claude.go's retry loop). These verbs run in a pane
// the user is already sitting in, so the launch is immediate and the only
// asynchronous part is codex's session-id discovery.

const (
	// codexDiscoverEvery/codexDiscoverRetries bound the wait for codex to
	// register its session.
	//
	// The window is MINUTES, not seconds, and the proof run is why: codex does
	// not record a session when the process starts — it records one when the
	// conversation does. A pane can sit at a codex splash screen for as long as
	// the human takes to type their first message, and a discovery that gave up
	// at 30 s would miss every one of them. The cost of waiting is one timer per
	// codex pane; the cost of giving up early is a pane that cannot be resumed
	// by id at all.
	codexDiscoverEvery   = 2 * time.Second
	codexDiscoverRetries = 150 // 5 minutes

	// nativeResumeDelay is how long after a restore the first resume sweep
	// runs. Restored panes spawn their shells in their own Started hooks, so
	// the sweep has to land after the actor tree has settled, not with it.
	nativeResumeDelay   = 1500 * time.Millisecond
	nativeResumeEvery   = 1 * time.Second
	nativeResumeRetries = 10
)

// handleNativeAgentCommand runs `##claude` / `##codex` in the calling pane.
//
// Subcommands are deliberately few. `new` is the escape hatch for "I want a
// fresh conversation in this pane"; everything else is passed to the CLI
// verbatim as launch flags, because guessing at which flags rysh should
// understand is how a wrapper starts drifting from the tool it wraps.
//
// TWO WAYS TO START OVER, and they differ in what happens to the ID:
//
//	new              → a fresh conversation under a NEW id. The pane stops
//	                   being keyed to its own uuid; the old transcript stays
//	                   exactly where it is and is still resumable by id.
//	--fresh-session  → a fresh conversation under the id the pane ALREADY has
//	                   (its pane id). This is the one to reach for when the id
//	                   is the thing you rely on — anything addressing the pane's
//	                   agent by pane id keeps working across the reset, which
//	                   `new` breaks by design.
//
// The one flag rysh adds by itself is the autonomy default — see
// nativeAgentArgs. It is merged at command-build time rather than stamped into
// agent.args, so what is recorded here stays the user's own args.
func (w *WorkspaceActor) handleNativeAgentCommand(out *strings.Builder, paneID, kind string, args []string) error {
	kind = nativeAgentKind(kind)
	if paneID == "" {
		fmt.Fprintf(out, "##%s: no active pane\n", kind)
		return fmt.Errorf("##%s: no active pane", kind)
	}

	args, freshSession := nativeAgentStripFreshSession(args)

	// One-shot first prompt (native_agent_prompt.go). Parsed here, with the
	// other rysh-owned flags, so neither spelling ever reaches the CLI — and
	// never reaches extraArgs, which is what keeps it out of agent.args and
	// therefore out of every restore.
	args, promptReq, perr := nativeAgentStripPromptFlags(args)
	if perr != nil {
		fmt.Fprintf(out, "##%s: %v\n", kind, perr)
		return fmt.Errorf("##%s: %v", kind, perr)
	}
	if promptReq.any() && kind == nativeAgentCodex {
		// Claude only, for now: every fleet agent is claude, and a codex
		// --fresh-session cannot keep the pane id the resume file is named by.
		fmt.Fprintf(out, "##%s: %s / %s are claude-only for now\n", kind, nativeAgentResumeDirFlag, nativeAgentPromptFileFlag)
		return fmt.Errorf("##%s: prompt flags are claude-only", kind)
	}
	// --resume-dir IMPLIES --fresh-session. A resume note is a summary written
	// for an empty context; handed to the agent's existing conversation it
	// would only duplicate what the agent already knows.
	if promptReq.ResumeDir != "" {
		freshSession = true
	}

	fresh := false
	if len(args) > 0 && strings.TrimSpace(args[0]) == "new" {
		fresh = true
		args = args[1:]
	}
	// The two disagree about the id, so there is no sensible merge of them.
	// Refusing costs one retyped line; guessing costs a conversation filed under
	// an id the user did not expect.
	if fresh && promptReq.ResumeDir != "" {
		fmt.Fprintf(out, "##%s: %s starts over under THIS pane's id (the resume file is named by it); it cannot be combined with `new`\n", kind, nativeAgentResumeDirFlag)
		return fmt.Errorf("##%s: new and %s are mutually exclusive", kind, nativeAgentResumeDirFlag)
	}
	if fresh && freshSession {
		fmt.Fprintf(out, "##%s: pick one — `new` starts over under a NEW id, `--fresh-session` starts over under this pane's id\n", kind)
		return fmt.Errorf("##%s: new and --fresh-session are mutually exclusive", kind)
	}
	extraArgs := strings.TrimSpace(strings.Join(args, " "))

	// Resolve and check the prompt BEFORE anything changes — before the pane is
	// even probed. A refusal here must leave the pane exactly as it was, in
	// particular the old transcript un-archived, so a typo in the folder name
	// costs a retyped line and nothing else.
	promptPath := ""
	var promptData []byte
	if promptReq.any() {
		var err error
		if promptPath, err = nativeAgentPromptPath(promptReq, paneID, w.cfg.RyshDir); err == nil {
			promptData, err = readNativeAgentPrompt(promptPath)
		}
		if err != nil {
			fmt.Fprintf(out, "##%s: %v\n  nothing was started and nothing was changed\n", kind, err)
			return fmt.Errorf("##%s: %v", kind, err)
		}
	}

	// Refuse rather than type into whatever is already there. A pane running an
	// agent would receive this as composer text, which is the failure mode that
	// looks like nothing happened until someone reads the transcript.
	if prog := w.nativeAgentForeground(paneID); prog != "" {
		fmt.Fprintf(out, "##%s: this pane is already running %q — exit it first, or open a new pane\n", kind, prog)
		return fmt.Errorf("##%s: pane is running %s", kind, prog)
	}

	snap := w.paneSnapshotByID(paneID)
	cwd, cwdSource := nativeAgentCwd(snap)

	meta := func(key string) string {
		if snap == nil || snap.Meta == nil {
			return ""
		}
		return strings.TrimSpace(snap.Meta[key])
	}

	// Stage the checked prompt last, just before the archive: every refusal
	// above has had its chance to leave the pane untouched.
	staged := ""
	if promptData != nil {
		var err error
		if staged, err = stageNativeAgentPrompt(nativeAgentPromptStageDir(), paneID, promptData); err != nil {
			fmt.Fprintf(out, "##%s: could not stage the first prompt: %v\n  nothing was started and nothing was changed\n", kind, err)
			return fmt.Errorf("##%s: %v", kind, err)
		}
	}
	// A staged copy the shell will never read is removed here instead.
	dropStaged := func() {
		if staged != "" {
			_ = os.Remove(staged)
		}
	}

	// Resume, not relaunch, when this pane already has a conversation — typing
	// `##claude` a second time in the same pane means "get me back to my agent",
	// and re-pinning an id claude already holds makes it refuse outright
	// ("Session ID <id> is already in use") and exit, leaving an empty pane.
	//
	// For claude the question is answered by CLAUDE'S OWN STATE rather than by
	// rysh's: the session file either exists or it does not. That keeps the
	// pane recoverable when the metadata is gone — the exact case the proof run
	// hit, where a pane that had lost its stamps tried to re-pin its old id and
	// no agent started at all.
	sid := nativeAgentSessionID(kind, paneID, meta)
	resume := false
	var archived []string
	switch {
	case freshSession:
		// SAME ID, EMPTY CONVERSATION. The pane id is restored as the session id
		// — a pane that had been re-pinned by `new` comes back to rysh's
		// invariant — and claude's transcript under that id is moved aside so the
		// CLI stops refusing it ("Session ID <id> is already in use"). The move is
		// what makes this recoverable: see archiveClaudeSession.
		//
		// Codex cannot honour the id half of the request AT ALL — it issues its
		// own ids and has no launch-time flag to pin one (checked against the
		// real CLI, see nativeAgentCommand) — so what it gets is the rest of the
		// request: the old id is dropped, no resume is attempted, and discovery
		// stamps whatever id the new conversation turns out to have.
		if kind == nativeAgentCodex {
			sid = ""
		} else {
			sid = paneID
			var aerr error
			archived, aerr = archiveClaudeSession(sid)
			if aerr != nil {
				dropStaged()
				fmt.Fprintf(out, "##%s: could not move the old transcript aside: %v\n", kind, aerr)
				return fmt.Errorf("##%s: %v", kind, aerr)
			}
		}
	case fresh:
		// A fresh conversation needs an id that names no existing session. For
		// claude that is a new uuid, which also moves the source of truth from
		// "the pane id" to the metadata — nativeAgentSessionID reads the stamp
		// first for exactly this reason. Codex issues its own, so there is
		// nothing to mint and the old stamp is cleared instead.
		if kind == nativeAgentCodex {
			sid = ""
		} else {
			sid = uuid.NewString()
		}
	case kind == nativeAgentCodex:
		// Codex can only be resumed by an id discovery actually found.
		resume = sid != ""
	default:
		resume = claudeSessionExists(sid)
	}

	spec := nativeAgentSpec{Kind: kind, SessionID: sid, Args: extraArgs, Resume: resume, PromptFile: staged}
	// No cd at launch: the pane's shell is already where the user put it. See
	// nativeAgentCommand.
	command := nativeAgentCommand(spec)

	w.stampNativeAgentMeta(paneID, kind, cwd, extraArgs, sid, fresh || freshSession, promptPath)
	if err := w.sendPaneExec(paneID, command); err != nil {
		dropStaged()
		fmt.Fprintf(out, "##%s: could not start it in this pane: %v\n", kind, err)
		return fmt.Errorf("##%s: %v", kind, err)
	}

	if kind == nativeAgentCodex {
		w.scheduleCodexDiscovery(paneID, cwd, time.Now(), codexDiscoverRetries)
	}

	verb := "starting"
	if resume {
		verb = "resuming"
	}
	if freshSession {
		verb = "starting a fresh"
	}
	fmt.Fprintf(out, "\n[rysh] %s %s in this pane — rysh will bring it back after a session restart\n", verb, kind)
	if sid != "" {
		note := ""
		if freshSession {
			note = "  (this pane's id — the conversation under it is new)"
		}
		fmt.Fprintf(out, "  session id : %s%s\n", sid, note)
	} else if kind == nativeAgentCodex {
		fmt.Fprintf(out, "  session id : (codex issues its own — rysh is watching for it)\n")
	}
	// Name the archive. A transcript that was moved is only recoverable by
	// someone who knows it still exists and what it is now called.
	for _, path := range archived {
		fmt.Fprintf(out, "  archived   : %s\n", path)
	}
	if freshSession && kind == nativeAgentCodex {
		fmt.Fprintf(out, "  note       : codex issues its own session ids, so a fresh codex cannot keep the pane id\n")
	}
	if cwd != "" {
		fmt.Fprintf(out, "  directory  : %s%s\n", cwd, nativeAgentCwdNote(cwdSource))
	}
	if promptPath != "" {
		fmt.Fprintf(out, "  first prompt: %s\n", promptPath)
		fmt.Fprintf(out, "                (one-shot: not stored in agent.args, never replayed on restore)\n")
	}
	fmt.Fprintf(out, "  approvals  : %s\n", nativeAgentApprovalNote(kind, extraArgs))
	return nil
}

// nativeAgentApprovalNote says out loud that the agent will not stop to ask.
// A pane running unattended with permissions bypassed is not something to infer
// from a missing prompt, and the line doubles as where the opt-out is written
// down — see nativeAgentApprovalFlags.
func nativeAgentApprovalNote(kind, extraArgs string) string {
	if nativeAgentHasApprovalFlag(kind, strings.Fields(extraArgs)) {
		return "yours — from the flags you passed"
	}
	flag := claudeAutonomyFlag
	if nativeAgentKind(kind) == nativeAgentCodex {
		flag = codexAutonomyFlag
	}
	return "off — launched with " + flag
}

// nativeAgentCwdNote explains a cwd that did not come from the shell itself, so
// a resume that later lands in the wrong directory is traceable to the moment
// the guess was made rather than to the restart that revealed it.
func nativeAgentCwdNote(source string) string {
	switch source {
	case "osc7", "live":
		return ""
	case "startup":
		return "  (pane startup dir — the shell's live cwd was unreadable)"
	case "daemon":
		return "  (rysh launch dir — the pane's cwd was unreadable; cd and re-run to correct it)"
	default:
		return ""
	}
}

// stampNativeAgentMeta records what a resume will need. Written BEFORE the
// launch: an agent that dies on startup still leaves a pane that says what it
// was meant to be, which is the difference between a diagnosable failure and an
// empty pane.
func (w *WorkspaceActor) stampNativeAgentMeta(paneID, kind, cwd, extraArgs, sid string, fresh bool, promptFrom string) {
	for _, kv := range nativeAgentMeta(kind, cwd, extraArgs, sid, fresh, promptFrom) {
		_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneSetMeta{Key: kv[0], Value: kv[1]})
	}
}

// nativeAgentMeta is the stamp as key/value pairs, in the order they are sent.
// Split out so the resume contract can be tested end to end: what a launcher
// stamps is exactly what nativeResumePlan reads back.
//
// promptFrom is the file a one-shot first prompt was read from, or "" — and ""
// is stamped too, so the key always describes the LATEST launch rather than
// one three launches ago. It is never part of agent.args: see
// native_agent_prompt.go for why the two must not mix.
func nativeAgentMeta(kind, cwd, extraArgs, sid string, fresh bool, promptFrom string) [][2]string {
	out := [][2]string{
		{metaAgentNative, "1"},
		{metaAgentKind, kind},
		{metaAgentCwd, cwd},
		// An empty value DELETES the key (MsgPaneSetMeta's convention), which is
		// what should happen: a relaunch without flags must not inherit the flags
		// of the launch before it, or a pane quietly keeps a permission mode the
		// user stopped asking for.
		{metaAgentArgs, extraArgs},
		{metaAgentPromptFrom, promptFrom},
	}
	if kind == nativeAgentCodex {
		if fresh {
			out = append(out, [2]string{metaCodexSessionID, ""}) // the old conversation is no longer this pane's
		}
		return out
	}
	return append(out, [2]string{metaClaudeSessionID, sid})
}

// nativeAgentForeground reports the pane's live foreground program, or "" for a
// pane sitting at its shell. A pane that cannot be probed reads as "" — the
// launch then proceeds, because refusing on a failed probe would make an
// unrelated transport hiccup look like a pane that is busy.
func (w *WorkspaceActor) nativeAgentForeground(paneID string) string {
	prog, err := w.ansaRouterFor().tr.Probe(paneID)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(prog)
}

// sendPaneExec runs a shell line in a pane.
func (w *WorkspaceActor) sendPaneExec(paneID, command string) error {
	return w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneExecShell{Command: command})
}

// ---------------------------------------------------------------------------
// codex session discovery
// ---------------------------------------------------------------------------

// scheduleCodexDiscovery re-enters the workspace mailbox to look for the
// rollout file codex is about to write. A timer plus a self-addressed message
// rather than a goroutine: the lookup needs the workspace's view of which
// session ids other panes have already claimed, and that state belongs to the
// mailbox goroutine.
func (w *WorkspaceActor) scheduleCodexDiscovery(paneID, cwd string, launchedAt time.Time, retries int) {
	if cwd == "" || retries <= 0 {
		return
	}
	time.AfterFunc(codexDiscoverEvery, func() {
		_ = w.pub.Send(msg.T("ws", "inbox"), &msg.MsgDiscoverCodexSession{
			PaneID:    paneID,
			Cwd:       cwd,
			NotBefore: launchedAt.Unix(),
			Retries:   retries,
		})
	})
}

// handleDiscoverCodexSession stamps the pane with the session codex opened.
func (w *WorkspaceActor) handleDiscoverCodexSession(m *msg.MsgDiscoverCodexSession) {
	snap := w.paneSnapshotByID(m.PaneID)
	if snap == nil {
		return // the pane went away; nothing to stamp
	}
	if snap.Meta != nil && strings.TrimSpace(snap.Meta[metaCodexSessionID]) != "" {
		return // already found, or set by hand
	}

	id, ok := discoverCodexSession(codexSessionsDir(), m.Cwd, time.Unix(m.NotBefore, 0), w.claimedCodexSessions())
	if ok {
		_ = w.pub.Send(msg.T("pane", m.PaneID, "inbox"), &msg.MsgPaneSetMeta{
			Key: metaCodexSessionID, Value: id,
		})
		return
	}
	if m.Retries > 1 {
		w.scheduleCodexDiscovery(m.PaneID, m.Cwd, time.Unix(m.NotBefore, 0), m.Retries-1)
	}
	// Out of retries: the pane keeps no codex.session_id and resume falls back
	// to `codex resume --last`. Silent by design — that fallback is the
	// pre-existing behaviour, not a new failure, and it is visible in
	// `##pane meta list` as an absent key.
}

// claimedCodexSessions is the set of codex session ids other panes already own.
//
// This is what keeps two codex agents launched into the SAME directory from
// both adopting the newest rollout: whichever pane's discovery runs first takes
// that id, and the second pane's scan skips it and keeps looking.
func (w *WorkspaceActor) claimedCodexSessions() map[string]bool {
	claimed := map[string]bool{}
	panes, err := w.ansaRouterFor().tr.Panes()
	if err != nil {
		return claimed
	}
	for _, p := range panes {
		if id := strings.TrimSpace(p.meta(metaCodexSessionID)); id != "" {
			claimed[id] = true
		}
	}
	return claimed
}

// ---------------------------------------------------------------------------
// resume after a stop/start
// ---------------------------------------------------------------------------

// scheduleNativeAgentResume arms the post-restore sweep.
func (w *WorkspaceActor) scheduleNativeAgentResume(delay time.Duration, retries int) {
	if retries <= 0 {
		return
	}
	time.AfterFunc(delay, func() {
		_ = w.pub.Send(msg.T("ws", "inbox"), &msg.MsgResumeNativeAgents{Retries: retries})
	})
}

// nativeResumeDelivery is one pane's resume command, ready to run.
type nativeResumeDelivery struct {
	PaneID  string
	Command string
}

// resumableAgentPane reports whether the restart sweep owns this pane's agent.
//
// Two stamps qualify. `agent.native` is the ##claude / ##codex launcher's.
// A FLEET pane is the other: fleetctl types its own `claude … --settings <f>`
// line and so never reaches the stamping launcher, but it records the same
// facts in the same vocabulary — `fleet.agent` for the CLI and `agent.args`
// for the launch flags INCLUDING --settings. Without this, a daemon restart
// brought back every ##claude pane and left every fleet agent a bare shell,
// with the metadata needed to resume it sitting unread on the pane. Requiring
// agent.args as well keeps the boundary: a pane that merely carries a fleet
// role (a human's shell, a rysh in-daemon agent) has nothing to relaunch.
func resumableAgentPane(p ansaPane) bool {
	if strings.TrimSpace(p.meta(metaAgentNative)) != "" {
		return true
	}
	switch strings.TrimSpace(p.meta("fleet.agent")) {
	case nativeAgentClaude, nativeAgentCodex:
		return ansaPaneAgentArgs(p) != ""
	}
	return false
}

// nativeAgentResumeCwd is where the resumed agent must be started: the stamped
// launch directory, else — for a fleet pane — its worktree, which is where
// fleetctl `cd`ed before launching and therefore where claude filed the session.
func nativeAgentResumeCwd(p ansaPane) string {
	if d := strings.TrimSpace(p.meta(metaAgentCwd)); d != "" {
		return d
	}
	return strings.TrimSpace(p.meta("fleet.worktree"))
}

// nativeResumePlan decides what a sweep should do, given the panes and a way to
// probe them. Split out from the sending so the decisions can be tested: which
// panes are rysh's to resume, which are already running something, and which
// have not come up yet.
//
// Returns the commands to deliver, the panes that need no further attention
// (`settled` — already running an agent), and how many panes could not be
// probed and are therefore worth another pass.
// sessionExists answers "does this agent still have a conversation under this
// id" — claudeSessionExists in production, a stub in tests.
func nativeResumePlan(
	panes []ansaPane,
	probe func(string) (string, error),
	sessionExists func(string) bool,
	done map[string]bool,
) (deliveries []nativeResumeDelivery, settled []string, pending int) {
	for _, p := range panes {
		if !resumableAgentPane(p) {
			// Not ours to resume. A claude the user typed at their own shell
			// carries no stamp and stays the user's — the whole boundary of
			// design 029 is this one line.
			continue
		}
		if done[p.ID] {
			continue
		}

		// The probe is both the readiness check and the double-launch guard. An
		// error means the pane is not answering yet, so it is worth retrying; a
		// non-empty foreground means something is already running there —
		// including, on a later pass, the agent an earlier pass started.
		prog, err := probe(p.ID)
		if err != nil {
			pending++
			continue
		}
		if strings.TrimSpace(prog) != "" {
			settled = append(settled, p.ID)
			continue
		}

		kind := nativeAgentKind(p.meta(metaAgentKind))
		if p.meta(metaAgentKind) == "" {
			kind = nativeAgentKind(p.meta("fleet.agent"))
		}
		sid := nativeAgentSessionID(kind, p.ID, p.meta)
		// A codex pane with no session id has NO CONVERSATION TO RESUME: codex
		// records a session when the talking starts, so an id that never
		// appeared means the human never sent a first message. Starting codex
		// fresh is the honest answer. The `--last` fallback is deliberately NOT
		// used here — it resumes the most recent session in the directory,
		// which on an unattended sweep means silently attaching a pane to a
		// conversation nobody asked for. `##ansa resume`, which a human types,
		// keeps that fallback.
		//
		// Claude is asked the same question of its own state: a session file
		// that has been deleted (or a `~/.claude` moved between machines) makes
		// `--resume` fail at the pane, where re-pinning the id starts a working
		// agent under the id the pane already believes it has.
		resume := sid != ""
		if kind != nativeAgentCodex {
			resume = sid != "" && sessionExists(sid)
		}

		deliveries = append(deliveries, nativeResumeDelivery{
			PaneID: p.ID,
			Command: nativeAgentCommand(nativeAgentSpec{
				Kind:      kind,
				SessionID: sid,
				Args:      ansaPaneAgentArgs(p),
				// The cd IS the point on this path: a restored pane's shell
				// starts at the session root, and claude stores its sessions
				// per project directory — a --resume run from the wrong
				// directory finds nothing.
				Cwd:    nativeAgentResumeCwd(p),
				Resume: resume,
			}),
		})
	}
	return deliveries, settled, pending
}

// handleResumeNativeAgents brings back every agent this workspace had launched.
//
// WHY THERE IS NO NUDGE HERE, unlike `##ansa resume`. That verb resumes an agent
// somebody KILLED mid-task, so it passes a prompt telling the agent to carry on
// — a resumed fleet worker that sits idle is a stopped worker with extra steps.
// This sweep runs on every session start, where the contract is the narrower
// one the founder set: bring the last conversation back live, and leave it to
// the human sitting in front of it. A nudge here would have every restored pane
// start working, and spending, the moment the session comes up.
func (w *WorkspaceActor) handleResumeNativeAgents(m *msg.MsgResumeNativeAgents) {
	r := w.ansaRouterFor()
	panes, err := r.tr.Panes()
	if err != nil {
		// "I could not look" is not "there is nothing there" — retry.
		w.scheduleNativeAgentResume(nativeResumeEvery, m.Retries-1)
		return
	}

	if w.nativeResumed == nil {
		w.nativeResumed = map[string]bool{}
	}
	deliveries, settled, pending := nativeResumePlan(panes, r.tr.Probe, claudeSessionExists, w.nativeResumed)

	for _, id := range settled {
		w.nativeResumed[id] = true
	}
	for _, d := range deliveries {
		// Marked before the send, not after. A delivery that fails leaves one
		// pane at its shell; a delivery that succeeds but is retried leaves two
		// agents resuming one session id, which is the worse of the two.
		w.nativeResumed[d.PaneID] = true
		_ = w.sendPaneExec(d.PaneID, d.Command)
	}

	if pending > 0 {
		w.scheduleNativeAgentResume(nativeResumeEvery, m.Retries-1)
	}
}
