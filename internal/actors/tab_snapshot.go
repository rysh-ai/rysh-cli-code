// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"sync"
	"time"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ---------------------------------------------------------------------------
// Snapshot
// ---------------------------------------------------------------------------

func (t *TabActor) collectSnapshot(layoutOnly, noHistories bool) domain.TabSnapshot {
	snap := domain.TabSnapshot{
		ID:              t.id,
		Title:           t.title,
		PipelineOutput:  t.pipelineBuffer.String(),
		PipelineActive:  t.pipelineActive,
		PipelineEnabled: t.pipelineEnabled,
		PipelineName:    t.pipelineName,
	}
	if len(t.laneRefs) > 0 && t.activeLane >= 0 && t.activeLane < len(t.laneRefs) {
		snap.ActivePaneID = t.laneRefs[t.activeLane].activePaneID
	}

	// Fetch the lanes concurrently (one round-trip instead of one per lane).
	// pipelineName is captured once; the actor is blocked at wg.Wait and does
	// not mutate it during the fan-out, so the goroutines read the local copy.
	pipelineName := t.pipelineName
	results := make([]domain.LaneSnapshot, len(t.laneRefs))
	fetch := func(i int, id string, flex int) {
		reply, err := t.pub.Request(
			msg.T("lane", id, "snapshot"),
			&msg.MsgGetLaneSnapshot{LayoutOnly: layoutOnly, NoHistories: noHistories},
			2*time.Second,
		)
		if err != nil {
			results[i] = domain.LaneSnapshot{ID: id, Flex: flex, Name: pipelineName}
			return
		}
		lReply, ok := reply.(*msg.MsgLaneSnapshotReply)
		if !ok {
			results[i] = domain.LaneSnapshot{ID: id, Flex: flex, Name: pipelineName}
			return
		}
		ls := lReply.Snapshot
		ls.Flex = flex // authoritative override
		// Lane name defaults to the tab's pipeline name when the lane has no
		// explicit name set, so every lane is labelled.
		if ls.Name == "" {
			ls.Name = pipelineName
		}
		results[i] = ls
	}
	// Concurrent only when there is more than one lane (see note in
	// WorkspaceActor.collectSnapshot): the cascade is depth-sequential.
	if len(t.laneRefs) > 1 {
		var wg sync.WaitGroup
		for i, lr := range t.laneRefs {
			wg.Add(1)
			go func(i int, id string, flex int) { defer wg.Done(); fetch(i, id, flex) }(i, lr.id, lr.flex)
		}
		wg.Wait()
	} else {
		for i, lr := range t.laneRefs {
			fetch(i, lr.id, lr.flex)
		}
	}
	snap.Lanes = append(snap.Lanes, results...)

	return snap
}

// ---------------------------------------------------------------------------
// KV helpers used by WorkspaceActor
// ---------------------------------------------------------------------------

// FlushAllPanes forces KV persistence for all pane actors in all lanes.
func (t *TabActor) FlushAllPanes() {
	for _, la := range t.laneActors {
		la.FlushAllPanes()
	}
}

// PaneIDs returns the IDs of all panes across all lanes in this tab (in order).
func (t *TabActor) PaneIDs() []string {
	var ids []string
	for _, lr := range t.laneRefs {
		if la, ok := t.laneActors[lr.id]; ok {
			ids = append(ids, la.PaneIDs()...)
		}
	}
	return ids
}

// ActivePaneID returns the ID of the currently active pane, or "".
func (t *TabActor) ActivePaneID() string {
	if len(t.laneRefs) == 0 || t.activeLane < 0 || t.activeLane >= len(t.laneRefs) {
		return ""
	}
	return t.laneRefs[t.activeLane].activePaneID
}

// PaneHistory returns the shell or prompt history for a specific pane.
func (t *TabActor) PaneHistory(paneID, mode string) []string {
	laneID, ok := t.paneToLane[paneID]
	if !ok {
		return nil
	}
	la, ok := t.laneActors[laneID]
	if !ok {
		return nil
	}
	return la.PaneHistory(paneID, mode)
}

// PaneSnapshot returns the current snapshot for a specific pane (no NATS round-trip).
func (t *TabActor) PaneSnapshot(paneID string) *domain.PaneSnapshot {
	laneID, ok := t.paneToLane[paneID]
	if !ok {
		return nil
	}
	la, ok := t.laneActors[laneID]
	if !ok {
		return nil
	}
	return la.PaneSnapshot(paneID)
}

// PanePrivateOutput returns the dedicated private (raw) output buffer for a pane.
func (t *TabActor) PanePrivateOutput(paneID string) string {
	laneID, ok := t.paneToLane[paneID]
	if !ok {
		return ""
	}
	la, ok := t.laneActors[laneID]
	if !ok {
		return ""
	}
	return la.PanePrivateOutput(paneID)
}

