package actors

import (
	"fmt"
	"strings"
	"time"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/webauto"
)

// ---------------------------------------------------------------------------
// Run recording — workspace wiring for `##auto web run --record`
//
// One recorder per pane, superseded by the next run on that pane. Stopping is
// always graceful (recStop, never Root.Stop) because stopping is what triggers
// the encode: killing the actor outright would leave a frames directory and no
// video. See web_recorder.go for the capture side.
// ---------------------------------------------------------------------------

// startWebRecorder resolves the recording plan for a run just dispatched on
// paneID and, when recording is enabled, spawns the recorder. supervised marks
// runs whose AutoLoopActor owns completion — those recorders span every pass of
// the loop and are stopped from handleAutoRunDone; unsupervised ones watch the
// step stream and stop themselves.
func (w *WorkspaceActor) startWebRecorder(out *strings.Builder, a *webauto.Automation,
	paneID, outputDir string, ov webAutoRunOverrides, supervised bool) {

	// A new run always supersedes the old recorder, recording or not: the
	// previous run's video must be closed out before this one starts.
	w.stopWebRecorder(paneID, "superseded by a new run on this pane")

	spec := webauto.ResolveRecord(a.Record, w.cfg.Automation.Web.Record, ov.record)
	if !spec.Enabled {
		return
	}
	startedAt := time.Now()
	path := spec.ResolvePath(outputDir, a.Name, startedAt)

	if w.autoRecorders == nil {
		w.autoRecorders = make(map[string]*actor.PID)
	}
	rec := NewWebRecorderActor(paneID, a.Name, spec, path, supervised, w.pub, w.nc)
	pid := w.actorSystem.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return rec }))
	w.autoRecorders[paneID] = pid

	fmt.Fprintf(out, "[web] recording: %s\n", spec.Describe(path))
	fmt.Fprintf(out, "[web] recording: screenshots are pixels of a logged-in browser — written locally, never shared or sent upstream\n")
}

// stopWebRecorder ends the active recorder for a pane, if any. The message is
// what makes it encode and report, so this is a Send and not a Stop.
func (w *WorkspaceActor) stopWebRecorder(paneID, reason string) {
	pid, ok := w.autoRecorders[paneID]
	if !ok {
		return
	}
	delete(w.autoRecorders, paneID)
	w.actorSystem.Root.Send(pid, &recStop{reason: reason})
}
