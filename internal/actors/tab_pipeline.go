// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"log/slog"
	"strings"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ---------------------------------------------------------------------------
// Input routing
// ---------------------------------------------------------------------------

func (t *TabActor) submitInput(ctx actor.Context, m *msg.MsgTabSubmitInput) {
	// Only pipeline mode is still routed through the tab.
	if m.Mode == "pipeline" {
		t.handlePipelineInput(ctx, m.Text)
		return
	}
	// All other input modes are now sent directly from workspace to pane.
}

// ---------------------------------------------------------------------------
// Pipeline helpers
// ---------------------------------------------------------------------------

func (t *TabActor) handlePipelineInput(ctx actor.Context, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	// Check if this is a pipeline command.
	parts := strings.Fields(text)
	cmd := parts[0]
	args := ""
	if len(parts) > 1 {
		args = strings.Join(parts[1:], " ")
	}

	var out strings.Builder
	handled := true
	switch cmd {
	case "help":
		cmdPipelineHelp(&out)
	case "list":
		cmdPipelineList(&out, t, args)
	case "load":
		cmdPipelineLoad(&out, t, args)
	case "unload":
		cmdPipelineUnload(&out, t, args)
	case "show":
		cmdPipelineShow(&out, t, args)
	case "build":
		cmdPipelineBuild(&out, t, args)
	case "run":
		t.cmdPipelineRunWithContext(ctx, "", args)
		return
	case "status":
		cmdPipelineStatus(&out, t)
	case "clear":
		cmdPipelineClear(&out, t)
	default:
		handled = false
	}

	if handled {
		// Write command output to pipeline buffer.
		if out.Len() > 0 {
			pipelineOutputSubject := msg.T("tab", t.id, "pipelineOutput")
			_ = t.pub.Send(pipelineOutputSubject, &msg.MsgPipelineOutputAppend{Text: out.String()})
		}
		return
	}

	// Not a command -- send as AI prompt to pipeline LLMPromptExecutionActor.
	if t.pipelineLLMPromptExecPID == nil {
		t.spawnPipelineActor(ctx)
	}
	if t.pipelineLLMPromptExecPID == nil {
		return // spawn failed
	}
	_ = t.pub.Send(
		msg.T("pane", t.pipelinePaneID, "llm_prompt_execution", "inbox"),
		&msg.MsgAgenticPrompt{Prompt: text},
	)
}

func (t *TabActor) spawnPipelineActor(ctx actor.Context) {
	if t.agSetup == nil {
		return
	}
	t.pipelinePaneID = "pipeline-" + t.id
	pipelineOutputSubject := msg.T("tab", t.id, "pipelineOutput")

	aa := agentic.NewLLMPromptExecutionActor(
		t.pipelinePaneID,
		t.cfg,
		t.pub,
		t.nc,
		t.agSetup.Provider,
		t.agSetup.ToolRegistry,
		t.agSetup.SystemPrompt,
		pipelineOutputSubject,
	)

	props := actor.PropsFromProducer(func() actor.Actor { return aa })
	pid, err := ctx.SpawnNamed(props, "pipeline-agentic")
	if err != nil {
		slog.Error("tab: spawn pipeline agentic", "err", err)
		return
	}
	t.pipelineLLMPromptExecPID = pid
}

// cmdPipelineRunWithContext runs a pipeline, spawning the pipeline actor if
// needed (requires actor.Context). Output is routed to paneID if non-empty
// (##pipe path) or to the pipeline output buffer (pipeline mode path).
func (t *TabActor) cmdPipelineRunWithContext(ctx actor.Context, paneID, name string) {
	// Spawn pipeline actor if needed.
	if t.pipelineLLMPromptExecPID == nil {
		t.spawnPipelineActor(ctx)
	}
	if t.pipelineLLMPromptExecPID == nil {
		text := "\n[pipeline] failed to start pipeline actor\n"
		if paneID != "" {
			_ = t.pub.SendPaneOutput(paneID, text)
		} else {
			pipelineOutputSubject := msg.T("tab", t.id, "pipelineOutput")
			_ = t.pub.Send(pipelineOutputSubject, &msg.MsgPipelineOutputAppend{Text: text})
		}
		return
	}

	var out strings.Builder
	cmdPipelineRun(&out, t, name)
	if out.Len() > 0 {
		if paneID != "" {
			_ = t.pub.SendPaneOutput(paneID, out.String())
		} else {
			pipelineOutputSubject := msg.T("tab", t.id, "pipelineOutput")
			_ = t.pub.Send(pipelineOutputSubject, &msg.MsgPipelineOutputAppend{Text: out.String()})
		}
	}
}
