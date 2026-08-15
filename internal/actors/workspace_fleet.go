// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
)

// The `##fleet` verb — the human's window onto the registry (design 028 §6.5).
//
// IT ASKS THE REGISTRY OVER NATS RATHER THAN READING ANY STATE OF ITS OWN, and
// that is the whole shape of this file. The workspace deliberately keeps no
// second copy of the fleet list: a copy would be the thing that drifts, and the
// registry actor is spawned at the actor-system root precisely so there is one
// answer to "what fleets exist".
//
// THE DEADLOCK THIS AVOIDS, stated because the safe version looks identical to
// the unsafe one: the registry's query handler answers from its cache and never
// calls back into the workspace (fleet.ServeQueries). If it reconciled on
// demand, this request — made from inside the workspace's own Receive — would
// wait for an answer that needs the workspace to serve a snapshot first. That
// is a deadlock broken only by a timeout, and it would surface as "the registry
// did not answer" on a completely healthy session.

// fleetQueryTimeout bounds a registry read from a ## command.
//
// Shorter than the CLI's budget: a human is watching a pane, and a registry
// that cannot answer in a second is not about to. It is also well above the
// registry's own work, which is a map read — anything slower is the bus, not
// the lookup.
const fleetQueryTimeout = 2 * time.Second

// handleFleetCommand is `##fleet list|show|register|forget|state`.
func (w *WorkspaceActor) handleFleetCommand(out *strings.Builder, paneID string, args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}

	switch sub {
	case "", "list", "ls":
		return w.fleetList(out)
	case "show":
		if len(args) < 2 {
			return w.fleetUsage(out, "##fleet show needs a fleet name")
		}
		return w.fleetShow(out, args[1])
	case "register":
		return w.fleetRegister(out, args[1:])
	case "forget":
		if len(args) < 2 {
			return w.fleetUsage(out, "##fleet forget needs a fleet name")
		}
		return w.fleetUpdate(out, fleet.Update{Op: fleet.OpForget, Name: args[1]},
			fmt.Sprintf("fleet %s forgotten — its panes are untouched and still running\n", args[1]))
	case "state":
		if len(args) < 3 {
			return w.fleetUsage(out, "##fleet state needs a fleet name and a state "+
				"(registered|up|down)")
		}
		return w.fleetUpdate(out, fleet.Update{Op: fleet.OpState, Name: args[1], State: args[2]},
			fmt.Sprintf("fleet %s is now %s\n", args[1], args[2]))
	default:
		return w.fleetUsage(out, fmt.Sprintf("unknown ##fleet subcommand: %q", sub))
	}
}

func (w *WorkspaceActor) fleetList(out *strings.Builder) error {
	reply, err := fleet.Ask(w.nc, fleet.Query{}, fleetQueryTimeout)
	if err != nil {
		return w.fleetRefused(out, err)
	}
	if len(reply.Fleets) == 0 {
		// The registry ANSWERED and there are none. That is a different fact
		// from a registry that did not answer, and the two must not print the
		// same line — see fleetRefused.
		out.WriteString("no fleets registered in this session\n")
		return nil
	}
	for _, f := range reply.Fleets {
		fmt.Fprintf(out, "  %-20s %-11s board=%-16s %s\n",
			f.Name, f.State, f.BoardID, fleetMemberSummary(f))
	}
	out.WriteString(fleetStalenessNote(reply.ReconciledAt))
	return nil
}

func (w *WorkspaceActor) fleetShow(out *strings.Builder, name string) error {
	reply, err := fleet.Ask(w.nc, fleet.Query{Name: name}, fleetQueryTimeout)
	if err != nil {
		return w.fleetRefused(out, err)
	}
	if len(reply.Fleets) == 0 {
		w.failRysh("no fleet named %q", name)
		fmt.Fprintf(out, "no fleet named %q\n", name)
		return fmt.Errorf("no fleet named %q", name)
	}
	f := reply.Fleets[0]
	fmt.Fprintf(out, "fleet %s  [%s]\n", f.Name, f.State)
	fmt.Fprintf(out, "  board    %s\n", f.BoardID)
	if f.Source != "" {
		fmt.Fprintf(out, "  source   %s\n", f.Source)
	}
	if f.RoadmapDir != "" {
		fmt.Fprintf(out, "  roadmap  %s\n", f.RoadmapDir)
	}
	if f.Tab != "" {
		fmt.Fprintf(out, "  tab      %s\n", f.Tab)
	}
	if len(f.Members) == 0 {
		out.WriteString("  members  none\n")
	}
	for _, m := range f.Members {
		fmt.Fprintf(out, "  %-8s %-24s %s\n", m.Role, m.Label, shortID(m.PaneID))
	}
	out.WriteString(fleetStalenessNote(reply.ReconciledAt))
	return nil
}

