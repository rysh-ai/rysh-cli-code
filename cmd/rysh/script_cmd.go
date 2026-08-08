package main

// `rysh script` — run a .rysh file: a bash script whose statement-position "##"
// lines are rysh commands (design 021).
//
//	rysh script <file> [--print] [--session <name>] [--tab-id <id>]
//	            [--pane-id <id>] [--] [script args...]
//
// The file is transpiled (internal/script) and handed to bash. Because bash is
// still the interpreter, everything bash can do keeps working; the only thing
// this command adds is that "##pane info" runs a rysh command instead of being
// a comment. Run the same file with `bash` and the rysh lines go back to being
// comments — which is the point, and what makes the format honest.
//
// --print writes the transpiled bash to stdout and exits, so the author can see
// exactly what will run (and pipe it to shellcheck).

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"github.com/rysh-ai/rysh-cli-code/internal/script"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

func runScriptCmd(cfg config.Config, args []string) error {
	// args[0] == "script".
	rest := args[1:]

	rest, printOnly := extractBoolFlag(rest, "--print", "--dump")
	rest, check := extractBoolFlag(rest, "--check")
	rest, ephemeral := extractBoolFlag(rest, "--ephemeral")
	rest, keep := extractBoolFlag(rest, "--keep")
	rest, allowAttached := extractBoolFlag(rest, "--allow-attached")
	rest, sess := extractStringFlag(rest, "--session")
	rest, tabID := extractStringFlag(rest, "--tab-id")
	rest, paneID := extractStringFlag(rest, "--pane-id")

	if len(rest) == 0 {
		return errors.New(progname.Rewrite(
			"usage: rysh script <file.rysh> [--print] [--session <name>] [--pane-id <id>] [args...]"))
	}
	path := rest[0]
	scriptArgs := rest[1:]
	// A leading "--" separates rysh script's flags from the script's own args,
	// so a script can take a --session of its own.
	if len(scriptArgs) > 0 && scriptArgs[0] == "--" {
		scriptArgs = scriptArgs[1:]
	}

	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read script: %w", err)
	}

	result, err := script.Transpile(string(src))
	if err != nil {
		// Transpile errors carry a source line; name the file so the message
		// reads like every other compiler's.
		return fmt.Errorf("%s:%s", path, strings.TrimPrefix(err.Error(), "line "))
	}

	if check {
		return checkScript(path, string(src), result)
	}

	if printOnly {
		fmt.Print(result.Bash)
		if !strings.HasSuffix(result.Bash, "\n") {
			fmt.Println()
		}
		return nil
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the rysh binary: %w", err)
	}

	if ephemeral {
		if sess != "" {
			return errors.New("--ephemeral and --session are mutually exclusive: one boots a throwaway session, the other names an existing one")
		}
		return runScriptEphemeral(cfg, path, result.Bash, self, tabID, paneID, scriptArgs, keep)
	}
	if keep {
		return errors.New("--keep only means something with --ephemeral")
	}
	if sess == "" {
		sess = cfg.SessionName
	}
	if err := refuseIfAttached(cfg, sess, allowAttached); err != nil {
		return err
	}

	return execTranspiled(path, result.Bash, self, sess, tabID, paneID, scriptArgs)
}

// refuseIfAttached stops a script from running against a session somebody is
// sitting in front of.
//
// A ## command FOCUSES the pane it targets — handleCLIRyshCommand resolves and
// focuses so the command behaves exactly as if typed there. That is right for a
// single command and hostile for a script: a loop over four panes yanks the
// user's cursor around four times, and a script that creates tabs rearranges
// the workspace under their hands. Worse, it is silent — the script looks like
// it worked, and the human wonders what happened to their screen.
//
// Refusing by default is the honest behaviour; --ephemeral is the right answer
// for CI, and --allow-attached is there for the case where the operator knows
// what they are doing and means it.
func refuseIfAttached(cfg config.Config, sessName string, allow bool) error {
	if allow {
		return nil
	}
	store, err := session.NewStore(cfg)
	if err != nil {
		// Not our error to report: the script is about to fail on the same
		// store with a better message.
		return nil
	}
	rec, err := store.Get(sessName)
	if err != nil {
		return nil // no such session yet; let the script's first command say so
	}
	attached := len(rec.AliveTUIPIDs())
	if attached == 0 && rec.AppClients == 0 {
		return nil
	}
	how := fmt.Sprintf("%d TUI", attached)
	if rec.AppClients > 0 {
		how = fmt.Sprintf("%s, %d app client(s)", how, rec.AppClients)
	}
	return fmt.Errorf(progname.Rewrite(
		"session %q has someone attached (%s).\n"+
			"  A ## command focuses the pane it targets, so a script would move their cursor\n"+
			"  and rearrange their workspace as it runs.\n"+
			"  Use --ephemeral for a throwaway session, name a detached one with --session,\n"+
			"  or pass --allow-attached if you mean to drive this one."), sessName, how)
}

