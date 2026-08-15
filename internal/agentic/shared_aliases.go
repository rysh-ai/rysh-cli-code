// SPDX-License-Identifier: Apache-2.0

// Package agentic shared_aliases.go — re-exports rysh-shared/agentic types.
package agentic

import (
	"github.com/nats-io/nats.go"

	sharedagentic "github.com/rysh-ai/rysh-cli-shared/agentic"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
	"github.com/rysh-ai/rysh-cli-code/internal/tools"
)

// LLMPromptExecutionActor manages the agentic conversation and spawns OrchestratorActor instances.
// This is a type alias — it IS rysh-shared/agentic.LLMPromptExecutionActor.
type LLMPromptExecutionActor = sharedagentic.LLMPromptExecutionActor

// ApprovalManager handles publishing and subscribing to approval request/response subjects.
type ApprovalManager = sharedagentic.ApprovalManager

// NewApprovalManager creates a new ApprovalManager for a pane.
var NewApprovalManager = sharedagentic.NewApprovalManager

// ApprovalRequestSubject returns the NATS subject for approval requests for a pane.
var ApprovalRequestSubject = sharedagentic.ApprovalRequestSubject

// ApprovalResponseSubject returns the NATS subject for approval responses for a pane.
var ApprovalResponseSubject = sharedagentic.ApprovalResponseSubject

// NewLLMPromptExecutionActor creates a new LLMPromptExecutionActor using the CLI config struct.
// This wrapper adapts the CLI config.Config to the shared constructor's signature.
// The maxIterations defaults to 50 if not set in config (config.AgenticConfig is loaded separately).
func NewLLMPromptExecutionActor(
	paneID string,
	cfg config.Config,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
	prov provider.AgenticProvider,
	toolRegistry *tools.ToolRegistry,
	systemPrompt string,
	pipelineOutputSubject string,
) *LLMPromptExecutionActor {
	const defaultMaxIterations = 50
	return sharedagentic.NewLLMPromptExecutionActor(
		paneID,
		defaultMaxIterations,
		pub,
		nc,
		prov,
		toolRegistry,
		systemPrompt,
		pipelineOutputSubject,
		"",  // no chat output routing for normal panes
		nil, // no KV store for this wrapper
	)
}
