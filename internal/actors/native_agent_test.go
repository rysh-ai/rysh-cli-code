// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// Design 029. The properties here are the ones that decide whether a resumed
// pane comes back as the SAME conversation or as a new one — everything else
// about these verbs is visible the moment you run them, and this is not.

// claude's id is pinned at launch and reused on resume. If the launch and the
// resume ever disagree about the id, the resume silently starts a new session
// and the user's conversation is gone — findable only by reading the transcript.
func TestClaudeLaunchAndResumeAgreeOnTheSessionId(t *testing.T) {
	const sid = "6f1c9e2a-0000-4000-8000-000000000001"

	launch := nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentClaude, SessionID: sid})
	if !strings.Contains(launch, "claude --session-id "+sid) {
		t.Errorf("launch must PIN the id: %q", launch)
	}

	resume := nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentClaude, SessionID: sid, Resume: true})
	if !strings.Contains(resume, "claude --resume "+sid) {
		t.Errorf("resume must reuse the same id: %q", resume)
	}
}

// The env scrub is what stops a claude launched from a rysh driven BY a claude
// from coming up as a child of the conversation that launched it. The daemon
// inherits those markers, and so does every pane shell under it.
func TestClaudeLaunchScrubsTheNestingMarkers(t *testing.T) {
	cmd := nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentClaude, SessionID: "x"})
	for _, v := range claudeCleanEnv {
		if !strings.Contains(cmd, "-u "+v) {
			t.Errorf("%s must be scrubbed from the launch env: %q", v, cmd)
		}
	}
}

// Codex is launched bare — it has no launch-time id flag — and resumed by the
// id discovery read back off its rollout file.
func TestCodexLaunchIsBareAndResumeNamesTheSession(t *testing.T) {
	launch := nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentCodex})
	if launch != "codex "+codexAutonomyFlag {
		t.Errorf("codex takes no session id at launch: %q", launch)
	}

	const sid = "019e60f7-d058-7ac1-9cd0-eb7be5280af2"
	resume := nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentCodex, SessionID: sid, Resume: true})
	if resume != "codex resume "+codexAutonomyFlag+" "+sid {
		t.Errorf("a known id must be resumed by id: %q", resume)
	}
	if strings.Contains(resume, "--last") {
		t.Errorf("a known id must not fall back to --last: %q", resume)
	}
}

// Flags go before the positional session id. `codex resume` is
// [OPTIONS] [SESSION_ID] [PROMPT]: an id placed before a flag leaves the flag
// sitting in the PROMPT slot, which starts the agent with a stray instruction.
func TestCodexResumeFlagsPrecedeTheSessionId(t *testing.T) {
	cmd := nativeAgentCommand(nativeAgentSpec{
		Kind: nativeAgentCodex, SessionID: "sid-1", Args: "--search", Resume: true,
	})
	if cmd != "codex resume "+codexAutonomyFlag+" --search sid-1" {
		t.Errorf("flags must precede the id: %q", cmd)
	}
}

// Both CLIs degrade to their cwd-scoped "most recent" verb rather than to
// nothing. It is the weaker guarantee — it cannot tell two agents in one
// directory apart — which is why it is the fallback and never the first choice.
func TestResumeWithoutAnIdFallsBackToTheMostRecentInThisDirectory(t *testing.T) {
	codex := nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentCodex, Resume: true})
	if codex != "codex resume --last "+codexAutonomyFlag {
		t.Errorf("codex fallback: %q", codex)
	}
	claude := nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentClaude, Resume: true})
	if !strings.Contains(claude, "claude --continue") {
		t.Errorf("claude fallback: %q", claude)
	}
}

// ---------------------------------------------------------------------------
// autonomy
// ---------------------------------------------------------------------------

