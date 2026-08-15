// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// native_agent.go — design 029. The pure half of `##claude` / `##codex`: which
// metadata a native agent pane carries, the shell lines that start and resume
// one, and how a codex session id is recovered from disk.
//
// WHAT "NATIVE" MEANS, and why it is a metadata flag rather than a heuristic.
// A pane running claude because the user typed `claude` at their shell is the
// user's; a pane running claude because they typed `##claude` is rysh's, and
// rysh owes it a resume after a stop/start. Nothing about the RUNNING process
// distinguishes the two — same binary, same argv shape — so the difference has
// to be recorded at launch. `agent.native` is that record. Reading the pane's
// foreground program instead would resurrect hand-typed agents too, which is
// the one behaviour the founder's framing rules out.
//
// WHY RESUME IS ENOUGH. Detach/attach keeps agents alive because the daemon
// owns the PTYs. Stop/start does not: every pane's shell is killed with the
// daemon, and restore spawns a fresh one. Both CLIs persist their conversation
// continuously, so nothing has to be captured at stop time — the whole job is
// starting the right resume command afterwards.

// The two CLIs this knows how to drive. An agent kind outside this set is not
// guessed at: see nativeAgentCommand.
const (
	nativeAgentClaude = "claude"
	nativeAgentCodex  = "codex"
)

// Pane metadata keys. `agent.args` and `fleet.agent` are deliberately the
// spellings fleetctl already stamps (see ansaPaneAgentArgs) — a second
// vocabulary for the same facts is how two subsystems end up disagreeing about
// which pane runs what.
const (
	// metaAgentNative marks the pane as rysh-launched. Its PRESENCE is the
	// whole signal; the value is only ever "1".
	//
	// CAREFUL — "native" means something ELSE one file over. `--agent rysh`
	// (ansaAgentRysh, workspace_ansa_kill.go) is rysh's own in-daemon agent,
	// which runs no subprocess at all and is resumed by continuing a
	// checkpointed turn. This key means the opposite kind of pane: a claude or
	// codex PROCESS that rysh started and therefore has to start again. A pane
	// never carries both.
	metaAgentNative = "agent.native"
	// metaAgentKind is "claude" or "codex".
	metaAgentKind = "agent.kind"
	// metaAgentCwd is the directory the agent was launched in, captured from
	// the pane's LIVE shell cwd. Load-bearing for both CLIs: claude stores
	// sessions per project directory, and codex rollouts are matched by cwd.
	metaAgentCwd = "agent.cwd"
	// metaAgentArgs are the extra flags the agent was launched with, so a
	// resumed agent comes back in the same permission mode it was started in.
	metaAgentArgs = "agent.args"
	// metaClaudeSessionID is the id pinned at launch with --session-id.
	metaClaudeSessionID = "claude.session_id"
	// metaCodexSessionID is the id RECOVERED after launch, because codex issues
	// its own — see discoverCodexSession.
	metaCodexSessionID = "codex.session_id"
)

// nativeAgentSpec is one agent invocation. Kept as a struct rather than a long
// argument list because launch and resume differ in exactly one field, and a
// bool in position four is how the wrong one gets built.
type nativeAgentSpec struct {
	// Kind is nativeAgentClaude or nativeAgentCodex.
	Kind string
	// SessionID is claude's pinned id, or codex's discovered one. Empty is a
	// legitimate state for codex (discovery has not finished, or failed) and a
	// degraded one for claude — both are handled, neither is invented.
	SessionID string
	// Args are extra CLI flags, appended verbatim.
	Args string
	// Cwd, when set, is prefixed as `cd <dir> &&`. See nativeAgentCommand.
	Cwd string
	// Resume selects the resume verb instead of the launch one.
	Resume bool
}

