// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ---------------------------------------------------------------------------
// User-defined commands: ONE hash
//
// `#nfre23 x` runs `bash .rysh/commands/nfre23 x` — a file named after the word
// under `.rysh/commands/`, with everything after the word passed to it as
// positional arguments.
//
// The prefix is the whole distinction. `##` is rysh's own vocabulary and only
// ever reaches the dispatch table; `#` is the user's and only ever reaches
// disk. Neither can be mistaken for the other, and a user cannot shadow
// `##secret` or `##policy` no matter what they name a file — which matters
// because `.rysh/commands/` travels with a checkout, so a script committed to a
// shared repo must not be able to redefine a governance command for whoever
// clones it. The single hash makes that structural rather than a lookup-order
// rule that could be got wrong later.
//
// A `#` line that does NOT resolve to a script is left alone and goes wherever
// it would have gone — in shell mode that is bash, where it has always been a
// comment. Only a word rysh can actually run is intercepted; see
// userCommandLineWord for the exact shape.
//
// The search path is ryshBaseDirs() — `./.rysh` then `$HOME/.rysh` — the same
// order skills, secrets and variables use, so a command can be per-project or
// per-user with no extra concept.
//
// Within each base the lookup is SESSION-SCOPED FIRST: session `macmini-rysh`
// resolves `.rysh/commands/macmini-rysh/<word>` before `.rysh/commands/<word>`.
// A session is the unit a user actually works in — one per machine, per fleet,
// per experiment — and a flat directory made every one of them share a
// namespace, so two sessions could not both define `#deploy` and a command
// written for one was typed by accident in another. The scoped directory is
// where `##commands new` writes, so the default is "mine"; the unscoped
// directory stays readable underneath it, which is what a command meant for
// EVERY session in the project (or, at $HOME, every session at all) is. See
// userCommandDirs.
//
// Execution is ASYNCHRONOUS. The workspace actor's mailbox is the session's
// single-threaded core: every pane snapshot, every other pane's command and
// every cron firing queue behind whatever it is doing. A user command is
// unbounded by construction — it can curl, it can build, it can sleep — so
// running it inline would freeze the whole multiplexer for its duration. Worse,
// a script that calls back into its own session (`rysh exec`, a `##` line under
// `rysh script`) would deadlock against the actor that is waiting for it. So
// the process is started on the mailbox goroutine, where the pane's identity is
// resolved, and everything after that happens on its own goroutine, publishing
// into the pane's buffers the way the PTY writer already does.
//
// Async does NOT mean the caller learns nothing. `handleRyshCommand` returns
// before the script has written a line, and the pane buffer it streams into is
// not something a CLI caller reads — so as first built, `rysh exec -- "nfre23"`
// produced no output at all, which is most of rysh's scripting surface made
// useless. The answer is a DEFERRED REPLY rather than a synchronous run:
// startUserCommand hands back a wait handle, handleRyshCommand parks it on
// w.pendingUserCommand, and the MsgCLIRyshCommand case in Receive answers the
// NATS request from a goroutine once the script exits. The actor is never
// blocked, and the caller gets the output and the exit code.
//
// The exit status is reported into the pane as well, because the two audiences
// are different and only one of them is guaranteed to exist: a `rysh exec`
// caller reads the error, a human who typed the word never does.
// ---------------------------------------------------------------------------

// userCommandsSubdir is the .rysh subdirectory holding user command scripts.
const userCommandsSubdir = "commands"

// userCommandExts are the suffixes tried after the bare name, so both
// `.rysh/commands/deploy` and `.rysh/commands/deploy.sh` answer to #deploy.
var userCommandExts = []string{"", ".sh", ".bash"}

// userCommandTimeout is a backstop, not a policy: a wedged script must not hold
// a goroutine and a process for the life of the session. Ten minutes matches
// the cap the bash tool uses for agent-issued commands.
const userCommandTimeout = 10 * time.Minute

// userCommandOutputLimit caps what one run may publish into a pane. A runaway
// loop printing to stdout would otherwise grow the pane's buffer without bound.
const userCommandOutputLimit = 1 << 20 // 1 MiB