// A `##claude` / `##codex` pane runs unattended, so it is launched with
// approvals off. An agent that stops on a permission prompt in a detached pane
// is indistinguishable from one that is working, which is the whole reason this
// is a default rather than something to remember to type.
func TestNativeAgentsLaunchWithApprovalsOff(t *testing.T) {
	claude := nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentClaude, SessionID: "s"})
	if !strings.Contains(claude, claudeAutonomyFlag) {
		t.Errorf("claude must skip permissions: %q", claude)
	}
	codex := nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentCodex})
	if !strings.Contains(codex, codexAutonomyFlag) {
		t.Errorf("codex must bypass approvals and sandbox: %q", codex)
	}
}

// RESUME IS WHERE THIS MATTERS MOST, and it is also where it could most easily
// be lost: the flag is merged at command-build time rather than read back from
// agent.args, so a pane stamped by an older binary still comes back autonomous.
func TestResumeIsAutonomousEvenWithNoStampedArgs(t *testing.T) {
	claude := nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentClaude, SessionID: "s", Resume: true})
	if !strings.Contains(claude, claudeAutonomyFlag) {
		t.Errorf("a resumed claude must not start asking: %q", claude)
	}
	codex := nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentCodex, SessionID: "s", Resume: true})
	if !strings.Contains(codex, codexAutonomyFlag) {
		t.Errorf("a resumed codex must not start asking: %q", codex)
	}
}

// THE OPT-OUT. Someone who states an approval or sandbox choice has made it —
// stacking a blanket bypass on top would ignore them, and in the allowlist case
// would silently widen exactly the boundary they were drawing.
func TestAnExplicitApprovalChoiceSuppressesTheDefault(t *testing.T) {
	cases := []struct{ kind, args string }{
		{nativeAgentClaude, "--permission-mode manual"},
		{nativeAgentClaude, "--permission-mode=plan"},
		{nativeAgentClaude, `--allowedTools "Bash(git *)"`},
		{nativeAgentCodex, "-a on-request"},
		{nativeAgentCodex, "--ask-for-approval=never"},
		{nativeAgentCodex, "-s workspace-write"},
		{nativeAgentCodex, "--approve-for-me"},
	}
	for _, c := range cases {
		got := nativeAgentArgs(c.kind, c.args)
		if strings.Contains(got, claudeAutonomyFlag) || strings.Contains(got, codexAutonomyFlag) {
			t.Errorf("%s %q: default must not be added on top, got %q", c.kind, c.args, got)
		}
		if !strings.HasPrefix(got, strings.Fields(c.args)[0]) {
			t.Errorf("%s %q: the user's own args must survive verbatim, got %q", c.kind, c.args, got)
		}
	}
}

// Unrelated flags are not an approval choice, so they get the default AND keep
// their place.
func TestUnrelatedFlagsStillGetTheDefault(t *testing.T) {
	got := nativeAgentArgs(nativeAgentClaude, "--model opus")
	if got != claudeAutonomyFlag+" --model opus" {
		t.Errorf("got %q", got)
	}
}

// THE cd IS LOAD-BEARING ON RESUME, AND WRONG ON LAUNCH.
//
// A pane restored after a stop/start comes up with a fresh shell at the session
// root, not where the agent was working — and claude stores its sessions per
// project directory, so `--resume` from the wrong directory finds nothing. At
// launch the pane's shell is already where the user put it, and prefixing a cd
// would overwrite their live cwd with a snapshot of it.
func TestOnlyResumeCarriesTheWorkingDirectory(t *testing.T) {
	dir := "/Users/x/my project"

	resume := nativeAgentCommand(nativeAgentSpec{
		Kind: nativeAgentClaude, SessionID: "s", Cwd: dir, Resume: true,
	})
	if !strings.HasPrefix(resume, "cd "+shellQuote(dir)+" && ") {
		t.Errorf("resume must cd into the agent's directory first: %q", resume)
	}

	launch := nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentClaude, SessionID: "s"})
	if strings.Contains(launch, "cd ") {
		t.Errorf("launch must not move the user's shell: %q", launch)
	}
}

