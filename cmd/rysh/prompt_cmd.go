// SPDX-License-Identifier: Apache-2.0

package main

// `rysh prompt` — submit a prompt to a pane in a RUNNING session and block
// until the agentic turn finishes (design 021 §3.7).
//
// This is the verb the CLI was missing. `rysh send` submits and returns
// immediately, so a script has no idea when — or whether — the work finished.
// `rysh run` does wait, but boots a throwaway session, so it cannot drive the
// session you are actually working in. Neither can express "ask the agent in
// this pane to do a thing, and tell me how it went", which is the single most
// useful thing a .rysh script can do.
//
//	rysh prompt [--session <n>] [--pane-id <p>] [--timeout <dur>] [--json]
//	            [--quiet] -- '<prompt text>'
//
// Exit codes are `rysh run`'s, deliberately — a script should not have to learn
// two tables (see decideRunOutcome):
//
//	0  done          2  paused/partial     4  budget exhausted
//	1  error         3  gate-blocked       5  timeout
//
// Fail-closed on approval gates matches `rysh run` and is the safe default for
// a script: even when a human happens to be attached to the session, nothing
// guarantees they are watching, and a script that blocks forever on an
// unnoticed prompt is worse than one that exits 3.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// promptDefaultTimeout bounds a turn when --timeout is not given. Longer than
// `rysh run`'s because the session is already up: the whole budget is the
// agent's, none of it is daemon boot.
const promptDefaultTimeout = 15 * time.Minute

// promptOptions is one parsed `rysh prompt`.
type promptOptions struct {
	Session, PaneID string
	Timeout         time.Duration
	JSON, Quiet     bool
	// Literal: no prefix of the text ("##>") is read as a directive
	// (MsgPaneExecPrompt.Literal). Needs --pane-id: it rides the pane message.
	Literal bool
	Text    string
}

const promptUsage = "usage: rysh prompt [--session <name>] [--pane-id <id>] [--timeout <dur>] [--json] [--quiet] [--literal] (--stdin | -- '<prompt>')"

// parsePromptArgs parses `rysh prompt`. With --stdin the text is stdin,
// exactly (the hosted runner sends API text this way, never in an argv).
func parsePromptArgs(args []string, stdin io.Reader) (promptOptions, error) {
	o := promptOptions{Timeout: promptDefaultTimeout}
	rest := args[1:]
	var timeoutStr string
	var fromStdin bool
	rest, o.Session = extractStringFlag(rest, "--session")
	rest, o.PaneID = extractStringFlag(rest, "--pane-id")
	rest, timeoutStr = extractStringFlag(rest, "--timeout")
	rest, o.JSON = extractBoolFlag(rest, "--json")
	rest, o.Quiet = extractBoolFlag(rest, "--quiet")
	rest, o.Literal = extractBoolFlag(rest, "--literal")
	rest, fromStdin = extractBoolFlag(rest, "--stdin")
	if i := indexOf(rest, "--"); i >= 0 {
		rest = rest[i+1:]
	}
	if fromStdin {
		if len(rest) > 0 {
			return o, errors.New(progname.Rewrite("--stdin and prompt text are mutually exclusive\n" + promptUsage))
		}
		b, err := io.ReadAll(io.LimitReader(stdin, promptMaxStdin+1))
		if err != nil {
			return o, fmt.Errorf("read stdin: %w", err)
		}
		if len(b) > promptMaxStdin {
			return o, fmt.Errorf("the prompt on stdin is longer than %d bytes", promptMaxStdin)
		}
		o.Text = string(b)
	} else {
		o.Text = strings.TrimSpace(strings.Join(rest, " "))
	}
	if strings.TrimSpace(o.Text) == "" {
		return o, errors.New(progname.Rewrite(promptUsage))
	}
	if o.Literal && o.PaneID == "" {
		return o, errors.New(progname.Rewrite("--literal needs --pane-id\n" + promptUsage))
	}
	if timeoutStr != "" {
		d, err := time.ParseDuration(timeoutStr)
		if err != nil {
			return o, fmt.Errorf("invalid --timeout %q: %w", timeoutStr, err)
		}
		o.Timeout = d
	}
	return o, nil
}

// promptMaxStdin caps a prompt read from stdin.
const promptMaxStdin = 1 << 20

// promptMessage is the pane message for one prompt.
func promptMessage(text string, literal bool) *msg.MsgPaneExecPrompt {
	return &msg.MsgPaneExecPrompt{Prompt: text, Literal: literal}
}