// claudeEnvScrub is the `env -u …` prefix that stops a claude started from
// inside another claude from coming up as a nested child of the conversation
// that launched it. The daemon inherits its environment from wherever rysh was
// started — which, for anyone driving rysh from an agent, is a claude session —
// and every pane shell inherits it in turn.
func claudeEnvScrub() string {
	var b strings.Builder
	b.WriteString("env")
	for _, v := range claudeCleanEnv {
		b.WriteString(" -u ")
		b.WriteString(v)
	}
	return b.String()
}

// AUTONOMY BY DEFAULT. A pane opened with `##claude` / `##codex` is an agent
// rysh is driving, not a shell the user is sitting at: nobody is watching it to
// answer an approval prompt, and a stopped-on-a-prompt agent in a detached pane
// looks identical to a working one until someone reads the screen. So both CLIs
// are launched with their approvals turned all the way off.
//
// These are the strongest flags each CLI has, and they are deliberate:
//
//	claude → --dangerously-skip-permissions, bypassing every permission check.
//	codex  → --dangerously-bypass-approvals-and-sandbox, which skips the
//	         prompts AND the sandbox. `-a never -s workspace-write` would leave
//	         the agent unable to touch anything outside its workspace, which for
//	         a fleet worktree is the common case rather than the exception.
//
// The pane is the boundary, not the flag: everything under `##` already runs
// with the user's own credentials in the user's own tree.
const (
	claudeAutonomyFlag = "--dangerously-skip-permissions"
	codexAutonomyFlag  = "--dangerously-bypass-approvals-and-sandbox"
)

// nativeAgentApprovalFlags are the flags that ALREADY state an approval or
// sandbox choice. If the user typed one of them, the default is not added on
// top — `##codex -a on-request` means they want to be asked, and
// `##claude --allowedTools "Bash(git *)"` means they want a narrow allowlist,
// which a blanket bypass would silently make meaningless.
//
// This is also the opt-out: `##claude --permission-mode manual` and
// `##codex --ask-for-approval on-request` get the old behaviour back.
var nativeAgentApprovalFlags = map[string][]string{
	nativeAgentClaude: {
		"--dangerously-skip-permissions",
		"--allow-dangerously-skip-permissions",
		"--permission-mode",
		"--allowedTools", "--allowed-tools",
		"--disallowedTools", "--disallowed-tools",
		"--tools",
	},
	nativeAgentCodex: {
		"--dangerously-bypass-approvals-and-sandbox",
		"--approve-for-me",
		"--ask-for-approval", "-a",
		"--sandbox", "-s",
	},
}

// nativeAgentArgs merges the default autonomy flag into the user's own args.
//
// The `--flag=value` form is matched as well as `--flag value`, because
// `##codex --ask-for-approval=never` is a choice the user made and stacking a
// bypass on top of it would ignore them on a technicality.
func nativeAgentArgs(kind, extra string) string {
	fields := strings.Fields(extra)
	if nativeAgentHasApprovalFlag(kind, fields) {
		return strings.Join(fields, " ")
	}
	flag := claudeAutonomyFlag
	if nativeAgentKind(kind) == nativeAgentCodex {
		flag = codexAutonomyFlag
	}
	return strings.TrimSpace(flag + " " + strings.Join(fields, " "))
}

func nativeAgentHasApprovalFlag(kind string, fields []string) bool {
	for _, f := range fields {
		name := f
		if i := strings.IndexByte(f, '='); i > 0 {
			name = f[:i]
		}
		for _, known := range nativeAgentApprovalFlags[nativeAgentKind(kind)] {
			if name == known {
				return true
			}
		}
	}
	return false
}