// A directory with a space (or a quote) must survive the shell, or the resume
// runs in the wrong place — or, worse, half of it runs as a second command.
func TestTheWorkingDirectoryIsQuotedForTheShell(t *testing.T) {
	cmd := nativeAgentCommand(nativeAgentSpec{
		Kind: nativeAgentClaude, SessionID: "s", Cwd: "/tmp/it's here", Resume: true,
	})
	if !strings.HasPrefix(cmd, `cd '/tmp/it'\''s here' && `) {
		t.Errorf("a quote in the path must be escaped: %q", cmd)
	}
}

// An agent kind this binary does not know resolves to claude rather than to an
// invented command line — the same rule ansaResumeCommand follows, and for the
// same reason: a made-up verb is a receipt for a launch that cannot run.
func TestAnUnknownAgentKindResolvesToClaude(t *testing.T) {
	if got := nativeAgentKind("gemini"); got != nativeAgentClaude {
		t.Errorf("unknown kind must resolve to claude, got %q", got)
	}
	if got := nativeAgentKind("codex"); got != nativeAgentCodex {
		t.Errorf("codex must survive normalisation, got %q", got)
	}
}

// The stamp wins over the pane id. `##claude new` mints a fresh uuid precisely
// so a pane can hold a conversation that is NOT keyed to its own id; if the
// reader preferred the pane id, `new` would silently resume the old session.
func TestAStampedSessionIdBeatsThePaneIdFallback(t *testing.T) {
	meta := func(k string) string {
		if k == metaClaudeSessionID {
			return "minted-by-claude-new"
		}
		return ""
	}
	if got := nativeAgentSessionID("claude", "pane-uuid", meta); got != "minted-by-claude-new" {
		t.Errorf("the stamp must win: %q", got)
	}
}

// OSC 7 is the primary cwd source: rysh injects a PROMPT_COMMAND that emits it
// before every prompt, so it is correct immediately after a `cd`, where the
// polling fallbacks are stale.
func TestTheLiveShellCwdIsPreferred(t *testing.T) {
	snap := &domain.PaneSnapshot{ShellCwd: "/live/dir", StartupDir: "/startup/dir"}
	dir, source := nativeAgentCwd(snap)
	if dir != "/live/dir" || source != "osc7" {
		t.Errorf("OSC 7 must win over the startup dir, got %q from %q", dir, source)
	}
}

// ---------------------------------------------------------------------------
// codex session discovery
// ---------------------------------------------------------------------------

// writeRollout fabricates a codex rollout file the way codex writes one: a
// session_meta first line, then conversation records.
func writeRollout(t *testing.T, dir, id, cwd string, mod time.Time) string {
	t.Helper()
	day := filepath.Join(dir, "2026", "08", "12")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(day, "rollout-2026-08-12T01-09-08-"+id+".jsonl")

	meta := map[string]any{
		"timestamp": "2026-08-12T01:09:08.000Z",
		"type":      "session_meta",
		"payload": map[string]any{
			"id": id, "cwd": cwd, "originator": "codex-tui", "cli_version": "0.133.0",
			// The real first line embeds the full system prompt; the reader
			// must cope with a long line rather than assume a short one.
			"base_instructions": strings.Repeat("You are Codex. ", 500),
		},
	}
	line, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	body := append(line, []byte("\n{\"type\":\"turn\"}\n")...)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	return path
}

// The first line of a REAL rollout (codex 0.133.0) nests the id and cwd under
// "payload". This is the shape discovery depends on, so it is pinned.
func TestParseCodexRolloutMetaReadsTheRealShape(t *testing.T) {
	line := []byte(`{"timestamp":"2026-05-25T21:08:34.009Z","type":"session_meta",` +
		`"payload":{"id":"019e60f7-d058-7ac1-9cd0-eb7be5280af2","timestamp":"2026-05-25T21:08:34.009Z",` +
		`"cwd":"/Users/halilagin","originator":"codex-tui","cli_version":"0.133.0"}}`)

	id, cwd, ok := parseCodexRolloutMeta(line)
	if !ok || id != "019e60f7-d058-7ac1-9cd0-eb7be5280af2" || cwd != "/Users/halilagin" {
		t.Errorf("real session_meta not parsed: id=%q cwd=%q ok=%v", id, cwd, ok)
	}
}