// fleetRegister is `##fleet register <name> [--board <id>] [--source <path>]
// [--roadmap <dir>]`.
//
// Registering is CHEAP AND SEPARATE FROM RUNNING (design 028 §6.8): it creates
// no panes and starts no claude, which is what makes twenty-five fleets
// affordable to track and what the concurrency cap bounds. `##fleet state <name>
// up` is the expensive half, and `fleetctl` is what actually opens the panes.
func (w *WorkspaceActor) fleetRegister(out *strings.Builder, args []string) error {
	if len(args) == 0 {
		return w.fleetUsage(out, "##fleet register needs a fleet name")
	}
	f := fleet.Fleet{Name: args[0], Created: time.Now().UnixMilli()}
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		if i+1 >= len(rest) {
			return w.fleetUsage(out, fmt.Sprintf("%s needs a value", rest[i]))
		}
		switch rest[i] {
		case "--board":
			f.BoardID = rest[i+1]
		case "--source":
			f.Source = rest[i+1]
		case "--roadmap":
			f.RoadmapDir = rest[i+1]
		case "--tab":
			f.Tab = rest[i+1]
		default:
			return w.fleetUsage(out, fmt.Sprintf("unknown flag %q to ##fleet register", rest[i]))
		}
		i++
	}

	return w.fleetUpdate(out, fleet.Update{Op: fleet.OpRegister, Fleet: f},
		fmt.Sprintf("fleet %s registered (board %s) — no panes were opened; "+
			"`##fleet state %s up` when it is running\n",
			fleet.Normalise(f).Name, fleet.Normalise(f).BoardID, fleet.Normalise(f).Name))
}

// fleetUpdate sends one write and reports it, or refuses out loud.
func (w *WorkspaceActor) fleetUpdate(out *strings.Builder, u fleet.Update, success string) error {
	if _, err := fleet.Send(w.nc, u, fleetQueryTimeout); err != nil {
		return w.fleetRefused(out, err)
	}
	out.WriteString(success)
	return nil
}

// fleetRefused reports a refusal in both registers: prose for the human reading
// the pane, and an error so a script's exit code means something.
//
// IT NEVER PRINTS AN EMPTY LIST. "The registry did not answer" and "there are
// no fleets" are the two facts this surface must keep apart, for the same
// reason `rysh board tail` keeps them apart: a confident empty answer is the
// most convincing way to be wrong.
func (w *WorkspaceActor) fleetRefused(out *strings.Builder, err error) error {
	w.failRysh("%v", err)
	fmt.Fprintf(out, "##fleet: %v\n", err)
	return err
}

// fleetMemberSummary is the one-line roster: how many members, and how they
// break down by role.
func fleetMemberSummary(f fleet.Fleet) string {
	if len(f.Members) == 0 {
		return "no members"
	}
	counts := map[string]int{}
	var order []string
	for _, m := range f.Members {
		role := m.Role
		if role == "" {
			role = "?"
		}
		if counts[role] == 0 {
			order = append(order, role)
		}
		counts[role]++
	}
	parts := make([]string, 0, len(order))
	for _, role := range order {
		parts = append(parts, fmt.Sprintf("%d %s", counts[role], role))
	}
	return fmt.Sprintf("%d members (%s)", len(f.Members), strings.Join(parts, ", "))
}

// fleetStalenessNote says how old the membership is, and says nothing when it
// is fresh.
//
// Membership is reconciled on the registry's own clock rather than on demand
// (fleet.ServeQueries explains why), so a roster can legitimately be up to one
// interval old. Disclosing that is the same discipline the board applies to its
// roster: an answer a reader cannot age is one they will eventually act on when
// it is wrong.
func fleetStalenessNote(reconciledAt int64) string {
	if reconciledAt == 0 {
		return "  (membership has not been checked against live panes yet)\n"
	}
	age := time.Since(time.UnixMilli(reconciledAt))
	if age < 2*afaReconcileInterval {
		return ""
	}
	return fmt.Sprintf("  (membership last checked %s ago)\n", age.Round(time.Second))
}

func (w *WorkspaceActor) fleetUsage(out *strings.Builder, why string) error {
	fmt.Fprintf(out, "%s\n", why)
	out.WriteString("  ##fleet list                      the fleets this session knows about\n")
	out.WriteString("  ##fleet show <name>               one fleet and its members\n")
	out.WriteString("  ##fleet register <name> [--board <id>] [--source <path>] [--roadmap <dir>] [--tab <id>]\n")
	out.WriteString("  ##fleet state <name> registered|up|down\n")
	out.WriteString("  ##fleet forget <name>             drop it from the registry (panes keep running)\n")
	w.failRyshUsage("%s", why)
	return fmt.Errorf("%s", why)
}