// runScriptEphemeral boots a throwaway session for the script's lifetime and
// tears it down afterwards.
//
// This is the hermetic mode: CI wants a script that cannot disturb — or be
// disturbed by — whatever the developer has on screen. It also sidesteps the
// focus-stealing problem, because a ## command focuses the pane it targets
// (handleCLIRyshCommand), which against a live attached session yanks the
// user's cursor around as the script runs.
//
// The session lifecycle is `rysh run`'s, reused verbatim: same spawn, same
// readiness wait, same teardown that stops the daemon before deleting its
// record so KV is flushed rather than abandoned.
func runScriptEphemeral(cfg config.Config, path, bash, self, tabID, paneID string, scriptArgs []string, keep bool) error {
	store, err := session.NewStore(cfg)
	if err != nil {
		return err
	}

	sessionName := fmt.Sprintf("script-%d", time.Now().UnixNano())
	h, err := spawnDaemon(sessionName, cfg.LogLevel, cfg.ConfigFile, false)
	if err != nil {
		return err
	}
	defer h.cleanup()

	rec, err := waitForSession(store, sessionName, scriptBootTimeout, h)
	if err != nil {
		cleanupRunSession(store, sessionName)
		return daemonStartError(sessionName, err)
	}
	fmt.Fprintf(os.Stderr, "[script] session %s up (PID %d, NATS %d)\n", sessionName, rec.PID, rec.NATSPort)

	// Teardown has to run on BOTH exits: the normal return and the os.Exit that
	// carries a failing script's exit code out of execTranspiled. A defer only
	// covers the first, so teardown is a function called from each — and
	// sync.Once makes running it twice harmless.
	var once sync.Once
	teardown := func() {
		once.Do(func() {
			if keep {
				fmt.Fprintf(os.Stderr, progname.Rewrite("[script] --keep: session %s left running; attach with \"rysh attach %s\"\n"),
					sessionName, sessionName)
				return
			}
			cleanupRunSession(store, sessionName)
			fmt.Fprintf(os.Stderr, "[script] session %s stopped and deleted\n", sessionName)
		})
	}
	defer teardown()

	return execTranspiledExit(path, bash, self, sessionName, tabID, paneID, scriptArgs, func(code int) {
		teardown()
		os.Exit(code)
	})
}

// scriptBootTimeout bounds daemon spawn + registry readiness for --ephemeral.
const scriptBootTimeout = 20 * time.Second

// checkScript verifies both halves of the .rysh contract and reports what it
// found, exiting non-zero if either fails.
//
// The transpile half is trivially checked (we just did it). The interesting
// half is the POLYGLOT property: that the file is also valid bash, with its ##
// lines inert. That claim is easy to make and easy to break — a rysh command
// that is the sole statement of an if/for/while body leaves bash with an empty
// block and a syntax error — so it is checked by asking bash itself, not by
// reasoning about it. Any line that needs the `: ##cmd` form is named.
func checkScript(path, src string, result *script.Result) error {
	fmt.Printf("%s: %d rysh command(s), %d line(s)\n", path, len(result.RyshLines), countLines(result.Bash))

	if _, err := exec.LookPath("bash"); err != nil {
		fmt.Println("  polyglot: SKIPPED (no bash on PATH to check with)")
		return nil
	}

	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(src)
	out, err := cmd.CombinedOutput()
	if err == nil {
		fmt.Println("  polyglot: OK (valid bash; ## lines are inert comments)")
		return nil
	}

	fmt.Println("  polyglot: FAILED — this file does not parse as plain bash:")
	for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		fmt.Printf("    %s\n", l)
	}
	fmt.Println("    A ## line is a comment to bash, so one used as the ONLY statement of an")
	fmt.Println("    if/for/while/case/function body leaves bash with an empty block. Write it")
	fmt.Println("    as `: ##command` there — `:` is bash's no-op, and rysh still runs it.")
	fmt.Println("    (The script still runs correctly under `rysh script`; only the")
	fmt.Println("     bash-compatibility half of the contract is broken.)")
	return errors.New("script is not valid bash")
}