// userCommandNamePattern is what may appear after `#`. It is deliberately
// narrower than a filename: the word arrives from user input and is joined onto
// a directory, so anything that could climb out of it — a separator, a leading
// dot, `..` — is rejected before it ever reaches the filesystem.
var userCommandNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validUserCommandName reports whether name may be resolved as a user command.
func validUserCommandName(name string) bool {
	return userCommandNamePattern.MatchString(name) && !strings.Contains(name, "..")
}

// usableSessionScope reports whether a session name may be joined onto the
// commands directory as a single path segment.
//
// It is the same rule as a command word, and for the same reason: the session
// name reaches here from configuration and from `--session` on the command
// line, so a name carrying a separator or `..` must be refused rather than
// allowed to point the lookup at some other directory. A name that fails it is
// not an error — the session simply has no scoped directory and reads and
// writes the unscoped one, which is exactly the pre-scoping behaviour.
func usableSessionScope(session string) bool {
	return validUserCommandName(session)
}

// userCommandDirs returns the directories a `#` word is resolved against, in
// order: for each base, the session's own directory before the base's shared
// one.
//
// Both halves are load-bearing. The scoped directory is what keeps one
// session's commands out of another's; the shared one is what a project-wide
// command lives in, and it is also every command that existed before scoping —
// dropping it would have silently unbound every `#word` already on disk.
func userCommandDirs(bases []string, session string) []string {
	dirs := make([]string, 0, len(bases)*2)
	for _, base := range bases {
		root := filepath.Join(base, userCommandsSubdir)
		if usableSessionScope(session) {
			dirs = append(dirs, filepath.Join(root, session))
		}
		dirs = append(dirs, root)
	}
	return dirs
}

// userCommandLineWord returns the command word of a single-`#` line, or "" when
// the line is not one.
//
// The word must follow the hash with nothing between them, which is what keeps
// this from swallowing ordinary text. `#deploy` is a command; `# deploy`,
// `#!/bin/sh`, `#-x` and a bare `#` are not, and neither is anything starting
// `##` — that is rysh's own prefix and is matched before this is ever asked.
// It is the same rule `rysh script` uses to decide that a `##` line is a
// command rather than a comment (internal/script/transpile.go).
//
// Note this only reports the SHAPE. A line of that shape whose word does not
// resolve to a script is still an ordinary line; the caller checks.
func userCommandLineWord(text string) string {
	if len(text) < 2 || text[0] != '#' {
		return ""
	}
	switch c := text[1]; {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
	default:
		return ""
	}
	fields := strings.Fields(text[1:])
	if len(fields) == 0 || !validUserCommandName(fields[0]) {
		return ""
	}
	return fields[0]
}

// ryshCommandLine normalises a command body supplied by a non-interactive
// caller (`rysh exec`, the rysh_command tool) into the prefixed line
// runRyshCommand expects.
//
// A body that already carries a `#` keeps it verbatim: one hash is a user
// command and two are a built-in, and re-prefixing either would produce a line
// with three hashes that resolves to neither. A bare body is a built-in, which
// is what `rysh exec -- "pane info"` has always meant.
func ryshCommandLine(body string) string {
	if strings.HasPrefix(body, "#") {
		return body
	}
	return "##" + body
}

// userCommand is one script found on the search path.
type userCommand struct {
	name string
	path string
}

// findUserCommandIn resolves a command word to a script under the given base
// directories, returning the first hit. Only regular files count: a directory
// named `deploy` is not a command, and neither is a dangling symlink.
//
// session selects the scoped directory searched ahead of each base's shared
// one; "" (or a name that is not a single safe segment) searches only the
// shared ones. See userCommandDirs.
func findUserCommandIn(bases []string, session, name string) (string, bool) {
	if !validUserCommandName(name) {
		return "", false
	}
	for _, dir := range userCommandDirs(bases, session) {
		for _, ext := range userCommandExts {
			candidate := filepath.Join(dir, name+ext)
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
				return candidate, true
			}
		}
	}
	return "", false
}

// findUserCommand resolves a command word against the live search path, scoped
// to the session it was typed in.
func (w *WorkspaceActor) findUserCommand(name string) (string, bool) {
	return findUserCommandIn(ryshBaseDirs(), w.sessionName, name)
}

