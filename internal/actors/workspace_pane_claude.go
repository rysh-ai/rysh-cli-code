package actors

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/google/uuid"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// `##pane new --claude [prompt]` — open a pane with an interactive claude in it.
//
// Three things make this worth having in rysh rather than in each supervisor
// that wants it:
//
//   - The session id is PINNED before launch and recorded in the pane's
//     metadata. claude only reports the id it chose when it exits, which is
//     exactly too late to follow a session that is still running.
//   - The prompt is handed over as argv, not typed. A prompt typed at a claude
//     that is still drawing its splash screen is silently dropped, and there is
//     no ready signal to wait for.
//   - The prompt travels through a file, because a `##` line is tokenised on
//     whitespace and re-joined with single spaces — runs of spaces collapse and
//     newlines cannot survive at all.
//
// It stays a normal interactive pane: the user can watch it, type into it, and
// take it over. Nothing here runs headless.

// claudeLaunchRetry bounds the wait for a just-created pane to exist. Pane
// creation is a message down the Workspace → Tab → Lane → PaneGroup chain, so
// the id does not exist yet when this command returns; the alias does.
const (
	claudeLaunchRetryEvery = 250 * time.Millisecond
	claudeLaunchRetries    = 40 // 10s — a cold pane group has never taken close
)

// stripClaudeFlag pulls `--claude` out of args, returning the rest and whether
// it was present. Anything left over is the prompt.
func stripClaudeFlag(args []string) (rest []string, withClaude bool) {
	for i, a := range args {
		if a == "--claude" {
			return append(append([]string{}, args[:i]...), args[i+1:]...), true
		}
	}
	return args, false
}

// handlePaneNewClaude creates the pane and schedules the launch.
func (w *WorkspaceActor) handlePaneNewClaude(ctx actor.Context, out *strings.Builder, paneID string, args []string) {
	prompt := strings.TrimSpace(strings.Join(args, " "))
	sessionID := uuid.NewString()

	promptFile := ""
	if prompt != "" {
		var err error
		promptFile, err = w.writeClaudePrompt(sessionID, prompt)
		if err != nil {
			fmt.Fprintf(out, "\n[rysh] ##pane new --claude: %v\n", err)
			w.failRysh("##pane new --claude: %v", err)
			return
		}
	}

	tab := w.resolveOriginTab(paneID)
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no active tab\n")
		w.failRysh("no active tab")
		return
	}
	if err := w.checkLimits(1); err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "\n[rysh] %v\n", err)
		return
	}

	// STACKED into the caller's group, not opened as a group of its own. Two
	// reasons, and the second is the one that bites: a claude pane belongs next
	// to whoever started it, and a pane that is the only member of its group
	// cannot currently be removed — `##pane delete` reports success and leaves
	// it on screen. A stacked pane has siblings and deletes cleanly.
	alias := w.generateUniqueAlias()
	_ = w.pub.Send(msg.T("tab", tab.id, "inbox"), &msg.MsgTabCreateStackedPane{Title: alias})
	w.resCounts.panes++
	w.restoreFocusAfterCreate(paneID)
	w.persistToKV()

	w.scheduleClaudeLaunch(alias, sessionID, promptFile, claudeLaunchRetries)

	fmt.Fprintf(out, "\n[rysh] new pane %q stacked beside the caller — starting claude\n", alias)
	fmt.Fprintf(out, "  session id : %s\n", sessionID)
	fmt.Fprintf(out, "  recorded in: ##pane meta --pane %s list\n", alias)
	if promptFile != "" {
		fmt.Fprintf(out, "  prompt     : %s\n", promptFile)
	}
}

// writeClaudePrompt stores a prompt where the pane's shell can read it back.
// Named by session id so a prompt file can always be traced to the conversation
// it started, and left in place afterwards for the same reason.
func (w *WorkspaceActor) writeClaudePrompt(sessionID, prompt string) (string, error) {
	dir := filepath.Join(w.baseDir(), ".rysh", "claude-panes")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, "prompt-"+sessionID+".txt")
	if err := os.WriteFile(path, []byte(prompt), 0o600); err != nil {
		return "", fmt.Errorf("write prompt: %w", err)
	}
	return path, nil
}

// scheduleClaudeLaunch re-enters the workspace mailbox after a short delay to
// finish the job once the pane exists.
//
// A timer plus a self-addressed message, rather than polling here: resolving an
// alias to a pane id reads workspace state, which belongs to the mailbox
// goroutine. Sending is the only thing safe to do from a timer.
func (w *WorkspaceActor) scheduleClaudeLaunch(alias, sessionID, promptFile string, retries int) {
	time.AfterFunc(claudeLaunchRetryEvery, func() {
		_ = w.pub.Send(msg.T("ws", "inbox"), &msg.MsgLaunchClaudeInPane{
			Alias:      alias,
			SessionID:  sessionID,
			PromptFile: promptFile,
			Retries:    retries,
		})
	})
}

// handleLaunchClaudeInPane runs on the workspace mailbox: resolve the pane,
// record what it is, and start claude in it.
func (w *WorkspaceActor) handleLaunchClaudeInPane(m *msg.MsgLaunchClaudeInPane) {
	paneID := w.resolvePaneID(m.Alias)
	if paneID == "" {
		if m.Retries > 0 {
			w.scheduleClaudeLaunch(m.Alias, m.SessionID, m.PromptFile, m.Retries-1)
		}
		// Out of retries: the pane never appeared, and there is nothing to
		// launch into. Silent by design — the pane's absence is already visible
		// in `##pane list`, and a workspace-level error has nowhere useful to go.
		return
	}

	// Record before launching. If claude dies on startup the pane still says
	// which session it was meant to be, which is what makes the failure
	// diagnosable instead of just empty.
	_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneSetMeta{
		Key: "claude.session_id", Value: m.SessionID,
	})
	if m.PromptFile != "" {
		_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneSetMeta{
			Key: "claude.prompt_file", Value: m.PromptFile,
		})
	}
	_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneExecShell{
		Command: claudeLaunchCommand(m.SessionID, m.PromptFile),
	})
}

// claudeCleanEnv unsets the markers a parent claude exports, so a claude
// started from inside another one is a top-level session rather than a nested
// child of the conversation that launched it.
var claudeCleanEnv = []string{
	"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_EXECPATH",
	"CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ID", "CLAUDE_SESSION_ID",
	"CLAUDE_PID", "CLAUDE_EFFORT",
}

// claudeLaunchCommand builds the shell line the pane runs.
func claudeLaunchCommand(sessionID, promptFile string) string {
	var b strings.Builder
	b.WriteString("env")
	for _, v := range claudeCleanEnv {
		b.WriteString(" -u ")
		b.WriteString(v)
	}
	b.WriteString(" claude --session-id ")
	b.WriteString(sessionID)
	if promptFile != "" {
		// $(cat …) rather than the text inline: the command has already been
		// through the ## tokeniser by the time it gets here, and the file is
		// what preserved the prompt's shape across it.
		b.WriteString(` "$(cat `)
		b.WriteString(promptFile)
		b.WriteString(`)"`)
	}
	return b.String()
}
