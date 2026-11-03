// SPDX-License-Identifier: Apache-2.0

package main

// `rysh wait` — block until a pane is idle, needs you, or has exited
// (design 069 S0), and `rysh screen` — print a pane's current screen.
//
//	rysh wait [--session <n>] --pane-id <pane> --until idle|needs-you|exited|any
//	          [--timeout <dur>] [--interval <dur>] [--json]
//	rysh screen [--session <n>] --pane-id <pane> [--json]
//
// The missing primitive for scripts and the SDKs: `rysh prompt` waits for a
// rysh-native turn, but nothing could wait on a claude or codex running in a
// pane. It is a CLI verb rather than a ## handler for the reason ##prompt is
// one (docs/scripting.md): a handler that blocked inside the WorkspaceActor
// would freeze the session. So it polls from outside, through two reads that
// already exist:
//
//   - `##pane info <pane> --json` — the pane's program and its "needs you"
//     state (root-order-044 #3), and whether the pane exists at all;
//   - the fleet watch's activity observer (actors.ObservePaneActivity) — the
//     same working/idle judgement a fleet makes.
//
// READ-ONLY: nothing here writes to a pane, answers a prompt or approves a
// gate. A pane that needs you stays needing you; the script decides.
//
// States: idle = readable, quiet, and not needing anyone; needs-you = the #3
// meta says so; exited = the pane is gone, or a program seen during this wait
// has returned to the shell; any = report one observation. An unreadable pane
// is unknown, and unknown is never idle.
//
// Exit codes are `rysh run`'s where they apply: 0 reached, 1 error (including
// waiting for idle/needs-you on a pane that is gone), 5 timeout.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/actors"
	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

const (
	waitDefaultTimeout  = 10 * time.Minute
	waitDefaultInterval = 2 * time.Second
	// waitObserveGap is the time between the observer's two screen reads; the
	// fleet watch uses the same value (afaObserveGap).
	waitObserveGap = 1200 * time.Millisecond
)

const waitUsage = "usage: rysh wait [--session <name>] --pane-id <pane> --until idle|needs-you|exited|any " +
	"[--timeout <dur>] [--interval <dur>] [--json]"

type waitOptions struct {
	Session  string
	PaneID   string
	Until    string
	Timeout  time.Duration
	Interval time.Duration
	JSON     bool
}

func parseWaitArgs(args []string) (waitOptions, error) {
	rest := args[1:]
	opts := waitOptions{Timeout: waitDefaultTimeout, Interval: waitDefaultInterval}
	var timeoutStr, intervalStr string
	rest, opts.Session = extractStringFlag(rest, "--session")
	rest, opts.PaneID = extractStringFlag(rest, "--pane-id")
	rest, opts.Until = extractStringFlag(rest, "--until")
	rest, timeoutStr = extractStringFlag(rest, "--timeout")
	rest, intervalStr = extractStringFlag(rest, "--interval")
	rest, opts.JSON = extractBoolFlag(rest, "--json")
	if len(rest) > 0 {
		return opts, fmt.Errorf("unexpected argument %q\n%s", rest[0], progname.Rewrite(waitUsage))
	}
	if opts.PaneID == "" || opts.Until == "" {
		return opts, errors.New(progname.Rewrite(waitUsage))
	}
	switch opts.Until {
	case "idle", "needs-you", "exited", "any":
	default:
		return opts, fmt.Errorf("unknown --until %q (want idle, needs-you, exited or any)", opts.Until)
	}
	if timeoutStr != "" {
		d, err := time.ParseDuration(timeoutStr)
		if err != nil || d < 0 {
			return opts, fmt.Errorf("invalid --timeout %q", timeoutStr)
		}
		opts.Timeout = d
	}
	if intervalStr != "" {
		d, err := time.ParseDuration(intervalStr)
		if err != nil || d <= 0 {
			return opts, fmt.Errorf("invalid --interval %q (want a positive duration)", intervalStr)
		}
		opts.Interval = d
	}
	return opts, nil
}

