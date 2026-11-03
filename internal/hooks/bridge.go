// SPDX-License-Identifier: Apache-2.0

package hooks

// bridge.go — the claude bridge (out/gap09-design.md §3b).
//
// rysh never sees a claude tool call: claude talks to its provider itself.
// Claude Code's own hook system is the one exact source of those events, so a
// claude pane launched by rysh with `claude_bridge: true` gets a --settings
// file (ClaudeSettings) whose hooks call `rysh hook emit`. Emit maps claude's
// event onto ours and runs the workspace's hooks.yaml right there, in the
// hook's own process — never inside the daemon, so a slow hook can delay only
// the call it guards, never the session.
//
// Claude's hook contract decides the exit codes: 0 is "go on"; 2 on PreToolUse
// refuses the call and hands stderr to the model; 2 on Stop would mean "do not
// stop", so Stop never gets it — not even for input emit cannot read, which is
// why each settings line names its event (--event). Stdout is never written: on UserPromptSubmit
// claude adds it to the conversation.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/checkpoint"
)

// claudeHookTimeout is the seconds claude waits for one `rysh hook emit`.
// Above MaxTimeout, so a hook that times out is reported by rysh (and, for
// pre-tool, blocks) rather than abandoned by claude, which would fail open.
const claudeHookTimeout = int((MaxTimeout + time.Minute) / time.Second)

// emitInputLimit bounds what is read from claude (a Write's full content can
// be large; the hooks still see it, up to this size).
const emitInputLimit = 8 << 20

type claudeHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type claudeMatcher struct {
	Matcher string       `json:"matcher,omitempty"`
	Hooks   []claudeHook `json:"hooks"`
}

// ClaudeSettings renders the --settings file for a bridged claude pane. The
// pane is not baked in: emit takes it from $RYSH_PANE, which every pane shell
// exports, else from claude's session id (rysh launches claude with
// --session-id <pane id>).
func ClaudeSettings(ryshBin, ryshDir, session string) ([]byte, error) {
	if ryshBin == "" || ryshDir == "" {
		return nil, errors.New("hooks: claude bridge needs the rysh binary and the .rysh directory")
	}
	base := shellQuote(ryshBin) + " hook emit --rysh-dir " + shellQuote(ryshDir)
	if session != "" {
		base += " --session " + shellQuote(session)
	}
	// Each line names its event, so even an input emit cannot read is
	// answered in that event's terms (FailureExit).
	one := func(event, suffix string) []claudeHook {
		cmd := base + " --event " + event + suffix
		return []claudeHook{{Type: "command", Command: cmd, Timeout: claudeHookTimeout}}
	}
	settings := map[string]any{
		"hooks": map[string][]claudeMatcher{
			// Anything but a clean 0 from rysh — not found, crashed, killed —
			// becomes 2: the gate fails closed even when rysh itself fails.
			"PreToolUse":       {{Matcher: "*", Hooks: one("PreToolUse", " || exit 2")}},
			"PostToolUse":      {{Matcher: "*", Hooks: one("PostToolUse", "")}},
			"Notification":     {{Hooks: one("Notification", "")}},
			"Stop":             {{Hooks: one("Stop", "")}},
			"UserPromptSubmit": {{Hooks: one("UserPromptSubmit", "")}},
		},
	}
	return json.MarshalIndent(settings, "", "  ")
}

// shellQuote quotes s for bash as one word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// claudeInput is the part of claude's hook input emit uses.
type claudeInput struct {
	HookEventName string          `json:"hook_event_name"`
	SessionID     string          `json:"session_id"`
	Cwd           string          `json:"cwd"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	Message       string          `json:"message"`
}

// EmitOptions are `rysh hook emit`'s flags.
type EmitOptions struct {
	RyshDir string
	PaneID  string
	Session string
	// Event is the claude hook event the settings line was written for.
	Event string
}

// claudeGateEvent is the one claude event whose exit 2 refuses something.
const claudeGateEvent = "PreToolUse"

// FailureExit is the code for a call emit cannot handle (unreadable input,
// bad flags). 2 refuses a PreToolUse, and an event that cannot be named might
// be one; for every other event 0, because 2 on Stop means "do not stop".
func FailureExit(event string) int {
	if event == "" || event == claudeGateEvent {
		return 2
	}
	return 0
}

// Emit handles one claude hook call and returns the exit code for claude.
func Emit(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, o EmitOptions) int {
	_ = stdout // never written; see the file comment
	data, err := io.ReadAll(io.LimitReader(stdin, emitInputLimit))
	var in claudeInput
	if err == nil {
		err = json.Unmarshal(data, &in)
	}
	if err == nil && o.Event != "" && in.HookEventName != "" && in.HookEventName != o.Event {
		err = fmt.Errorf("input is for %s, the line for %s", in.HookEventName, o.Event)
		if in.HookEventName == claudeGateEvent {
			o.Event = claudeGateEvent // either one gating is enough to refuse
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "rysh hook: unreadable hook input: %v\n", err)
		return FailureExit(o.Event)
	}
	if in.HookEventName == "" {
		in.HookEventName = o.Event
	}
	pane := strings.TrimSpace(o.PaneID)
	if pane == "" {
		pane = strings.TrimSpace(os.Getenv("RYSH_PANE"))
	}
	if pane == "" {
		pane = strings.TrimSpace(in.SessionID)
	}
	cfg := Load(o.RyshDir)
	env := Env{Dir: workspaceDir(o.RyshDir), Vars: []string{"RYSH_PANE=" + pane}}
	if o.Session != "" {
		env.Vars = append(env.Vars, "RYSH_SESSION="+o.Session)
	}
	p := Payload{PaneID: pane, Program: "claude", Tool: in.ToolName, Input: in.ToolInput, Cwd: in.Cwd, Reason: in.Message}

	switch in.HookEventName {
	case "PreToolUse":
		ctx, cancel := context.WithTimeout(ctx, MaxTimeout)
		defer cancel()
		if blocked, why := cfg.Gate(ctx, PreTool, p, env); blocked {
			fmt.Fprintln(stderr, why)
			return 2
		}
	case "PostToolUse":
		report(stderr, cfg.Run(ctx, PostTool, p, env))
	case "Notification":
		report(stderr, cfg.Run(ctx, NeedsYou, p, env))
	case "Stop":
		report(stderr, cfg.Run(ctx, Finish, p, env))
	case "UserPromptSubmit":
		// One checkpoint per turn, before the turn edits anything.
		if cfg.AutoCheckpoints && in.Cwd != "" {
			if store, err := checkpoint.Open(in.Cwd, pane); err == nil {
				if _, _, err := store.Save(ctx, "turn start (claude prompt)"); err != nil {
					fmt.Fprintf(stderr, "rysh hook: checkpoint: %v\n", err)
				}
			}
		}
	}
	if cfg.Err != nil {
		fmt.Fprintf(stderr, "rysh hook: %v\n", cfg.Err)
	}
	return 0
}

func report(w io.Writer, results []Result) {
	for _, r := range results {
		if r.Err != nil {
			fmt.Fprintf(w, "rysh hook %q: %v\n", r.Run, r.Err)
		}
	}
}

// workspaceDir is the directory hooks run in: the workspace root that holds
// the .rysh directory.
func workspaceDir(ryshDir string) string {
	if ryshDir == "" {
		return ""
	}
	d := strings.TrimRight(ryshDir, "/")
	if strings.HasSuffix(d, "/.rysh") {
		return strings.TrimSuffix(d, "/.rysh")
	}
	return d
}