// A flattened record from some other version must still read. Discovery is a
// heuristic over a file format nobody promised us; tolerating both shapes costs
// two struct fields.
func TestParseCodexRolloutMetaToleratesAFlatShape(t *testing.T) {
	id, cwd, ok := parseCodexRolloutMeta([]byte(`{"type":"session_meta","id":"abc","cwd":"/w"}`))
	if !ok || id != "abc" || cwd != "/w" {
		t.Errorf("flat shape not parsed: id=%q cwd=%q ok=%v", id, cwd, ok)
	}
	if _, _, ok := parseCodexRolloutMeta([]byte(`not json`)); ok {
		t.Error("garbage must not parse as a session")
	}
}

// Discovery matches on the launch directory and ignores sessions that predate
// the launch — the two facts that separate "the codex I just started" from
// every other codex this user has ever run.
func TestDiscoveryMatchesTheDirectoryAndTheLaunchTime(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	writeRollout(t, dir, "old-session", "/work/a", now.Add(-time.Hour))
	writeRollout(t, dir, "other-dir", "/work/b", now)
	want := writeRollout(t, dir, "just-launched", "/work/a", now)
	_ = want

	got, ok := discoverCodexSession(dir, "/work/a", now.Add(-time.Second), nil)
	if !ok || got != "just-launched" {
		t.Errorf("got %q (ok=%v), want just-launched", got, ok)
	}

	if _, ok := discoverCodexSession(dir, "/work/never-used", now.Add(-time.Second), nil); ok {
		t.Error("a directory with no session must not match one")
	}
}

// TWO CODEX PANES IN ONE DIRECTORY. Both rollouts match on cwd and time, so the
// only thing that can tell them apart is that the first pane already took one.
// Without the claimed set they would both adopt the newest rollout and resume
// each other's conversation.
func TestDiscoverySkipsSessionsAnotherPaneAlreadyClaimed(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	writeRollout(t, dir, "pane-one-session", "/shared", now)
	writeRollout(t, dir, "pane-two-session", "/shared", now.Add(time.Second))

	first, ok := discoverCodexSession(dir, "/shared", now.Add(-time.Second), nil)
	if !ok || first != "pane-two-session" {
		t.Fatalf("newest wins: got %q (ok=%v)", first, ok)
	}

	second, ok := discoverCodexSession(dir, "/shared", now.Add(-time.Second),
		map[string]bool{first: true})
	if !ok || second != "pane-one-session" {
		t.Errorf("the second pane must find its OWN session, got %q (ok=%v)", second, ok)
	}
}

// Nothing yet is the normal state right after launch — codex writes the file a
// moment later — and must read as "retry", never as an error.
func TestDiscoveryReportsNotFoundRatherThanFailing(t *testing.T) {
	if _, ok := discoverCodexSession(t.TempDir(), "/work", time.Now(), nil); ok {
		t.Error("an empty sessions directory must not yield a session")
	}
	if _, ok := discoverCodexSession("", "/work", time.Now(), nil); ok {
		t.Error("an unset sessions directory must not yield a session")
	}
	if _, ok := discoverCodexSession(t.TempDir(), "", time.Now(), nil); ok {
		t.Error("an unknown cwd cannot be matched against")
	}
}

