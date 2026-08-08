package actors

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// F-7c: command history rides layout-only snapshots because the TUI reads it
// from there for arrow-key recall — but it is not the "small capped list" the
// code assumed. Capped means 1000 ENTRIES, and every pane seeds the SAME session
// history file, so 50 panes each carried an identical copy: measured live at
// 28.9 KB of a 29.9 KB layout-only pane reply (97.5%), re-serialized on every
// cascade at 10Hz.
//
// includeHistories is what lets a caller that reads no history — the web
// server's streamPaneVT — keep them off the wire. These tests pin both halves:
// the opt-out must actually drop them, and the default must not.

// paneWithHistory builds a PaneActor carrying history in every mode, standing in
// for a pane seeded from the session history file.
func paneWithHistory() *PaneActor {
	entries := make([]string, 0, 64)
	for i := 0; i < 64; i++ {
		entries = append(entries, "git commit -m "+strings.Repeat("x", 40))
	}
	return &PaneActor{
		id:              "pane-1",
		mergedHistory:   entries,
		shellHistory:    entries,
		promptHistory:   entries,
		ryshHistory:     entries,
		chatHistory:     entries,
		externalHistory: entries,
	}
}

// The whole point of the flag: histories must leave the snapshot entirely.
func TestBuildSnapshotOmitsHistoriesWhenNotRequested(t *testing.T) {
	p := paneWithHistory()

	snap := p.buildSnapshot(false, false, false)

	for _, tc := range []struct {
		name string
		got  []string
	}{
		{"ShellHistory", snap.ShellHistory},
		{"MergedHistory", snap.MergedHistory},
		{"PromptHistory", snap.PromptHistory},
		{"RyshHistory", snap.RyshHistory},
		{"ChatHistory", snap.ChatHistory},
		{"ExternalHistory", snap.ExternalHistory},
	} {
		if len(tc.got) != 0 {
			t.Errorf("%s carried %d entries with includeHistories=false — F-7c is back",
				tc.name, len(tc.got))
		}
	}
}

// The TUI's activeHistory() reads ShellHistory/PromptHistory straight out of its
// LAYOUT snapshot. A layout-only build must therefore still carry history, or
// arrow-key recall silently dies — the reason this is an opt-out, not a default.
func TestLayoutOnlySnapshotStillCarriesHistoryForTheTUI(t *testing.T) {
	p := paneWithHistory()

	// includeContent=false is the layout-only cascade; includeHistories=true is
	// what every caller but streamPaneVT passes.
	snap := p.buildSnapshot(false, false, true)

	if len(snap.ShellHistory) == 0 {
		t.Error("layout-only snapshot dropped ShellHistory — the TUI's arrow-key recall reads it from here")
	}
	if len(snap.PromptHistory) == 0 {
		t.Error("layout-only snapshot dropped PromptHistory — prompt-mode recall reads it from here")
	}
	// Layout-only still means no display buffers; the histories are the exception.
	if snap.Output != "" || snap.ShellOutput != "" {
		t.Error("layout-only snapshot leaked display buffers")
	}
}

// The saving has to be real, not cosmetic: history is ~97% of a layout-only pane
// snapshot, so dropping it must collapse the encoded reply.
func TestOmittingHistoriesCollapsesTheEncodedSnapshot(t *testing.T) {
	p := paneWithHistory()

	withHist := encodedSnapshotSize(t, p.buildSnapshot(false, false, true))
	without := encodedSnapshotSize(t, p.buildSnapshot(false, false, false))

	if without >= withHist/4 {
		t.Errorf("dropping histories shrank the snapshot only %d -> %d bytes; "+
			"history is meant to dominate it (measured 97%% live)", withHist, without)
	}
}

// encodedSnapshotSize is the wire size of a pane snapshot — what the cascade
// actually pays per pane, per request.
func encodedSnapshotSize(t *testing.T, snap domain.PaneSnapshot) int {
	t.Helper()
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	return len(data)
}
