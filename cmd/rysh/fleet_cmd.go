// SPDX-License-Identifier: Apache-2.0

package main

// `rysh fleet` — the registry's command-line surface (design 028 §6.5, `E-40`).
//
// THIS IS THE SURFACE `fleetctl` READS AND WRITES. The manifest at
// `.rysh/fleet/<name>.json` stays as a cache, but it stops being the truth: it
// is an untracked file shared by every fleet in the workspace, and two live
// failures came from exactly that — a sibling fleet's teardown deleted a live
// fleet's manifest, and a `worktree` step reported `created: true` for a
// directory that did not exist. A registry owned by the daemon can be wrong
// about the world, but it cannot be silently deleted by a neighbour.
//
// Every subcommand prints JSON with --json so a script can be written against
// it, and prose otherwise so a human reading a pane is not parsing braces.
//
// Exit codes: 0 done, 1 refused or unreachable. AN UNANSWERED QUERY IS NOT AN
// EMPTY SESSION and exits non-zero with nothing on stdout — the same rule
// `rysh board tail` follows, because `rysh fleet ls | jq length` returning 0 on
// a dead daemon is the most convincing way to be wrong.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

const fleetUsage = "usage: rysh fleet ls [--json] [--session <name>]\n" +
	"       rysh fleet show <name> [--json] [--session <name>]\n" +
	"       rysh fleet register <name> [--board <id>] [--source <path>] [--roadmap <dir>] [--tab <id>]\n" +
	"       rysh fleet state <name> registered|up|down\n" +
	"       rysh fleet member add <fleet> --pane <id> [--role <r>] [--label <l>] [--unit <u>]\n" +
	"                                     [--parent <pane-id>] [--worktree <path>] [--session-id <id>]\n" +
	"       rysh fleet member rm <fleet> --pane <id>\n" +
	"       rysh fleet forget <name>\n" +
	"       rysh fleet status [<fleet>] [--json]           the status ledger: work, activity, health, blockers\n" +
	"       rysh fleet status <idle|progress|blocked|done> [--as <pane>] [--fleet <f>] [--task <t>]\n" +
	"                         [--blocked-by <who>[,<who>]] [--note <n>] [--pct 0-100] [--eta 20m]\n" +
	"                                                       declare YOUR status (design 054/055)\n" +
	"       rysh fleet watch <fleet> [--policy smart|always|off] [--interval 10m] [--stall 5m]\n" +
	"                        [--answer agent|scout] [--scout-model haiku]   who answers a status check-in (design 056)\n" +
	"       rysh fleet goal set <fleet> [--member <who>] --goal '<text>' [--done-when '<check>'] [--due 4h]\n" +
	"       rysh fleet goal met|drop <fleet> [--member <who>] [--note <n>]\n" +
	"       rysh fleet goal show <fleet> [--json]          goals, the executor's verdict, each member's progress\n" +
	"       rysh fleet goal exec <fleet> [--mode adaptive|fixed|off] [--min 5m] [--max 30m]   the goal executor (design 055)\n" +
	"       registering opens NO panes; it is the cheap half of a fleet"