// waitObs is one look at a pane.
type waitObs struct {
	Gone      bool
	NeedsYou  bool
	Attention string
	Program   string
	Activity  string // working | idle | unknown
}

// waitOutcome is how a wait ended.
type waitOutcome struct {
	Status    string // reached | timeout | gone
	ExitCode  int
	State     string // idle | working | needs-you | exited | unknown
	Activity  string
	NeedsYou  bool
	Attention string
	Program   string
	WaitedMs  int64
}

// waitState names what a pane is doing right now. seenProgram is the latch
// that lets "exited" mean "the program I was watching is gone".
func waitState(o waitObs, seenProgram bool) string {
	switch {
	case o.Gone:
		return "exited"
	case seenProgram && o.Program == "":
		return "exited"
	case o.NeedsYou:
		return "needs-you"
	case o.Activity == "":
		return actors.PaneActivityUnknown
	}
	return o.Activity
}

// waitReached decides whether an observation satisfies --until.
func waitReached(until string, o waitObs, state string) bool {
	switch until {
	case "any":
		return true
	case "needs-you":
		return !o.Gone && o.NeedsYou
	case "idle":
		return !o.Gone && !o.NeedsYou && o.Activity == actors.PaneActivityIdle
	case "exited":
		return state == "exited"
	}
	return false
}

// waitLoop polls observe until --until holds, the pane is gone, or timeout
// passes. Pure apart from observe/now/sleep, which the tests replace.
func waitLoop(until string, observe func() waitObs, interval, timeout time.Duration,
	now func() time.Time, sleep func(time.Duration)) waitOutcome {
	start := now()
	seenProgram := false
	for {
		o := observe()
		if o.Program != "" {
			seenProgram = true
		}
		state := waitState(o, seenProgram)
		out := waitOutcome{
			State: state, Activity: o.Activity, NeedsYou: o.NeedsYou,
			Attention: o.Attention, Program: o.Program,
			WaitedMs: now().Sub(start).Milliseconds(),
		}
		if out.Activity == "" && !o.Gone {
			out.Activity = actors.PaneActivityUnknown
		}
		switch {
		case waitReached(until, o, state):
			out.Status, out.ExitCode = "reached", runExitDone
			return out
		case o.Gone:
			out.Status, out.ExitCode = "gone", runExitError
			return out
		}
		left := timeout - now().Sub(start)
		if left <= 0 {
			out.Status, out.ExitCode = "timeout", runExitTimeout
			return out
		}
		if left < interval {
			sleep(left)
		} else {
			sleep(interval)
		}
	}
}

// waitObsFromInfo turns one `##pane info <pane> --json` answer into an
// observation. Only the workspace saying "pane not found" makes a pane gone; a
// lookup that failed for any other reason is unknown. activity observes the
// resolved pane id.
func waitObsFromInfo(ok bool, output, errText string, activity func(paneID string) string) waitObs {
	if !ok {
		if strings.Contains(errText, "pane not found") {
			return waitObs{Gone: true}
		}
		return waitObs{Activity: actors.PaneActivityUnknown}
	}
	var rec actors.PaneRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &rec); err != nil || rec.ID == "" {
		return waitObs{Activity: actors.PaneActivityUnknown}
	}
	o := waitObs{NeedsYou: rec.NeedsYou, Attention: rec.Attention, Program: rec.Program,
		Activity: actors.PaneActivityUnknown}
	if activity != nil {
		o.Activity = activity(rec.ID)
	}
	return o
}

