// SPDX-License-Identifier: Apache-2.0

package msg

// The fleet registry's subjects (design 028 §6.5, `E-40`).
//
// Two request subjects and no publish-only ones, deliberately. A registry
// update that is fire-and-forget is a registry that reports success it does not
// have — the failure this whole track keeps re-learning — and `fleetctl` needs
// to know that a fleet it just created is actually recorded before it starts
// putting agents in it.
//
// Built with T(...) and never as literals: T's prefix is the SESSION NAME
// (rysh-shared/msg/topics.go), so a literal breaks the moment a session is
// called anything other than the default.

// FleetQuerySubject is where the fleet actor answers "what fleets exist?".
//
// SAME SHAPE AND SAME REASON AS BoardQuerySubject: a reader ASKS THE ACTOR that
// owns the registry rather than opening the KV bucket at a second call site.
// F-23 is what happens when a second call site derives a bucket name — the read
// failed while LOOKING HEALTHY, because an empty answer and a wrong bucket are
// indistinguishable.
func FleetQuerySubject() string { return T("fleet", "query") }

// FleetUpdateSubject is where the fleet actor accepts registrations, membership
// changes and state transitions.
func FleetUpdateSubject() string { return T("fleet", "update") }

// FleetEventSubject is where the fleet actor publishes status-ledger changes
// (fleet.LedgerEvent, plain JSON like the query and update subjects): one event
// per member whose state, task, note, progress or health changed (design 093
// §12.3 H3 — "ledger change events added to the fleet actor"). One exact
// subject, so a reader never needs a wildcard subscription.
func FleetEventSubject() string { return T("fleet", "events") }

// FleetBoardQuerySubject is where the FleetBoardActor answers with the fleet
// board's rows (design 030 §9.2, W2).
func FleetBoardQuerySubject() string { return T("fleetboard", "query") }

// FleetBoardUpdateSubject carries one fleetboard.ReplyRow per changed row. No
// agent pane subscribes to it, so reading the fleet board wakes nobody.
func FleetBoardUpdateSubject() string { return T("fleetboard", "update") }
