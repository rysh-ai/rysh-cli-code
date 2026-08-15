// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"strings"

	sharedagentic "github.com/rysh-ai/rysh-cli-shared/agentic"
)

// Prompts holds the prompt content used across the agentic system. Each field
// corresponds to a markdown file in rysh-cli/rysh-cli-agent-prompts/.
//
// Empty fields fall back to the built-in constants below. Production main.go
// populates this from the embedded //go:embed FS at startup; tests typically
// leave it nil to exercise the fallbacks.
type Prompts struct {
	Default             string // system_default.md
	TodoGuidance        string // system_todo_guidance.md
	EnvBlockTemplate    string // system_env_block.md
	EmailGovernance     string // system_email_governance.md
	SubAgent            string // system_sub_agent.md (overrides shared default)
	CompactionSummarize string // system_compaction_summarize.md (overrides shared default)
}

// Built-in fallbacks. Kept here so tests and library callers can construct a
// Setup without dragging the embedded FS along. Production main.go replaces
// these from the embedded files so editing the .md files is the single source
// of truth.
const (
	fallbackDefaultPrompt = `You are an expert software engineer working as an agentic coding assistant inside rysh.
You have access to tools for reading files, editing files, running commands, and searching code.

Guidelines:
- Use tools to gather information before making changes.
- Make minimal, focused changes that solve the problem.
- Verify your changes work by running relevant tests or commands.
- If you're unsure, ask the user for clarification rather than guessing.
- Prefer editing existing files over creating new ones.
- Always explain what you're doing and why.`

	fallbackEmailGovernance = `You are in HUMAN-GOVERNED email mode. Incoming emails are displayed to the human but NOT auto-replied.
The human will instruct you on how to handle each email.

Available email tools:
- email_list: List recent emails from the inbox
- email_read: Read the full content of an email by UID
- email_draft: Create a draft reply or new email (does NOT send)
- email_send: Send a draft or compose and send directly (requires approval)
- email_attach: Add a file attachment to a draft

Important guidelines:
- Always show the human a preview before sending (use email_draft first, then email_send).
- Wait for explicit send confirmation before calling email_send.
- You also have all standard tools (bash, file_read, grep, etc.) for looking up information.`

	fallbackWhatsAppGovernance = `You are in HUMAN-GOVERNED WhatsApp mode. Incoming WhatsApp messages are displayed to the human but NOT auto-replied. The human will instruct you on how to handle each message.

Available WhatsApp tools:
- whatsapp_list: List recent inbound WhatsApp messages (short ID, sender, snippet)
- whatsapp_read: Read the full content of a message by its ID
- whatsapp_draft: Create a reply draft (does NOT send)
- whatsapp_send: Send a draft after the human approves (requires approval)
- whatsapp_send_template: Send a pre-approved template message when the recipient's 24h session window has closed (requires approval)

Workflow:
- When the human asks to see messages, use whatsapp_list and whatsapp_read.
- To reply, ALWAYS create a draft first with whatsapp_draft and show its full text to the human.
- Only call whatsapp_send after the human explicitly confirms (e.g. types "send").
- If a send fails because the 24h window is closed, tell the human and only call whatsapp_send_template after they explicitly approve it.
- Keep replies concise and appropriate for a WhatsApp chat (plain text, no markdown tables).
- You also have all standard tools (bash, file_read, grep, etc.) for looking up information.`

	fallbackSlackGovernance = `You are wired to Slack. The operator switches this channel's governance at runtime between "ai" (you reply autonomously) and "human" (every reply is a draft the human approves).

Available Slack tools:
- slack_list: List recent inbound Slack messages (short ID, sender, channel, snippet)
- slack_read: Read the full content of a message by its ID
- slack_draft: Create a reply draft with channel and thread_ts (does NOT post)
- slack_send: Post a draft after the human approves (requires approval)

How to behave:
- HUMAN-GOVERNED turns are explicitly marked in the prompt (e.g. "HUMAN-GOVERNED: prepare a reply DRAFT"). For those: create the draft with slack_draft (passing the channel and thread_ts of the message you are replying to), show the FULL draft text, and say: Type "send" to post this reply. NEVER call slack_send in the same turn you created a draft — only after the human explicitly confirms (e.g. types "send").
- On ordinary autonomous turns (no human-governed marker), reply by writing your response text directly — it is delivered to Slack automatically. Do NOT call slack_draft or slack_send for normal replies.
- Keep replies concise and appropriate for Slack (short sentences, minimal formatting).
- You also have all standard tools (bash, file_read, grep, etc.) for looking up information.`
)

// resolve returns the field value or the fallback if empty. Trailing
// whitespace is trimmed so concatenation with appended sections stays tidy.
func (p *Prompts) resolve(value, fallback string) string {
	if p == nil {
		value = ""
	}
	if strings.TrimSpace(value) == "" {
		return strings.TrimRight(fallback, "\n")
	}
	return strings.TrimRight(value, "\n")
}

func (p *Prompts) defaultPrompt() string {
	if p == nil {
		return strings.TrimRight(fallbackDefaultPrompt, "\n")
	}
	return p.resolve(p.Default, fallbackDefaultPrompt)
}

func (p *Prompts) todoGuidance() string {
	if p == nil {
		return ""
	}
	return strings.TrimRight(p.TodoGuidance, "\n")
}

func (p *Prompts) envBlockTemplate() string {
	if p == nil {
		return ""
	}
	return strings.TrimRight(p.EnvBlockTemplate, "\n")
}

func (p *Prompts) emailGovernance() string {
	if p == nil {
		return strings.TrimRight(fallbackEmailGovernance, "\n")
	}
	return p.resolve(p.EmailGovernance, fallbackEmailGovernance)
}

func (p *Prompts) whatsappGovernance() string {
	return strings.TrimRight(fallbackWhatsAppGovernance, "\n")
}

func (p *Prompts) slackGovernance() string {
	return strings.TrimRight(fallbackSlackGovernance, "\n")
}

// ApplySharedOverrides forwards the sub-agent and compaction prompts (if
// non-empty) into the rysh-shared agentic package's exported variables. Those
// vars are consulted by code that runs INSIDE rysh-shared (spawned child
// orchestrators, compaction summarisation) and cannot easily reach back into
// rysh-cli; pushing the values down is simpler than threading them through
// every constructor.
func (p *Prompts) ApplySharedOverrides() {
	if p == nil {
		return
	}
	if s := strings.TrimSpace(p.SubAgent); s != "" {
		sharedagentic.DefaultSubAgentSystemPrompt = strings.TrimRight(p.SubAgent, "\n")
	}
	if s := strings.TrimSpace(p.CompactionSummarize); s != "" {
		sharedagentic.DefaultCompactionSummarizePrompt = strings.TrimRight(p.CompactionSummarize, "\n")
	}
}