func runFleetCmd(cfg config.Config, args []string) error {
	// args[0] == "fleet".
	rest := args[1:]
	if len(rest) == 0 {
		return errors.New(progname.Rewrite(fleetUsage))
	}
	sub := rest[0]
	rest = rest[1:]

	rest, sess := extractStringFlag(rest, "--session")
	rest, jsonOut := extractBoolFlag(rest, "--json")
	// The board's resolver, not session.NewStore(cfg): since design 054 every
	// fleet AGENT runs `rysh fleet status` from its own worktree, and a
	// worktree has no .rysh — found live, every declaration failed with
	// `session "<name>" not found` while the same agent's `board ack` worked.
	store, sess, err := resolveBoardSession(cfg, sess)
	if err != nil {
		return err
	}

	switch sub {
	case "ls", "list":
		return runFleetList(store, sess, "", jsonOut)

	case "show":
		if len(rest) == 0 {
			return errors.New(progname.Rewrite(fleetUsage))
		}
		return runFleetList(store, sess, rest[0], jsonOut)

	case "register":
		if len(rest) == 0 {
			return errors.New(progname.Rewrite(fleetUsage))
		}
		f := fleet.Fleet{Name: rest[0], Created: time.Now().UnixMilli()}
		flags := rest[1:]
		flags, f.BoardID = extractStringFlag(flags, "--board")
		flags, f.Source = extractStringFlag(flags, "--source")
		flags, f.RoadmapDir = extractStringFlag(flags, "--roadmap")
		flags, f.Tab = extractStringFlag(flags, "--tab")
		if err := unknownFleetFlags(flags); err != nil {
			return err
		}
		return runFleetUpdate(store, sess, fleet.Update{Op: fleet.OpRegister, Fleet: f}, jsonOut)

	case "state":
		if len(rest) < 2 {
			return errors.New(progname.Rewrite(fleetUsage))
		}
		return runFleetUpdate(store, sess,
			fleet.Update{Op: fleet.OpState, Name: rest[0], State: rest[1]}, jsonOut)

	case "forget":
		if len(rest) == 0 {
			return errors.New(progname.Rewrite(fleetUsage))
		}
		return runFleetUpdate(store, sess, fleet.Update{Op: fleet.OpForget, Name: rest[0]}, jsonOut)

	case "member":
		return runFleetMember(store, sess, rest, jsonOut)

	case "status":
		return runFleetStatus(store, sess, rest, jsonOut)

	case "watch":
		return runFleetWatch(store, sess, rest, jsonOut)

	case "goal", "goals":
		return runFleetGoal(store, sess, rest, jsonOut)

	default:
		return fmt.Errorf("unknown subcommand %q\n%s", sub, progname.Rewrite(fleetUsage))
	}
}

func runFleetMember(store *session.Store, sess string, rest []string, jsonOut bool) error {
	if len(rest) < 2 {
		return errors.New(progname.Rewrite(fleetUsage))
	}
	op, name := rest[0], rest[1]
	flags := rest[2:]

	var m fleet.Member
	flags, m.PaneID = extractStringFlag(flags, "--pane")
	flags, m.Role = extractStringFlag(flags, "--role")
	flags, m.Label = extractStringFlag(flags, "--label")
	flags, m.Unit = extractStringFlag(flags, "--unit")
	flags, m.Parent = extractStringFlag(flags, "--parent")
	flags, m.Worktree = extractStringFlag(flags, "--worktree")
	flags, m.SessionID = extractStringFlag(flags, "--session-id")
	if err := unknownFleetFlags(flags); err != nil {
		return err
	}
	if strings.TrimSpace(m.PaneID) == "" {
		// A member IS a pane. Without an id there is nothing to record, and
		// recording a label alone would build a roster that cannot be addressed
		// — the failure mode the board's pane-id-is-identity rule exists for.
		return errors.New(progname.Rewrite("rysh fleet member: --pane <id> is required"))
	}

	switch op {
	case "add", "set", "upsert":
		return runFleetUpdate(store, sess,
			fleet.Update{Op: fleet.OpMemberUpsert, Name: name, Member: m}, jsonOut)
	case "rm", "remove", "del":
		return runFleetUpdate(store, sess,
			fleet.Update{Op: fleet.OpMemberRemove, Name: name, PaneID: m.PaneID}, jsonOut)
	default:
		return fmt.Errorf("unknown member op %q\n%s", op, progname.Rewrite(fleetUsage))
	}
}

func runFleetList(store *session.Store, sess, name string, jsonOut bool) error {
	reply, err := cli.FleetList(store, sess, name)
	if err != nil {
		if errors.Is(err, fleet.ErrNoRegistry) {
			return fleetNoRegistryError(err)
		}
		return err
	}

	if jsonOut {
		enc, merr := json.MarshalIndent(reply, "", "  ")
		if merr != nil {
			return merr
		}
		fmt.Println(string(enc))
		return nil
	}
	fmt.Print(renderFleetList(reply, name))
	return nil
}