// userCommandsWriteDir is where a NEW command is created: always the
// project-local .rysh, never $HOME, and by default the calling session's own
// directory under it.
//
// Reads walk both bases, but writes must not. ryshBaseDirs drops the local base
// when ./.rysh does not exist yet, which would make the very first
// `##commands new` in a project silently create a GLOBAL command instead — the
// same promotion ryshBaseDirs' own comment warns about. The persisted-store
// layer resolves this the same way (store.go writes to ".rysh/<kind>"
// unconditionally while reading through ryshBaseDirs).
//
// session is the scope to write into; "" means the shared directory, which is
// what `##commands new --shared` asks for and what a session whose name cannot
// be a path segment falls back to.
func userCommandsWriteDir(session string) string {
	dir := filepath.Join(".rysh", userCommandsSubdir)
	if usableSessionScope(session) {
		return filepath.Join(dir, session)
	}
	return dir
}

// listUserCommandsIn enumerates the commands reachable from bases for one
// session, sorted by name. An earlier directory shadows a later one, so the
// session's own `deploy` hides the project's shared `deploy` and that hides the
// user's global one — exactly the order findUserCommandIn resolves.
//
// Another session's directory is never listed. It sits inside a directory that
// IS walked (`.rysh/commands/<other>` under `.rysh/commands`), but only regular
// files count as commands, so it is skipped for the same reason a directory
// named `deploy` is not a command.
func listUserCommandsIn(bases []string, session string) []userCommand {
	seen := map[string]string{}
	for _, dir := range userCommandDirs(bases, session) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			info, err := e.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			name := userCommandNameFor(e.Name())
			if name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = filepath.Join(dir, e.Name())
		}
	}
	out := make([]userCommand, 0, len(seen))
	for name, path := range seen {
		out = append(out, userCommand{name: name, path: path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// userCommandNameFor maps a filename to the word that invokes it, or "" when no
// word could reach it — a name with a separator or a leading dot can never be
// typed after `#`.
//
// A name that matches a built-in is fine and needs no special case: `#pane`
// and `##pane` are different words. That is what the one-hash prefix buys.
func userCommandNameFor(filename string) string {
	name := filename
	for _, ext := range userCommandExts {
		if ext != "" && strings.HasSuffix(filename, ext) {
			name = strings.TrimSuffix(filename, ext)
			break
		}
	}
	if !validUserCommandName(name) {
		return ""
	}
	return name
}

// userCommandEnv builds the environment a user command runs under.
//
// The rysh variables are appended AFTER the inherited environment for the same
// reason pane shells append theirs: an outer session's exported identity must
// not survive into a script that belongs to this one. RYSH_BIN completes the
// set — with it, and with the pane's address, a user command can drive the
// session it was typed into rather than merely run beside it.
func userCommandEnv(base []string, self, session, name, path string, coords paneCoords) []string {
	env := append(append([]string{}, base...),
		paneIdentityEnv(session, coords.TabID, coords.LaneID, coords.GroupID, coords.PaneID)...)
	env = append(env,
		"RYSH_COMMAND="+name,
		"RYSH_COMMAND_FILE="+path,
	)
	if self != "" {
		env = append(env, "RYSH_BIN="+self)
	}
	return env
}

// startUserCommand launches a user command and returns immediately.
//
// Everything that needs the actor — the pane's coordinates, the session name,
// the output routing — is captured here, on the mailbox goroutine. sink is the
// same publisher handleRyshCommand's own output goes through, so a script's
// stdout lands in whichever buffers the command would have printed to.
//
// The returned wait blocks until the script exits and yields everything it
// printed plus its failure, if any. Interactive callers drop it — the pane has
// already had the output streamed into it. The CLI keeps it: `rysh exec` has a
// caller waiting on the other end of a NATS reply, and answering it with an
// empty string would make a user command look like it did nothing. Waiting is
// safe only because it happens OFF this goroutine; see the file header.
//
// It may be called at most once.
func (w *WorkspaceActor) startUserCommand(paneID, name, path string, args []string, sink func(string)) func() (string, error) {
	coords, _ := w.findPaneCoords(paneID)
	self, _ := os.Executable()
	env := userCommandEnv(os.Environ(), self, w.sessionName, name, path, coords)
	dir := w.baseDir()

	type finished struct {
		out string
		err error
	}
	// Buffered, so the run completes and is collected even when nobody waits.
	done := make(chan finished, 1)
	go func() {
		out, err := runUserCommand(name, path, args, env, dir, sink)
		done <- finished{out, err}
	}()
	return func() (string, error) {
		f := <-done
		return f.out, f.err
	}
}

// takePendingUserCommand returns and clears the wait handle left by the ##
// command that just ran, or nil when it was not a user command.
func (w *WorkspaceActor) takePendingUserCommand() func() (string, error) {
	wait := w.pendingUserCommand
	w.pendingUserCommand = nil
	return wait
}

// mergeUserCommandResponse folds a finished user command into the response the
// CLI path had already built. That response carries whatever the dispatch layer
// printed before the script started (normally nothing) and, more importantly,
// the failures that mean the command never ran at all — an unresolvable pane,
// an empty command — which must survive rather than be overwritten by a script
// result that does not exist.
func mergeUserCommandResponse(resp *msg.MsgCLIResponse, out string, err error) *msg.MsgCLIResponse {
	if resp == nil || !resp.OK {
		return resp
	}
	merged := *resp
	merged.Output += out
	if err != nil {
		merged.OK = false
		merged.Error = err.Error()
	}
	return &merged
}

// runUserCommand runs the script, publishing its output as it arrives and
// returning the same text together with the script's failure. It is
// package-level and takes everything it needs so it can be tested without an
// actor.
func runUserCommand(name, path string, args, env []string, dir string, sink func(string)) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), userCommandTimeout)
	defer cancel()

	// Everything published is also kept, so a caller that waited (the CLI) gets
	// the same text the pane saw. Bounded by userCommandOutputLimit, which is
	// what keeps "kept" from meaning "unbounded".
	var kept strings.Builder
	emit := func(s string) {
		kept.WriteString(s)
		sink(s)
	}

	// Run "bash <file>" rather than executing the file directly: the contract is
	// "it is a bash script", so neither an execute bit nor a shebang line should
	// be something the author has to remember. Arguments follow the path, so the
	// script sees them as $1, $2, ... the way any bash script does.
	cmd := exec.CommandContext(ctx, "bash", append([]string{path}, args...)...)
	cmd.Dir = dir
	cmd.Env = env
	// No stdin: the pane's terminal belongs to the pane's own shell, and a script
	// that reads from it would block forever behind a prompt nobody can answer.
	cmd.Stdin = nil

	out := &userCommandOut{sink: emit, limit: userCommandOutputLimit}
	cmd.Stdout = out
	// Same writer for both, which is what makes os/exec share ONE pipe between
	// them: two pipes would mean two copier goroutines writing to out at once.
	cmd.Stderr = cmd.Stdout
	// A script that leaves a background child holding the output pipe would
	// otherwise keep Wait blocked long after the script itself exited.
	cmd.WaitDelay = 3 * time.Second

	err := cmd.Run()
	out.finish()

	// The exit status is reported into the pane as well as returned, because the
	// two audiences are different and only one of them is guaranteed to exist: a
	// `rysh exec` caller reads the error, while someone who typed ##name into a
	// pane never sees it. A failure that only one of them learns about is the
	// worst outcome this feature could have.
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		emit(fmt.Sprintf("\n[rysh] #%s timed out after %s\n", name, userCommandTimeout))
		return kept.String(), fmt.Errorf("#%s timed out after %s", name, userCommandTimeout)
	case err == nil:
		if out.truncated {
			emit(fmt.Sprintf("\n[rysh] #%s output truncated at %d bytes\n", name, userCommandOutputLimit))
		}
		return kept.String(), nil
	default:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			emit(fmt.Sprintf("\n[rysh] #%s exited %d\n", name, ee.ExitCode()))
			return kept.String(), fmt.Errorf("#%s exited %d", name, ee.ExitCode())
		}
		emit(fmt.Sprintf("\n[rysh] #%s failed to run: %v\n", name, err))
		return kept.String(), fmt.Errorf("#%s failed to run: %w", name, err)
	}
}

