// SPDX-License-Identifier: Apache-2.0

package domain

// Remote panes (feature #14, design 070). A pane shown through `##remote add`
// lives on another machine; its snapshot meta says which one and what its own
// rysh observed it doing, so the agents list, the board and the TUI can show a
// remote agent next to the local ones.
const (
	// PaneMetaRemoteHost is the ssh alias a remote pane is reached through.
	PaneMetaRemoteHost = "remote.host"
	// PaneMetaActivity is the remote rysh's observation: working or idle.
	// Needs-you travels in the attention keys, as for a local pane.
	PaneMetaActivity = "activity"

	PaneActivityWorking = "working"
	PaneActivityIdle    = "idle"
)