// countLines counts the lines in s the way `wc -l` would.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n")
}

// execTranspiled writes the prelude + transpiled body to a temp dir and runs
// bash over them, replacing this process's stdio.
//
// The body keeps the source file's BASE NAME so bash's diagnostics name
// something the author recognises ("deploy.rysh: line 12: ..."), and the
// prelude sources it rather than inlining so those line numbers stay true.
func execTranspiled(srcPath, bash, self, sess, tabID, paneID string, scriptArgs []string) error {
	return execTranspiledExit(srcPath, bash, self, sess, tabID, paneID, scriptArgs, os.Exit)
}

// scriptEnv builds the environment a transpiled script runs under.
//
// Targeting is set only where it was actually resolved. A pane's shell exports
// its own identity (RYSH_SESSION / RYSH_TAB / RYSH_LANE / RYSH_STACK /
// RYSH_PANE), so writing an empty value here would not mean "unset" — it would
// OVERWRITE the pane's own address, and a script run inside a pane with no
// --pane-id would go back to addressing whichever pane is active. It would look
// like it worked, and land somewhere else.
//
// Inherited coordinates are kept only while they still describe the target.
// They stop describing it in two ways, and an id that survives either one is
// worse than no id: it resolves somewhere, just not where the script meant.
//
//   - Another session. `--ephemeral` boots a throwaway one and `--session`
//     names someone else's; the pane we were started from does not exist there.
//   - Explicit targeting. --pane-id/--tab-id name a pane that may sit in another
//     tab, lane and stack, so whatever was not pinned is no longer implied.
func scriptEnv(base []string, self, sess, tabID, paneID, absSrc string) []string {
	env := append(base,
		"RYSH_BIN="+self,
		"RYSH_SCRIPT="+absSrc,
		"RYSH_SCRIPT_VERSION="+script.Version,
	)
	inherited := envValue(base, "RYSH_SESSION")
	elsewhere := sess != "" && inherited != "" && sess != inherited
	pinned := tabID != "" || paneID != ""

	if sess != "" {
		env = append(env, "RYSH_SESSION="+sess)
	}
	// Written even when empty in these cases: empty means "unset" to the
	// prelude, whereas leaving the inherited value is what points a script at
	// the wrong pane.
	if pinned || elsewhere {
		env = append(env,
			"RYSH_TAB="+tabID,
			"RYSH_PANE="+paneID,
			"RYSH_LANE=",
			"RYSH_STACK=",
		)
	}
	return env
}

// envValue reads a variable from an environment slice the way exec does: the
// last assignment wins.
func envValue(env []string, name string) string {
	val := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			val = kv[len(name)+1:]
		}
	}
	return val
}

// execTranspiledExit is execTranspiled with the exit path injected, so
// --ephemeral can tear its session down before the process goes away.
func execTranspiledExit(srcPath, bash, self, sess, tabID, paneID string, scriptArgs []string, exit func(int)) error {
	dir, err := os.MkdirTemp("", "rysh-script-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	bodyPath := filepath.Join(dir, filepath.Base(srcPath))
	if err := os.WriteFile(bodyPath, []byte(bash), 0o600); err != nil {
		return fmt.Errorf("write transpiled script: %w", err)
	}
	preludePath := filepath.Join(dir, "prelude.sh")
	if err := os.WriteFile(preludePath, []byte(script.Prelude(bodyPath)), 0o600); err != nil {
		return fmt.Errorf("write prelude: %w", err)
	}

	absSrc, err := filepath.Abs(srcPath)
	if err != nil {
		absSrc = srcPath
	}

	cmd := exec.Command("bash", append([]string{preludePath}, scriptArgs...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = scriptEnv(os.Environ(), self, sess, tabID, paneID, absSrc)

	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// The script's exit code is the command's exit code — that is the
			// whole point of threading status through the ## layer.
			exit(ee.ExitCode())
		}
		return fmt.Errorf("run script: %w", err)
	}
	return nil
}
