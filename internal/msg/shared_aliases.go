// Package msg shared_aliases.go — re-exports rysh-shared/msg types as type aliases.
// Type aliases (=) are NOT new types — they are the same underlying Go type.
// This is required so that messages sent from rysh-cli actors to rysh-shared/agentic
// actors match the type switches in the shared actors' Receive() methods.
package msg

import sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"

// ---------------------------------------------------------------------------
// Infrastructure aliases
// ---------------------------------------------------------------------------

type NATSEnvelope = sharedmsg.NATSEnvelope
type RequestEnvelope = sharedmsg.RequestEnvelope
type CodecRegistry = sharedmsg.CodecRegistry
type NATSPublisher = sharedmsg.NATSPublisher

var NewCodecRegistry = sharedmsg.NewCodecRegistry
var NewNATSPublisher = sharedmsg.NewNATSPublisher

// ---------------------------------------------------------------------------
// Output message aliases (these were defined locally in messages.go — remove those)
// ---------------------------------------------------------------------------

type MsgPaneOutputAppend = sharedmsg.MsgPaneOutputAppend
type MsgPaneShellOutputAppend = sharedmsg.MsgPaneShellOutputAppend
type MsgPaneAIOutputAppend = sharedmsg.MsgPaneAIOutputAppend
type MsgPaneChatOutputAppend = sharedmsg.MsgPaneChatOutputAppend
type MsgPaneRyshOutputAppend = sharedmsg.MsgPaneRyshOutputAppend
type MsgPaneExternalOutputAppend = sharedmsg.MsgPaneExternalOutputAppend
type MsgPaneStatusUpdate = sharedmsg.MsgPaneStatusUpdate
type MsgPipelineOutputAppend = sharedmsg.MsgPipelineOutputAppend

// ---------------------------------------------------------------------------
// History message aliases
// ---------------------------------------------------------------------------

type MsgPaneHistoryAppend = sharedmsg.MsgPaneHistoryAppend
type MsgPaneShellHistoryAppend = sharedmsg.MsgPaneShellHistoryAppend
type MsgPaneAIHistoryAppend = sharedmsg.MsgPaneAIHistoryAppend
type MsgPaneChatHistoryAppend = sharedmsg.MsgPaneChatHistoryAppend
type MsgPaneRyshHistoryAppend = sharedmsg.MsgPaneRyshHistoryAppend
type MsgPaneExternalHistoryAppend = sharedmsg.MsgPaneExternalHistoryAppend

// ---------------------------------------------------------------------------
// Conversation message aliases (new unified types)
// ---------------------------------------------------------------------------

type ConversationMessage = sharedmsg.ConversationMessage
type MessageOrigin = sharedmsg.MessageOrigin
type MsgConversationAppend = sharedmsg.MsgConversationAppend
type MsgConversationHistoryAppend = sharedmsg.MsgConversationHistoryAppend
type ConversationType = sharedmsg.ConversationType
type TurnType = sharedmsg.TurnType
type InputType = sharedmsg.InputType
type MessageSource = sharedmsg.MessageSource

// Conversation type constants
const (
	ConvShell   = sharedmsg.ConvShell
	ConvAI      = sharedmsg.ConvAI
	ConvRysh    = sharedmsg.ConvRysh
	ConvChat    = sharedmsg.ConvChat
	ConvEmail   = sharedmsg.ConvEmail
	ConvSlack   = sharedmsg.ConvSlack
	ConvChatbot = sharedmsg.ConvChatbot
)

// Turn type constants
const (
	TurnQuestion = sharedmsg.TurnQuestion
	TurnAnswer   = sharedmsg.TurnAnswer
)

// Input type constants
const (
	InputShell    = sharedmsg.InputShell
	InputPrompt   = sharedmsg.InputPrompt
	InputCommand  = sharedmsg.InputCommand
	InputApproval = sharedmsg.InputApproval
	InputMessage  = sharedmsg.InputMessage
)

// Message source constants
const (
	SourceHuman    = sharedmsg.SourceHuman
	SourceAI       = sharedmsg.SourceAI
	SourceExternal = sharedmsg.SourceExternal
	SourceAgent    = sharedmsg.SourceAgent
	SourceSubagent = sharedmsg.SourceSubagent
	SourceHumanoid = sharedmsg.SourceHumanoid
	SourceSystem   = sharedmsg.SourceSystem
)