// userCommandOut publishes a script's output as it arrives, up to a limit.
//
// Only one goroutine writes to it (os/exec's single copier for the shared
// stdout/stderr pipe), so it needs no lock.
type userCommandOut struct {
	sink      func(string)
	limit     int
	written   int
	truncated bool
	lastByte  byte
}

func (u *userCommandOut) Write(p []byte) (int, error) {
	if u.truncated {
		// Keep draining: returning an error here would make the script's next
		// write fail with EPIPE, turning "too chatty" into "killed".
		return len(p), nil
	}
	chunk := p
	if remaining := u.limit - u.written; len(chunk) > remaining {
		chunk = chunk[:remaining]
		u.truncated = true
	}
	if len(chunk) > 0 {
		u.written += len(chunk)
		u.lastByte = chunk[len(chunk)-1]
		u.sink(string(chunk))
	}
	return len(p), nil
}

// finish closes off output that did not end in a newline, so the next thing
// printed into the pane starts on its own line.
func (u *userCommandOut) finish() {
	if u.written > 0 && u.lastByte != '\n' {
		u.sink("\n")
	}
}

// ---------------------------------------------------------------------------
// ##commands
// ---------------------------------------------------------------------------

const userCommandTemplate = `#!/usr/bin/env bash
# %s — a rysh user command. Run it by typing #%s in any pane.
#
# Arguments after the command word arrive as "$@".
# The pane this was typed in is addressable as $RYSH_PANE (also $RYSH_TAB,
# $RYSH_LANE, $RYSH_STACK, $RYSH_SESSION), and $RYSH_BIN is the rysh binary,
# so a command can drive the session it was called from.
set -euo pipefail

echo "hello from #%s ${*:-}"
`