func runFleetUpdate(store *session.Store, sess string, u fleet.Update, jsonOut bool) error {
	reply, err := cli.FleetUpdate(store, sess, u)
	if err != nil {
		if errors.Is(err, fleet.ErrNoRegistry) {
			return fleetNoRegistryError(err)
		}
		if jsonOut && reply != nil {
			enc, merr := json.MarshalIndent(reply, "", "  ")
			if merr == nil {
				fmt.Println(string(enc))
			}
		}
		return err
	}

	if jsonOut {
		enc, merr := json.MarshalIndent(reply, "", "  ")
		if merr != nil {
			return merr
		}
		fmt.Println(string(enc))
		return nil
	}
	if reply.Fleet != nil {
		fmt.Printf("%s: %s (board %s, %d members)\n",
			u.Op, reply.Fleet.Name, reply.Fleet.BoardID, len(reply.Fleet.Members))
		return nil
	}
	fmt.Printf("%s: done\n", u.Op)
	return nil
}

// fleetNoRegistryError is what a caller is told when nobody answered.
//
// The wording is the safety argument, exactly as it is for `rysh board tail`:
// nothing goes to stdout on this path — not even an empty JSON document — so a
// script doing `rysh fleet ls --json | jq` gets a non-zero status and nothing to
// parse, rather than a convincing "this session runs no fleets".
func fleetNoRegistryError(err error) error {
	return fmt.Errorf("%s\n  underlying: %w", progname.Rewrite(
		"rysh fleet: the session's fleet registry did not answer.\n"+
			"  THIS IS NOT AN EMPTY SESSION — nothing can be said about which fleets exist.\n"+
			"  The registry actor is not running, or this session has no bus."),
		err)
}

// renderFleetList formats an answer for a human. Pure, so the empty-registry
// wording is regression-testable without a daemon.
func renderFleetList(r *fleet.Reply, name string) string {
	var b strings.Builder
	if len(r.Fleets) == 0 {
		if name != "" {
			fmt.Fprintf(&b, "no fleet named %q\n", name)
			return b.String()
		}
		// The registry ANSWERED. This is a real empty, distinct from the
		// no-answer path above, and the wording says which one the reader got.
		b.WriteString("no fleets registered in this session\n")
		return b.String()
	}
	for _, f := range r.Fleets {
		fmt.Fprintf(&b, "%-20s %-11s board=%-16s %d members\n",
			f.Name, f.State, f.BoardID, len(f.Members))
		if name == "" {
			continue
		}
		for _, m := range f.Members {
			fmt.Fprintf(&b, "    %-10s %-24s %s\n", m.Role, m.Label, m.PaneID)
		}
	}
	if r.ReconciledAt == 0 {
		b.WriteString("(membership has not been checked against live panes yet)\n")
	}
	return b.String()
}

func unknownFleetFlags(rest []string) error {
	for _, a := range rest {
		if strings.HasPrefix(a, "-") {
			return fmt.Errorf("unknown flag %q\n%s", a, progname.Rewrite(fleetUsage))
		}
	}
	return nil
}

