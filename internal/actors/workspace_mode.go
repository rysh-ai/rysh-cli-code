// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/browserinstance"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ---------------------------------------------------------------------------
// ##mode — per-pane input-mode enable/disable
// ---------------------------------------------------------------------------

// normalizeModeName maps user-facing aliases to canonical mode names. Returns ""
// for an unrecognized mode. "ai" is the user-facing label for "prompt".
func normalizeModeName(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "sh", "shell":
		return "shell"
	case "ai", "prompt":
		return "prompt"
	case "rysh":
		return "rysh"
	case "chat":
		return "chat"
	case "external", "ext":
		return "external"
	case "email", "mail", "inbox":
		return "email"
	case "web", "browser":
		return "web"
	default:
		return ""
	}
}

// modeDisplayLabel renders a canonical mode name for human output.
func modeDisplayLabel(mode string) string {
	if mode == "prompt" {
		return "prompt (ai)"
	}
	return mode
}

func (w *WorkspaceActor) handleModeSubcommand(out *strings.Builder, paneID string, args []string) {
	sub := "list"
	if len(args) > 0 {
		sub = strings.ToLower(args[0])
	}
	switch sub {
	case "list", "ls":
		w.modeList(out, paneID)
	case "profiles", "list-profiles", "profile-list":
		w.modeListProfiles(out, paneID)
	case "new", "add", "enable":
		w.modeNew(out, paneID, args[1:])
	case "delete", "remove", "disable", "del", "rm":
		w.modeDelete(out, paneID, args[1:])
	case "web":
		w.modeWeb(out, paneID, args[1:])
	default:
		w.modeHelp(out)
		w.failRyshUsage("unknown ##mode subcommand: %q", sub)
	}
}

func (w *WorkspaceActor) modeHelp(out *strings.Builder) {
	ryshWriter(out).Usage(
		"##mode list                         list enabled + available input modes",
		"##mode new <mode>                   enable a mode (shell|prompt(ai)|rysh|chat|external|email|web)",
		"##mode new web [--profile N] [url]  enable web mode bound to a persistent browser profile",
		"##mode profiles                     list known browser profiles (logins persist per profile)",
		"##mode web ai [--pane <id|name>] <prompt>  send a prompt to a pane's web AI (default: this pane; --pane targets another)",
		"##mode web ai [--pane <id|name>] history [print] [N]  print a pane's last N Ask Rysh turns (default 5)",
		"##mode delete <mode>                disable a mode (shell cannot be disabled)",
	)
}

// modeWeb handles `##mode web <action>` — acting on a pane's web mode without
// switching the display to it. Currently supports `ai <prompt>`, which sends a
// prompt to the pane's "Ask Rysh" web AI assistant (the AI drives this pane's
// embedded browser via its browser tools) from any mode, e.g. shell.
func (w *WorkspaceActor) modeWeb(out *strings.Builder, paneID string, args []string) {
	if len(args) == 0 {
		ryshWriter(out).UsageLine("##mode web ai <prompt>   (send a prompt to this pane's web AI assistant)")
		w.failRyshUsage("usage: %s", "##mode web ai <prompt>   (send a prompt to this pane's web AI assistant)")
		return
	}
	switch strings.ToLower(args[0]) {
	case "ai", "prompt", "ask":
		w.sendWebAIPrompt(out, paneID, strings.Join(args[1:], " "))
	default:
		fmt.Fprintf(out, "\n[rysh] unknown web action %q (usage: ##mode web ai <prompt>)\n", args[0])
		w.failRysh("unknown web action %q (usage: ##mode web ai <prompt>)", args[0])
	}
}

