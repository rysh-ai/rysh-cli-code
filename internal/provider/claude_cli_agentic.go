package provider

// claude_cli_agentic.go — makes ClaudeCLI reachable from the agentic selection
// seam (design 002 §3.2: "ClaudeCLI (Complete-only; Caps().Tools=false)").
//
// Before this adapter existed, no provider name selected the CLI in agentic
// mode: NewAgenticProvider routed every Claude-family name to the HTTP API,
// and the keyless fallback in agentic.Setup was the mock — so a user logged in
// via the `claude` binary but holding no API key could never serve a pane.
// RYSH_PROVIDER=claude-cli (or `##pane provider claude-cli`) now selects it
// explicitly, and it needs no API key (RequiresAPIKey("claude-cli") == false):
// the CLI carries its own credentials.

import (
	"context"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// ClaudeCLIAgentic adapts the Complete-only ClaudeCLI to the AgenticProvider
// surface the pane executor requires.
type ClaudeCLIAgentic struct {
	cli *ClaudeCLI
}

// NewClaudeCLIAgenticProvider builds the CLI adapter from config. Model ""
// leaves the CLI's own default in charge.
func NewClaudeCLIAgenticProvider(cfg config.Config) *ClaudeCLIAgentic {
	return &ClaudeCLIAgentic{cli: &ClaudeCLI{
		command:      cfg.ClaudeCommand,
		systemPrompt: cfg.SystemPrompt,
		model:        cfg.DefaultModel,
	}}
}

// Name distinguishes the CLI from the HTTP "claude" provider in pane snapshots
// and usage attribution.
func (c *ClaudeCLIAgentic) Name() string { return "claude-cli" }

func (c *ClaudeCLIAgentic) Complete(ctx context.Context, prompt string) (string, error) {
	return c.cli.Complete(ctx, prompt)
}

// Caps declares the honest capability row: no tool calls, no streaming. The
// structural default in Caps() would report Tools=true because
// CompleteWithTools exists below; the Reporter declaration wins (resolution
// order in capabilities.go), so tool-dependent consumers — e.g. design 006
// §4.4 human-governed humanoids — degrade loudly instead of waiting for tool
// calls that can never come.
func (c *ClaudeCLIAgentic) Caps() Capabilities { return Capabilities{} }

// CompleteWithTools satisfies the executor's AgenticProvider seam. Tool specs
// are dropped — Caps().Tools == false announces that — and the conversation is
// flattened to a single prompt; the CLI's text answer comes back as an
// end-turn response, never a fabricated tool call. The orchestrator's system
// prompt is forwarded so the assembled prompt (project memory, env block) is
// not silently replaced by the static file Complete() loads; only when the
// orchestrator sends none does the configured file apply.
func (c *ClaudeCLIAgentic) CompleteWithTools(
	ctx context.Context,
	conversation []ConversationTurn,
	_ []ToolSpec,
	systemPrompt string,
) (*AgenticResponse, error) {
	sys := systemPrompt
	if sys == "" {
		if fromFile, err := c.cli.loadSystemPrompt(); err == nil {
			sys = fromFile
		}
	}
	text, err := c.cli.completeWithSystem(ctx, sys, flattenConversation(conversation))
	if err != nil {
		return nil, err
	}
	return &AgenticResponse{
		TextBlocks: []TextBlock{{Text: text}},
		StopReason: StopReasonEndTurn,
	}, nil
}

// flattenConversation renders a multi-turn conversation as one prompt for a
// Complete-only backend. A single user turn passes through verbatim; longer
// histories get role prefixes so the CLI still sees who said what. Tool-result
// turns keep their content (bracketed) — with Tools=false no NEW tool calls
// are ever requested, but restored conversations may contain old ones.
func flattenConversation(conversation []ConversationTurn) string {
	if len(conversation) == 1 && conversation[0].Role == "user" {
		return conversation[0].Content
	}
	var b strings.Builder
	for _, t := range conversation {
		if t.Content == "" {
			continue
		}
		switch t.Role {
		case "assistant":
			b.WriteString("Assistant: ")
		case "tool":
			b.WriteString("[tool result] ")
		default:
			b.WriteString("User: ")
		}
		b.WriteString(t.Content)
		b.WriteString("\n\n")
	}
	return strings.TrimSuffix(b.String(), "\n\n")
}