// runFleetStatus is `rysh fleet status`: with a work state it DECLARES the
// caller's status; without one it prints the ledger.
//
// The declaration form is what every fleet agent runs (the rysh-fleet-agent
// skill's contract), so its errors are written for an agent to act on: each
// refusal says what to pass instead.
func runFleetStatus(store *session.Store, sess string, rest []string, jsonOut bool) error {
	rest, as := extractStringFlag(rest, "--as")
	rest, fleetName := extractStringFlag(rest, "--fleet")
	if as == "" {
		as = os.Getenv("RYSH_PANE")
	}

	if len(rest) > 0 && fleet.ValidWork(strings.ToLower(rest[0])) {
		u := fleet.StatusUpdate{Work: strings.ToLower(rest[0])}
		flags := rest[1:]
		var task string
		flags, task = extractStringFlag(flags, "--task")
		flags, u.Note = extractStringFlag(flags, "--note")
		var pct, eta string
		flags, pct = extractStringFlag(flags, "--pct")
		flags, eta = extractStringFlag(flags, "--eta")
		if pct != "" {
			n, perr := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(pct), "%"))
			if perr != nil {
				return fmt.Errorf("--pct %q is not a number (0–100)", pct)
			}
			u.Pct = &n
		}
		var eerr error
		if u.EtaSec, eerr = parseSeconds(eta); eerr != nil {
			return fmt.Errorf("--eta: %w", eerr)
		}
		for {
			var b string
			flags, b = extractStringFlag(flags, "--blocked-by")
			if b == "" {
				break
			}
			u.BlockedBy = append(u.BlockedBy, b)
		}
		u.Task = task
		// A blocked or done declaration with no --task keeps the task it is
		// about; only progress and idle name a (new or no) task.
		u.KeepTask = task == "" && (u.Work == fleet.WorkBlocked || u.Work == fleet.WorkDone)
		if err := unknownFleetFlags(flags); err != nil {
			return err
		}
		if len(flags) > 0 {
			return fmt.Errorf("unexpected argument %q — quote the task: --task '<what>'", flags[0])
		}
		if as == "" {
			return errors.New(progname.Rewrite(
				"rysh fleet status: who is declaring? Pass --as <your pane id> ($RYSH_PANE is not set in this shell)"))
		}
		reply, err := cli.FleetUpdate(store, sess, fleet.Update{Op: fleet.OpStatus, Name: fleetName, PaneID: as, Status: u})
		if err != nil {
			if errors.Is(err, fleet.ErrNoRegistry) {
				return fleetNoRegistryError(err)
			}
			return err
		}
		if jsonOut {
			enc, _ := json.MarshalIndent(reply, "", "  ")
			fmt.Println(string(enc))
			return nil
		}
		m := reply.Member
		fmt.Printf("recorded: %s is %s", m.Label, m.Status.Work)
		if m.Status.Task != "" {
			fmt.Printf(" — %s", m.Status.Task)
		}
		fmt.Println()
		if m.Status.Pct != nil || m.Status.EtaAt > 0 {
			fmt.Printf("  progress")
			if m.Status.Pct != nil {
				fmt.Printf(" %d%%", *m.Status.Pct)
			}
			if m.Status.EtaAt > 0 {
				fmt.Printf(" · eta %s", time.UnixMilli(m.Status.EtaAt).Format("15:04"))
			}
			fmt.Println()
		}
		if reply.AnsweredCheckin != "" {
			fmt.Printf("  answered check-in %s\n", reply.AnsweredCheckin)
		}
		if reply.AnsweredGoalAsk != "" {
			fmt.Printf("  answered the goal executor's %s\n", reply.AnsweredGoalAsk)
		}
		if reply.Fleet != nil && len(m.Status.BlockedBy) > 0 {
			fmt.Printf("  blocked by %s — the fleet actor will tell you when they are resolved\n",
				strings.Join(statusLabels(reply.Fleet, m.Status.BlockedBy), ", "))
		}
		return nil
	}

	if err := unknownFleetFlags(rest); err != nil {
		return err
	}
	name := fleetName
	if len(rest) > 0 {
		name = rest[0]
	}
	reply, err := cli.FleetList(store, sess, name)
	if err != nil {
		if errors.Is(err, fleet.ErrNoRegistry) {
			return fleetNoRegistryError(err)
		}
		return err
	}
	if name == "" && as != "" {
		// Inside a fleet pane, the bare form means "my fleet".
		for _, f := range reply.Fleets {
			for _, m := range f.Members {
				if m.PaneID == as {
					reply.Fleets = []fleet.Fleet{f}
				}
			}
		}
	}
	if jsonOut {
		enc, merr := json.MarshalIndent(reply, "", "  ")
		if merr != nil {
			return merr
		}
		fmt.Println(string(enc))
		return nil
	}
	if len(reply.Fleets) == 0 {
		fmt.Print(renderFleetList(reply, name))
		return nil
	}
	now := time.Now().UnixMilli()
	for i := range reply.Fleets {
		fmt.Print(fleet.RenderStatus(&reply.Fleets[i], now))
	}
	return nil
}

