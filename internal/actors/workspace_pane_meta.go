package actors

import (
	"sort"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// Helpers for `##pane meta` and for any command that has to talk about a pane
// other than the active one.

// splitPaneTarget pulls a `--pane <ref>` / `--pane=<ref>` target out of args and
// returns the remaining words plus the pane REF to act on, defaulting to
// fallback (the caller's pane). The ref is resolved by the caller, so an id, an
// auto-title or a given-name all work.
//
// Word-wise, unlike extractPaneFlag in workspace_mode.go, which takes the whole
// prompt as one string: a ## command arrives already split, and re-joining it
// only to split it again loses nothing but invites quoting bugs.
//
// Separate from the ##cmd selector parser on purpose: this is "which pane is
// this command ABOUT", a single target, where ##cmd's selectors describe a
// scope to broadcast across.
func splitPaneTarget(args []string, fallback string) (rest []string, pane string) {
	pane = fallback
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--pane" && i+1 < len(args):
			pane = args[i+1]
			return append(append([]string{}, args[:i]...), args[i+2:]...), pane
		case strings.HasPrefix(args[i], "--pane="):
			pane = strings.TrimPrefix(args[i], "--pane=")
			return append(append([]string{}, args[:i]...), args[i+1:]...), pane
		}
	}
	return args, pane
}

// sortedMetaKeys orders a metadata map so listings are stable. Go randomises
// map iteration, and a pane listing that reorders itself between two calls
// looks like the pane changed.
func sortedMetaKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// paneMeta reads a pane's metadata out of its tab snapshot. Metadata lives on
// the pane actor, so this is a query rather than workspace state: the workspace
// deliberately keeps no second copy that could drift.
func (w *WorkspaceActor) paneMeta(paneID string) map[string]string {
	ps := w.paneSnapshotByID(paneID)
	if ps == nil {
		return nil
	}
	return ps.Meta
}

// paneSnapshotByID finds a pane anywhere in the workspace by id.
func (w *WorkspaceActor) paneSnapshotByID(paneID string) *domain.PaneSnapshot {
	tab := w.findPaneTab(paneID)
	if tab == nil {
		return nil
	}
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		return nil
	}
	for _, lane := range tabSnap.Lanes {
		for _, g := range lane.PaneGroups {
			for i := range g.Panes {
				if g.Panes[i].ID == paneID {
					return &g.Panes[i]
				}
			}
		}
	}
	return nil
}

// extractBoolArg removes a valueless flag from args, reporting whether it was
// there.
func extractBoolArg(args []string, flag string) (rest []string, found bool) {
	rest = make([]string, 0, len(args))
	for _, a := range args {
		if a == flag {
			found = true
			continue
		}
		rest = append(rest, a)
	}
	return rest, found
}
