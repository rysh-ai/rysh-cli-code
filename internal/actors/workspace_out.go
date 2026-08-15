// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"io"
	"strings"
)

// ---------------------------------------------------------------------------
// The ## command output vocabulary
//
// Command handlers write their output as raw Fprintf calls — about 1870 of
// them across internal/actors. Most are fine: a line of output is a line of
// output. The part that is not fine is the furniture around them, which was
// copy-pasted between handlers and drifted:
//
//	strings.Repeat("-", N) appeared 107 times in ELEVEN different widths
//	(35, 50, 55, 56, 60, 66, 70, 72, 80, 100, 110)
//
// Two commands printing the same shape of table underlined it to different
// widths, which is visible to the user and means nothing. This file gives the
// furniture names and one width.
//
// This is deliberately NOT a general output framework, and no attempt is made
// to route all 1870 Fprintf calls through it. Handlers keep printing their
// content directly; only the repeated structure moves here.
// ---------------------------------------------------------------------------

// ryshRuleWidth is the one width for a horizontal rule in ## command output.
//
// 60 rather than a wider value because it is the plurality of what was already
// there and it fits an 80-column terminal with room to spare. A few listings
// print rows longer than this (##tab list renders a 36-character uuid); they
// did so under a 60-wide rule before this change too.
const ryshRuleWidth = 60

// ryshOut wraps the builder a ## command handler writes into, and gives the
// repeated pieces of a command's output a name.
//
// Handlers still take a *strings.Builder — that is what makes them testable
// without an actor system, and it is not worth changing. ryshOut is a thin
// adapter over one, constructed where it helps and ignored where it does not.
type ryshOut struct{ b io.Writer }

// ryshWriter adapts a writer for the vocabulary below. It takes io.Writer
// rather than *strings.Builder because a couple of usage helpers are shared
// with non-actor callers that already write to an io.Writer; every handler in
// this package passes its *strings.Builder, which satisfies it.
func ryshWriter(b io.Writer) ryshOut { return ryshOut{b: b} }

// Header opens a block of command output: a leading blank line, the [rysh]
// tag, and the title. This is the shape nearly every handler already used.
func (o ryshOut) Header(format string, a ...any) {
	fmt.Fprintf(o.b, "\n[rysh] "+format+"\n", a...)
}

// Tagged is Header with a tag other than "rysh" — [pipeline], [hop], [upstream]
// and friends, which several commands use to mark which subsystem is talking.
func (o ryshOut) Tagged(tag, format string, a ...any) {
	fmt.Fprintf(o.b, "\n["+tag+"] "+format+"\n", a...)
}

// Rule writes the standard horizontal rule.
func (o ryshOut) Rule() {
	fmt.Fprintf(o.b, "%s\n", strings.Repeat("-", ryshRuleWidth))
}

// Row writes one line of content inside a block.
func (o ryshOut) Row(format string, a ...any) {
	fmt.Fprintf(o.b, format+"\n", a...)
}

// Field writes an aligned "name : value" line, the shape every ##* info
// command uses for its detail block.
//
// The 13-column label matches the widest of the hand-aligned blocks already in
// the tree (##upstream status). The others use 11 or 12 and are left alone:
// re-aligning a block that is already internally consistent changes what the
// user sees for no benefit. New blocks should use this.
func (o ryshOut) Field(name, format string, a ...any) {
	fmt.Fprintf(o.b, "  %-13s: "+format+"\n", append([]any{name}, a...)...)
}

// Usage writes a usage block: the "usage:" line, one indented line per form,
// and a trailing blank line separating it from whatever prints next.
//
// The trailing blank is the standard. Ten of the fourteen [rysh] usage blocks
// already ended with one and four did not, which is the same coin-flip
// inconsistency the rule widths had.
//
// An empty form emits a bare blank line rather than an indented one, so a
// block can separate sections without the caller dropping back to Fprintf.
func (o ryshOut) Usage(forms ...string) {
	o.usage("rysh", forms)
}

// UsageIn is Usage for commands that tag their output with their own
// subsystem — [agents], [humanoids], [pipeline] — matching UnknownIn.
func (o ryshOut) UsageIn(tag string, forms ...string) {
	o.usage(tag, forms)
}

func (o ryshOut) usage(tag string, forms []string) {
	fmt.Fprintf(o.b, "\n[%s] usage:\n", tag)
	for _, f := range forms {
		if f == "" {
			fmt.Fprintln(o.b)
			continue
		}
		fmt.Fprintf(o.b, "  %s\n", f)
	}
	fmt.Fprintf(o.b, "\n")
}