// Constructor aliases
var (
	NewTurnID         = sharedmsg.NewTurnID
	NowMs             = sharedmsg.NowMs
	NewShellQuestion  = sharedmsg.NewShellQuestion
	NewShellAnswer    = sharedmsg.NewShellAnswer
	NewAIQuestion     = sharedmsg.NewAIQuestion
	NewAIAnswer       = sharedmsg.NewAIAnswer
	NewRyshQuestion   = sharedmsg.NewRyshQuestion
	NewRyshAnswer     = sharedmsg.NewRyshAnswer
	NewChatMessage    = sharedmsg.NewChatMessage
	NewChannelMessage = sharedmsg.NewChannelMessage
)

// Memory constructor and helper aliases
var (
	NewMemoryEntry            = sharedmsg.NewMemoryEntry
	FormatMemoryForPrompt     = sharedmsg.FormatMemoryForPrompt
	MemorySummarizationPrompt = sharedmsg.MemorySummarizationPrompt
)

// ---------------------------------------------------------------------------
// Agentic message aliases (these were defined locally in agentic_messages.go — that file will be deleted)
// ---------------------------------------------------------------------------

type MsgAgenticPrompt = sharedmsg.MsgAgenticPrompt
type MsgAgenticCancel = sharedmsg.MsgAgenticCancel
type MsgAgenticContinue = sharedmsg.MsgAgenticContinue
type MsgSetRunBudget = sharedmsg.MsgSetRunBudget
type MsgGetRunStatus = sharedmsg.MsgGetRunStatus
type MsgRunStatusReply = sharedmsg.MsgRunStatusReply
type MsgAgenticStep = sharedmsg.MsgAgenticStep
type MsgGetSessionMemory = sharedmsg.MsgGetSessionMemory
type MsgSessionMemoryReply = sharedmsg.MsgSessionMemoryReply
type MsgSessionMemoryReplace = sharedmsg.MsgSessionMemoryReplace
type MsgSetGroundingMode = sharedmsg.MsgSetGroundingMode
type MsgGetGroundingState = sharedmsg.MsgGetGroundingState
type MsgGroundingStateReply = sharedmsg.MsgGroundingStateReply
type GroundingReportInfo = sharedmsg.GroundingReportInfo
type MsgOrchestratorDone = sharedmsg.MsgOrchestratorDone
type MsgToolCall = sharedmsg.MsgToolCall
type MsgToolResult = sharedmsg.MsgToolResult
type MsgAgenticOutput = sharedmsg.MsgAgenticOutput
type MsgAgenticStatus = sharedmsg.MsgAgenticStatus
type ApprovalType = sharedmsg.ApprovalType
type ApprovalDecision = sharedmsg.ApprovalDecision
type Choice = sharedmsg.Choice
type DiffPayload = sharedmsg.DiffPayload
type MsgApprovalRequest = sharedmsg.MsgApprovalRequest
type MsgApprovalResponse = sharedmsg.MsgApprovalResponse
type MsgSpawnSubOrchestrator = sharedmsg.MsgSpawnSubOrchestrator

// Browser-action messages (server-side AI ↔ web-pane executor).
type MsgBrowserActionRequest = sharedmsg.MsgBrowserActionRequest
type MsgBrowserActionResponse = sharedmsg.MsgBrowserActionResponse
type MsgSubOrchestratorResult = sharedmsg.MsgSubOrchestratorResult
type MsgGetConversationHistory = sharedmsg.MsgGetConversationHistory
type ConversationTurnInfo = sharedmsg.ConversationTurnInfo
type ToolCallInfo = sharedmsg.ToolCallInfo
type MsgConversationHistoryReply = sharedmsg.MsgConversationHistoryReply
type MsgRestoreConversation = sharedmsg.MsgRestoreConversation
type MsgSetChatOutputPane = sharedmsg.MsgSetChatOutputPane
type MsgCreateApprovalPane = sharedmsg.MsgCreateApprovalPane
type MsgDestroyApprovalPane = sharedmsg.MsgDestroyApprovalPane
type MsgPaneSetApprovalPaneGroups = sharedmsg.MsgPaneSetApprovalPaneGroups

// ---------------------------------------------------------------------------
// Memory message aliases
// ---------------------------------------------------------------------------

type MemoryEntry = sharedmsg.MemoryEntry
type MemoryState = sharedmsg.MemoryState
type MsgMemoryAppend = sharedmsg.MsgMemoryAppend
type MsgMemoryGet = sharedmsg.MsgMemoryGet
type MsgMemoryGetReply = sharedmsg.MsgMemoryGetReply
type MsgMemorySummarize = sharedmsg.MsgMemorySummarize
type MsgMemorySummarizeDone = sharedmsg.MsgMemorySummarizeDone
type MsgMemoryStateUpdate = sharedmsg.MsgMemoryStateUpdate