// sendWebAIPrompt dispatches a prompt to a web pane's "Ask Rysh" AI assistant.
// Shared by `##mode web ai <prompt>` and the `##webai <prompt>` shorthand. The
// rawPrompt is the (possibly quote-wrapped, whitespace-tokenized-then-rejoined)
// prompt text.
func (w *WorkspaceActor) sendWebAIPrompt(out *strings.Builder, paneID, rawPrompt string) {
	// An optional `--pane <id|name>` redirects the prompt to ANOTHER pane's "Ask
	// Rysh" panel (e.g. `##webai --pane editor list the items on the left`), so one
	// pane can drive another web pane's browser AI. Without it the prompt targets
	// the pane the command was typed in.
	targetRef, stripped := extractPaneFlag(rawPrompt)
	paneLabel := "this pane"
	if targetRef != "" {
		resolved := w.resolvePaneID(targetRef)
		if resolved == "" {
			fmt.Fprintf(out, "\n[rysh] pane %q not found\n", targetRef)
			w.failRysh("pane %q not found", targetRef)
			return
		}
		paneID = resolved
		rawPrompt = stripped
		paneLabel = fmt.Sprintf("pane %q", targetRef)
	}

	// `history [print] [N]` prints the target pane's recent Ask Rysh turns instead
	// of sending a prompt (a turn = a user prompt + the assistant's answer).
	if fields := strings.Fields(rawPrompt); len(fields) > 0 && strings.EqualFold(fields[0], "history") {
		w.webAIHistory(out, paneID, paneLabel, fields[1:])
		return
	}

	prompt := stripSurroundingQuotes(strings.TrimSpace(rawPrompt))
	if prompt == "" {
		ryshWriter(out).UsageLine("##webai [--pane <id|name>] <prompt> | history [print] [N]")
		w.failRyshUsage("usage: %s", "##webai [--pane <id|name>] <prompt> | history [print] [N]")
		return
	}
	// The AI assistant drives the target pane's embedded browser, so web mode must
	// be enabled there (it owns the browser binding + AI→chat routing).
	snap, ok := w.requestPaneSnapshot(paneID)
	webOn := false
	if ok {
		for _, m := range snap.EnabledModes {
			if m == "web" {
				webOn = true
				break
			}
		}
	}
	if !webOn {
		fmt.Fprintf(out, "\n[rysh] web mode is not enabled on %s — run `##mode new web [--profile N] [url]` there first\n", paneLabel)
		w.failRysh("web mode is not enabled on %s — run `##mode new web [--profile N] [url]` there first", paneLabel)
		return
	}
	// Tell the desktop app to render this prompt as a human bubble in the target
	// pane's Ask Rysh panel (so a shell-mode prompt looks identical to one typed in
	// that pane's chat box). Sent before dispatch so the app captures the AI-reply
	// offset before any output streams.
	_ = w.pub.Send(msg.T("pane", paneID, "web", "prompt"),
		&msg.MsgWebPromptDispatched{PaneID: paneID, Prompt: prompt})
	// Dispatch through the normal prompt path on the TARGET pane: executePrompt
	// runs the LLM with that pane's browser tools and (for web panes) routes output
	// to its chat channel.
	_ = w.pub.Send(msg.T("pane", paneID, "inbox"),
		&msg.MsgPaneSubmitInput{Text: prompt, Mode: "prompt"})
	if targetRef != "" {
		fmt.Fprintf(out, "\n[rysh] → web AI [%s]: %s\n", targetRef, prompt)
	} else {
		fmt.Fprintf(out, "\n[rysh] → web AI: %s\n", prompt)
	}
}

// extractPaneFlag pulls an optional `--pane <ref>` / `--pane=<ref>` target out of
// a web-AI prompt and returns the pane ref plus the prompt with the flag removed.
// The ref is an id, auto-title, or given-name (resolved by resolvePaneID). Tokens
// are whitespace-split, matching how the caller already normalizes the prompt via
// strings.Join.
func extractPaneFlag(s string) (target, rest string) {
	toks := strings.Fields(s)
	kept := make([]string, 0, len(toks))
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch {
		case t == "--pane":
			if i+1 < len(toks) {
				target = toks[i+1]
				i++
			}
		case strings.HasPrefix(t, "--pane="):
			target = strings.TrimPrefix(t, "--pane=")
		default:
			kept = append(kept, t)
		}
	}
	return target, strings.Join(kept, " ")
}

// askRyshTurn pairs a human prompt with the assistant's answer — one "turn" of a
// pane's Ask Rysh conversation.
type askRyshTurn struct {
	prompt string
	answer string
}

