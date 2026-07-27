package actors

import (
	"reflect"
	"testing"

	"github.com/asynkron/protoactor-go/actor"
)

// The refactor's main risk is silent data loss: if the cascade builds a
// different document than the direct read, layout/state fields vanish from KV.
// These tests pin the two documents together.
//
// They drive the FALLBACK branch on purpose (no PIDs registered), which is safe
// to call with a nil actor.Context because requestKV short-circuits on a nil
// pid before ever touching the context. That keeps the comparison free of any
// actor-system setup while still executing the real toKVViaActors code path.

func TestLaneToKVViaActorsMatchesDirectRead(t *testing.T) {
	mk := func() *LaneActor {
		l := &LaneActor{
			id:          "lane-1",
			flex:        17,
			name:        "left",
			activeGroup: 1,
			groupRefs: []*laneGroupRef{
				{id: "g0", rowFlex: 13, activePaneID: "p0", paneCount: 1},
				{id: "g1", rowFlex: 7, activePaneID: "p1", paneCount: 2},
			},
			groupActors: map[string]*PaneGroupActor{
				"g0": {id: "g0", activePane: 0, paneRefs: []*paneRefInGroup{{id: "p0", title: "one"}}},
				"g1": {id: "g1", activePane: 1, paneRefs: []*paneRefInGroup{{id: "p1", title: "two"}, {id: "p2", title: "three"}}},
			},
			groupPIDs: map[string]*actor.PID{}, // none: force the fallback branch
		}
		return l
	}

	direct := mk().ToKV()
	viaActors := mk().toKVViaActors(nil)

	if !reflect.DeepEqual(direct, viaActors) {
		t.Errorf("lane KV mismatch — the cascade must not drop or alter fields\n direct=%+v\n  cascade=%+v",
			direct, viaActors)
	}
	// Spot-check the fields that carry layout state, so a DeepEqual that happens
	// to compare two equally-empty documents cannot pass vacuously.
	if viaActors.Flex != 17 || len(viaActors.PaneGroups) != 2 || len(viaActors.GroupRowFlex) != 2 {
		t.Fatalf("cascade produced a degenerate document: %+v", viaActors)
	}
	if viaActors.GroupRowFlex[0] != 13 || viaActors.GroupRowFlex[1] != 7 {
		t.Errorf("rowFlex weights not carried through: %v", viaActors.GroupRowFlex)
	}
	if len(viaActors.PaneGroups[1].PaneRefs) != 2 {
		t.Errorf("stacked pane refs not carried through: %+v", viaActors.PaneGroups[1])
	}
}

func TestTabToKVViaActorsMatchesDirectRead(t *testing.T) {
	mk := func() *TabActor {
		lane := func(id string, flex int) *LaneActor {
			return &LaneActor{
				id: id, flex: flex, activeGroup: 0,
				groupRefs:   []*laneGroupRef{{id: id + "-g0", rowFlex: 10, activePaneID: id + "-p0", paneCount: 1}},
				groupActors: map[string]*PaneGroupActor{id + "-g0": {id: id + "-g0", paneRefs: []*paneRefInGroup{{id: id + "-p0", title: "t"}}}},
				groupPIDs:   map[string]*actor.PID{},
			}
		}
		tb := &TabActor{
			id: "tab-1", title: "work", activeLane: 1, pipelineName: "no-pipeline",
			laneRefs: []*laneRef{
				{id: "l0", flex: 23, activePaneID: "l0-p0", paneCount: 1},
				{id: "l1", flex: 7, activePaneID: "l1-p0", paneCount: 1},
			},
			laneActors: map[string]*LaneActor{"l0": lane("l0", 23), "l1": lane("l1", 7)},
			lanePIDs:   map[string]*actor.PID{}, // none: force the fallback branch
		}
		return tb
	}

	direct := mk().ToKV()
	viaActors := mk().toKVViaActors(nil)

	if !reflect.DeepEqual(direct, viaActors) {
		t.Errorf("tab KV mismatch — the cascade must not drop or alter fields\n direct=%+v\n  cascade=%+v",
			direct, viaActors)
	}
	if len(viaActors.Lanes) != 2 {
		t.Fatalf("cascade produced a degenerate document: %+v", viaActors)
	}
	// The tab's laneRef flex is authoritative and must win over the lane's own
	// copy — this is what keeps a resize from being lost on the next restore.
	if viaActors.Lanes[0].Flex != 23 || viaActors.Lanes[1].Flex != 7 {
		t.Errorf("authoritative lane flex not applied: %d, %d",
			viaActors.Lanes[0].Flex, viaActors.Lanes[1].Flex)
	}
	if viaActors.ActiveLane != 1 || viaActors.Title != "work" {
		t.Errorf("tab-owned fields not carried through: %+v", viaActors)
	}
}

// requestKV must fail closed (so the caller falls back to a direct read) rather
// than panicking, when there is no actor to ask.
func TestRequestKVNilPIDFallsBack(t *testing.T) {
	// nil ctx is fine: the nil-pid guard returns before the context is used.
	reply, ok := requestKV[*paneGroupKVReply](nil, nil, &paneGroupKVRequest{})
	if ok {
		t.Errorf("expected ok=false for a nil pid, got reply=%+v", reply)
	}
	if reply != nil {
		t.Errorf("expected a zero reply for a nil pid, got %+v", reply)
	}
}