func statusLabels(f *fleet.Fleet, ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, fleet.MemberLabel(f, id))
	}
	return out
}

// runFleetWatch is `rysh fleet watch <fleet> [--policy] [--interval] [--stall]`.
func runFleetWatch(store *session.Store, sess string, rest []string, jsonOut bool) error {
	if len(rest) == 0 {
		return errors.New(progname.Rewrite(fleetUsage))
	}
	name := rest[0]
	flags := rest[1:]
	var w fleet.Watch
	var interval, stall string
	flags, w.Policy = extractStringFlag(flags, "--policy")
	flags, interval = extractStringFlag(flags, "--interval")
	flags, stall = extractStringFlag(flags, "--stall")
	flags, w.Answer = extractStringFlag(flags, "--answer")
	flags, w.ScoutModel = extractStringFlag(flags, "--scout-model")
	if err := unknownFleetFlags(flags); err != nil {
		return err
	}
	var err error
	if w.IntervalSec, err = parseSeconds(interval); err != nil {
		return fmt.Errorf("--interval: %w", err)
	}
	if w.StallSec, err = parseSeconds(stall); err != nil {
		return fmt.Errorf("--stall: %w", err)
	}
	reply, err := cli.FleetUpdate(store, sess, fleet.Update{Op: fleet.OpWatch, Name: name, Watch: w})
	if err != nil {
		if errors.Is(err, fleet.ErrNoRegistry) {
			return fleetNoRegistryError(err)
		}
		return err
	}
	if jsonOut {
		enc, _ := json.MarshalIndent(reply, "", "  ")
		fmt.Println(string(enc))
		return nil
	}
	r := reply.Fleet.Watch.Resolved()
	fmt.Printf("fleet %s: watch %s, check-in every %s, stalled after %s, answered by %s",
		reply.Fleet.Name, r.Policy, r.IntervalText(), fleet.Watch{IntervalSec: r.StallSec}.IntervalText(), r.Answer)
	if r.Answer == fleet.AnswerScout {
		fmt.Printf(" (%s; the agent only when it cannot tell)", r.ScoutModel)
	}
	fmt.Println()
	return nil
}

// parseSeconds takes "10m", "90s", "1h" or a bare number of seconds; "" is 0
// (the default).
func parseSeconds(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return int(d.Seconds()), nil
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n < 0 {
		return 0, fmt.Errorf("%q is not a duration (10m, 90s) or a number of seconds", s)
	}
	return n, nil
}