// webAIHistory prints the target pane's recent Ask Rysh turns. args holds the
// tokens after the `history` keyword (e.g. ["print", "5"]); a trailing number
// sets how many turns to show (default 5, max 50). The originating pane receives
// the formatted transcript via out.
func (w *WorkspaceActor) webAIHistory(out *strings.Builder, paneID, paneLabel string, args []string) {
	n := 5
	for _, a := range args {
		if v, err := strconv.Atoi(a); err == nil && v > 0 {
			n = v
		}
	}
	if n > 50 {
		n = 50
	}

	// "Ask Rysh" is the web pane's AI assistant, so web mode must be enabled there.
	snap, ok := w.requestPaneSnapshot(paneID)
	webOn := false
	if ok {
		for _, m := range snap.EnabledModes {
			if m == "web" {
				webOn = true
				break
			}
		}
	}
	if !webOn {
		fmt.Fprintf(out, "\n[rysh] web mode is not enabled on %s — run `##mode new web [--profile N] [url]` there first\n", paneLabel)
		w.failRysh("web mode is not enabled on %s — run `##mode new web [--profile N] [url]` there first", paneLabel)
		return
	}

	// A web pane's LLM conversation IS its Ask Rysh exchange. LastN counts messages
	// (user/assistant/tool) and a single turn can carry several tool messages, so
	// fetch generously and fold the transcript into turns below.
	res, err := w.pub.Request(
		msg.T("pane", paneID, "llm_prompt_execution", "inbox"),
		&msg.MsgGetConversationHistory{LastN: 100},
		3*time.Second,
	)
	if err != nil {
		fmt.Fprintf(out, "\n[rysh] could not read Ask Rysh history for %s (no active session yet?)\n", paneLabel)
		w.failRysh("could not read Ask Rysh history for %s (no active session yet?)", paneLabel)
		return
	}
	reply, ok := res.(*msg.MsgConversationHistoryReply)
	if !ok {
		fmt.Fprintf(out, "\n[rysh] unexpected reply reading Ask Rysh history for %s\n", paneLabel)
		return
	}

	turns := groupAskRyshTurns(reply.Turns)
	if len(turns) == 0 {
		fmt.Fprintf(out, "\n[rysh] %s has no Ask Rysh history yet\n", paneLabel)
		return
	}
	if len(turns) > n {
		turns = turns[len(turns)-n:]
	}

	label := "turns"
	if len(turns) == 1 {
		label = "turn"
	}
	fmt.Fprintf(out, "\n[rysh] Ask Rysh history — %s (last %d %s):\n", paneLabel, len(turns), label)
	for i, t := range turns {
		fmt.Fprintf(out, "\n  ── turn %d ──\n", i+1)
		out.WriteString("  you:\n")
		out.WriteString(webAIIndent(webAITrunc(t.prompt, 4000), "    "))
		out.WriteString("  rysh:\n")
		ans := t.answer
		if ans == "" {
			ans = "(no text answer)"
		}
		out.WriteString(webAIIndent(webAITrunc(ans, 4000), "    "))
	}
}

// groupAskRyshTurns folds a flat LLM transcript (user/assistant/tool messages)
// into question→answer turns: each genuine "user" prompt starts a turn whose
// answer is the assistant's prose replies before the next prompt. Tool calls and
// tool results are omitted so a turn shows just the human prompt and the answer.
func groupAskRyshTurns(turns []msg.ConversationTurnInfo) []askRyshTurn {
	var out []askRyshTurn
	curIdx := -1
	for _, t := range turns {
		switch t.Role {
		case "user":
			if t.ToolCallID != "" { // a tool result, not a human prompt
				continue
			}
			out = append(out, askRyshTurn{prompt: strings.TrimSpace(t.Content)})
			curIdx = len(out) - 1
		case "assistant":
			if curIdx < 0 {
				continue
			}
			if txt := strings.TrimSpace(t.Content); txt != "" {
				if out[curIdx].answer != "" {
					out[curIdx].answer += "\n"
				}
				out[curIdx].answer += txt
			}
		}
	}
	return out
}