// UsageLine writes a single-form usage on one line — "[rysh] usage: ##x <args>"
// — which is what a handler prints when a command has exactly one shape and a
// full block would be noise. There were ninety-six of these; they already
// agreed on their trailing newline, and naming them keeps it that way.
func (o ryshOut) UsageLine(form string) {
	o.UsageLineIn("rysh", form)
}

// UsageLineIn is UsageLine for subsystem-tagged commands, matching UsageIn and
// UnknownIn.
func (o ryshOut) UsageLineIn(tag, form string) {
	fmt.Fprintf(o.b, "\n[%s] usage: %s\n", tag, form)
}

// Warn reports a problem the user can act on — a missing pane, an unknown
// argument, a refused operation.
func (o ryshOut) Warn(format string, a ...any) {
	fmt.Fprintf(o.b, "\n[rysh] "+format+"\n", a...)
}

// Unknown reports an unrecognised subcommand and lists what is available.
//
// `command` is the CANONICAL name, not whatever the user typed: by the time a
// handler runs, the dispatch table has already resolved aliases, so it cannot
// know whether the user wrote ##ws or ##workspace. Echoing the canonical name
// also points at the spelling ##help lists.
func (o ryshOut) Unknown(command, sub string, forms ...string) {
	fmt.Fprintf(o.b, "\n[rysh] unknown subcommand for ##%s: %q\n", command, sub)
	for _, f := range forms {
		fmt.Fprintf(o.b, "  %s\n", f)
	}
	fmt.Fprintf(o.b, "\n")
}

// UnknownIn is Unknown for the commands that tag their output with their own
// subsystem — [mcp], [cron], [snat], [agents] — rather than the generic
// [rysh]. That tag is worth keeping: it tells the user which subsystem
// answered, which matters when a command spawns work elsewhere.
//
// It takes no forms because every one of these commands already has a
// dedicated help function that the caller invokes next. Only the one-line
// preamble was being retyped.
func (o ryshOut) UnknownIn(tag, sub string) {
	fmt.Fprintf(o.b, "\n[%s] unknown subcommand: %q\n", tag, sub)
}

// UnknownValue reports an unrecognised value in a position deeper than the
// subcommand — `##public pane <action>`, where the action is the third word.
// `what` names the position, e.g. "action for ##public pane".
func (o ryshOut) UnknownValue(what, value string, forms ...string) {
	fmt.Fprintf(o.b, "\n[rysh] unknown %s: %q\n", what, value)
	for _, f := range forms {
		fmt.Fprintf(o.b, "  %s\n", f)
	}
	fmt.Fprintf(o.b, "\n")
}

// ---------------------------------------------------------------------------
// Command failure
//
// A ## command handler reports trouble by printing prose — "[rysh] pane not
// found", a usage line, an unknown-subcommand block. That is right for the
// human at the keyboard and useless to a script, which needs an exit code
// (design 021 §3.6). These helpers record that a command did not do what was
// asked, so handleRyshCommand can turn it into one.
//
// They deliberately do NOT change what is printed. The alternative — routing
// every failure through a new output helper — would have rewritten ~156
// hand-formatted messages across 35 files, churning output that tests pin and
// users read, to fix a problem that is entirely about the return value. So a
// failure site keeps its Fprintf and gains one line.
//
// The sink lives on WorkspaceActor rather than being threaded through every
// handler signature because handlers take a *strings.Builder and nothing else;
// giving 40 of them (and their ~150 sub-functions) an error return would be a
// far larger and riskier change than the problem warrants. It is safe because
// a protoactor mailbox is processed serially: exactly one ## command is in
// flight per workspace at a time.
// ---------------------------------------------------------------------------

// failRysh records that the command in flight did not do what was asked.
//
// The message is for the caller's exit path (stderr, `rysh exec --json`), not
// for the pane — the handler has already printed the human-readable form. Keep
// it short and specific: "pane not found: web" beats "error".
//
// The FIRST failure wins. A handler that reports a problem and then prints
// follow-up guidance should not have its message replaced by the guidance.
func (w *WorkspaceActor) failRysh(format string, a ...any) {
	if w.ryshFail == nil {
		w.ryshFail = fmt.Errorf(format, a...)
	}
}

// failRyshUsage records a wrong invocation — a missing argument, an unknown
// subcommand, an unparseable value. Split from failRysh only so the intent
// reads at the call site; both produce the same non-zero exit.
func (w *WorkspaceActor) failRyshUsage(format string, a ...any) {
	w.failRysh(format, a...)
}

// takeRyshFailure returns and clears the recorded failure.
func (w *WorkspaceActor) takeRyshFailure() error {
	err := w.ryshFail
	w.ryshFail = nil
	return err
}
