package actors

import (
	"log/slog"
	"strings"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/bridge"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// LoadedPipeline represents a pipeline loaded from a YAML file.
type LoadedPipeline struct {
	Name         string          `json:"name"`          // from rysh.name in YAML
	File         string          `json:"file"`          // source filename
	Language     string          `json:"language"`      // e.g., "golang"
	Phases       []PipelinePhase `json:"phases"`        // ordered phases
	Status       string          `json:"status"`        // "idle", "running", "done", "error"
	CurrentPhase int             `json:"current_phase"` // index of currently executing phase (-1 if idle)
}

// PipelinePhase represents a single phase in a pipeline.
type PipelinePhase struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// laneRef records the layout metadata for a lane within a tab.
type laneRef struct {
	id           string
	flex         int
	activePaneID string // cached active pane ID from the lane
	paneCount    int    // cached pane count from the lane
}

// TabActor manages the lanes within a single tab.
//
// All fields are unguarded -- the proto.actor mailbox guarantees sequential Receive().
type TabActor struct {
	id      string
	title   string
	cfg     config.Config
	pub     *msg.NATSPublisher
	agSetup *agentic.Setup
	nc      *nats.Conn
	kvStore nats.KeyValue // rysh-panes bucket
	secrets *secretResolver // workspace-scoped ##secret lookup, threaded to panes
	br      *bridge.NATSBridge

	// Unguarded state.
	laneRefs     []*laneRef
	activeLane   int
	laneSubjects map[string]string // laneID -> inbox subject
	lanePIDs     map[string]*actor.PID
	laneActors   map[string]*LaneActor
	paneToLane   map[string]string // paneID -> laneID

	// initialPaneTitles holds pane titles to create as lanes during *actor.Started.
	// This avoids the publish-before-subscribe race by creating lanes inline
	// instead of relying on NATS messages that may arrive before the bridge is ready.
	// Consumed once in *actor.Started and set to nil.
	initialPaneTitles []string

	// initialLaneTitles seeds a full grid during *actor.Started: one lane per
	// outer slice, with one pane (pane group) per inner title. Takes precedence
	// over initialPaneTitles. Like initialPaneTitles it is created inline to
	// avoid the publish-before-subscribe race. Consumed once and set to nil.
	initialLaneTitles [][]string

	// restoreData is set when the actor should restore from KV on *actor.Started.
	// It is consumed once and set to nil.
	restoreData *tabKV

	// Pipeline state
	pipelineEnabled          bool // gate for ##pipe commands and pipeline mode
	pipelineActive           bool
	pipelineBuffer           strings.Builder
	pipelineLLMPromptExecPID *actor.PID
	pipelinePaneID           string                     // synthetic pane ID: "pipeline-" + tabID
	pipelines                map[string]*LoadedPipeline // name -> pipeline
	pipelineName             string                     // label shown on first pane border; default "no-pipeline"
}

// NewTabActor creates a new TabActor.
//
// initialPaneTitles specifies pane titles that will be created as lanes during
// *actor.Started, eliminating the publish-before-subscribe race that occurs
// when the caller publishes MsgTabCreatePane before the bridge is ready.
func NewTabActor(
	id, title string,
	cfg config.Config,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
	agSetup *agentic.Setup,
	kvStore nats.KeyValue,
	secrets *secretResolver,
	initialPaneTitles []string,
) *TabActor {
	return &TabActor{
		id:                id,
		title:             title,
		cfg:               cfg,
		pub:               pub,
		agSetup:           agSetup,
		nc:                nc,
		kvStore:           kvStore,
		secrets:           secrets,
		initialPaneTitles: initialPaneTitles,
		laneSubjects:      make(map[string]string),
		lanePIDs:          make(map[string]*actor.PID),
		laneActors:        make(map[string]*LaneActor),
		paneToLane:        make(map[string]string),
		pipelines:         make(map[string]*LoadedPipeline),
		pipelineName:      "no-pipeline",
	}
}

// NewTabActorFromKV creates a TabActor that will restore state from
// restoreData on its first *actor.Started message.
func NewTabActorFromKV(
	cfg config.Config,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
	agSetup *agentic.Setup,
	kvStore nats.KeyValue,
	secrets *secretResolver,
	kv tabKV,
) *TabActor {
	ta := NewTabActor(kv.ID, kv.Title, cfg, pub, nc, agSetup, kvStore, secrets, nil)
	ta.restoreData = &kv
	return ta
}

// Receive implements actor.Actor.
func (t *TabActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		t.br = bridge.New(t.nc, ctx.Self(), ctx.ActorSystem(), t.pub.Codecs())
		_ = t.br.AddSubject(msg.T("tab", t.id, "inbox"))
		_ = t.br.AddSubject(msg.T("tab", t.id, "snapshot"))
		_ = t.br.AddSubject(msg.T("tab", t.id, "pipelineOutput"))

		// Create initial lanes synchronously before any NATS messages arrive.
		// This eliminates the publish-before-subscribe race that occurred when
		// the workspace sent MsgTabCreatePane via NATS before the bridge was ready.
		if len(t.initialLaneTitles) > 0 {
			// Grid seed: each lane gets its own set of pane titles.
			for _, paneTitles := range t.initialLaneTitles {
				t.createLaneWithPanes(ctx, paneTitles)
			}
			t.initialLaneTitles = nil
			if len(t.laneRefs) > 0 {
				t.equalizeLanes()
				t.activeLane = 0
				t.normalizeFlex()
			}
		} else if len(t.initialPaneTitles) > 0 {
			for _, title := range t.initialPaneTitles {
				t.createLane(ctx, title)
			}
			t.initialPaneTitles = nil
		}

		// If restore data was provided (session resume path), restore lanes now.
		if t.restoreData != nil {
			t.doRestoreFromKV(ctx, *t.restoreData)
			t.restoreData = nil
		}
		replayScope(t.agSetup, agentic.ScopeTab, agentic.ScopeIDs{TabID: t.id})

		// Report readiness (and the initial active pane) to the parent
		// workspace via a direct in-process send. The workspace's createTab
		// runs syncActivePane immediately after Spawn — a NATS request that
		// can be published before this actor's bridge subscription is live.
		// Core NATS drops publishes with no subscriber, the request times out,
		// and the workspace's activePaneID stays "" with no retry — wedging
		// all input routing for the session (the TUI drops keystrokes when
		// the snapshot carries no active pane). This in-process notification
		// is delivery-guaranteed and heals that race deterministically.
		// Lane/pane IDs are generated synchronously in createLane /
		// createLaneWithPanes / doRestoreFromKV, so laneRefs is authoritative
		// here even though the lane actors themselves start asynchronously.
		if parent := ctx.Parent(); parent != nil {
			activePaneID := ""
			paneCount := 0
			for _, lr := range t.laneRefs {
				paneCount += lr.paneCount
			}
			if t.activeLane >= 0 && t.activeLane < len(t.laneRefs) {
				activePaneID = t.laneRefs[t.activeLane].activePaneID
			}
			ctx.Send(parent, &tabReadyMsg{
				tabID:        t.id,
				activePaneID: activePaneID,
				paneCount:    paneCount,
			})
		}

	case *actor.Stopping:
		teardownScope(t.agSetup, agentic.ScopeTab, t.id)
		if t.br != nil {
			t.br.Stop()
			t.br = nil
		}

	case *msg.MsgSetWorkingDir:
		// Update this tab's config copy so lanes/groups/panes created later use
		// the new cwd, then fan the change out to existing lanes.
		t.cfg.WorkingDirectory = m.Dir
		for _, lr := range t.laneRefs {
			_ = t.pub.Send(msg.T("lane", lr.id, "inbox"), &msg.MsgSetWorkingDir{Dir: m.Dir})
		}

	case *msg.MsgTabCreatePane:
		t.createLane(ctx, m.Title)

	case *msg.MsgTabCreatePaneDown:
		t.createPaneInLane(ctx, m.Title)

	case *msg.MsgTabClosePane:
		t.closePaneInLane(ctx)

	case *msg.MsgTabFocus:
		switch m.Direction {
		case msg.DirNext:
			t.focusNextPaneGlobal()
		case msg.DirPrev:
			t.focusPrevPaneGlobal()
		case msg.DirLeft:
			t.focusLaneLeft()
		case msg.DirRight:
			t.focusLaneRight()
		case msg.DirUp:
			t.focusLaneUp()
		case msg.DirDown:
			t.focusLaneDown()
		}

	case *msg.MsgTabFocusPaneByID:
		t.focusPaneByID(m.ID)

	case *msg.MsgTabCreateStackedPane:
		paneID := uuid.NewString()
		t.forwardToActiveLane(&msg.MsgLaneCreateStackedPane{
			PaneID: paneID,
			Title:  m.Title,
		})
		if len(t.laneRefs) > 0 && t.activeLane >= 0 && t.activeLane < len(t.laneRefs) {
			t.paneToLane[paneID] = t.laneRefs[t.activeLane].id
		}
		t.updateActivePaneFromLane()

	case *msg.MsgTabStackedPane:
		t.forwardToActiveLane(&msg.MsgLaneStackedPane{Direction: m.Direction})
		t.updateActivePaneFromLane()

	case *msg.MsgTabStackedPaneSelect:
		t.forwardToActiveLane(&msg.MsgLaneStackedPaneSelect{Index: m.Index})
		t.updateActivePaneFromLane()

	case *msg.MsgTabStackedPaneMove:
		t.forwardToActiveLane(&msg.MsgLaneStackedPaneMove{Direction: m.Direction})
		t.updateActivePaneFromLane()

	case *msg.MsgTabEqualizeHorizontal:
		slog.Debug("tab: received MsgTabEqualizeHorizontal", "numLanes", len(t.laneRefs))
		t.equalizeLanes()

	case *msg.MsgTabEqualizePanes:
		t.equalizeLanes()

	case *msg.MsgTabResizePaneWidth:
		t.resizeActiveLane(m.Delta)

	case *msg.MsgTabSwapPane:
		t.swapLanes()

	case *msg.MsgTabResizePane:
		t.resizeActiveLane(m.Delta)

	case *msg.MsgTabResizePaneHeight:
		slog.Debug("tab: received MsgTabResizePaneHeight", "delta", m.Delta, "activeLane", t.activeLane, "numLanes", len(t.laneRefs))
		t.forwardToActiveLane(&msg.MsgLaneResizeGroupHeight{Delta: m.Delta})

	case *msg.MsgTabEqualizeVertical:
		slog.Debug("tab: received MsgTabEqualizeVertical", "activeLane", t.activeLane)
		t.forwardToActiveLane(&msg.MsgLaneEqualizeGroups{})

	case *msg.MsgTabEqualizeAll:
		slog.Debug("tab: received MsgTabEqualizeAll", "numLanes", len(t.laneRefs))
		t.equalizeLanes()
		t.forwardToAllLanes(&msg.MsgLaneEqualizeGroups{})

	// Serialise for persistence on THIS actor's goroutine, cascading to the
	// lanes (and through them to the pane groups). See kv_cascade.go.
	case *tabKVRequest:
		ctx.Respond(&tabKVReply{kv: t.toKVViaActors(ctx)})

	case *msg.MsgTabSubmitInput:
		t.submitInput(ctx, m)

	case *msg.MsgTogglePipelineMode:
		// Pipeline INPUT mode is only meaningful with a pipeline enabled on
		// this tab. Without the guard, an accidental toggle (ctrl+p p sits
		// right next to ctrl+p s) flipped a plain tab into pipeline mode: the
		// focused pane started rendering the tab's (empty) pipeline view, so
		// the same content followed every focus change — which users read as
		// "panes moving to wherever I click". Deactivating is always allowed.
		switch {
		case t.pipelineActive:
			t.pipelineActive = false
		case t.pipelineEnabled:
			t.pipelineActive = true
		}

	case *msg.MsgTabPipelineEnable:
		t.pipelineEnabled = true

	case *msg.MsgTabPipelineDisable:
		t.pipelineEnabled = false
		t.pipelineActive = false // also deactivate pipeline mode

	case *msg.MsgPipelineCommand:
		if m.Cmd == "run" {
			t.cmdPipelineRunWithContext(ctx, m.PaneID, m.Args)
		}

	case *msg.MsgPipelineOutputAppend:
		t.pipelineBuffer.WriteString(m.Text)

	// --- CLI targeted operations ---

	case *msg.MsgTabDeleteLane:
		t.deleteLaneByID(ctx, m.LaneID)

	case *msg.MsgTabCreatePaneGroupInLane:
		t.createPaneGroupInLane(ctx, m.LaneID, m.Title, m.GroupID, m.WorkingDir, m.PaneID, m.PaneType)

	case *msg.MsgTabCreateGrid:
		t.createGridHere(ctx, m.LaneTitles)

	case *msg.MsgTabCreateGroupsInLane:
		t.createGroupsInLane(m.LaneID, m.Titles)

	case *msg.MsgTabCreateStackedPaneInLane:
		t.createStackedPaneInLane(m.LaneID, m.PaneGroupID, m.Title)

	case *msg.MsgTabSetActive:
		// Nothing specific to do for now.

	case *msg.MsgTabSetInactive:
		// Nothing specific to do for now.

	case *msg.RequestEnvelope:
		switch inner := m.Inner.(type) {
		case *msg.MsgGetTabSnapshot:
			snap := t.collectSnapshot(inner.LayoutOnly, inner.NoHistories)
			_ = m.Reply(&msg.MsgTabSnapshotReply{Snapshot: snap})
		case *msg.MsgGetActivePane:
			// Refresh lane pane counts to avoid stale cached values.
			t.refreshAllLanePaneCounts()
			paneID := ""
			totalPanes := 0
			for _, lr := range t.laneRefs {
				totalPanes += lr.paneCount
			}
			if len(t.laneRefs) > 0 && t.activeLane >= 0 && t.activeLane < len(t.laneRefs) {
				paneID = t.laneRefs[t.activeLane].activePaneID
			}
			_ = m.Reply(&msg.MsgActivePaneReply{PaneID: paneID, PaneCount: totalPanes})
		}
	}
}