// handleUserCommandsCommand implements ##commands.
func (w *WorkspaceActor) handleUserCommandsCommand(out *strings.Builder, args []string) error {
	o := ryshWriter(out)
	sub := "list"
	if len(args) > 0 && args[0] != "" {
		sub = args[0]
	}
	switch sub {
	case "list":
		return w.userCommandsList(o)
	case "path":
		return w.userCommandsPath(o)
	case "show":
		return w.userCommandsShow(o, args[1:])
	case "new":
		return w.userCommandsNew(o, args[1:])
	default:
		o.Unknown("commands", sub,
			"##commands list", "##commands path",
			"##commands show <name>", "##commands new <name> [--shared]")
		return fmt.Errorf("unknown subcommand for ##commands: %q", sub)
	}
}

func (w *WorkspaceActor) userCommandsList(o ryshOut) error {
	cmds := listUserCommandsIn(ryshBaseDirs(), w.sessionName)
	o.Header("user commands (%d) — session %s", len(cmds), w.sessionName)
	o.Rule()
	if len(cmds) == 0 {
		o.Row("  none yet — ##commands new <name> creates one")
		o.Rule()
		return nil
	}
	// No shadowing to report: #name and ##name are different words, so a file
	// may be called `pane` or `secret` and still run.
	for _, c := range cmds {
		o.Row("  %-20s %s", "#"+c.name, c.path)
	}
	o.Rule()
	return nil
}

func (w *WorkspaceActor) userCommandsPath(o ryshOut) error {
	o.Header("user commands are looked up in order")
	o.Field("session", "%s", w.sessionName)
	o.Rule()
	// Walked base by base rather than over userCommandDirs, so each row can name
	// its scope without inferring it from the path — a session named `commands`
	// would make that inference wrong.
	for _, base := range ryshBaseDirs() {
		root := filepath.Join(base, userCommandsSubdir)
		if usableSessionScope(w.sessionName) {
			w.userCommandsPathRow(o, "session", filepath.Join(root, w.sessionName))
		}
		w.userCommandsPathRow(o, "shared", root)
	}
	o.Rule()
	o.Field("new ones go", "%s", userCommandsWriteDir(w.sessionName))
	o.Row("  a file named <word> (or <word>.sh) runs as #<word>")
	o.Row("  ##commands new <name> --shared writes to %s instead,", userCommandsWriteDir(""))
	o.Row("  where every session in this project can see it")
	o.Row("  one hash is yours, two are rysh's — ##help lists the built-ins")
	return nil
}

