// SPDX-License-Identifier: Apache-2.0

package actors

import "github.com/asynkron/protoactor-go/actor"

// spawnDetached spawns props at the actor system root rather than as a child of
// ctx, so the new actor's LIFETIME is owned by bookkeeping instead of by
// protoactor supervision.
//
// It exists for one reason: ##move. Stopping an actor stops all of its children
// (actorContext.handleStop → stopAllChildren), so a pane spawned by its pane
// group dies when that group is closed — including a pane that had since been
// moved into a different group, lane or tab and was happily running there. The
// group would have been killing a pane it no longer holds.
//
// The trade is explicit teardown: nothing stops a detached actor implicitly, so
// whoever holds it must stop it. PaneGroupActor does that in its *actor.Stopping
// handler, which keeps the close cascade (tab → lane → group → panes) intact —
// it just runs one hop by hand instead of by supervision.
//
// Failure supervision is unchanged in practice: a root-spawned actor that panics
// is handled by protoactor's root guardian with the default restart strategy,
// the same strategy the pane group used.
func spawnDetached(ctx actor.Context, props *actor.Props) *actor.PID {
	system := ctx.ActorSystem()
	if system == nil {
		// No system to spawn into (unit tests build actors directly); fall back
		// to a child spawn so behaviour degrades to the pre-move arrangement
		// rather than nil-panicking.
		return ctx.Spawn(props)
	}
	return system.Root.Spawn(props)
}
