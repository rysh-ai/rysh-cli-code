package channels

import (
	"regexp"
	"strings"

	sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"
)

// slackflow converts the agentic session's conversation/step stream into
// Slack messaging standards, so a Slack user sees a fluent conversation flow:
//
//   - intermediate steps arrive as SHORT TITLES ONLY (one compact mrkdwn line
//     per step, rendered as a de-emphasized context block in the reply
//     thread) — enough to know the session is progressing, without the full
//     transcript the pane terminal shows;
//   - the final answer arrives as a normal Slack message with the session's
//     markdown converted to Slack mrkdwn.

// stepEmoji maps step-event kinds to the emoji that prefixes the title line.
var stepEmoji = map[string]string{
	sharedmsg.StepRunStart:      "⏳",
	sharedmsg.StepToolStart:     "🔧",
	sharedmsg.StepToolEnd:       "🔧",
	sharedmsg.StepSubAgentStart: "🤖",
	sharedmsg.StepSubAgentEnd:   "🤖",
	sharedmsg.StepApprovalWait:  "✋",
	sharedmsg.StepCompaction:    "🗜️",
	sharedmsg.StepPaused:        "⏸️",
	sharedmsg.StepResumed:       "▶️",
	sharedmsg.StepDone:          "✅",
	sharedmsg.StepError:         "❌",
	sharedmsg.StepFinalAnswer:   "💬",
	sharedmsg.StepGrounded:      "🧭",
	sharedmsg.StepQuestion:      "❓",
}

// SlackStepLine renders one agentic step event as a single compact mrkdwn
// line for the Slack progress flow: an emoji, optional nesting marker for
// sub-agent depth, and the step TITLE only (never the step detail/payload).
func SlackStepLine(step *sharedmsg.MsgAgenticStep) string {
	if step == nil {
		return ""
	}
	emoji, ok := stepEmoji[step.Kind]
	if !ok {
		emoji = "•"
	}
	prefix := ""
	if step.Depth > 0 {
		prefix = strings.Repeat("↳ ", step.Depth)
	}
	title := strings.TrimSpace(step.Title)
	if title == "" {
		title = step.Kind
	}
	return emoji + " " + prefix + slackEscape(title)
}

// slackStepKindsForwarded is the set of step kinds worth a line in the Slack
// flow. tool_start is skipped (tool_end already carries the title plus the
// outcome digest, and one line per tool keeps the thread readable);
// done/final_answer are skipped because the flushed final response follows
// immediately and would make them redundant noise.
var slackStepKindsForwarded = map[string]bool{
	sharedmsg.StepRunStart:      true,
	sharedmsg.StepToolEnd:       true,
	sharedmsg.StepSubAgentStart: true,
	sharedmsg.StepSubAgentEnd:   true,
	sharedmsg.StepApprovalWait:  true,
	sharedmsg.StepCompaction:    true,
	sharedmsg.StepPaused:        true,
	sharedmsg.StepResumed:       true,
	sharedmsg.StepError:         true,
	sharedmsg.StepGrounded:      true,
	sharedmsg.StepQuestion:      true,
}

// ShouldForwardStepToSlack reports whether a step event earns a progress line
// in the Slack flow.
func ShouldForwardStepToSlack(step *sharedmsg.MsgAgenticStep) bool {
	return step != nil && slackStepKindsForwarded[step.Kind]
}

var (
	mdBold      = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	mdHeader    = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	mdBullet    = regexp.MustCompile(`(?m)^(\s*)[-*]\s+`)
	mdLinkPair  = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	mdCodeFence = regexp.MustCompile("(?s)```[a-zA-Z0-9_+-]*\n")
)

// ToSlackMrkdwn converts the session's markdown-ish LLM output to Slack
// mrkdwn so the final answer reads natively in Slack:
//
//	**bold**        → *bold*
//	# Heading       → *Heading*
//	- bullet        → • bullet
//	[text](url)     → <url|text>
//	```lang fences  → ``` (Slack fences carry no language tag)
//
// Inline code, plain *italic*, and everything inside code fences are left
// as-is (Slack renders backtick spans natively).
func ToSlackMrkdwn(text string) string {
	// Convert fenced-code language tags first so later passes don't touch
	// fence internals structurally (regexes below are line-anchored and
	// conservative, so fences stay intact).
	text = mdCodeFence.ReplaceAllString(text, "```\n")
	text = mdBold.ReplaceAllString(text, "*$1*")
	text = mdHeader.ReplaceAllString(text, "*$1*")
	text = mdBullet.ReplaceAllString(text, "$1• ")
	text = mdLinkPair.ReplaceAllString(text, "<$2|$1>")
	return text
}

// slackEscape escapes the three characters Slack requires escaping in
// mrkdwn/text payloads.
func slackEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
