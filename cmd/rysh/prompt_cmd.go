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

func runPromptCmd(cfg config.Config, args []string) error {
	// args[0] == "prompt".
	rest := args[1:]

	rest, sessName := extractStringFlag(rest, "--session")
	rest, paneID := extractStringFlag(rest, "--pane-id")
	rest, timeoutStr := extractStringFlag(rest, "--timeout")
	rest, asJSON := extractBoolFlag(rest, "--json")
	rest, quiet := extractBoolFlag(rest, "--quiet")

	if i := indexOf(rest, "--"); i >= 0 {
		rest = rest[i+1:]
	}
	text := strings.TrimSpace(strings.Join(rest, " "))
	if text == "" {
		return errors.New(progname.Rewrite(
			"usage: rysh prompt [--session <name>] [--pane-id <id>] [--timeout <dur>] [--json] [--quiet] -- '<prompt>'"))
	}

	timeout := promptDefaultTimeout
	if timeoutStr != "" {
		d, err := time.ParseDuration(timeoutStr)
		if err != nil {
			return fmt.Errorf("invalid --timeout %q: %w", timeoutStr, err)
		}
		timeout = d
	}

	if sessName == "" {
		sessName = cfg.SessionName
	}
	store, err := session.NewStore(cfg)
	if err != nil {
		return err
	}

	outcome, output := awaitPromptTurn(store, sessName, paneID, text, timeout, quiet)

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
func awaitPromptTurn(store *session.Store, sessName, paneID, text string, timeout time.Duration, quiet bool) (runOutcome, string) {
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

	if err := submitPrompt(c, paneID, text); err != nil {
		return runOutcome{Status: "error", ExitCode: runExitError, Detail: err.Error()}, ""
	}

	outcome := decideRunOutcome(events, time.After(timeout))
	return outcome, collected.String()
}

// submitPrompt routes the prompt to a specific pane, or to the workspace (which
// forwards to the active pane) when none is named. This mirrors PaneSendInput —
// the PaneActor only understands the typed exec messages, so a MsgSubmitInput
// sent straight to a pane inbox would be dropped.
func submitPrompt(c *cli.Client, paneID, text string) error {
	if paneID != "" {
		if err := c.SendToSubject(msg.T("pane", paneID, "inbox"), &msg.MsgPaneExecPrompt{Prompt: text}); err != nil {
			return fmt.Errorf("submit prompt to pane %s: %w", paneID, err)
		}
		return nil
	}
	if err := c.Send(&msg.MsgSubmitInput{Text: text, Mode: "prompt"}); err != nil {
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