func waitResultJSON(paneID, until string, o waitOutcome) string {
	return jsonLine(struct {
		Status    string `json:"status"`
		ExitCode  int    `json:"exit_code"`
		PaneID    string `json:"pane_id"`
		Until     string `json:"until"`
		State     string `json:"state"`
		Activity  string `json:"activity"`
		NeedsYou  bool   `json:"needs_you"`
		Attention string `json:"attention,omitempty"`
		Program   string `json:"program,omitempty"`
		WaitedMs  int64  `json:"waited_ms"`
	}{o.Status, o.ExitCode, paneID, until, o.State, o.Activity, o.NeedsYou, o.Attention, o.Program, o.WaitedMs})
}

// runWaitCmd implements `rysh wait`. It returns the exit code.
func runWaitCmd(cfg config.Config, args []string) (int, error) {
	opts, err := parseWaitArgs(args)
	if err != nil {
		return runExitError, err
	}
	if opts.Session == "" {
		opts.Session = cfg.SessionName
	}
	store, err := session.NewStore(cfg)
	if err != nil {
		return runExitError, err
	}
	c, err := cli.Connect(store, opts.Session)
	if err != nil {
		return runExitError, err
	}
	defer c.Close()

	observe := func() waitObs {
		resp, err := cli.RyshCommandResult(store, opts.Session, "", "", "pane info "+opts.PaneID+" --json")
		if err != nil {
			return waitObs{Activity: actors.PaneActivityUnknown}
		}
		return waitObsFromInfo(resp.OK, resp.Output, resp.Error+"\n"+resp.Output, func(id string) string {
			return actors.ObservePaneActivity(c.Publisher(), id, waitObserveGap)
		})
	}
	o := waitLoop(opts.Until, observe, opts.Interval, opts.Timeout, time.Now, time.Sleep)

	if opts.JSON {
		fmt.Println(waitResultJSON(opts.PaneID, opts.Until, o))
	} else {
		fmt.Println(o.State)
		if o.ExitCode != runExitDone {
			fmt.Fprintf(os.Stderr, "[wait] %s: pane %s is %s after %s\n", o.Status, opts.PaneID, o.State,
				(time.Duration(o.WaitedMs) * time.Millisecond).Round(time.Millisecond))
		}
	}
	return o.ExitCode, nil
}

// runScreenCmd implements `rysh screen`: the pane's current screen rows.
func runScreenCmd(cfg config.Config, args []string) error {
	rest := args[1:]
	rest, sess := extractStringFlag(rest, "--session")
	rest, paneID := extractStringFlag(rest, "--pane-id")
	rest, asJSON := extractBoolFlag(rest, "--json")
	if paneID == "" || len(rest) > 0 {
		return errors.New(progname.Rewrite("usage: rysh screen [--session <name>] --pane-id <pane> [--json]"))
	}
	if sess == "" {
		sess = cfg.SessionName
	}
	store, err := session.NewStore(cfg)
	if err != nil {
		return err
	}
	// Resolve a given-name or title to the id first, as `rysh wait` does: the
	// screen read addresses the pane by id.
	resp, err := cli.RyshCommandResult(store, sess, "", "", "pane info "+paneID+" --json")
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("pane %s: %s", paneID, strings.TrimSpace(resp.Error+" "+resp.Output))
	}
	var rec actors.PaneRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(resp.Output)), &rec); err != nil || rec.ID == "" {
		return fmt.Errorf("pane %s: unreadable pane record", paneID)
	}
	c, err := cli.Connect(store, sess)
	if err != nil {
		return err
	}
	defer c.Close()
	rows, err := actors.ReadPaneScreen(c.Publisher(), rec.ID)
	if err != nil {
		return fmt.Errorf("read screen of pane %s: %w", rec.ID, err)
	}
	if asJSON {
		if rows == nil {
			rows = []string{}
		}
		fmt.Println(jsonLine(struct {
			PaneID string   `json:"pane_id"`
			Rows   []string `json:"rows"`
		}{rec.ID, rows}))
		return nil
	}
	for _, r := range rows {
		fmt.Println(strings.TrimRight(r, " "))
	}
	return nil
}
