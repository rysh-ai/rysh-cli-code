package channels

import (
	"strings"
	"testing"

	sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"
)

func TestSlackStepLine(t *testing.T) {
	cases := []struct {
		step *sharedmsg.MsgAgenticStep
		want string
	}{
		{&sharedmsg.MsgAgenticStep{Kind: sharedmsg.StepToolEnd, Title: "bash: go test ./... — ✓ 12 lines"},
			"🔧 bash: go test ./... — ✓ 12 lines"},
		{&sharedmsg.MsgAgenticStep{Kind: sharedmsg.StepSubAgentStart, Title: "sub-agent: audit the msg package", Depth: 0},
			"🤖 sub-agent: audit the msg package"},
		{&sharedmsg.MsgAgenticStep{Kind: sharedmsg.StepToolEnd, Title: "grep: TODO", Depth: 1},
			"🔧 ↳ grep: TODO"},
		{&sharedmsg.MsgAgenticStep{Kind: sharedmsg.StepPaused, Title: "paused — interrupted by user"},
			"⏸️ paused — interrupted by user"},
		{&sharedmsg.MsgAgenticStep{Kind: "unknown_kind", Title: ""},
			"• unknown_kind"},
	}
	for _, c := range cases {
		if got := SlackStepLine(c.step); got != c.want {
			t.Errorf("SlackStepLine(%s) = %q, want %q", c.step.Kind, got, c.want)
		}
	}
	if SlackStepLine(nil) != "" {
		t.Error("nil step should render empty")
	}
	// Slack-reserved characters are escaped.
	esc := SlackStepLine(&sharedmsg.MsgAgenticStep{Kind: sharedmsg.StepToolEnd, Title: "bash: a < b && c > d"})
	if !strings.Contains(esc, "&lt;") || !strings.Contains(esc, "&amp;&amp;") || !strings.Contains(esc, "&gt;") {
		t.Errorf("title not escaped: %q", esc)
	}
}

func TestShouldForwardStepToSlack(t *testing.T) {
	forwarded := []string{
		sharedmsg.StepRunStart, sharedmsg.StepToolEnd, sharedmsg.StepSubAgentStart,
		sharedmsg.StepSubAgentEnd, sharedmsg.StepApprovalWait, sharedmsg.StepPaused,
		sharedmsg.StepResumed, sharedmsg.StepError, sharedmsg.StepCompaction,
	}
	for _, k := range forwarded {
		if !ShouldForwardStepToSlack(&sharedmsg.MsgAgenticStep{Kind: k}) {
			t.Errorf("kind %s should be forwarded", k)
		}
	}
	// tool_start / done / final_answer stay out of the Slack flow (redundant
	// with tool_end and the flushed final reply).
	suppressed := []string{sharedmsg.StepToolStart, sharedmsg.StepDone, sharedmsg.StepFinalAnswer}
	for _, k := range suppressed {
		if ShouldForwardStepToSlack(&sharedmsg.MsgAgenticStep{Kind: k}) {
			t.Errorf("kind %s should NOT be forwarded", k)
		}
	}
	if ShouldForwardStepToSlack(nil) {
		t.Error("nil step should not be forwarded")
	}
}

func TestToSlackMrkdwn(t *testing.T) {
	in := "# Result\n\n**Done** — see [docs](https://example.com/x).\n- first\n- second\n\n```go\nfmt.Println(1)\n```"
	out := ToSlackMrkdwn(in)
	for _, want := range []string{
		"*Result*",
		"*Done*",
		"<https://example.com/x|docs>",
		"• first",
		"• second",
		"```\nfmt.Println(1)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("mrkdwn missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "**") || strings.Contains(out, "# ") {
		t.Errorf("markdown residue left in:\n%s", out)
	}
}
