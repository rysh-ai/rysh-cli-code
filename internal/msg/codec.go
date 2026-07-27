package msg

import (
	"encoding/json"

	sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"
)

// ---------------------------------------------------------------------------
// TypeTag constants — stable string names for CLI-specific message types.
// Shared types (output, history, agentic) are defined in shared_aliases.go.
// ---------------------------------------------------------------------------

const (
	// TUI → WorkspaceActor
	TagCreateTab           = "MsgCreateTab"
	TagCreatePane          = "MsgCreatePane"
	TagCreatePaneDown      = "MsgCreatePaneDown"
	TagClosePane           = "MsgClosePane"
	TagFocusNextTab        = "MsgFocusNextTab"
	TagFocusPrevTab        = "MsgFocusPrevTab"
	TagFocusTabIndex       = "MsgFocusTabIndex"
	TagMoveTab             = "MsgMoveTab"
	TagFocusPane           = "MsgFocusPane"
	TagFocusPaneByID       = "MsgFocusPaneByID"
	TagResizePane          = "MsgResizePane"
	TagResizePaneHeight    = "MsgResizePaneHeight"
	TagSubmitInput         = "MsgSubmitInput"
	TagWebPromptDispatched = "MsgWebPromptDispatched"
	TagWebActivate         = "MsgWebActivate"
	TagWebDeactivate       = "MsgWebDeactivate"
	TagRenamePane          = "MsgRenamePane"
	TagRenameTab           = "MsgRenameTab"
	TagRenameLane          = "MsgRenameLane"
	TagShutdown            = "MsgShutdown"
	TagSwitchWorkspace     = "MsgSwitchWorkspace"
	TagReconcileWorkspaces = "MsgReconcileWorkspaces"

	// MCP restart-state (follow-up 6b). Published session-globally to
	// T("mcp","status"); rendered by the TUI footer.
	TagMCPStatus = "MsgMCPStatus"

	// Prompt auto-reload trigger (follow-up 2b). Published to ws.inbox by
	// the fsnotify watcher; the active WorkspaceActor runs the same reload +
	// broadcast path as ##agent reload-prompts.
	TagReloadPromptsRequest = "MsgReloadPromptsRequest"

	// Broadcast down Workspace → Tab → Lane → PaneGroup to update pane cwd.
	TagSetWorkingDir = "MsgSetWorkingDir"

	// WorkspaceActor → TabActor
	TagTabCreatePane       = "MsgTabCreatePane"
	TagTabCreatePaneDown   = "MsgTabCreatePaneDown"
	TagTabClosePane        = "MsgTabClosePane"
	TagTabFocus            = "MsgTabFocus"
	TagTabFocusPaneByID    = "MsgTabFocusPaneByID"
	TagTabResizePane       = "MsgTabResizePane"
	TagTabResizePaneHeight = "MsgTabResizePaneHeight"
	TagTabSubmitInput      = "MsgTabSubmitInput"
	TagTabSetActive        = "MsgTabSetActive"
	TagTabSetInactive      = "MsgTabSetInactive"

	// TabActor → LaneActor
	TagLaneCreatePaneGroup   = "MsgLaneCreatePaneGroup"
	TagLaneClosePaneGroup    = "MsgLaneClosePaneGroup"
	TagLaneCloseActivePane   = "MsgLaneCloseActivePane"
	TagLaneFocusGroup        = "MsgLaneFocusGroup"
	TagLaneFocusPaneByID     = "MsgLaneFocusPaneByID"
	TagLaneCreateStackedPane = "MsgLaneCreateStackedPane"
	TagLaneStackedPane       = "MsgLaneStackedPane"
	TagLaneStackedPaneSelect = "MsgLaneStackedPaneSelect"
	TagLaneStackedPaneMove   = "MsgLaneStackedPaneMove"
	TagLaneResizeGroupHeight = "MsgLaneResizeGroupHeight"
	TagGetLaneSnapshot       = "MsgGetLaneSnapshot"
	TagLaneSnapshotReply     = "MsgLaneSnapshotReply"
	TagGetLaneActivePane     = "MsgGetLaneActivePane"
	TagLaneActivePaneReply   = "MsgLaneActivePaneReply"
	// LaneActor → PaneGroupActor
	TagGetPaneGroupSnapshot     = "MsgGetPaneGroupSnapshot"
	TagPaneGroupSnapshotReply   = "MsgPaneGroupSnapshotReply"
	TagGetPaneGroupActivePane   = "MsgGetPaneGroupActivePane"
	TagPaneGroupActivePaneReply = "MsgPaneGroupActivePaneReply"

	// Stacked panes: TUI → Workspace
	TagCreateStackedPane = "MsgCreateStackedPane"
	TagStackedPaneRotate = "MsgStackedPaneRotate"
	TagStackedPaneSelect = "MsgStackedPaneSelect"
	TagStackedPaneMove   = "MsgStackedPaneMove"

	// Stacked panes: Workspace → Tab
	TagTabCreateStackedPane = "MsgTabCreateStackedPane"
	TagTabStackedPane       = "MsgTabStackedPane"
	TagTabStackedPaneSelect = "MsgTabStackedPaneSelect"
	TagTabStackedPaneMove   = "MsgTabStackedPaneMove"

	// Stacked panes: Tab/Lane → PaneGroup
	TagPaneGroupCreateStackedPane = "MsgPaneGroupCreateStackedPane"
	TagPaneGroupStackedPane       = "MsgPaneGroupStackedPane"
	TagPaneGroupStackedPaneSelect = "MsgPaneGroupStackedPaneSelect"
	TagPaneGroupFocusPaneByID     = "MsgPaneGroupFocusPaneByID"
	TagPaneGroupStackedPaneMove   = "MsgPaneGroupStackedPaneMove"

	// Given-name messages (Workspace → Pane, direct)
	TagPaneSetGivenName = "MsgPaneSetGivenName"

	// Per-pane provider override (Workspace → Pane, direct; design 002 §3.4)
	TagPaneSetProvider = "MsgPaneSetProvider"

	// Per-pane mode enable/disable (Workspace → Pane, direct)
	TagPaneEnableMode    = "MsgPaneEnableMode"
	TagPaneDisableMode   = "MsgPaneDisableMode"
	TagPaneActivateMode  = "MsgPaneActivateMode"
	TagPaneWebHeadless   = "MsgPaneWebHeadless"
	TagPaneImportCookies = "MsgPaneImportCookies"

	// PaneGroupActor → PaneActor
	TagPaneSubmitInput = "MsgPaneSubmitInput"
	TagPaneExecShell   = "MsgPaneExecShell"
	TagPaneExecPrompt  = "MsgPaneExecPrompt"
	TagPaneExecRysh    = "MsgPaneExecRysh"
	TagPaneExecChat    = "MsgPaneExecChat"
	TagPaneSetTitle    = "MsgPaneSetTitle"
	TagPaneStop        = "MsgPaneStop"

	// PaneActor → LLMActor
	TagExecPrompt   = "MsgExecPrompt"
	TagCancelPrompt = "MsgCancelPrompt"

	// Snapshot request/reply
	TagGetWorkspaceSnapshot   = "MsgGetWorkspaceSnapshot"
	TagWorkspaceSnapshotReply = "MsgWorkspaceSnapshotReply"
	TagGetTabSnapshot         = "MsgGetTabSnapshot"
	TagTabSnapshotReply       = "MsgTabSnapshotReply"
	TagGetPaneSnapshot        = "MsgGetPaneSnapshot"
	TagPaneSnapshotReply      = "MsgPaneSnapshotReply"
	TagGetPaneVT              = "MsgGetPaneVT"
	TagPaneVTReply            = "MsgPaneVTReply"
	TagGetPaneScrollback      = "MsgGetPaneScrollback"
	TagPaneScrollbackReply    = "MsgPaneScrollbackReply"

	TagGetPaneScrollbackDelta   = "MsgGetPaneScrollbackDelta"
	TagPaneScrollbackDeltaReply = "MsgPaneScrollbackDeltaReply"
	TagGetMirrorScrollback      = "MsgGetMirrorScrollback"
	TagMirrorScrollbackReply    = "MsgMirrorScrollbackReply"
	TagGetMirrorPaneVT          = "MsgGetMirrorPaneVT"
	TagMirrorPaneVTReply        = "MsgMirrorPaneVTReply"
	TagMirrorPaneVTFrame        = "MsgMirrorPaneVTFrame"

	// Supervision notifications
	TagPaneTerminated = "MsgPaneTerminated"
	TagTabTerminated  = "MsgTabTerminated"

	// TabActor active-pane query
	TagGetActivePane   = "MsgGetActivePane"
	TagActivePaneReply = "MsgActivePaneReply"

	// Pane listener (cross-pane output listening)
	TagStartPaneListener = "MsgStartPaneListener"
	TagStopPaneListener  = "MsgStopPaneListener"

	// Hop commands
	TagPaneHopContent = "MsgPaneHopContent"
	TagPaneHopResume  = "MsgPaneHopResume"
	TagPaneHopClear   = "MsgPaneHopClear"

	// Pipeline mode
	TagTogglePipelineMode = "MsgTogglePipelineMode"
	TagPipelineCommand    = "MsgPipelineCommand"
	TagTabPipelineEnable  = "MsgTabPipelineEnable"
	TagTabPipelineDisable = "MsgTabPipelineDisable"

	// Layout management: TUI → Workspace
	TagEqualizeHorizontal = "MsgEqualizeHorizontal"
	TagEqualizeVertical   = "MsgEqualizeVertical"
	TagEqualizeAll        = "MsgEqualizeAll"
	TagEqualizePanes      = "MsgEqualizePanes"
	TagResizePaneWidth    = "MsgResizePaneWidth"
	TagSwapPane           = "MsgSwapPane"

	// Layout management: Workspace → Tab
	TagTabEqualizeHorizontal = "MsgTabEqualizeHorizontal"
	TagTabEqualizeVertical   = "MsgTabEqualizeVertical"
	TagTabEqualizeAll        = "MsgTabEqualizeAll"
	TagTabEqualizePanes      = "MsgTabEqualizePanes"
	TagTabResizePaneWidth    = "MsgTabResizePaneWidth"
	TagTabSwapPane           = "MsgTabSwapPane"

	// Layout management: Tab → Lane
	TagLaneEqualizeGroups = "MsgLaneEqualizeGroups"

	// CLI messages
	TagCLICreateTab                 = "MsgCLICreateTab"
	TagCLIDeleteTab                 = "MsgCLIDeleteTab"
	TagCLICreateLane                = "MsgCLICreateLane"
	TagCLIDeleteLane                = "MsgCLIDeleteLane"
	TagCLICreatePaneGroup           = "MsgCLICreatePaneGroup"
	TagCLIDeletePaneGroup           = "MsgCLIDeletePaneGroup"
	TagCLICreatePane                = "MsgCLICreatePane"
	TagCLIDeletePane                = "MsgCLIDeletePane"
	TagCLICreateStackedPane         = "MsgCLICreateStackedPane"
	TagCLIPipelineEnable            = "MsgCLIPipelineEnable"
	TagCLIPipelineDisable           = "MsgCLIPipelineDisable"
	TagCLIResponse                  = "MsgCLIResponse"
	TagCLIRyshCommand               = "MsgCLIRyshCommand"
	TagTabDeleteLane                = "MsgTabDeleteLane"
	TagTabCreatePaneGroupInLane     = "MsgTabCreatePaneGroupInLane"
	TagTabCreateGrid                = "MsgTabCreateGrid"
	TagTabCreateGroupsInLane        = "MsgTabCreateGroupsInLane"
	TagTabCreateStackedPaneInLane   = "MsgTabCreateStackedPaneInLane"
	TagLaneDeletePaneGroup          = "MsgLaneDeletePaneGroup"
	TagLaneCreateStackedPaneInGroup = "MsgLaneCreateStackedPaneInGroup"
	TagPaneGroupDeletePane          = "MsgPaneGroupDeletePane"

	// Remote upstream / pane sharing
	TagPaneShareStart           = "MsgPaneShareStart"
	TagPaneShareStop            = "MsgPaneShareStop"
	TagPaneShareStatus          = "MsgPaneShareStatus"
	TagPaneShareStatusReply     = "MsgPaneShareStatusReply"
	TagPaneSetSharingState      = "MsgPaneSetSharingState"
	TagRemoteUpstreamConnect    = "MsgRemoteUpstreamConnect"
	TagRemoteUpstreamDisconnect = "MsgRemoteUpstreamDisconnect"
	TagRemoteUpstreamStatus     = "MsgRemoteUpstreamStatus"

	// Shared-panes-via-upstream
	TagShareEntity               = "MsgShareEntity"
	TagShareForgedAPI            = "MsgShareForgedAPI"
	TagPaneRegisterForgedProxies = "MsgPaneRegisterForgedProxies"
	TagUnshareEntity             = "MsgUnshareEntity"
	TagShareStatus               = "MsgShareStatus"
	TagShareStatusReply          = "MsgShareStatusReply"
	TagShareList                 = "MsgShareList"
	TagShareListReply            = "MsgShareListReply"
	TagUpstreamCommand           = "MsgUpstreamCommand"
	TagUpstreamCommandAck        = "MsgUpstreamCommandAck"
	TagShareRegisterAck          = "MsgShareRegisterAck"
	TagUpstreamSharesList        = "MsgUpstreamSharesList"
	TagUpstreamSubscribe         = "MsgUpstreamSubscribe"
	TagUpstreamUnsubscribe       = "MsgUpstreamUnsubscribe"
	TagUpstreamSendCommand       = "MsgUpstreamSendCommand"
	TagShareOutput               = "MsgShareOutput"

	// Controller mode (remote upstream)
	TagSetControllerMode    = "MsgSetControllerMode"
	TagSetConnectedPane     = "MsgSetConnectedPane"
	TagRemoteForwardCommand = "MsgRemoteForwardCommand"
	TagExecRyshOnPane       = "MsgExecRyshOnPane"
	TagMirrorTabOp          = "MsgMirrorTabOp"
	TagMirrorMaximizePane   = "MsgMirrorMaximizePane"
	TagRemotePaneFullscreen = "MsgRemotePaneFullscreen"

	// Share restrictions
	TagShareDisableMode         = "MsgShareDisableMode"
	TagShareEnableMode          = "MsgShareEnableMode"
	TagShareShellAllow          = "MsgShareShellAllow"
	TagShareShellForbid         = "MsgShareShellForbid"
	TagShareShellClear          = "MsgShareShellClear"
	TagShareSetFileBrowse       = "MsgShareSetFileBrowse"
	TagShareShowRestrictions    = "MsgShareShowRestrictions"
	TagShareRestrictionsUpdated = "MsgShareRestrictionsUpdated"
	TagPaneSetShareRestrictions = "MsgPaneSetShareRestrictions"

	// WebSocket connection reliability
	TagUpstreamReconnected      = "MsgUpstreamReconnected"
	TagUpstreamConnectionClosed = "MsgUpstreamConnectionClosed"

	// Autonomous agents
	TagAgentCreate         = "MsgAgentCreate"
	TagAgentDelete         = "MsgAgentDelete"
	TagAgentStop           = "MsgAgentStop"
	TagAgentContinue       = "MsgAgentContinue"
	TagAgentActivate       = "MsgAgentActivate"
	TagAgentDeactivate     = "MsgAgentDeactivate"
	TagAgentList           = "MsgAgentList"
	TagAgentListReply      = "MsgAgentListReply"
	TagAgentPrompt         = "MsgAgentPrompt"
	TagAgentRegisterPane   = "MsgAgentRegisterPane"
	TagAgentUnregisterPane = "MsgAgentUnregisterPane"

	// Humanoids (agents with external communication channels)
	TagHumanoidCreate            = "MsgHumanoidCreate"
	TagHumanoidDelete            = "MsgHumanoidDelete"
	TagHumanoidStop              = "MsgHumanoidStop"
	TagHumanoidContinue          = "MsgHumanoidContinue"
	TagHumanoidActivate          = "MsgHumanoidActivate"
	TagHumanoidDeactivate        = "MsgHumanoidDeactivate"
	TagHumanoidList              = "MsgHumanoidList"
	TagHumanoidListReply         = "MsgHumanoidListReply"
	TagHumanoidPrompt            = "MsgHumanoidPrompt"
	TagHumanoidRegisterPane      = "MsgHumanoidRegisterPane"
	TagHumanoidUnregisterPane    = "MsgHumanoidUnregisterPane"
	TagHumanoidChannelStart      = "MsgHumanoidChannelStart"
	TagHumanoidChannelStop       = "MsgHumanoidChannelStop"
	TagHumanoidChannelStatus     = "MsgHumanoidChannelStatus"
	TagHumanoidSetReplyMode      = "MsgHumanoidSetReplyMode"
	TagHumanoidSetGovernance     = "MsgHumanoidSetGovernance"
	TagHumanoidSetProvider       = "MsgHumanoidSetProvider"
	TagHumanoidInboundMessage    = "MsgHumanoidInboundMessage"
	TagHumanoidOutboundMessage   = "MsgHumanoidOutboundMessage"
	TagHumanoidEmailList         = "MsgHumanoidEmailList"
	TagHumanoidEmailRead         = "MsgHumanoidEmailRead"
	TagHumanoidEmailListReply    = "MsgHumanoidEmailListReply"
	TagHumanoidEmailReadReply    = "MsgHumanoidEmailReadReply"
	TagHumanoidEmailChanged      = "MsgHumanoidEmailChanged"
	TagHumanoidSetFocus          = "MsgHumanoidSetFocus"
	TagHumanoidEmailCompose      = "MsgHumanoidEmailCompose"
	TagHumanoidEmailComposeReply = "MsgHumanoidEmailComposeReply"

	TagHumanoidWhatsAppList      = "MsgHumanoidWhatsAppList"
	TagHumanoidWhatsAppRead      = "MsgHumanoidWhatsAppRead"
	TagHumanoidWhatsAppListReply = "MsgHumanoidWhatsAppListReply"
	TagHumanoidWhatsAppReadReply = "MsgHumanoidWhatsAppReadReply"
	TagHumanoidWhatsAppChanged   = "MsgHumanoidWhatsAppChanged"

	// Pane ← Humanoid
	TagPaneSetHumanoid = "MsgPaneSetHumanoid"

	// Contact pairing & allowlists (WS3, design 003) — humanoid.{name}.pairing
	TagChannelPairRequest   = "MsgChannelPairRequest"
	TagChannelPairApprove   = "MsgChannelPairApprove"
	TagChannelAllow         = "MsgChannelAllow"
	TagChannelPairList      = "MsgChannelPairList"
	TagChannelPairListReply = "MsgChannelPairListReply"
	TagChannelPairQR        = "MsgChannelPairQR"
	TagChannelPairStatus    = "MsgChannelPairStatus"
	TagChannelPairLink      = "MsgChannelPairLink"

	// Attention mechanism
	TagAttentionEvent   = "MsgAttentionEvent"
	TagAttentionAck     = "MsgAttentionAck"
	TagAttentionEnable  = "MsgAttentionEnable"
	TagAttentionDisable = "MsgAttentionDisable"

	// Raw/interactive terminal mode
	TagPaneResize      = "MsgPaneResize"
	TagPaneResized     = "MsgPaneResized"
	TagRawKeyInput     = "MsgRawKeyInput"
	TagPaneClearOutput = "MsgPaneClearOutput"
	TagPaneNativeMode  = "MsgPaneNativeMode"
	TagRelayActivate   = "MsgRelayActivate"
	TagRelayDeactivate = "MsgRelayDeactivate"

	// Interactive sharing
	TagPaneRawOutputAppend         = "MsgPaneRawOutputAppend"
	TagPaneShareModeChange         = "MsgPaneShareModeChange"
	TagPaneRawDirty                = "MsgPaneRawDirty"
	TagPaneReplayShareState        = "MsgPaneReplayShareState"
	TagRemoteInteractiveModeChange = "MsgRemoteInteractiveModeChange"
	TagPaneSetRemoteSubscriber     = "MsgPaneSetRemoteSubscriber"
	TagRemoteVTScreenUpdate        = "MsgRemoteVTScreenUpdate"
	TagRemoteScrollbackAppend      = "MsgRemoteScrollbackAppend"
	TagMirrorDirty                 = "MsgMirrorDirty"
	TagLayoutDirty                 = "MsgLayoutDirty"
)

