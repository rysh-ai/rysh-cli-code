// SPDX-License-Identifier: Apache-2.0

package actors

import "strings"

// handlePublicCommand and handlePrivateCommand implement `##public` and
// `##private`: printing the active pane's output either redacted (public) or
// raw (private). They are deliberately kept as two near-identical handlers
// rather than one parameterised by visibility — the two commands print
// different things through different code paths, and the symmetry in their
// argument handling is a coincidence of shape, not a shared abstraction.

func (w *WorkspaceActor) handlePublicCommand(out *strings.Builder, paneID string, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if sub == "" {
		sub = "pane"
	}
	switch sub {
	case "pane":
		action := "print"
		if len(args) > 1 {
			action = args[1]
		}
		switch action {
		case "print":
			w.cmdPublicPanePrint(out, paneID)
		default:
			ryshWriter(out).UnknownValue("action for ##public pane", action,
				"##public pane print   print redacted (public) output of the active pane",
			)
			w.failRyshUsage("unknown %s: %q", "action for ##public pane", action)
		}
	default:
		ryshWriter(out).Unknown("public", sub,
			"##public pane print   print redacted (public) output of the active pane",
		)
		w.failRyshUsage("unknown subcommand for ##%s: %q", "public", sub)
	}
}

func (w *WorkspaceActor) handlePrivateCommand(out *strings.Builder, paneID string, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if sub == "" {
		sub = "pane"
	}
	switch sub {
	case "pane":
		action := "print"
		if len(args) > 1 {
			action = args[1]
		}
		switch action {
		case "print":
			w.cmdPrivatePanePrint(out, paneID)
		default:
			ryshWriter(out).UnknownValue("action for ##private pane", action,
				"##private pane print   print raw (private) output of the active pane",
			)
			w.failRyshUsage("unknown %s: %q", "action for ##private pane", action)
		}
	default:
		ryshWriter(out).Unknown("private", sub,
			"##private pane print   print raw (private) output of the active pane",
		)
		w.failRyshUsage("unknown subcommand for ##%s: %q", "private", sub)
	}
}