// PaneChatOutput returns the chat output buffer for a pane.
// Cross-actor read — informational only.
func (t *TabActor) PaneChatOutput(paneID string) string {
	laneID, ok := t.paneToLane[paneID]
	if !ok {
		return ""
	}
	la, ok := t.laneActors[laneID]
	if !ok {
		return ""
	}
	return la.PaneChatOutput(paneID)
}

// PaneHoppedInfo returns the hopped content info for a pane.
// Cross-actor read — informational only.
func (t *TabActor) PaneHoppedInfo(paneID string) *HoppedInfo {
	laneID, ok := t.paneToLane[paneID]
	if !ok {
		return nil
	}
	la, ok := t.laneActors[laneID]
	if !ok {
		return nil
	}
	return la.PaneHoppedInfo(paneID)
}

// AppendPaneSystemOutput appends text to a pane's display-only output buffer.
func (t *TabActor) AppendPaneSystemOutput(paneID, text string) {
	laneID, ok := t.paneToLane[paneID]
	if !ok {
		return
	}
	la, ok := t.laneActors[laneID]
	if !ok {
		return
	}
	la.AppendPaneSystemOutput(paneID, text)
}

// AppendPaneRyshOutput appends text to a pane's rysh-mode output buffer.
func (t *TabActor) AppendPaneRyshOutput(paneID, text string) {
	laneID, ok := t.paneToLane[paneID]
	if !ok {
		return
	}
	la, ok := t.laneActors[laneID]
	if !ok {
		return
	}
	la.AppendPaneRyshOutput(paneID, text)
}

// IsGivenNameTakenInLane checks if the given name is already used by another
// pane within the same lane as the specified pane.
func (t *TabActor) IsGivenNameTakenInLane(paneID, name string) bool {
	laneID, ok := t.paneToLane[paneID]
	if !ok {
		return false
	}
	la, ok := t.laneActors[laneID]
	if !ok {
		return false
	}
	return la.HasGivenName(name, paneID)
}

// PaneRefs returns a flattened copy of all pane references for KV persistence.
func (t *TabActor) PaneRefs() []paneKV {
	var refs []paneKV
	for _, lr := range t.laneRefs {
		if la, ok := t.laneActors[lr.id]; ok {
			for _, gr := range la.groupRefs {
				if ga, ok := la.groupActors[gr.id]; ok {
					for _, ref := range ga.paneRefs {
						refs = append(refs, paneKV{
							ID:    ref.id,
							Title: ref.title,
						})
					}
				}
			}
		}
	}
	return refs
}

// ActivePaneIndex returns the 0-based active lane index.
func (t *TabActor) ActivePaneIndex() int { return t.activeLane }

// ---------------------------------------------------------------------------
// tabKV for serialisation (shared with WorkspaceActor)
// ---------------------------------------------------------------------------

type tabKV struct {
	ID              string                     `json:"id"`
	Title           string                     `json:"title"`
	Lanes           []laneKV                   `json:"lanes"`
	ActiveLane      int                        `json:"active_lane"`
	PipelineOutput  string                     `json:"pipeline_output,omitempty"`
	PipelineActive  bool                       `json:"pipeline_active,omitempty"`
	PipelineEnabled bool                       `json:"pipeline_enabled,omitempty"`
	Pipelines       map[string]*LoadedPipeline `json:"pipelines,omitempty"`
	PipelineName    string                     `json:"pipeline_name,omitempty"`
}

// toKVViaActors serialises the tab for persistence, asking each lane to
// serialise ITSELF on its own goroutine (which in turn cascades to its pane
// groups) rather than reading the lane structs directly. Must be called from
// inside this tab's Receive. See kv_cascade.go for why.
//
// Tab-owned fields — including each lane's flex weight, which lives on this
// actor's laneRefs and is authoritative — are read directly here; only the lane
// documents come from the child actors.
func (t *TabActor) toKVViaActors(ctx actor.Context) tabKV {
	kv := tabKV{
		ID:              t.id,
		Title:           t.title,
		ActiveLane:      t.activeLane,
		Lanes:           make([]laneKV, len(t.laneRefs)),
		PipelineOutput:  t.pipelineBuffer.String(),
		PipelineActive:  t.pipelineActive,
		PipelineEnabled: t.pipelineEnabled,
		Pipelines:       t.pipelines,
		PipelineName:    t.pipelineName,
	}
	for i, lr := range t.laneRefs {
		var lkv laneKV
		if reply, ok := requestKV[*laneKVReply](ctx, t.lanePIDs[lr.id], &laneKVRequest{}); ok {
			lkv = reply.kv
		} else if la, ok := t.laneActors[lr.id]; ok {
			// Fallback: lane actor gone or slow — direct read beats dropping it.
			lkv = la.ToKV()
		} else {
			lkv = laneKV{ID: lr.id}
		}
		lkv.Flex = lr.flex // tab-owned and authoritative
		kv.Lanes[i] = lkv
	}
	return kv
}