// THE DEFECT THE PROOF RUN FOUND, as a test.
//
// rysh captures the shell's LOGICAL cwd — bash reports `/tmp/x` after
// `cd /tmp/x` — while codex records the PHYSICAL one from getcwd(),
// `/private/tmp/x`, because /tmp is a symlink on macOS. Both name one
// directory, and the exact string comparison this started with matched
// neither, so discovery silently found nothing and every codex pane fell
// through to the `--last` fallback it was written to replace.
//
// Reproduced here with an explicit symlink rather than by relying on /tmp, so
// it fails on any platform that has symlinks at all.
func TestDiscoveryMatchesADirectoryReachedThroughASymlink(t *testing.T) {
	root := t.TempDir()
	physical := filepath.Join(root, "physical")
	if err := os.MkdirAll(physical, 0o755); err != nil {
		t.Fatal(err)
	}
	logical := filepath.Join(root, "via-link")
	if err := os.Symlink(physical, logical); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	sessions := t.TempDir()
	now := time.Now()
	// Codex writes the resolved path, exactly as observed in a real rollout.
	writeRollout(t, sessions, "session-through-link", physical, now)

	// rysh asks with the path the user's shell reported: the symlink.
	got, ok := discoverCodexSession(sessions, logical, now.Add(-time.Second), nil)
	if !ok || got != "session-through-link" {
		t.Errorf("a session in the same directory reached via a symlink must match: got %q (ok=%v)", got, ok)
	}
}

// The symlink tolerance must not turn into "any directory matches". A genuinely
// different directory still has to be rejected, or one pane adopts another's
// conversation.
func TestSymlinkToleranceStillRejectsADifferentDirectory(t *testing.T) {
	sessions := t.TempDir()
	now := time.Now()
	other := t.TempDir()
	writeRollout(t, sessions, "elsewhere", other, now)

	mine := t.TempDir()
	if _, ok := discoverCodexSession(sessions, mine, now.Add(-time.Second), nil); ok {
		t.Error("a session from another directory must not match")
	}
}