// userCommandsPathRow prints one search-path row for ##commands path.
func (w *WorkspaceActor) userCommandsPathRow(o ryshOut, scope, dir string) {
	state := "missing"
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		state = "present"
	}
	o.Row("  %-8s %-8s %s", state, scope, dir)
}

func (w *WorkspaceActor) userCommandsShow(o ryshOut, args []string) error {
	if len(args) == 0 || args[0] == "" {
		o.UsageLine("##commands show <name>")
		return fmt.Errorf("##commands show requires a name")
	}
	name := args[0]
	if !validUserCommandName(name) {
		o.Warn("not a usable command name: %q", name)
		return fmt.Errorf("invalid user command name: %q", name)
	}
	path, ok := w.findUserCommand(name)
	if !ok {
		o.Warn("no user command %q — ##commands list shows what exists", name)
		return fmt.Errorf("no user command %q", name)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		o.Warn("cannot read %s: %v", path, err)
		return fmt.Errorf("read %s: %w", path, err)
	}
	o.Header("#%s", name)
	o.Field("file", "%s", path)
	o.Rule()
	o.Row("%s", strings.TrimRight(string(body), "\n"))
	o.Rule()
	return nil
}

func (w *WorkspaceActor) userCommandsNew(o ryshOut, args []string) error {
	// --shared writes the project-wide directory instead of this session's, for
	// a command every session should answer to. It is a flag rather than a
	// second subcommand because it changes one thing about the same operation,
	// and it may sit on either side of the name the way any flag may.
	args, shared := extractUserCommandShared(args)
	if len(args) == 0 || args[0] == "" {
		o.UsageLine("##commands new <name> [--shared]")
		return fmt.Errorf("##commands new requires a name")
	}
	name := args[0]
	if !validUserCommandName(name) {
		o.Warn("not a usable command name: %q (letters, digits, . _ - and must start with a letter or digit)", name)
		return fmt.Errorf("invalid user command name: %q", name)
	}
	// A built-in of the same name is NOT refused: #pane and ##pane are different
	// words, so the file runs. The earlier cut had to refuse it, back when both
	// lived under ##; the one-hash prefix removed the collision entirely.
	//
	// An existing command is refused whatever scope it is in, including a shared
	// one this session would now shadow. Creating a file that silently takes
	// over a word already answering in this session is the surprise the scoping
	// change could most easily introduce, so `new` names the file it found and
	// leaves the decision to the user.
	if existing, ok := w.findUserCommand(name); ok {
		o.Warn("#%s already exists: %s", name, existing)
		return fmt.Errorf("#%s already exists", name)
	}

	scope := w.sessionName
	if shared {
		scope = ""
	}
	dir := userCommandsWriteDir(scope)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		o.Warn("cannot create %s: %v", dir, err)
		return fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, name)
	body := fmt.Sprintf(userCommandTemplate, name, name, name)
	// 0755 so the file is also runnable on its own — rysh invokes it through
	// bash, but the whole point is that it is an ordinary script.
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		o.Warn("cannot write %s: %v", path, err)
		return fmt.Errorf("write %s: %w", path, err)
	}
	o.Header("created #%s", name)
	o.Field("file", "%s", path)
	if shared || !usableSessionScope(w.sessionName) {
		o.Field("scope", "shared — every session in this project sees it")
	} else {
		o.Field("scope", "session %s (##commands new %s --shared for every session)", w.sessionName, name)
	}
	o.Row("  edit it, then run it by typing #%s", name)
	return nil
}

// extractUserCommandShared pulls a --shared flag out of ##commands new's
// arguments, returning the rest. The flag may appear anywhere, since the
// command line is split on whitespace with no notion of flag order.
func extractUserCommandShared(args []string) ([]string, bool) {
	rest := make([]string, 0, len(args))
	shared := false
	for _, a := range args {
		if a == "--shared" {
			shared = true
			continue
		}
		rest = append(rest, a)
	}
	return rest, shared
}