// ToKV serialises the tab state for JetStream KV persistence.
//
// Deprecated for cross-actor use: this reads child lane structs directly and
// therefore races with those actors. It is retained only as the fallback path
// in toKVViaActors / the workspace shutdown flush (and for tests). Callers on
// another goroutine must use the cascade instead.
func (t *TabActor) ToKV() tabKV {
	kv := tabKV{
		ID:              t.id,
		Title:           t.title,
		ActiveLane:      t.activeLane,
		Lanes:           make([]laneKV, len(t.laneRefs)),
		PipelineOutput:  t.pipelineBuffer.String(),
		PipelineActive:  t.pipelineActive,
		PipelineEnabled: t.pipelineEnabled,
		Pipelines:       t.pipelines,
		PipelineName:    t.pipelineName,
	}
	for i, lr := range t.laneRefs {
		if la, ok := t.laneActors[lr.id]; ok {
			lkv := la.ToKV()
			lkv.Flex = lr.flex // authoritative
			kv.Lanes[i] = lkv
		} else {
			kv.Lanes[i] = laneKV{
				ID:   lr.id,
				Flex: lr.flex,
			}
		}
	}
	return kv
}

// doRestoreFromKV restores tab and lane state from persisted tabKV data.
// Must be called from within Receive() (i.e., with a valid actor.Context).
func (t *TabActor) doRestoreFromKV(ctx actor.Context, kv tabKV) {
	t.id = kv.ID
	t.title = kv.Title
	t.activeLane = kv.ActiveLane
	t.pipelineEnabled = kv.PipelineEnabled
	t.pipelineActive = kv.PipelineActive
	if kv.PipelineOutput != "" {
		t.pipelineBuffer.WriteString(kv.PipelineOutput)
	}
	if kv.Pipelines != nil {
		t.pipelines = kv.Pipelines
	}
	if kv.PipelineName != "" {
		t.pipelineName = kv.PipelineName
	} else {
		t.pipelineName = "no-pipeline"
	}

	for _, lkv := range kv.Lanes {
		lkvCopy := lkv // capture for closure
		la := NewLaneActorFromKV(t.id, t.cfg, t.pub, t.nc, t.agSetup, t.kvStore, t.secrets, lkvCopy)
		laneProps := actor.PropsFromProducer(func() actor.Actor { return la })
		pid := ctx.Spawn(laneProps)

		ref := &laneRef{
			id:   lkvCopy.ID,
			flex: lkvCopy.Flex,
		}
		// Compute pane count and active pane from KV data.
		totalPanes := 0
		for _, gkv := range lkvCopy.PaneGroups {
			totalPanes += len(gkv.PaneRefs)
		}
		ref.paneCount = totalPanes
		if lkvCopy.ActiveGroup >= 0 && lkvCopy.ActiveGroup < len(lkvCopy.PaneGroups) {
			ag := lkvCopy.PaneGroups[lkvCopy.ActiveGroup]
			if ag.ActivePane >= 0 && ag.ActivePane < len(ag.PaneRefs) {
				ref.activePaneID = ag.PaneRefs[ag.ActivePane].ID
			}
		}

		t.laneRefs = append(t.laneRefs, ref)
		t.lanePIDs[lkvCopy.ID] = pid
		t.laneActors[lkvCopy.ID] = la
		t.laneSubjects[lkvCopy.ID] = msg.T("lane", lkvCopy.ID, "inbox")

		// Update pane-to-lane mapping.
		for _, gkv := range lkvCopy.PaneGroups {
			for _, pk := range gkv.PaneRefs {
				t.paneToLane[pk.ID] = lkvCopy.ID
			}
		}
	}

	// Normalize flex values: if total flex is below 10 (e.g. old sessions with
	// flex=1 per lane), scale all flex values up so 10% resize increments work.
	if len(t.laneRefs) > 1 {
		totalFlex := 0
		for _, lr := range t.laneRefs {
			totalFlex += lr.flex
		}
		if totalFlex > 0 && totalFlex < 10*len(t.laneRefs) {
			scale := (10*len(t.laneRefs) + totalFlex - 1) / totalFlex
			if scale > 1 {
				for _, lr := range t.laneRefs {
					lr.flex *= scale
				}
			}
		}
	}
}