// THE SECOND DEFECT THE PROOF RUN FOUND, as a test.
//
// `claude --session-id <id>` on an id claude already holds exits immediately —
// "Session ID <id> is already in use", confirmed against the real CLI — so the
// launch/resume choice cannot be made from rysh's metadata alone: a pane that
// has lost its stamps would re-pin its own id and start nothing at all.
// Existence is asked of claude's own files instead.
//
// The lookup is a GLOB on the uuid, so it holds no opinion about how claude
// names project directories.
func TestAnExistingClaudeSessionIsDetectedWhereverItIsFiled(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	const sid = "6f1c9e2a-0000-4000-8000-00000000abcd"
	// A project directory whose name is a mangling of some path — the point is
	// that discovery does not have to know the mangling rule.
	proj := filepath.Join(cfg, "projects", "-private-tmp-whatever-work")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}

	if claudeSessionExists(sid) {
		t.Error("no session file yet, so nothing must be reported as existing")
	}
	if err := os.WriteFile(filepath.Join(proj, sid+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !claudeSessionExists(sid) {
		t.Error("an existing session file must be found regardless of its project directory")
	}
	if claudeSessionExists("6f1c9e2a-0000-4000-8000-0000000000ff") {
		t.Error("an unrelated id must not match")
	}
}

// A session id arrives from pane metadata, which anyone can write with
// `##pane meta set`. It must not be able to walk out of the projects directory.
func TestClaudeSessionLookupRefusesAPathAsASessionId(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if claudeSessionExists("../../etc/passwd") {
		t.Error("a path must never be accepted as a session id")
	}
	if claudeSessionExists("") {
		t.Error("an empty id names no session")
	}
}

// $CODEX_HOME is how the CLI is pointed at a different state directory, and
// discovery has to follow it or it reads the wrong machine's sessions.
func TestCodexSessionsDirFollowsCodexHome(t *testing.T) {
	t.Setenv("CODEX_HOME", "/custom/codex")
	if got := codexSessionsDir(); got != filepath.Join("/custom/codex", "sessions") {
		t.Errorf("CODEX_HOME ignored: %q", got)
	}
}

// ---------------------------------------------------------------------------
// --fresh-session
// ---------------------------------------------------------------------------

// The flag is rysh's, not the CLI's, so it must never reach the launch line —
// claude and codex both abort on an unknown flag, which would turn "start over"
// into an empty pane. Position must not matter either: people type it first as
// often as last.
func TestFreshSessionFlagIsStrippedFromAnyPosition(t *testing.T) {
	cases := [][]string{
		{"--fresh-session"},
		{"--fresh-session", "--model", "opus"},
		{"--model", "opus", "--fresh-session"},
		{"--model", "opus", "--fresh"},
	}
	for _, args := range cases {
		rest, fresh := nativeAgentStripFreshSession(args)
		if !fresh {
			t.Errorf("%v: flag not recognised", args)
		}
		for _, a := range rest {
			if strings.HasPrefix(a, "--fresh") {
				t.Errorf("%v: flag survived into the CLI args: %v", args, rest)
			}
		}
	}
	rest, fresh := nativeAgentStripFreshSession([]string{"--model", "opus"})
	if fresh {
		t.Error("no flag present, yet it was reported")
	}
	if len(rest) != 2 {
		t.Errorf("unrelated args must survive verbatim: %v", rest)
	}
	// A near-miss is somebody else's flag. Swallowing it would drop an argument
	// the CLI was meant to see.
	if _, fresh := nativeAgentStripFreshSession([]string{"--fresh-session-id"}); fresh {
		t.Error("only the exact spellings are rysh's")
	}
}

// THE POINT OF THE WHOLE FEATURE: the old transcript stops being a session, so
// `claude --session-id <pane id>` is accepted again — and it is MOVED, not
// deleted, because one typed word must not be able to destroy a conversation.
func TestFreshSessionArchivesTheTranscriptInsteadOfDeletingIt(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	const sid = "6f1c9e2a-0000-4000-8000-000000000042"

	proj := filepath.Join(cfg, "projects", "-Users-x-work")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(proj, sid+".jsonl")
	if err := os.WriteFile(live, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	archived, err := archiveClaudeSession(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 {
		t.Fatalf("expected one archived transcript, got %v", archived)
	}
	if claudeSessionExists(sid) {
		t.Error("the id must be free for --session-id again")
	}
	if strings.HasSuffix(archived[0], ".jsonl") {
		t.Errorf("an archive that still ends in .jsonl is still a session to claude: %q", archived[0])
	}
	body, err := os.ReadFile(archived[0])
	if err != nil || !strings.Contains(string(body), `"type":"user"`) {
		t.Errorf("the conversation must survive the move: %v %q", err, body)
	}
}

// Nothing to archive is the ordinary case (a pane's first `##claude
// --fresh-session`), not an error — and a session id from pane metadata must
// not be able to rename a file outside the projects directory.
func TestFreshSessionArchiveIsSafeAndQuietWithNothingToMove(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	archived, err := archiveClaudeSession("6f1c9e2a-0000-4000-8000-0000000000aa")
	if err != nil || len(archived) != 0 {
		t.Errorf("no transcript is not a failure: %v %v", archived, err)
	}
	if archived, err := archiveClaudeSession("../../etc/passwd"); err != nil || len(archived) != 0 {
		t.Errorf("a path must never be accepted as a session id: %v %v", archived, err)
	}
}

// Two resets inside one second must not overwrite the first archive — the
// second rename would otherwise take the same name and the earlier
// conversation would be gone with no trace.
func TestASecondArchiveInTheSameSecondStepsAside(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	const sid = "6f1c9e2a-0000-4000-8000-000000000043"
	proj := filepath.Join(cfg, "projects", "-Users-x-work")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(proj, sid+".jsonl")

	var seen []string
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(live, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		archived, err := archiveClaudeSession(sid)
		if err != nil || len(archived) != 1 {
			t.Fatalf("archive %d: %v %v", i, archived, err)
		}
		seen = append(seen, archived[0])
	}
	if seen[0] == seen[1] {
		t.Errorf("the second archive overwrote the first: %q", seen[0])
	}
	for _, p := range seen {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("archive missing: %q: %v", p, err)
		}
	}
}