// webAIIndent prefixes every line of s (used to indent a transcript block).
func webAIIndent(s, prefix string) string {
	if s == "" {
		return prefix + "\n"
	}
	var b strings.Builder
	for _, ln := range strings.Split(s, "\n") {
		b.WriteString(prefix)
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	return b.String()
}

// webAITrunc caps a transcript field so a long answer can't flood the pane.
func webAITrunc(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n… (truncated)"
}

// stripSurroundingQuotes removes a single pair of matching surrounding single or
// double quotes (the prompt in `##mode web ai "..."` arrives quoted because the
// command is whitespace-tokenized).
func stripSurroundingQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// modeListProfiles lists the persistent browser profiles known to this
// workspace. Each profile keeps its own cookie/login jar, so re-running
// `##mode new web --profile <name>` re-attaches to the same logged-in session.
func (w *WorkspaceActor) modeListProfiles(out *strings.Builder, paneID string) {
	regs, err := browserinstance.LoadRegistry(w.browserWorkDir())
	if err != nil {
		fmt.Fprintf(out, "\n[rysh] failed to read browser profiles: %v\n", err)
		w.failRysh("failed to read browser profiles: %v", err)
		return
	}

	// Highlight the profile bound to this pane (if web mode is enabled).
	bound := ""
	if snap, ok := w.requestPaneSnapshot(paneID); ok {
		bound = snap.WebProfile
	}

	out.WriteString("\n[rysh] browser profiles (web mode):\n")
	if len(regs) == 0 {
		out.WriteString("  (none yet — created on first `##mode new web [--profile <name>] [url]`)\n")
	} else {
		for _, p := range regs {
			marker := "  "
			if p.Name == bound {
				marker = "* " // bound to this pane
			}
			fmt.Fprintf(out, "  %s- %s\n", marker, p.Name)
		}
	}
	out.WriteString("  usage: ##mode new web --profile <name> [url]   (a logged-in session persists per profile)\n")
}

func (w *WorkspaceActor) modeList(out *strings.Builder, paneID string) {
	snap, ok := w.requestPaneSnapshot(paneID)
	if !ok {
		out.WriteString("\n[rysh] failed to read pane snapshot\n")
		return
	}
	enabled := snap.EnabledModes
	if len(enabled) == 0 {
		enabled = []string{"shell", "prompt", "rysh", "chat"}
	}
	enabledSet := map[string]bool{}
	for _, m := range enabled {
		enabledSet[m] = true
	}

	label := paneID
	if len(label) > 8 {
		label = label[:8]
	}
	fmt.Fprintf(out, "\n[rysh] input modes for pane %s:\n", label)
	out.WriteString("  enabled:\n")
	for _, m := range canonicalModeOrder {
		if !enabledSet[m] {
			continue
		}
		if m == "web" {
			fmt.Fprintf(out, "    - web (profile %q, url %q)\n", snap.WebProfile, snap.WebURL)
		} else {
			fmt.Fprintf(out, "    - %s\n", modeDisplayLabel(m))
		}
	}
	var avail []string
	for _, m := range canonicalModeOrder {
		if !enabledSet[m] {
			avail = append(avail, modeDisplayLabel(m))
		}
	}
	if len(avail) == 0 {
		out.WriteString("  available: (all enabled)\n")
	} else {
		fmt.Fprintf(out, "  available: %s\n", strings.Join(avail, ", "))
	}
	if enabledSet["external"] && snap.ExternalOutput == "" {
		out.WriteString("  note: 'external' stays hidden in the double-Esc cycle until a humanoid registers output\n")
	}
}

func (w *WorkspaceActor) modeNew(out *strings.Builder, paneID string, args []string) {
	if len(args) == 0 {
		ryshWriter(out).UsageLine("##mode new <mode>  (shell|prompt(ai)|rysh|chat|external|email|web)")
		w.failRyshUsage("usage: %s", "##mode new <mode>  (shell|prompt(ai)|rysh|chat|external|email|web)")
		return
	}
	mode := normalizeModeName(args[0])
	if mode == "" {
		fmt.Fprintf(out, "\n[rysh] unknown mode %q (valid: shell, prompt(ai), rysh, chat, external, email, web)\n", args[0])
		w.failRysh("unknown mode %q (valid: shell, prompt(ai), rysh, chat, external, email, web)", args[0])
		return
	}

	if mode == "web" {
		profile, url := parseWebArgs(args[1:])
		profile = browserinstance.SanitizeProfile(profile)
		dir, err := browserinstance.EnsureProfile(w.browserWorkDir(), profile)
		if err != nil {
			fmt.Fprintf(out, "\n[rysh] failed to create browser profile %q: %v\n", profile, err)
			w.failRysh("failed to create browser profile %q: %v", profile, err)
			return
		}
		_ = w.pub.Send(msg.T("pane", paneID, "inbox"),
			&msg.MsgPaneEnableMode{PaneID: paneID, Mode: "web", WebProfile: profile, WebURL: url})
		fmt.Fprintf(out, "\n[rysh] enabled web mode (profile %q → %s, url %s)\n", profile, dir, url)
		// The pane binding is real either way, but only a rich renderer paints
		// the page. Enabling web mode from a bare terminal otherwise looks like
		// a no-op — the command succeeds and the pane shows a placeholder — so
		// say which of the two is happening, and how to get a live page.
		if !w.hasRichRenderer() {
			out.WriteString("  no browser-capable client is attached, so this pane shows the binding, not the page\n")
			out.WriteString("  to automate it here:      ##web headless on\n")
			out.WriteString("  to see the page:          attach the desktop app, or `##rysh web start` and open the URL\n")
		}
		return
	}

	_ = w.pub.Send(msg.T("pane", paneID, "inbox"),
		&msg.MsgPaneEnableMode{PaneID: paneID, Mode: mode})
	fmt.Fprintf(out, "\n[rysh] enabled mode %q\n", modeDisplayLabel(mode))
}

func (w *WorkspaceActor) modeDelete(out *strings.Builder, paneID string, args []string) {
	if len(args) == 0 {
		ryshWriter(out).UsageLine("##mode delete <mode>")
		w.failRyshUsage("usage: %s", "##mode delete <mode>")
		return
	}
	mode := normalizeModeName(args[0])
	if mode == "" {
		fmt.Fprintf(out, "\n[rysh] unknown mode %q (valid: prompt(ai), rysh, chat, external, email, web)\n", args[0])
		w.failRysh("unknown mode %q (valid: prompt(ai), rysh, chat, external, email, web)", args[0])
		return
	}
	if mode == "shell" {
		out.WriteString("\n[rysh] shell mode cannot be disabled\n")
		return
	}
	_ = w.pub.Send(msg.T("pane", paneID, "inbox"),
		&msg.MsgPaneDisableMode{PaneID: paneID, Mode: mode})
	fmt.Fprintf(out, "\n[rysh] disabled mode %q\n", modeDisplayLabel(mode))
}

// parseWebArgs parses the trailing args of `##mode new web [--profile N] [url]`.
// Defaults: profile "default", url "about:blank". A bare host is upgraded to https.
func parseWebArgs(args []string) (profile, url string) {
	profile = browserinstance.DefaultProfile
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--profile" || a == "-p":
			if i+1 < len(args) {
				profile = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--profile="):
			profile = strings.TrimPrefix(a, "--profile=")
		default:
			if url == "" {
				url = a
			}
		}
	}
	return profile, normalizeWebURL(url)
}

func normalizeWebURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" || u == "about:blank" {
		return "about:blank"
	}
	if !strings.Contains(u, "://") {
		return "https://" + u
	}
	return u
}

// browserWorkDir resolves the directory that anchors .rysh/browser-instances:
// cfg.WorkingDirectory (the dir containing rysh.config.yaml) when set, else the
// daemon's cwd — matching where panes run and where session cleanup looks.
func (w *WorkspaceActor) browserWorkDir() string {
	if wd := strings.TrimSpace(w.cfg.WorkingDirectory); wd != "" {
		return wd
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// requestPaneSnapshot fetches a pane's snapshot via request/reply (same pattern
// as showShareRestrictions).
func (w *WorkspaceActor) requestPaneSnapshot(paneID string) (domain.PaneSnapshot, bool) {
	res, err := w.pub.Request(msg.T("pane", paneID, "inbox"), &msg.MsgGetPaneSnapshot{}, time.Second)
	if err != nil {
		return domain.PaneSnapshot{}, false
	}
	reply, ok := res.(*msg.MsgPaneSnapshotReply)
	if !ok {
		return domain.PaneSnapshot{}, false
	}
	return reply.Snapshot, true
}