// nativeAgentCommand builds the shell line for one launch or resume.
//
// The two CLIs need genuinely different handling, and the difference is not
// cosmetic:
//
//	claude → --session-id <id> at LAUNCH, --resume <id> afterwards. The id is
//	         ours to choose, so the pane's own uuid becomes the session id and
//	         there is one fact with one source.
//	codex  → no launch-time id flag exists (checked against the real CLI), so
//	         launch is bare and the id is discovered from the rollout file
//	         afterwards. `codex resume <id>` then names the conversation
//	         exactly.
//
// The codex fallback when no id was discovered is `resume --last`, which is
// filtered to the current working directory. That is the pre-existing fleet
// behaviour and carries its known ambiguity — two codex agents sharing one cwd
// both resume whichever ran last — so it is the fallback, never the first
// choice. claude's equivalent degraded path is `--continue`, the same
// cwd-scoped most-recent semantics, for the same reason.
//
// Flags go BEFORE the session id on codex's resume line. `codex resume` takes
// [OPTIONS] [SESSION_ID] [PROMPT], so an id trailing a flag list is unambiguous
// while a flag trailing the id risks being read as the PROMPT positional.
//
// The `cd` prefix is only wanted on the restore path and is therefore driven by
// the field rather than assumed: a pane resuming at daemon start comes up with
// a fresh shell at the session root, which is not where the agent was working,
// and claude cannot find a session stored under a directory it is not in. At
// launch time the pane's shell is ALREADY in the right place — prefixing a cd
// there would only overwrite the user's own `cd` with a stale snapshot of it.
// The autonomy flag is merged in HERE rather than stamped into agent.args at
// launch, so a resume re-derives it. That is what makes a pane stamped by an
// older binary — or by fleetctl, which stamps the same key — come back
// autonomous too, instead of resuming into an agent that stops on its first
// approval prompt with nobody watching.
func nativeAgentCommand(s nativeAgentSpec) string {
	args := ""
	if a := nativeAgentArgs(s.Kind, s.Args); a != "" {
		args = " " + a
	}
	sid := strings.TrimSpace(s.SessionID)

	var cmd string
	switch s.Kind {
	case nativeAgentCodex:
		switch {
		case s.Resume && sid != "":
			cmd = "codex resume" + args + " " + sid
		case s.Resume:
			cmd = "codex resume --last" + args
		default:
			cmd = "codex" + args
		}
	default: // claude, and anything unknown — see nativeAgentKind
		switch {
		case s.Resume && sid != "":
			cmd = claudeEnvScrub() + " claude --resume " + sid + args
		case s.Resume:
			cmd = claudeEnvScrub() + " claude --continue" + args
		case sid != "":
			cmd = claudeEnvScrub() + " claude --session-id " + sid + args
		default:
			cmd = claudeEnvScrub() + " claude" + args
		}
	}

	if dir := strings.TrimSpace(s.Cwd); dir != "" {
		cmd = "cd " + shellQuote(dir) + " && " + cmd
	}
	return cmd
}

// nativeAgentKind normalises a pane's recorded agent kind.
//
// An unrecognised value resolves to claude rather than to a made-up command
// line, matching ansaResumeCommand: a pane stamped with something this binary
// does not know was stamped by a newer one, and inventing a resume verb for it
// would produce a receipt for a launch that never happened.
func nativeAgentKind(s string) string {
	switch strings.TrimSpace(s) {
	case nativeAgentCodex:
		return nativeAgentCodex
	default:
		return nativeAgentClaude
	}
}

// nativeAgentSessionID reads the session id for a pane's agent kind. The
// claude fallback to the pane id is fleetctl's invariant — the pane id IS the
// session id unless something has explicitly re-pinned it (`##claude new`).
// Codex has no such fallback: its ids are issued by the CLI, so an unknown one
// stays unknown rather than becoming a uuid that names no conversation.
func nativeAgentSessionID(kind, paneID string, meta func(string) string) string {
	if nativeAgentKind(kind) == nativeAgentCodex {
		return strings.TrimSpace(meta(metaCodexSessionID))
	}
	if sid := strings.TrimSpace(meta(metaClaudeSessionID)); sid != "" {
		return sid
	}
	return paneID
}