// Follow-up 2b: prompt hot-reload broadcast.
type MsgReloadPrompts = sharedmsg.MsgReloadPrompts

// ---------------------------------------------------------------------------
// Tag constant aliases (re-declared from sharedmsg since const can't be aliased)
// ---------------------------------------------------------------------------

const (
	// Output tags
	TagPaneOutputAppend         = sharedmsg.TagPaneOutputAppend
	TagPaneShellOutputAppend    = sharedmsg.TagPaneShellOutputAppend
	TagPaneAIOutputAppend       = sharedmsg.TagPaneAIOutputAppend
	TagPaneChatOutputAppend     = sharedmsg.TagPaneChatOutputAppend
	TagPaneRyshOutputAppend     = sharedmsg.TagPaneRyshOutputAppend
	TagPaneExternalOutputAppend = sharedmsg.TagPaneExternalOutputAppend
	TagPaneStatusUpdate         = sharedmsg.TagPaneStatusUpdate
	TagPipelineOutputAppend     = sharedmsg.TagPipelineOutputAppend

	// History tags
	TagPaneHistoryAppend         = sharedmsg.TagPaneHistoryAppend
	TagPaneShellHistoryAppend    = sharedmsg.TagPaneShellHistoryAppend
	TagPaneAIHistoryAppend       = sharedmsg.TagPaneAIHistoryAppend
	TagPaneChatHistoryAppend     = sharedmsg.TagPaneChatHistoryAppend
	TagPaneRyshHistoryAppend     = sharedmsg.TagPaneRyshHistoryAppend
	TagPaneExternalHistoryAppend = sharedmsg.TagPaneExternalHistoryAppend

	// Unified conversation tags (new)
	TagConversationAppend        = sharedmsg.TagConversationAppend
	TagConversationHistoryAppend = sharedmsg.TagConversationHistoryAppend

	// Agentic tags
	TagAgenticPrompt             = sharedmsg.TagAgenticPrompt
	TagAgenticCancel             = sharedmsg.TagAgenticCancel
	TagOrchestratorDone          = sharedmsg.TagOrchestratorDone
	TagToolCall                  = sharedmsg.TagToolCall
	TagToolResult                = sharedmsg.TagToolResult
	TagAgenticOutput             = sharedmsg.TagAgenticOutput
	TagAgenticStatus             = sharedmsg.TagAgenticStatus
	TagApprovalRequest           = sharedmsg.TagApprovalRequest
	TagApprovalResponse          = sharedmsg.TagApprovalResponse
	TagSpawnSubOrchestrator      = sharedmsg.TagSpawnSubOrchestrator
	TagSubOrchestratorResult     = sharedmsg.TagSubOrchestratorResult
	TagGetConversationHistory    = sharedmsg.TagGetConversationHistory
	TagConversationHistoryReply  = sharedmsg.TagConversationHistoryReply
	TagRestoreConversation       = sharedmsg.TagRestoreConversation
	TagSetChatOutputPane         = sharedmsg.TagSetChatOutputPane
	TagCreateApprovalPane        = sharedmsg.TagCreateApprovalPane
	TagDestroyApprovalPane       = sharedmsg.TagDestroyApprovalPane
	TagPaneSetApprovalPaneGroups = sharedmsg.TagPaneSetApprovalPaneGroups
	TagMemoryStateUpdate         = sharedmsg.TagMemoryStateUpdate

	// Memory tags
	TagMemoryAppend        = sharedmsg.TagMemoryAppend
	TagMemoryGet           = sharedmsg.TagMemoryGet
	TagMemoryGetReply      = sharedmsg.TagMemoryGetReply
	TagMemorySummarize     = sharedmsg.TagMemorySummarize
	TagMemorySummarizeDone = sharedmsg.TagMemorySummarizeDone

	// Approval decision constants
	ApprovalTypeDiff        = sharedmsg.ApprovalTypeDiff
	ApprovalTypeDestructive = sharedmsg.ApprovalTypeDestructive
	ApprovalTypeChoice      = sharedmsg.ApprovalTypeChoice
	ApprovalTypeQuestion    = sharedmsg.ApprovalTypeQuestion

	DecisionYes               = sharedmsg.DecisionYes
	DecisionYesAlways         = sharedmsg.DecisionYesAlways
	DecisionNo                = sharedmsg.DecisionNo
	DecisionNoWithExplanation = sharedmsg.DecisionNoWithExplanation
	DecisionChoiceSelected    = sharedmsg.DecisionChoiceSelected
)
