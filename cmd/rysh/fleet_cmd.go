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
	if sess == "" {
		sess = os.Getenv("RYSH_SESSION")
	}
	if sess == "" {
		sess = cfg.SessionName
	}
	store, err := session.NewStore(cfg)
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