// nativeAgentCwd resolves the directory an agent should be launched in, and
// reports where the answer came from so a receipt can be honest about it.
//
// OSC 7 first: rysh injects a PROMPT_COMMAND that emits the shell's cwd before
// every prompt, so this is exact and current as of the last prompt — it is
// right immediately after a `cd`, where the polling fallbacks are stale for up
// to their cache interval. resolvePaneRootWithSource is the same
// live-then-startup-then-daemon ladder `##share` uses.
func nativeAgentCwd(snap *domain.PaneSnapshot) (dir, source string) {
	if snap != nil {
		if d := strings.TrimSpace(snap.ShellCwd); d != "" {
			return d, "osc7"
		}
		return resolvePaneRootWithSource(snap.ShellPID, snap.StartupDir)
	}
	if wd, err := os.Getwd(); err == nil {
		return wd, "daemon"
	}
	return "", ""
}

// ---------------------------------------------------------------------------
// does a claude session already exist?
// ---------------------------------------------------------------------------

// claudeSessionExists reports whether claude already holds a conversation under
// this session id.
//
// IT DECIDES WHICH FLAG THE PANE GETS, and getting it wrong is fatal to the
// launch rather than merely untidy: `claude --session-id <id>` on an id that
// already exists exits immediately with "Session ID <id> is already in use"
// (observed against the real CLI during the design-029 proof run), leaving an
// empty pane and no agent. Asking the filesystem is what makes the choice
// depend on claude's actual state instead of on rysh's bookkeeping being
// intact — metadata can be lost, deleted with `##pane meta delete`, or never
// have been written, and none of those should cost the user a conversation.
//
// A GLOB, NOT A COMPUTED PATH. Claude files a session under a directory named
// after the project it was started in, with the separators rewritten
// (`/Users/x/p.ai` → `-Users-x-p-ai`). Reproducing that transformation would be
// guessing at another tool's internals, and it would have to be re-guessed
// whenever they change. Session ids are UUIDs, so the FILENAME alone is
// unambiguous across every project directory — the glob costs one readdir and
// assumes nothing about the naming.
func claudeSessionExists(sessionID string) bool {
	sid := strings.TrimSpace(sessionID)
	if sid == "" {
		return false
	}
	// A session id reaches this from pane metadata, so it must not be able to
	// escape the search directory with a path separator.
	if strings.ContainsAny(sid, `/\`) {
		return false
	}
	matches, err := filepath.Glob(filepath.Join(claudeProjectsDir(), "*", sid+".jsonl"))
	return err == nil && len(matches) > 0
}

// ---------------------------------------------------------------------------
// --fresh-session: a clean conversation under the id the pane already has
// ---------------------------------------------------------------------------

// The flag spellings that mean "start over, but keep my id". `--fresh` is
// accepted as the short form because it is what people type; neither spelling
// is passed through to the CLI (nativeAgentStripFreshSession removes them), so
// a future claude/codex flag of the same name would be shadowed here — which is
// why the long form is the documented one.
var nativeAgentFreshSessionFlags = []string{"--fresh-session", "--fresh"}

// nativeAgentStripFreshSession pulls the flag out of an argument list, from
// ANY position: `##claude --fresh-session --model opus` and
// `##claude --model opus --fresh-session` are the same request, and a
// position-sensitive parse would send the flag on to a CLI that does not know
// it and abort the launch.
func nativeAgentStripFreshSession(args []string) (rest []string, fresh bool) {
	rest = make([]string, 0, len(args))
	for _, a := range args {
		if nativeAgentIsFreshSessionFlag(a) {
			fresh = true
			continue
		}
		rest = append(rest, a)
	}
	return rest, fresh
}

func nativeAgentIsFreshSessionFlag(arg string) bool {
	a := strings.TrimSpace(arg)
	for _, f := range nativeAgentFreshSessionFlags {
		if a == f {
			return true
		}
	}
	return false
}

// claudeArchivedSuffix marks a transcript rysh moved aside. It deliberately
// does NOT end in `.jsonl`: claude finds sessions by globbing that extension,
// so the renamed file stops being a session while staying exactly where its
// owner would look for it.
const claudeArchivedSuffix = ".rysh-archived-"

// archiveClaudeSession clears the way for `claude --session-id <sid>` to be
// accepted again, WITHOUT destroying the conversation it replaces.
//
// The collision is the whole reason this exists: claude exits immediately with
// "Session ID <id> is already in use" when the id it is handed already names a
// transcript, so a fresh conversation under the SAME id is only possible if the
// old transcript stops being one. Renaming rather than deleting is the founder-
// safe half — `--fresh-session` is a one-word request typed into a live pane,
// and a word should not be able to destroy a day's conversation. The archived
// file sits beside the original and can be renamed back.
//
// The same glob as claudeSessionExists, for the same reason: claude's project
// directory naming is its own business, and a UUID filename is unambiguous
// across all of them.
func archiveClaudeSession(sessionID string) (archived []string, err error) {
	sid := strings.TrimSpace(sessionID)
	if sid == "" || strings.ContainsAny(sid, `/\`) {
		return nil, nil
	}
	matches, gerr := filepath.Glob(filepath.Join(claudeProjectsDir(), "*", sid+".jsonl"))
	if gerr != nil {
		return nil, gerr
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	for _, path := range matches {
		dest := claudeArchivePath(path, stamp)
		if rerr := os.Rename(path, dest); rerr != nil {
			// Report the first failure but keep going: one unwritable project
			// directory must not leave the other transcripts half-moved, and
			// the caller needs to know the launch is about to hit the
			// already-in-use exit rather than silently pinning nothing.
			if err == nil {
				err = rerr
			}
			continue
		}
		archived = append(archived, dest)
	}
	return archived, err
}

// claudeArchivePath names the archive, stepping aside if that name is taken —
// two `--fresh-session` runs inside the same second are rare but a lost
// transcript is not an acceptable way to find out.
func claudeArchivePath(path, stamp string) string {
	base := path + claudeArchivedSuffix + stamp
	dest := base
	for i := 1; ; i++ {
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			return dest
		}
		dest = base + "-" + strconv.Itoa(i)
	}
}

// claudeProjectsDir is where claude keeps its per-project session files,
// honouring the same override the CLI reads.
func claudeProjectsDir() string {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return filepath.Join(dir, "projects")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// ---------------------------------------------------------------------------
// codex session discovery
// ---------------------------------------------------------------------------

// Codex writes one file per session at
// $CODEX_HOME/sessions/YYYY/MM/DD/rollout-<timestamp>-<uuid>.jsonl, whose first
// line is a session_meta record carrying the id and the directory the session
// was started in. That file is the only place a running codex publishes its own
// id, which is why discovery reads it rather than asking the CLI.
//
// This is a HEURISTIC, and is treated as one: it matches on the launch
// directory and a time window, prefers the newest match, and refuses ids
// already claimed by another pane. If codex ever grows a launch-time id or name
// flag, pin the id at launch and delete all of this.

// codexRolloutGlob is the filename shape discovery looks for.
const codexRolloutPrefix = "rollout-"

// codexDiscoverySlack widens the launch-time window backwards. The rollout's
// mtime is when codex created the file, which is after the launch line was
// typed — but the pane's clock and the file's are the same clock, so the slack
// only has to absorb the delivery hop, not clock skew.
const codexDiscoverySlack = 2 * time.Second

// codexSessionMeta is the first line of a rollout file. Only the two fields
// discovery matches on are decoded; the rest of the record is large (it carries
// the full system prompt) and is deliberately left unread.
type codexSessionMeta struct {
	Type    string `json:"type"`
	Payload struct {
		ID  string `json:"id"`
		Cwd string `json:"cwd"`
	} `json:"payload"`
	// ID and Cwd at the top level are a tolerated alternative shape. Codex
	// 0.133 nests them under payload; decoding both means a flattened record
	// from another version reads rather than silently failing to match.
	ID  string `json:"id"`
	Cwd string `json:"cwd"`
}

// parseCodexRolloutMeta pulls the session id and working directory out of a
// rollout's first line.
func parseCodexRolloutMeta(line []byte) (id, cwd string, ok bool) {
	var m codexSessionMeta
	if err := json.Unmarshal(line, &m); err != nil {
		return "", "", false
	}
	id, cwd = m.Payload.ID, m.Payload.Cwd
	if id == "" {
		id, cwd = m.ID, m.Cwd
	}
	if id == "" {
		return "", "", false
	}
	return id, cwd, true
}

// readCodexRolloutMeta reads just the first line of a rollout file.
//
// bufio with a bounded buffer rather than os.ReadFile: a rollout of a long
// conversation is megabytes, and every byte after the first newline is
// irrelevant here. The buffer is generous because that first line embeds the
// agent's base instructions.
func readCodexRolloutMeta(path string) (id, cwd string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 64*1024)
	line, err := r.ReadBytes('\n')
	if len(line) == 0 && err != nil {
		return "", "", false
	}
	return parseCodexRolloutMeta(line)
}

// codexSessionsDir is where codex keeps its rollouts, honouring $CODEX_HOME the
// same way the CLI does.
func codexSessionsDir() string {
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		return filepath.Join(home, "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "sessions")
}

// codexRolloutCandidate is one rollout that could belong to a pane.
type codexRolloutCandidate struct {
	ID      string
	ModTime time.Time
}

// sameDirectory compares two paths as DIRECTORIES rather than as strings.
//
// Found by the design-029 proof run, and it defeated discovery completely:
// rysh captures the shell's LOGICAL cwd (bash reports `/tmp/x` after `cd
// /tmp/x`), while codex records the PHYSICAL one it gets from getcwd()
// (`/private/tmp/x`, because /tmp is a symlink on macOS). Both name the same
// directory and no string comparison will ever say so.
//
// The raw comparison stays as the fast path — it is the common case, and it
// keeps the answer correct when a path cannot be resolved at all.
func sameDirectory(a, b string) bool {
	if a == b {
		return true
	}
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false
	}
	return ra == rb
}

// discoverCodexSession finds the session a just-launched codex opened.
//
// The three filters, in the order they eliminate the most:
//
//	mtime  — a rollout older than this launch belongs to an earlier session.
//	cwd    — codex records the directory it was started in, compared against
//	         the pane's live cwd AS A DIRECTORY (see sameDirectory: the two
//	         come from different sources and spell symlinks differently).
//	claimed — an id already stamped on another pane is that pane's, even if it
//	         matches on both other counts. This is what makes two codex panes
//	         launched into the SAME directory resolve to different sessions
//	         instead of both adopting the newest rollout.
//
// The newest surviving candidate wins. Returns ok=false when nothing matches,
// which is a normal early state — codex writes the file a moment after start —
// and the caller retries.
func discoverCodexSession(dir, cwd string, notBefore time.Time, claimed map[string]bool) (string, bool) {
	dir = strings.TrimSpace(dir)
	cwd = strings.TrimSpace(cwd)
	if dir == "" || cwd == "" {
		return "", false
	}
	cutoff := notBefore.Add(-codexDiscoverySlack)
	// Resolved once here rather than per candidate: the walk visits every
	// rollout on the machine, and EvalSymlinks is a syscall per call.
	cwdResolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		cwdResolved = cwd
	}

	var best codexRolloutCandidate
	// A walk error on one entry must not abandon the search: an unreadable
	// day-directory from another user's umask is not a reason to report "no
	// session", which would send the pane down the --last fallback for good.
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, codexRolloutPrefix) || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.ModTime().Before(cutoff) {
			return nil
		}
		if !best.ModTime.IsZero() && info.ModTime().Before(best.ModTime) {
			return nil // already have a newer match; skip the read
		}
		id, rcwd, ok := readCodexRolloutMeta(path)
		if !ok || id == "" || claimed[id] {
			return nil
		}
		if rcwd != cwd && rcwd != cwdResolved && !sameDirectory(rcwd, cwd) {
			return nil
		}
		best = codexRolloutCandidate{ID: id, ModTime: info.ModTime()}
		return nil
	})

	if best.ID == "" {
		return "", false
	}
	return best.ID, true
}