// runFleetGoal is `rysh fleet goal set|met|drop|show|exec` (design 055).
func runFleetGoal(store *session.Store, sess string, rest []string, jsonOut bool) error {
	if len(rest) < 2 {
		return errors.New(progname.Rewrite(fleetUsage))
	}
	verb, name, flags := rest[0], rest[1], rest[2:]
	switch verb {
	case "show":
		if err := unknownFleetFlags(flags); err != nil {
			return err
		}
		reply, err := cli.FleetList(store, sess, name)
		if err != nil {
			if errors.Is(err, fleet.ErrNoRegistry) {
				return fleetNoRegistryError(err)
			}
			return err
		}
		if len(reply.Fleets) == 0 {
			return fmt.Errorf("no fleet named %q", name)
		}
		if jsonOut {
			enc, _ := json.MarshalIndent(reply.Fleets[0], "", "  ")
			fmt.Println(string(enc))
			return nil
		}
		fmt.Print(fleet.RenderGoals(&reply.Fleets[0], time.Now().UnixMilli()))
		return nil

	case "exec":
		var g fleet.GoalExec
		var mn, mx string
		flags, g.Mode = extractStringFlag(flags, "--mode")
		flags, mn = extractStringFlag(flags, "--min")
		flags, mx = extractStringFlag(flags, "--max")
		if err := unknownFleetFlags(flags); err != nil {
			return err
		}
		var err error
		if g.MinSec, err = parseSeconds(mn); err != nil {
			return fmt.Errorf("--min: %w", err)
		}
		if g.MaxSec, err = parseSeconds(mx); err != nil {
			return fmt.Errorf("--max: %w", err)
		}
		reply, err := cli.FleetUpdate(store, sess, fleet.Update{Op: fleet.OpGoalExec, Name: name, GoalExec: g})
		if err != nil {
			if errors.Is(err, fleet.ErrNoRegistry) {
				return fleetNoRegistryError(err)
			}
			return err
		}
		if jsonOut {
			enc, _ := json.MarshalIndent(reply, "", "  ")
			fmt.Println(string(enc))
			return nil
		}
		r := reply.Fleet.GoalExec.Resolved()
		fmt.Printf("fleet %s: goal executor %s, every %s–%s\n", reply.Fleet.Name, r.Mode,
			fleet.DurationText(time.Duration(r.MinSec)*time.Second), fleet.DurationText(time.Duration(r.MaxSec)*time.Second))
		return nil

	case "set", "met", "drop", "dropped":
		gu := fleet.GoalUpdate{SetBy: os.Getenv("RYSH_PANE")}
		var due string
		flags, gu.Member = extractStringFlag(flags, "--member")
		flags, gu.Text = extractStringFlag(flags, "--goal")
		flags, gu.DoneWhen = extractStringFlag(flags, "--done-when")
		flags, due = extractStringFlag(flags, "--due")
		flags, gu.Note = extractStringFlag(flags, "--note")
		if err := unknownFleetFlags(flags); err != nil {
			return err
		}
		if len(flags) > 0 {
			return fmt.Errorf("unexpected argument %q — quote the goal: --goal '<text>'", flags[0])
		}
		if gu.SetBy == "" {
			gu.SetBy = fleet.BlockerHuman
		}
		var err error
		if gu.DueSec, err = parseSeconds(due); err != nil {
			return fmt.Errorf("--due: %w", err)
		}
		switch verb {
		case "met":
			gu.State = fleet.GoalMet
		case "drop", "dropped":
			gu.State = fleet.GoalDropped
		}
		reply, err := cli.FleetUpdate(store, sess, fleet.Update{Op: fleet.OpGoal, Name: name, Goal: gu})
		if err != nil {
			if errors.Is(err, fleet.ErrNoRegistry) {
				return fleetNoRegistryError(err)
			}
			return err
		}
		if jsonOut {
			enc, _ := json.MarshalIndent(reply, "", "  ")
			fmt.Println(string(enc))
			return nil
		}
		f := reply.Fleet
		g, whose := f.Goal, "fleet "+f.Name
		if gu.Member != "" {
			if m, merr := fleet.ResolveMember(f, gu.Member); merr == nil {
				g, whose = m.Goal, m.Label
			}
		}
		if g != nil {
			done := "set"
			if g.State == fleet.GoalMet || g.State == fleet.GoalDropped {
				done = g.State
			}
			fmt.Printf("%s goal %s: %s\n", whose, done, g.Text)
			if g.DueAt > 0 {
				fmt.Printf("  due %s\n", time.UnixMilli(g.DueAt).Format("2006-01-02 15:04"))
			}
		}
		return nil
	}
	return fmt.Errorf("unknown goal verb %q (want set, met, drop, show or exec)\n%s", verb, progname.Rewrite(fleetUsage))
}
