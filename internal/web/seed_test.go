package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// The seed exists because ONE full snapshot could not be written inside
// writeWait over a tunnel: 17.10 MB measured on a 166-pane session, against a
// 10s deadline and a link carrying 95-400 KB/s. The browser showed nothing at
// all, forever, because F-7a leaves stream clients with no fallback poll.
//
// So the property that matters is a bound on the LARGEST message, not on the
// total. These tests pin that bound and the two caps that make it reachable.

func seedTestSnapshot(panes int, contentBytes, historyEntries int) *domain.WorkspaceSnapshot {
	ps := make([]domain.PaneSnapshot, 0, panes)
	for i := 0; i < panes; i++ {
		hist := make([]string, historyEntries)
		for h := range hist {
			hist[h] = fmt.Sprintf("git commit -m 'entry %d of pane %d'", h, i)
		}
		ps = append(ps, domain.PaneSnapshot{
			ID:           fmt.Sprintf("pane-%d", i),
			Output:       strings.Repeat("x", contentBytes),
			ShellHistory: hist,
		})
	}
	return &domain.WorkspaceSnapshot{
		Tabs: []domain.TabSnapshot{{
			Lanes: []domain.LaneSnapshot{{
				PaneGroups: []domain.PaneGroupSnapshot{{Panes: ps}},
			}},
		}},
	}
}

// No batch may approach writeWait's budget. A 256 KB batch is ~2.7s at the
// 95 KB/s floor; a single 17 MB message was 180s, which is the bug.
func TestSeedBatchesStayUnderTheWriteBudget(t *testing.T) {
	// 166 panes, each with more content than the cap allows, so the batcher is
	// working against real pressure rather than a toy.
	snap := seedTestSnapshot(166, 200_000, 400)

	batches := paneContentBatches(snap, seedBatchMaxBytes)
	if len(batches) == 0 {
		t.Fatal("no batches produced")
	}

	// Generous headroom over seedBatchMaxBytes for JSON overhead and escaping,
	// but far below anything that could threaten a 10s deadline.
	const hardCeiling = 2 * seedBatchMaxBytes
	biggest := 0
	for i, b := range batches {
		if len(b) > biggest {
			biggest = len(b)
		}
		if len(b) > hardCeiling {
			t.Errorf("batch %d is %d bytes, over the %d ceiling — one oversized write is what "+
				"blew writeWait and left the browser blank", i, len(b), hardCeiling)
		}
	}
	t.Logf("%d panes -> %d batches, largest %d KB", 166, len(batches), biggest/1024)

	// Every pane must appear exactly once: a seed that silently drops panes
	// renders a workspace with holes in it.
	seen := map[string]int{}
	for _, b := range batches {
		var env struct {
			Data struct {
				Panes []struct {
					PaneID string `json:"pane_id"`
				} `json:"panes"`
			} `json:"data"`
		}
		if err := json.Unmarshal(b, &env); err != nil {
			t.Fatalf("batch is not valid JSON: %v", err)
		}
		for _, p := range env.Data.Panes {
			seen[p.PaneID]++
		}
	}
	if len(seen) != 166 {
		t.Errorf("batches covered %d panes, want 166", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("pane %s appeared %d times, want once", id, n)
		}
	}
}

// The renderer caps every buffer at a 20 KB tail and re-applies that on the next
// append regardless of the seed, so anything above it is bytes the client throws
// away. One pane was measured shipping 682 KB into such a seed.
func TestSeedCapsContentToWhatTheClientKeeps(t *testing.T) {
	p := &domain.PaneSnapshot{
		ID:     "pane-1",
		Output: strings.Repeat("a", 500_000),
	}
	got := seedFromPane(p)
	if len(got.Output) > maxSeedPaneContent {
		t.Errorf("seeded output is %d bytes, want <= %d — the client trims to this anyway",
			len(got.Output), maxSeedPaneContent)
	}
	if len(got.Output) == 0 {
		t.Error("seeded output is empty; the cap keeps the TAIL, it does not drop the buffer")
	}
}

// History is capped rather than deduped: measured on a live session, 143 panes
// held 83 distinct histories with a true common prefix of 11 entries, so there
// was nothing meaningful to send once. The cap is the only lever, and it is
// user-visible in the browser — recall reaches back exactly this far.
func TestSeedCapsHistoryAndKeepsTheMostRecent(t *testing.T) {
	hist := make([]string, 1090) // the longest pane measured on the live session
	for i := range hist {
		hist[i] = fmt.Sprintf("cmd-%d", i)
	}
	got := seedFromPane(&domain.PaneSnapshot{ID: "pane-1", ShellHistory: hist})

	if len(got.ShellHistory) != maxSeedHistoryEntries {
		t.Fatalf("seeded %d history entries, want %d", len(got.ShellHistory), maxSeedHistoryEntries)
	}
	// Recall walks backwards from the newest, so the newest must survive.
	if last := got.ShellHistory[len(got.ShellHistory)-1]; last != "cmd-1089" {
		t.Errorf("last seeded entry is %q, want the newest (cmd-1089) — the cap kept the wrong end", last)
	}
	if first := got.ShellHistory[0]; first != "cmd-990" {
		t.Errorf("first seeded entry is %q, want cmd-990", first)
	}

	// A history shorter than the cap must pass through untouched.
	short := []string{"one", "two"}
	if got := seedFromPane(&domain.PaneSnapshot{ID: "p", ShellHistory: short}); len(got.ShellHistory) != 2 {
		t.Errorf("a %d-entry history became %d entries", len(short), len(got.ShellHistory))
	}
}