// jsonDecoder builds a typed JSON decoder for use in DefaultCodecRegistry.
// This is a local copy since sharedmsg.jsonDecoder is unexported.
func jsonDecoder[T any]() func([]byte) (interface{}, error) {
	return func(b []byte) (interface{}, error) {
		v := new(T)
		if len(b) > 0 {
			if err := json.Unmarshal(b, v); err != nil {
				return nil, err
			}
		}
		return v, nil
	}
}

// DefaultCodecRegistry returns a CodecRegistry pre-registered with all message
// types: shared agentic/output types (from rysh-shared) plus CLI-specific
// workspace routing, snapshot, and sharing messages.
func DefaultCodecRegistry() *CodecRegistry {
	// Start with all shared types (agentic, output, history, status).
	r := sharedmsg.DefaultCodecRegistry()

	// CLI-specific workspace routing messages.
	r.Register(TagCreateTab, "*msg.MsgCreateTab", jsonDecoder[MsgCreateTab]())
	r.Register(TagCreatePane, "*msg.MsgCreatePane", jsonDecoder[MsgCreatePane]())
	r.Register(TagCreatePaneDown, "*msg.MsgCreatePaneDown", jsonDecoder[MsgCreatePaneDown]())
	r.Register(TagClosePane, "*msg.MsgClosePane", jsonDecoder[MsgClosePane]())
	r.Register(TagFocusNextTab, "*msg.MsgFocusNextTab", jsonDecoder[MsgFocusNextTab]())
	r.Register(TagFocusPrevTab, "*msg.MsgFocusPrevTab", jsonDecoder[MsgFocusPrevTab]())
	r.Register(TagFocusTabIndex, "*msg.MsgFocusTabIndex", jsonDecoder[MsgFocusTabIndex]())
	r.Register(TagMoveTab, "*msg.MsgMoveTab", jsonDecoder[MsgMoveTab]())
	r.Register(TagFocusPane, "*msg.MsgFocusPane", jsonDecoder[MsgFocusPane]())
	r.Register(TagFocusPaneByID, "*msg.MsgFocusPaneByID", jsonDecoder[MsgFocusPaneByID]())
	r.Register(TagResizePane, "*msg.MsgResizePane", jsonDecoder[MsgResizePane]())
	r.Register(TagResizePaneHeight, "*msg.MsgResizePaneHeight", jsonDecoder[MsgResizePaneHeight]())
	r.Register(TagSubmitInput, "*msg.MsgSubmitInput", jsonDecoder[MsgSubmitInput]())
	r.Register(TagWebPromptDispatched, "*msg.MsgWebPromptDispatched", jsonDecoder[MsgWebPromptDispatched]())
	r.Register(TagWebActivate, "*msg.MsgWebActivate", jsonDecoder[MsgWebActivate]())
	r.Register(TagWebDeactivate, "*msg.MsgWebDeactivate", jsonDecoder[MsgWebDeactivate]())
	r.Register(TagRenamePane, "*msg.MsgRenamePane", jsonDecoder[MsgRenamePane]())
	r.Register(TagRenameTab, "*msg.MsgRenameTab", jsonDecoder[MsgRenameTab]())
	r.Register(TagRenameLane, "*msg.MsgRenameLane", jsonDecoder[MsgRenameLane]())
	r.Register(TagShutdown, "*msg.MsgShutdown", jsonDecoder[MsgShutdown]())
	r.Register(TagSwitchWorkspace, "*msg.MsgSwitchWorkspace", jsonDecoder[MsgSwitchWorkspace]())
	r.Register(TagReconcileWorkspaces, "*msg.MsgReconcileWorkspaces", jsonDecoder[MsgReconcileWorkspaces]())
	r.Register(TagMCPStatus, "*msg.MsgMCPStatus", jsonDecoder[MsgMCPStatus]())
	r.Register(TagReloadPromptsRequest, "*msg.MsgReloadPromptsRequest", jsonDecoder[MsgReloadPromptsRequest]())
	r.Register(TagSetWorkingDir, "*msg.MsgSetWorkingDir", jsonDecoder[MsgSetWorkingDir]())
	r.Register(TagTabCreatePane, "*msg.MsgTabCreatePane", jsonDecoder[MsgTabCreatePane]())
	r.Register(TagTabCreatePaneDown, "*msg.MsgTabCreatePaneDown", jsonDecoder[MsgTabCreatePaneDown]())
	r.Register(TagTabClosePane, "*msg.MsgTabClosePane", jsonDecoder[MsgTabClosePane]())
	r.Register(TagTabFocus, "*msg.MsgTabFocus", jsonDecoder[MsgTabFocus]())
	r.Register(TagTabFocusPaneByID, "*msg.MsgTabFocusPaneByID", jsonDecoder[MsgTabFocusPaneByID]())
	r.Register(TagTabResizePane, "*msg.MsgTabResizePane", jsonDecoder[MsgTabResizePane]())
	r.Register(TagTabResizePaneHeight, "*msg.MsgTabResizePaneHeight", jsonDecoder[MsgTabResizePaneHeight]())
	r.Register(TagTabSubmitInput, "*msg.MsgTabSubmitInput", jsonDecoder[MsgTabSubmitInput]())
	r.Register(TagTabCreateStackedPane, "*msg.MsgTabCreateStackedPane", jsonDecoder[MsgTabCreateStackedPane]())
	r.Register(TagTabStackedPane, "*msg.MsgTabStackedPane", jsonDecoder[MsgTabStackedPane]())
	r.Register(TagTabStackedPaneSelect, "*msg.MsgTabStackedPaneSelect", jsonDecoder[MsgTabStackedPaneSelect]())
	r.Register(TagTabStackedPaneMove, "*msg.MsgTabStackedPaneMove", jsonDecoder[MsgTabStackedPaneMove]())
	r.Register(TagTabSetActive, "*msg.MsgTabSetActive", jsonDecoder[MsgTabSetActive]())
	r.Register(TagTabSetInactive, "*msg.MsgTabSetInactive", jsonDecoder[MsgTabSetInactive]())
	r.Register(TagLaneCreatePaneGroup, "*msg.MsgLaneCreatePaneGroup", jsonDecoder[MsgLaneCreatePaneGroup]())
	r.Register(TagLaneClosePaneGroup, "*msg.MsgLaneClosePaneGroup", jsonDecoder[MsgLaneClosePaneGroup]())
	r.Register(TagLaneCloseActivePane, "*msg.MsgLaneCloseActivePane", jsonDecoder[MsgLaneCloseActivePane]())
	r.Register(TagLaneFocusGroup, "*msg.MsgLaneFocusGroup", jsonDecoder[MsgLaneFocusGroup]())
	r.Register(TagLaneFocusPaneByID, "*msg.MsgLaneFocusPaneByID", jsonDecoder[MsgLaneFocusPaneByID]())
	r.Register(TagLaneCreateStackedPane, "*msg.MsgLaneCreateStackedPane", jsonDecoder[MsgLaneCreateStackedPane]())
	r.Register(TagLaneStackedPane, "*msg.MsgLaneStackedPane", jsonDecoder[MsgLaneStackedPane]())
	r.Register(TagLaneStackedPaneSelect, "*msg.MsgLaneStackedPaneSelect", jsonDecoder[MsgLaneStackedPaneSelect]())
	r.Register(TagLaneStackedPaneMove, "*msg.MsgLaneStackedPaneMove", jsonDecoder[MsgLaneStackedPaneMove]())
	r.Register(TagLaneResizeGroupHeight, "*msg.MsgLaneResizeGroupHeight", jsonDecoder[MsgLaneResizeGroupHeight]())
	r.Register(TagGetLaneSnapshot, "*msg.MsgGetLaneSnapshot", jsonDecoder[MsgGetLaneSnapshot]())
	r.Register(TagLaneSnapshotReply, "*msg.MsgLaneSnapshotReply", jsonDecoder[MsgLaneSnapshotReply]())
	r.Register(TagGetLaneActivePane, "*msg.MsgGetLaneActivePane", jsonDecoder[MsgGetLaneActivePane]())
	r.Register(TagLaneActivePaneReply, "*msg.MsgLaneActivePaneReply", jsonDecoder[MsgLaneActivePaneReply]())
	r.Register(TagGetPaneGroupSnapshot, "*msg.MsgGetPaneGroupSnapshot", jsonDecoder[MsgGetPaneGroupSnapshot]())
	r.Register(TagPaneGroupSnapshotReply, "*msg.MsgPaneGroupSnapshotReply", jsonDecoder[MsgPaneGroupSnapshotReply]())
	r.Register(TagGetPaneGroupActivePane, "*msg.MsgGetPaneGroupActivePane", jsonDecoder[MsgGetPaneGroupActivePane]())
	r.Register(TagPaneGroupActivePaneReply, "*msg.MsgPaneGroupActivePaneReply", jsonDecoder[MsgPaneGroupActivePaneReply]())
	r.Register(TagPaneSubmitInput, "*msg.MsgPaneSubmitInput", jsonDecoder[MsgPaneSubmitInput]())
	r.Register(TagPaneExecShell, "*msg.MsgPaneExecShell", jsonDecoder[MsgPaneExecShell]())
	r.Register(TagPaneExecPrompt, "*msg.MsgPaneExecPrompt", jsonDecoder[MsgPaneExecPrompt]())
	r.Register(TagPaneExecRysh, "*msg.MsgPaneExecRysh", jsonDecoder[MsgPaneExecRysh]())
	r.Register(TagPaneExecChat, "*msg.MsgPaneExecChat", jsonDecoder[MsgPaneExecChat]())
	r.Register(TagPaneSetTitle, "*msg.MsgPaneSetTitle", jsonDecoder[MsgPaneSetTitle]())
	r.Register(TagPaneSetGivenName, "*msg.MsgPaneSetGivenName", jsonDecoder[MsgPaneSetGivenName]())
	r.Register(TagPaneSetProvider, "*msg.MsgPaneSetProvider", jsonDecoder[MsgPaneSetProvider]())
	r.Register(TagPaneEnableMode, "*msg.MsgPaneEnableMode", jsonDecoder[MsgPaneEnableMode]())
	r.Register(TagPaneActivateMode, "*msg.MsgPaneActivateMode", jsonDecoder[MsgPaneActivateMode]())
	r.Register(TagPaneWebHeadless, "*msg.MsgPaneWebHeadless", jsonDecoder[MsgPaneWebHeadless]())
	r.Register(TagPaneImportCookies, "*msg.MsgPaneImportCookies", jsonDecoder[MsgPaneImportCookies]())
	r.Register(TagPaneDisableMode, "*msg.MsgPaneDisableMode", jsonDecoder[MsgPaneDisableMode]())
	r.Register(TagPaneStop, "*msg.MsgPaneStop", jsonDecoder[MsgPaneStop]())
	r.Register(TagPaneResize, "*msg.MsgPaneResize", jsonDecoder[MsgPaneResize]())
	r.Register(TagPaneResized, "*msg.MsgPaneResized", jsonDecoder[MsgPaneResized]())
	r.Register(TagRawKeyInput, "*msg.MsgRawKeyInput", jsonDecoder[MsgRawKeyInput]())
	r.Register(TagPaneClearOutput, "*msg.MsgPaneClearOutput", jsonDecoder[MsgPaneClearOutput]())
	r.Register(TagPaneNativeMode, "*msg.MsgPaneNativeMode", jsonDecoder[MsgPaneNativeMode]())
	r.Register(TagRelayActivate, "*msg.MsgRelayActivate", jsonDecoder[MsgRelayActivate]())
	r.Register(TagRelayDeactivate, "*msg.MsgRelayDeactivate", jsonDecoder[MsgRelayDeactivate]())
	r.Register(TagPaneRawOutputAppend, "*msg.MsgPaneRawOutputAppend", jsonDecoder[MsgPaneRawOutputAppend]())
	r.Register(TagPaneShareModeChange, "*msg.MsgPaneShareModeChange", jsonDecoder[MsgPaneShareModeChange]())
	r.Register(TagPaneRawDirty, "*msg.MsgPaneRawDirty", jsonDecoder[MsgPaneRawDirty]())
	r.Register(TagPaneReplayShareState, "*msg.MsgPaneReplayShareState", jsonDecoder[MsgPaneReplayShareState]())
	r.Register(TagRemoteInteractiveModeChange, "*msg.MsgRemoteInteractiveModeChange", jsonDecoder[MsgRemoteInteractiveModeChange]())
	r.Register(TagPaneSetRemoteSubscriber, "*msg.MsgPaneSetRemoteSubscriber", jsonDecoder[MsgPaneSetRemoteSubscriber]())
	r.Register(TagRemoteVTScreenUpdate, "*msg.MsgRemoteVTScreenUpdate", jsonDecoder[MsgRemoteVTScreenUpdate]())
	r.Register(TagRemoteScrollbackAppend, "*msg.MsgRemoteScrollbackAppend", jsonDecoder[MsgRemoteScrollbackAppend]())
	r.Register(TagMirrorDirty, "*msg.MsgMirrorDirty", jsonDecoder[MsgMirrorDirty]())
	r.Register(TagLayoutDirty, "*msg.MsgLayoutDirty", jsonDecoder[MsgLayoutDirty]())
	r.Register(TagExecPrompt, "*msg.MsgExecPrompt", jsonDecoder[MsgExecPrompt]())
	r.Register(TagCancelPrompt, "*msg.MsgCancelPrompt", jsonDecoder[MsgCancelPrompt]())
	r.Register(TagGetWorkspaceSnapshot, "*msg.MsgGetWorkspaceSnapshot", jsonDecoder[MsgGetWorkspaceSnapshot]())
	r.Register(TagWorkspaceSnapshotReply, "*msg.MsgWorkspaceSnapshotReply", jsonDecoder[MsgWorkspaceSnapshotReply]())
	r.Register(TagGetTabSnapshot, "*msg.MsgGetTabSnapshot", jsonDecoder[MsgGetTabSnapshot]())
	r.Register(TagTabSnapshotReply, "*msg.MsgTabSnapshotReply", jsonDecoder[MsgTabSnapshotReply]())
	r.Register(TagGetPaneSnapshot, "*msg.MsgGetPaneSnapshot", jsonDecoder[MsgGetPaneSnapshot]())
	r.Register(TagPaneSnapshotReply, "*msg.MsgPaneSnapshotReply", jsonDecoder[MsgPaneSnapshotReply]())
	r.Register(TagGetPaneVT, "*msg.MsgGetPaneVT", jsonDecoder[MsgGetPaneVT]())
	r.Register(TagPaneVTReply, "*msg.MsgPaneVTReply", jsonDecoder[MsgPaneVTReply]())
	r.Register(TagGetPaneScrollback, "*msg.MsgGetPaneScrollback", jsonDecoder[MsgGetPaneScrollback]())
	r.Register(TagPaneScrollbackReply, "*msg.MsgPaneScrollbackReply", jsonDecoder[MsgPaneScrollbackReply]())
	r.Register(TagGetPaneScrollbackDelta, "*msg.MsgGetPaneScrollbackDelta", jsonDecoder[MsgGetPaneScrollbackDelta]())
	r.Register(TagPaneScrollbackDeltaReply, "*msg.MsgPaneScrollbackDeltaReply", jsonDecoder[MsgPaneScrollbackDeltaReply]())
	r.Register(TagGetMirrorScrollback, "*msg.MsgGetMirrorScrollback", jsonDecoder[MsgGetMirrorScrollback]())
	r.Register(TagMirrorScrollbackReply, "*msg.MsgMirrorScrollbackReply", jsonDecoder[MsgMirrorScrollbackReply]())
	r.Register(TagGetMirrorPaneVT, "*msg.MsgGetMirrorPaneVT", jsonDecoder[MsgGetMirrorPaneVT]())
	r.Register(TagMirrorPaneVTReply, "*msg.MsgMirrorPaneVTReply", jsonDecoder[MsgMirrorPaneVTReply]())
	r.Register(TagMirrorPaneVTFrame, "*msg.MsgMirrorPaneVTFrame", jsonDecoder[MsgMirrorPaneVTFrame]())
	r.Register(TagPaneTerminated, "*msg.MsgPaneTerminated", jsonDecoder[MsgPaneTerminated]())
	r.Register(TagTabTerminated, "*msg.MsgTabTerminated", jsonDecoder[MsgTabTerminated]())
	r.Register(TagGetActivePane, "*msg.MsgGetActivePane", jsonDecoder[MsgGetActivePane]())
	r.Register(TagActivePaneReply, "*msg.MsgActivePaneReply", jsonDecoder[MsgActivePaneReply]())
	r.Register(TagStartPaneListener, "*msg.MsgStartPaneListener", jsonDecoder[MsgStartPaneListener]())
	r.Register(TagStopPaneListener, "*msg.MsgStopPaneListener", jsonDecoder[MsgStopPaneListener]())
	r.Register(TagPaneHopContent, "*msg.MsgPaneHopContent", jsonDecoder[MsgPaneHopContent]())
	r.Register(TagPaneHopResume, "*msg.MsgPaneHopResume", jsonDecoder[MsgPaneHopResume]())
	r.Register(TagPaneHopClear, "*msg.MsgPaneHopClear", jsonDecoder[MsgPaneHopClear]())
	r.Register(TagTogglePipelineMode, "*msg.MsgTogglePipelineMode", jsonDecoder[MsgTogglePipelineMode]())
	r.Register(TagTabPipelineEnable, "*msg.MsgTabPipelineEnable", jsonDecoder[MsgTabPipelineEnable]())
	r.Register(TagTabPipelineDisable, "*msg.MsgTabPipelineDisable", jsonDecoder[MsgTabPipelineDisable]())
	r.Register(TagPipelineCommand, "*msg.MsgPipelineCommand", jsonDecoder[MsgPipelineCommand]())
	r.Register(TagEqualizeHorizontal, "*msg.MsgEqualizeHorizontal", jsonDecoder[MsgEqualizeHorizontal]())
	r.Register(TagEqualizePanes, "*msg.MsgEqualizePanes", jsonDecoder[MsgEqualizePanes]())
	r.Register(TagResizePaneWidth, "*msg.MsgResizePaneWidth", jsonDecoder[MsgResizePaneWidth]())
	r.Register(TagEqualizeVertical, "*msg.MsgEqualizeVertical", jsonDecoder[MsgEqualizeVertical]())
	r.Register(TagEqualizeAll, "*msg.MsgEqualizeAll", jsonDecoder[MsgEqualizeAll]())
	r.Register(TagSwapPane, "*msg.MsgSwapPane", jsonDecoder[MsgSwapPane]())
	r.Register(TagTabEqualizeHorizontal, "*msg.MsgTabEqualizeHorizontal", jsonDecoder[MsgTabEqualizeHorizontal]())
	r.Register(TagTabEqualizePanes, "*msg.MsgTabEqualizePanes", jsonDecoder[MsgTabEqualizePanes]())
	r.Register(TagTabResizePaneWidth, "*msg.MsgTabResizePaneWidth", jsonDecoder[MsgTabResizePaneWidth]())
	r.Register(TagTabEqualizeVertical, "*msg.MsgTabEqualizeVertical", jsonDecoder[MsgTabEqualizeVertical]())
	r.Register(TagTabEqualizeAll, "*msg.MsgTabEqualizeAll", jsonDecoder[MsgTabEqualizeAll]())
	r.Register(TagTabSwapPane, "*msg.MsgTabSwapPane", jsonDecoder[MsgTabSwapPane]())
	r.Register(TagLaneEqualizeGroups, "*msg.MsgLaneEqualizeGroups", jsonDecoder[MsgLaneEqualizeGroups]())
	r.Register(TagCLICreateTab, "*msg.MsgCLICreateTab", jsonDecoder[MsgCLICreateTab]())
	r.Register(TagCLIDeleteTab, "*msg.MsgCLIDeleteTab", jsonDecoder[MsgCLIDeleteTab]())
	r.Register(TagCLICreateLane, "*msg.MsgCLICreateLane", jsonDecoder[MsgCLICreateLane]())
	r.Register(TagCLIDeleteLane, "*msg.MsgCLIDeleteLane", jsonDecoder[MsgCLIDeleteLane]())
	r.Register(TagCLICreatePaneGroup, "*msg.MsgCLICreatePaneGroup", jsonDecoder[MsgCLICreatePaneGroup]())
	r.Register(TagCLIDeletePaneGroup, "*msg.MsgCLIDeletePaneGroup", jsonDecoder[MsgCLIDeletePaneGroup]())
	r.Register(TagCLICreatePane, "*msg.MsgCLICreatePane", jsonDecoder[MsgCLICreatePane]())
	r.Register(TagCLIDeletePane, "*msg.MsgCLIDeletePane", jsonDecoder[MsgCLIDeletePane]())
	r.Register(TagCLICreateStackedPane, "*msg.MsgCLICreateStackedPane", jsonDecoder[MsgCLICreateStackedPane]())
	r.Register(TagCLIPipelineEnable, "*msg.MsgCLIPipelineEnable", jsonDecoder[MsgCLIPipelineEnable]())
	r.Register(TagCLIPipelineDisable, "*msg.MsgCLIPipelineDisable", jsonDecoder[MsgCLIPipelineDisable]())
	r.Register(TagCLIResponse, "*msg.MsgCLIResponse", jsonDecoder[MsgCLIResponse]())
	r.Register(TagCLIRyshCommand, "*msg.MsgCLIRyshCommand", jsonDecoder[MsgCLIRyshCommand]())
	r.Register(TagTabDeleteLane, "*msg.MsgTabDeleteLane", jsonDecoder[MsgTabDeleteLane]())
	r.Register(TagTabCreatePaneGroupInLane, "*msg.MsgTabCreatePaneGroupInLane", jsonDecoder[MsgTabCreatePaneGroupInLane]())
	r.Register(TagTabCreateGrid, "*msg.MsgTabCreateGrid", jsonDecoder[MsgTabCreateGrid]())
	r.Register(TagTabCreateGroupsInLane, "*msg.MsgTabCreateGroupsInLane", jsonDecoder[MsgTabCreateGroupsInLane]())
	r.Register(TagTabCreateStackedPaneInLane, "*msg.MsgTabCreateStackedPaneInLane", jsonDecoder[MsgTabCreateStackedPaneInLane]())
	r.Register(TagLaneDeletePaneGroup, "*msg.MsgLaneDeletePaneGroup", jsonDecoder[MsgLaneDeletePaneGroup]())
	r.Register(TagLaneCreateStackedPaneInGroup, "*msg.MsgLaneCreateStackedPaneInGroup", jsonDecoder[MsgLaneCreateStackedPaneInGroup]())
	r.Register(TagPaneGroupDeletePane, "*msg.MsgPaneGroupDeletePane", jsonDecoder[MsgPaneGroupDeletePane]())
	r.Register(TagPaneShareStart, "*msg.MsgPaneShareStart", jsonDecoder[MsgPaneShareStart]())
	r.Register(TagPaneShareStop, "*msg.MsgPaneShareStop", jsonDecoder[MsgPaneShareStop]())
	r.Register(TagPaneShareStatus, "*msg.MsgPaneShareStatus", jsonDecoder[MsgPaneShareStatus]())
	r.Register(TagPaneShareStatusReply, "*msg.MsgPaneShareStatusReply", jsonDecoder[MsgPaneShareStatusReply]())
	r.Register(TagPaneSetSharingState, "*msg.MsgPaneSetSharingState", jsonDecoder[MsgPaneSetSharingState]())
	r.Register(TagRemoteUpstreamConnect, "*msg.MsgRemoteUpstreamConnect", jsonDecoder[MsgRemoteUpstreamConnect]())
	r.Register(TagRemoteUpstreamDisconnect, "*msg.MsgRemoteUpstreamDisconnect", jsonDecoder[MsgRemoteUpstreamDisconnect]())
	r.Register(TagRemoteUpstreamStatus, "*msg.MsgRemoteUpstreamStatus", jsonDecoder[MsgRemoteUpstreamStatus]())
	r.Register(TagShareEntity, "*msg.MsgShareEntity", jsonDecoder[MsgShareEntity]())
	r.Register(TagShareForgedAPI, "*msg.MsgShareForgedAPI", jsonDecoder[MsgShareForgedAPI]())
	r.Register(TagPaneRegisterForgedProxies, "*msg.MsgPaneRegisterForgedProxies", jsonDecoder[MsgPaneRegisterForgedProxies]())
	r.Register(TagUnshareEntity, "*msg.MsgUnshareEntity", jsonDecoder[MsgUnshareEntity]())
	r.Register(TagShareStatus, "*msg.MsgShareStatus", jsonDecoder[MsgShareStatus]())
	r.Register(TagShareStatusReply, "*msg.MsgShareStatusReply", jsonDecoder[MsgShareStatusReply]())
	r.Register(TagShareList, "*msg.MsgShareList", jsonDecoder[MsgShareList]())
	r.Register(TagShareListReply, "*msg.MsgShareListReply", jsonDecoder[MsgShareListReply]())
	r.Register(TagUpstreamCommand, "*msg.MsgUpstreamCommand", jsonDecoder[MsgUpstreamCommand]())
	r.Register(TagUpstreamCommandAck, "*msg.MsgUpstreamCommandAck", jsonDecoder[MsgUpstreamCommandAck]())
	r.Register(TagShareRegisterAck, "*msg.MsgShareRegisterAck", jsonDecoder[MsgShareRegisterAck]())
	r.Register(TagUpstreamSharesList, "*msg.MsgUpstreamSharesList", jsonDecoder[MsgUpstreamSharesList]())
	r.Register(TagUpstreamSubscribe, "*msg.MsgUpstreamSubscribe", jsonDecoder[MsgUpstreamSubscribe]())
	r.Register(TagUpstreamUnsubscribe, "*msg.MsgUpstreamUnsubscribe", jsonDecoder[MsgUpstreamUnsubscribe]())
	r.Register(TagUpstreamSendCommand, "*msg.MsgUpstreamSendCommand", jsonDecoder[MsgUpstreamSendCommand]())
	r.Register(TagShareOutput, "*msg.MsgShareOutput", jsonDecoder[MsgShareOutput]())
	r.Register(TagSetControllerMode, "*msg.MsgSetControllerMode", jsonDecoder[MsgSetControllerMode]())
	r.Register(TagSetConnectedPane, "*msg.MsgSetConnectedPane", jsonDecoder[MsgSetConnectedPane]())
	r.Register(TagRemoteForwardCommand, "*msg.MsgRemoteForwardCommand", jsonDecoder[MsgRemoteForwardCommand]())
	r.Register(TagExecRyshOnPane, "*msg.MsgExecRyshOnPane", jsonDecoder[MsgExecRyshOnPane]())
	r.Register(TagMirrorTabOp, "*msg.MsgMirrorTabOp", jsonDecoder[MsgMirrorTabOp]())
	r.Register(TagMirrorMaximizePane, "*msg.MsgMirrorMaximizePane", jsonDecoder[MsgMirrorMaximizePane]())
	r.Register(TagRemotePaneFullscreen, "*msg.MsgRemotePaneFullscreen", jsonDecoder[MsgRemotePaneFullscreen]())
	// Share restrictions.
	r.Register(TagShareDisableMode, "*msg.MsgShareDisableMode", jsonDecoder[MsgShareDisableMode]())
	r.Register(TagShareEnableMode, "*msg.MsgShareEnableMode", jsonDecoder[MsgShareEnableMode]())
	r.Register(TagShareShellAllow, "*msg.MsgShareShellAllow", jsonDecoder[MsgShareShellAllow]())
	r.Register(TagShareShellForbid, "*msg.MsgShareShellForbid", jsonDecoder[MsgShareShellForbid]())
	r.Register(TagShareShellClear, "*msg.MsgShareShellClear", jsonDecoder[MsgShareShellClear]())
	r.Register(TagShareSetFileBrowse, "*msg.MsgShareSetFileBrowse", jsonDecoder[MsgShareSetFileBrowse]())
	r.Register(TagShareShowRestrictions, "*msg.MsgShareShowRestrictions", jsonDecoder[MsgShareShowRestrictions]())
	r.Register(TagShareRestrictionsUpdated, "*msg.MsgShareRestrictionsUpdated", jsonDecoder[MsgShareRestrictionsUpdated]())
	r.Register(TagPaneSetShareRestrictions, "*msg.MsgPaneSetShareRestrictions", jsonDecoder[MsgPaneSetShareRestrictions]())

	r.Register(TagUpstreamReconnected, "*msg.MsgUpstreamReconnected", jsonDecoder[MsgUpstreamReconnected]())
	r.Register(TagUpstreamConnectionClosed, "*msg.MsgUpstreamConnectionClosed", jsonDecoder[MsgUpstreamConnectionClosed]())
	r.Register(TagCreateStackedPane, "*msg.MsgCreateStackedPane", jsonDecoder[MsgCreateStackedPane]())
	r.Register(TagStackedPaneRotate, "*msg.MsgStackedPaneRotate", jsonDecoder[MsgStackedPaneRotate]())
	r.Register(TagStackedPaneSelect, "*msg.MsgStackedPaneSelect", jsonDecoder[MsgStackedPaneSelect]())
	r.Register(TagStackedPaneMove, "*msg.MsgStackedPaneMove", jsonDecoder[MsgStackedPaneMove]())
	r.Register(TagPaneGroupCreateStackedPane, "*msg.MsgPaneGroupCreateStackedPane", jsonDecoder[MsgPaneGroupCreateStackedPane]())
	r.Register(TagPaneGroupStackedPane, "*msg.MsgPaneGroupStackedPane", jsonDecoder[MsgPaneGroupStackedPane]())
	r.Register(TagPaneGroupStackedPaneSelect, "*msg.MsgPaneGroupStackedPaneSelect", jsonDecoder[MsgPaneGroupStackedPaneSelect]())
	r.Register(TagPaneGroupFocusPaneByID, "*msg.MsgPaneGroupFocusPaneByID", jsonDecoder[MsgPaneGroupFocusPaneByID]())
	r.Register(TagPaneGroupStackedPaneMove, "*msg.MsgPaneGroupStackedPaneMove", jsonDecoder[MsgPaneGroupStackedPaneMove]())

	// Approval flow among panes
	r.Register(TagCreateApprovalPane, "*msg.MsgCreateApprovalPane", jsonDecoder[MsgCreateApprovalPane]())
	r.Register(TagDestroyApprovalPane, "*msg.MsgDestroyApprovalPane", jsonDecoder[MsgDestroyApprovalPane]())
	r.Register(TagPaneSetApprovalPaneGroups, "*msg.MsgPaneSetApprovalPaneGroups", jsonDecoder[MsgPaneSetApprovalPaneGroups]())

	// Autonomous agent messages.
	r.Register(TagAgentCreate, "*msg.MsgAgentCreate", jsonDecoder[MsgAgentCreate]())
	r.Register(TagAgentDelete, "*msg.MsgAgentDelete", jsonDecoder[MsgAgentDelete]())
	r.Register(TagAgentStop, "*msg.MsgAgentStop", jsonDecoder[MsgAgentStop]())
	r.Register(TagAgentContinue, "*msg.MsgAgentContinue", jsonDecoder[MsgAgentContinue]())
	r.Register(TagAgentActivate, "*msg.MsgAgentActivate", jsonDecoder[MsgAgentActivate]())
	r.Register(TagAgentDeactivate, "*msg.MsgAgentDeactivate", jsonDecoder[MsgAgentDeactivate]())
	r.Register(TagAgentList, "*msg.MsgAgentList", jsonDecoder[MsgAgentList]())
	r.Register(TagAgentListReply, "*msg.MsgAgentListReply", jsonDecoder[MsgAgentListReply]())
	r.Register(TagAgentPrompt, "*msg.MsgAgentPrompt", jsonDecoder[MsgAgentPrompt]())
	r.Register(TagAgentRegisterPane, "*msg.MsgAgentRegisterPane", jsonDecoder[MsgAgentRegisterPane]())
	r.Register(TagAgentUnregisterPane, "*msg.MsgAgentUnregisterPane", jsonDecoder[MsgAgentUnregisterPane]())

	// Humanoid messages.
	r.Register(TagHumanoidCreate, "*msg.MsgHumanoidCreate", jsonDecoder[MsgHumanoidCreate]())
	r.Register(TagHumanoidDelete, "*msg.MsgHumanoidDelete", jsonDecoder[MsgHumanoidDelete]())
	r.Register(TagHumanoidStop, "*msg.MsgHumanoidStop", jsonDecoder[MsgHumanoidStop]())
	r.Register(TagHumanoidContinue, "*msg.MsgHumanoidContinue", jsonDecoder[MsgHumanoidContinue]())
	r.Register(TagHumanoidActivate, "*msg.MsgHumanoidActivate", jsonDecoder[MsgHumanoidActivate]())
	r.Register(TagHumanoidDeactivate, "*msg.MsgHumanoidDeactivate", jsonDecoder[MsgHumanoidDeactivate]())
	r.Register(TagHumanoidList, "*msg.MsgHumanoidList", jsonDecoder[MsgHumanoidList]())
	r.Register(TagHumanoidListReply, "*msg.MsgHumanoidListReply", jsonDecoder[MsgHumanoidListReply]())
	r.Register(TagHumanoidPrompt, "*msg.MsgHumanoidPrompt", jsonDecoder[MsgHumanoidPrompt]())
	r.Register(TagHumanoidRegisterPane, "*msg.MsgHumanoidRegisterPane", jsonDecoder[MsgHumanoidRegisterPane]())
	r.Register(TagHumanoidUnregisterPane, "*msg.MsgHumanoidUnregisterPane", jsonDecoder[MsgHumanoidUnregisterPane]())
	r.Register(TagHumanoidChannelStart, "*msg.MsgHumanoidChannelStart", jsonDecoder[MsgHumanoidChannelStart]())
	r.Register(TagHumanoidChannelStop, "*msg.MsgHumanoidChannelStop", jsonDecoder[MsgHumanoidChannelStop]())
	r.Register(TagHumanoidChannelStatus, "*msg.MsgHumanoidChannelStatus", jsonDecoder[MsgHumanoidChannelStatus]())
	r.Register(TagHumanoidSetReplyMode, "*msg.MsgHumanoidSetReplyMode", jsonDecoder[MsgHumanoidSetReplyMode]())
	r.Register(TagHumanoidSetGovernance, "*msg.MsgHumanoidSetGovernance", jsonDecoder[MsgHumanoidSetGovernance]())
	r.Register(TagHumanoidSetProvider, "*msg.MsgHumanoidSetProvider", jsonDecoder[MsgHumanoidSetProvider]())
	r.Register(TagHumanoidInboundMessage, "*msg.MsgHumanoidInboundMessage", jsonDecoder[MsgHumanoidInboundMessage]())
	r.Register(TagHumanoidOutboundMessage, "*msg.MsgHumanoidOutboundMessage", jsonDecoder[MsgHumanoidOutboundMessage]())
	r.Register(TagHumanoidEmailList, "*msg.MsgHumanoidEmailList", jsonDecoder[MsgHumanoidEmailList]())
	r.Register(TagHumanoidEmailRead, "*msg.MsgHumanoidEmailRead", jsonDecoder[MsgHumanoidEmailRead]())
	r.Register(TagHumanoidEmailListReply, "*msg.MsgHumanoidEmailListReply", jsonDecoder[MsgHumanoidEmailListReply]())
	r.Register(TagHumanoidEmailReadReply, "*msg.MsgHumanoidEmailReadReply", jsonDecoder[MsgHumanoidEmailReadReply]())
	r.Register(TagHumanoidEmailChanged, "*msg.MsgHumanoidEmailChanged", jsonDecoder[MsgHumanoidEmailChanged]())
	r.Register(TagHumanoidSetFocus, "*msg.MsgHumanoidSetFocus", jsonDecoder[MsgHumanoidSetFocus]())
	r.Register(TagHumanoidEmailCompose, "*msg.MsgHumanoidEmailCompose", jsonDecoder[MsgHumanoidEmailCompose]())
	r.Register(TagHumanoidEmailComposeReply, "*msg.MsgHumanoidEmailComposeReply", jsonDecoder[MsgHumanoidEmailComposeReply]())
	r.Register(TagHumanoidWhatsAppList, "*msg.MsgHumanoidWhatsAppList", jsonDecoder[MsgHumanoidWhatsAppList]())
	r.Register(TagHumanoidWhatsAppRead, "*msg.MsgHumanoidWhatsAppRead", jsonDecoder[MsgHumanoidWhatsAppRead]())
	r.Register(TagHumanoidWhatsAppListReply, "*msg.MsgHumanoidWhatsAppListReply", jsonDecoder[MsgHumanoidWhatsAppListReply]())
	r.Register(TagHumanoidWhatsAppReadReply, "*msg.MsgHumanoidWhatsAppReadReply", jsonDecoder[MsgHumanoidWhatsAppReadReply]())
	r.Register(TagHumanoidWhatsAppChanged, "*msg.MsgHumanoidWhatsAppChanged", jsonDecoder[MsgHumanoidWhatsAppChanged]())
	r.Register(TagPaneSetHumanoid, "*msg.MsgPaneSetHumanoid", jsonDecoder[MsgPaneSetHumanoid]())

	// Contact pairing & allowlists (WS3, design 003).
	r.Register(TagChannelPairRequest, "*msg.MsgChannelPairRequest", jsonDecoder[MsgChannelPairRequest]())
	r.Register(TagChannelPairApprove, "*msg.MsgChannelPairApprove", jsonDecoder[MsgChannelPairApprove]())
	r.Register(TagChannelAllow, "*msg.MsgChannelAllow", jsonDecoder[MsgChannelAllow]())
	r.Register(TagChannelPairList, "*msg.MsgChannelPairList", jsonDecoder[MsgChannelPairList]())
	r.Register(TagChannelPairListReply, "*msg.MsgChannelPairListReply", jsonDecoder[MsgChannelPairListReply]())
	r.Register(TagChannelPairQR, "*msg.MsgChannelPairQR", jsonDecoder[MsgChannelPairQR]())
	r.Register(TagChannelPairStatus, "*msg.MsgChannelPairStatus", jsonDecoder[MsgChannelPairStatus]())
	r.Register(TagChannelPairLink, "*msg.MsgChannelPairLink", jsonDecoder[MsgChannelPairLink]())

	// Attention mechanism
	r.Register(TagAttentionEvent, "*msg.MsgAttentionEvent", jsonDecoder[MsgAttentionEvent]())
	r.Register(TagAttentionAck, "*msg.MsgAttentionAck", jsonDecoder[MsgAttentionAck]())
	r.Register(TagAttentionEnable, "*msg.MsgAttentionEnable", jsonDecoder[MsgAttentionEnable]())
	r.Register(TagAttentionDisable, "*msg.MsgAttentionDisable", jsonDecoder[MsgAttentionDisable]())

	return r
}
