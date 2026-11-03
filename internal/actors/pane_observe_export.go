// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/courier"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Read-only pane observation for callers OUTSIDE the daemon: `rysh wait` and
// `rysh screen` (design 069). They reuse the fleet watch's own observer rather
// than a copy, so "idle" means the same thing to a script as it does to a
// fleet. Nothing here writes to a pane.

// Pane activity words, as the fleet ledger spells them.
const (
	PaneActivityWorking = fleet.ActivityWorking
	PaneActivityIdle    = fleet.ActivityIdle
	PaneActivityUnknown = fleet.ActivityUnknown
)

// ObservePaneActivity reports whether a pane's agent is working, idle or
// unknown, with the fleet watch's four signals (see observePane). It takes at
// least gap: two screen reads gap apart. An unreadable pane is unknown, never
// idle.
func ObservePaneActivity(pub *msg.NATSPublisher, paneID string, gap time.Duration) string {
	t, snapErr := courier.SnapshotVia(pub, paneID)
	return observeActivity(pub, paneID, readProcTable(), gap, t, snapErr)
}

// ReadPaneScreen returns the pane's current screen rows (its VT grid), the
// same read ansa and the fleet watch make.
func ReadPaneScreen(pub *msg.NATSPublisher, paneID string) ([]string, error) {
	return (&natsAnsaTransport{pub: pub}).Screen(paneID)
}
