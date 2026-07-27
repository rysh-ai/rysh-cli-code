package actors

import (
	"fmt"
	"regexp"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// dynamicModeNamePattern validates a per-humanoid dynamic mode name: a letter or
// digit followed by letters, digits, underscores or hyphens (e.g. "slack-bot").
var dynamicModeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// validDynamicModeName reports whether mode is a legal dynamic (humanoid) mode
// name. Fixed modes are excluded — they go through the normal validPaneModes path.
func validDynamicModeName(mode string) bool {
	return !validPaneModes[mode] && dynamicModeNamePattern.MatchString(mode)
}

// notifyLayoutDirty signals stream clients (the desktop app and the TUI) to
// re-fetch a layout-only snapshot. Mode / web-binding changes aren't structural
// layout changes, so without this they would not reach stream clients until the
// next genuine layout change (e.g. creating a pane) — which made `##mode new web`
// appear to do nothing until an unrelated action forced a refresh.
func (p *PaneActor) notifyLayoutDirty() {
	_ = p.pub.Send(msg.T("ws", "layoutDirty"), &msg.MsgLayoutDirty{})
}

// canonicalModeOrder is the fixed display/cycle order for input modes. enabledModes
// is always kept sorted by this order so every frontend cycles deterministically
// regardless of the order modes were enabled in.
var canonicalModeOrder = []string{"shell", "prompt", "rysh", "chat", "external", "email", "web"}

// validPaneModes is the set of modes that can be enabled/disabled via ##mode.
// "email" is a rich, interactive mode (a three-column inbox client rendered in the
// TUI and the desktop app) that a humanoid with an email channel enables on its
// registered pane; like "web" it has a dedicated render surface rather than a
// plain output buffer.
var validPaneModes = map[string]bool{
	"shell": true, "prompt": true, "rysh": true, "chat": true, "external": true, "email": true, "web": true,
}

// webPaneState is the lock-free mirror of a pane's web-mode binding, stored
// in PaneActor.webStateAtomic for reads from the agentic env-block provider
// goroutine. Empty Profile = web mode off.
type webPaneState struct {
	Profile string
	URL     string
}

// webEnabled reports whether "web" is in the pane's enabled modes.
func (p *PaneActor) webEnabled() bool {
	for _, m := range p.enabledModes {
		if m == "web" {
			return true
		}
	}
	return false
}

// routeAIToChat tells the pane's agentic actor whether to emit its output to the
// pane's CHAT channel instead of the AI channel. Web-mode panes route there so
// the "Ask Rysh" browser-agent conversation stays distinct from the pane's
// shell/ai modes (chat is its own buffer and never dual-publishes to the merged
// shell stream). Setting PaneID to the pane's own id routes to its chat buffer;
// "" restores normal AI-channel output.
func (p *PaneActor) routeAIToChat(on bool) {
	target := ""
	if on {
		target = p.id
	}
	_ = p.pub.Send(p.llmPromptExecInbox, &msg.MsgSetChatOutputPane{PaneID: target})
}

// canonicalIndex returns a mode's position in canonicalModeOrder (len if unknown,
// so unknown modes sort last).
func canonicalIndex(mode string) int {
	for i, m := range canonicalModeOrder {
		if m == mode {
			return i
		}
	}
	return len(canonicalModeOrder)
}

// insertModeInOrder returns modes with `mode` inserted at its canonical position
// (no-op if already present).
func insertModeInOrder(modes []string, mode string) []string {
	for _, m := range modes {
		if m == mode {
			return modes
		}
	}
	out := make([]string, 0, len(modes)+1)
	inserted := false
	idx := canonicalIndex(mode)
	for _, m := range modes {
		if !inserted && canonicalIndex(m) > idx {
			out = append(out, mode)
			inserted = true
		}
		out = append(out, m)
	}
	if !inserted {
		out = append(out, mode)
	}
	return out
}

// handleEnableMode adds a mode to the pane's enabled set (idempotent). For "web"
// it (re)binds the browser profile/url — re-enabling web is how the URL is
// refreshed (the desktop app renders the browser; the pane owns only this state).
func (p *PaneActor) handleEnableMode(mode, webProfile, webURL string, humanoid bool) {
	if !validPaneModes[mode] {
		// Dynamic per-humanoid mode (registered by a HumanoidActor via
		// MsgPaneEnableMode with Humanoid=true). Track it, add it to the cycle
		// (after the fixed modes), and create its buffer. The humanoid flag keeps
		// ##mode strict — only humanoid registration can enable a non-fixed mode.
		if humanoid && validDynamicModeName(mode) {
			if p.humanoidModes == nil {
				p.humanoidModes = make(map[string]bool)
			}
			p.humanoidModes[mode] = true
			if p.modeOutputs == nil {
				p.modeOutputs = make(map[string]*cappedBuffer)
			}
			if p.modeOutputs[mode] == nil {
				p.modeOutputs[mode] = &cappedBuffer{}
			}
			p.enabledModes = insertModeInOrder(p.enabledModes, mode)
			p.kvDirty = true
			p.notifyLayoutDirty()
			return
		}
		_ = p.pub.SendPaneRyshOutput(p.id,
			fmt.Sprintf("[rysh] error: invalid mode %q (valid: shell, prompt, rysh, chat, external, web)\n", mode))
		return
	}

	already := false
	for _, m := range p.enabledModes {
		if m == mode {
			already = true
			break
		}
	}

	if mode == "web" {
		p.webProfile = webProfile
		p.webURL = webURL
		p.webStateAtomic.Store(webPaneState{Profile: webProfile, URL: webURL})
		// Explicit "(re)activate web now" signal: bump on every invocation so the
		// app switches the pane back to web and rebinds even when the profile/url
		// are unchanged and the pane was showing another mode.
		p.webActivateSeq++
		if !already {
			p.enabledModes = insertModeInOrder(p.enabledModes, mode)
		}
		p.kvDirty = true
		p.routeAIToChat(true) // Ask Rysh output → chat channel (distinct from shell/ai)
		p.notifyLayoutDirty()
		// Deterministically tell the desktop app to switch this pane to web mode
		// now, rather than relying on it noticing the web_activate_seq bump in a
		// snapshot (timing-fragile — the pane otherwise only flipped to web after
		// an unrelated click forced a fresh snapshot).
		_ = p.pub.Send(msg.T("pane", p.id, "web", "activate"),
			&msg.MsgWebActivate{PaneID: p.id, Profile: p.webProfile, URL: p.webURL})
		verb := "enabled"
		if already {
			verb = "updated"
		}
		_ = p.pub.SendPaneRyshOutput(p.id,
			fmt.Sprintf("[rysh] %s web mode (profile %q, url %q)\n", verb, webProfile, webURL))
		return
	}

	if already {
		_ = p.pub.SendPaneRyshOutput(p.id, fmt.Sprintf("[rysh] mode %q is already enabled\n", mode))
		return
	}
	p.enabledModes = insertModeInOrder(p.enabledModes, mode)
	p.kvDirty = true
	p.notifyLayoutDirty()
	_ = p.pub.SendPaneRyshOutput(p.id, fmt.Sprintf("[rysh] enabled mode %q\n", mode))
}

// handleDisableMode removes a mode from the pane's enabled set. "shell" can never
// be disabled. If the pane's current mode is removed, it snaps back to "shell".
func (p *PaneActor) handleDisableMode(mode string, humanoid bool) {
	if mode == "shell" {
		_ = p.pub.SendPaneRyshOutput(p.id, "[rysh] error: shell mode cannot be disabled\n")
		return
	}
	// A dynamic humanoid mode is valid to disable too (when the humanoid
	// unregisters); otherwise the mode must be a known fixed mode.
	dynamic := humanoid || p.humanoidModes[mode]
	if !validPaneModes[mode] && !dynamic {
		_ = p.pub.SendPaneRyshOutput(p.id,
			fmt.Sprintf("[rysh] error: invalid mode %q (valid: prompt, rysh, chat, external, web)\n", mode))
		return
	}

	filtered := make([]string, 0, len(p.enabledModes))
	found := false
	for _, m := range p.enabledModes {
		if m == mode {
			found = true
			continue
		}
		filtered = append(filtered, m)
	}
	if !found {
		_ = p.pub.SendPaneRyshOutput(p.id, fmt.Sprintf("[rysh] mode %q is not enabled\n", mode))
		return
	}
	p.enabledModes = filtered
	if p.mode == mode {
		p.mode = "shell" // backend clamp; the TUI clamps its own active input mode too
	}
	if dynamic {
		delete(p.humanoidModes, mode)
		delete(p.modeOutputs, mode)
	}
	if mode == "web" {
		p.webURL, p.webProfile, p.webTitle = "", "", ""
		p.webStateAtomic.Store(webPaneState{})
		p.routeAIToChat(false) // web disabled → restore normal AI-channel output
		// Deterministically tell the desktop app to drop this pane off web display
		// now, mirroring the MsgWebActivate push on enable. Web display is fully
		// push-driven, so the app never has to infer deactivation from a snapshot's
		// cleared web binding (timing-fragile).
		_ = p.pub.Send(msg.T("pane", p.id, "web", "deactivate"), &msg.MsgWebDeactivate{PaneID: p.id})
	}
	p.kvDirty = true
	p.notifyLayoutDirty()
	_ = p.pub.SendPaneRyshOutput(p.id, fmt.Sprintf("[rysh] disabled mode %q\n", mode))
}