func runPromptCmd(cfg config.Config, args []string) error {
	o, err := parsePromptArgs(args, os.Stdin)
	if err != nil {
		return err
	}
	sessName, paneID, text, timeout, asJSON, quiet := o.Session, o.PaneID, o.Text, o.Timeout, o.JSON, o.Quiet

	if sessName == "" {
		sessName = cfg.SessionName
	}
	store, err := session.NewStore(cfg)
	if err != nil {
		return err
	}

	outcome, output := awaitPromptTurn(store, sessName, paneID, text, timeout, quiet, o.Literal)

	if asJSON {
		fmt.Println(promptResultJSON(outcome, sessName, output))
	} else if outcome.Detail != "" && outcome.ExitCode != runExitDone {
		fmt.Fprintf(os.Stderr, "[prompt] %s\n", outcome.Detail)
	}
	if outcome.ExitCode != runExitDone {
		os.Exit(outcome.ExitCode)
	}
	return nil
}

// awaitPromptTurn subscribes first, submits second, and waits for a terminal
// phase.
//
// The ordering is not incidental: a fast turn can reach "done" before a
// subscription registered afterwards would see anything, and the command would
// hang until its timeout on work that had already finished.
func awaitPromptTurn(store *session.Store, sessName, paneID, text string, timeout time.Duration, quiet, literal bool) (runOutcome, string) {
	c, err := cli.Connect(store, sessName)
	if err != nil {
		return runOutcome{Status: "error", ExitCode: runExitError, Detail: err.Error()}, ""
	}
	defer c.Close()

	events := make(chan runEvent, 256)
	pushEvent := func(ev runEvent) {
		select {
		case events <- ev:
		default:
		}
	}

	var collected strings.Builder

	subStatus, err := c.Subscribe(msg.T("pane", "*", "llm_prompt_execution", "status"), func(decoded interface{}) {
		if st, ok := decoded.(*msg.MsgAgenticStatus); ok {
			pushEvent(runEvent{Kind: runEvPhase, Phase: st.Phase})
		}
	})
	if err != nil {
		return runOutcome{Status: "error", ExitCode: runExitError, Detail: "subscribe status: " + err.Error()}, ""
	}
	defer func() { _ = subStatus.Unsubscribe() }()

	subOutput, err := c.Subscribe(msg.T("pane", "*", "llm_prompt_execution", "output"), func(decoded interface{}) {
		out, ok := decoded.(*msg.MsgAgenticOutput)
		if !ok {
			return
		}
		switch out.Type {
		case "text":
			collected.WriteString(out.Content)
			if !quiet {
				fmt.Print(out.Content)
			}
		case "error":
			if !quiet {
				fmt.Fprintf(os.Stderr, "[agent error] %s\n", strings.TrimSpace(out.Content))
			}
		}
	})
	if err != nil {
		return runOutcome{Status: "error", ExitCode: runExitError, Detail: "subscribe output: " + err.Error()}, ""
	}
	defer func() { _ = subOutput.Unsubscribe() }()

	subApproval, err := c.Subscribe(msg.T("pane", "*", "approval", "request"), func(decoded interface{}) {
		if req, ok := decoded.(*msg.MsgApprovalRequest); ok {
			pushEvent(runEvent{
				Kind:   runEvApproval,
				Detail: fmt.Sprintf("approval requested (%s): %s", req.Type, req.Description),
			})
		}
	})
	if err != nil {
		return runOutcome{Status: "error", ExitCode: runExitError, Detail: "subscribe approvals: " + err.Error()}, ""
	}
	defer func() { _ = subApproval.Unsubscribe() }()

	// Make sure the subscriptions are live on the server before submitting.
	if err := c.Flush(); err != nil {
		return runOutcome{Status: "error", ExitCode: runExitError, Detail: "flush subscriptions: " + err.Error()}, ""
	}

	if err := submitPrompt(c, paneID, text, literal); err != nil {
		return runOutcome{Status: "error", ExitCode: runExitError, Detail: err.Error()}, ""
	}

	outcome := decideRunOutcome(events, time.After(timeout))
	return outcome, collected.String()
}

// submitPrompt routes the prompt to a specific pane, or to the workspace (which
// forwards to the active pane) when none is named. This mirrors PaneSendInput —
// the PaneActor only understands the typed exec messages, so a MsgSubmitInput
// sent straight to a pane inbox would be dropped.
func submitPrompt(c *cli.Client, paneID, text string, literal bool) error {
	if paneID != "" {
		if err := c.SendToSubject(msg.T("pane", paneID, "inbox"), promptMessage(text, literal)); err != nil {
			return fmt.Errorf("submit prompt to pane %s: %w", paneID, err)
		}
		return nil
	}
	if err := c.Send(cli.SubmitInputMessage(text, "prompt")); err != nil {
		return fmt.Errorf("submit prompt: %w", err)
	}
	return nil
}

// promptResultJSON renders the machine-readable line for --json.
func promptResultJSON(o runOutcome, sessName, output string) string {
	return jsonLine(struct {
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
		Session  string `json:"session"`
		Detail   string `json:"detail,omitempty"`
		Output   string `json:"output,omitempty"`
	}{o.Status, o.ExitCode, sessName, o.Detail, output})
}
