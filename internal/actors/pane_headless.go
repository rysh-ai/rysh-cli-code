package actors

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/browserinstance"
	"github.com/rysh-ai/rysh-cli-code/internal/cdp"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// handleWebHeadless services MsgPaneWebHeadless: spawn/stop/inspect the
// pane's CLI-owned headless browser executor. Exactly ONE executor must
// answer `pane.<id>.browser.request` at a time, so turning headless on
// unbinds any desktop-app view (web.deactivate push) and turning it off
// rebinds the app (web.activate push) when web mode is still enabled.
func (p *PaneActor) handleWebHeadless(ctx actor.Context, m *msg.MsgPaneWebHeadless) {
	switch m.Op {
	case "on":
		profile := browserinstance.SanitizeProfile(m.Profile)
		if m.Profile == "" {
			profile = browserinstance.SanitizeProfile(p.webProfile) // fall back to the pane's web binding
		}
		url := m.URL
		if url == "" {
			url = p.webURL
		}

		// Restart cleanly when already running (profile/URL may differ).
		if p.headlessPID != nil {
			ctx.Stop(p.headlessPID)
			p.headlessPID = nil
		}

		// Enable web-mode state inline (mirrors handleEnableMode's web branch
		// WITHOUT the app-activate push — the headless executor owns the
		// browser now): prompt context (env-block web state), chat routing,
		// and mode cycling all behave exactly as app-backed web mode.
		p.webProfile, p.webURL = profile, url
		p.webStateAtomic.Store(webPaneState{Profile: profile, URL: url})
		p.webActivateSeq++
		if !p.webEnabled() {
			p.enabledModes = insertModeInOrder(p.enabledModes, "web")
		}
		p.routeAIToChat(true)
		p.kvDirty = true
		p.notifyLayoutDirty()
		// Unbind any desktop-app view so the app stops answering
		// browser.request for this pane — one executor at a time.
		_ = p.pub.Send(msg.T("pane", p.id, "web", "deactivate"), &msg.MsgWebDeactivate{PaneID: p.id})

		h := NewHeadlessBrowserActor(p.id, profile, url, p.headlessProfileDir(profile), p.pub, p.nc)
		props := actor.PropsFromProducer(func() actor.Actor { return h })
		p.headlessPID = ctx.Spawn(props)
		p.headlessProfile = profile
		// The actor reports readiness (or launch failure) to rysh output itself.

	case "off":
		if p.headlessPID == nil {
			_ = p.pub.SendPaneRyshOutput(p.id, "[web] headless browser is not running\n")
			return
		}
		ctx.Stop(p.headlessPID)
		p.headlessPID = nil
		p.headlessProfile = ""
		_ = p.pub.SendPaneRyshOutput(p.id, "[web] headless browser stopped\n")
		// Hand the browser back to the desktop app when web mode is still on.
		if p.webEnabled() && p.webProfile != "" {
			p.webActivateSeq++
			_ = p.pub.Send(msg.T("pane", p.id, "web", "activate"),
				&msg.MsgWebActivate{PaneID: p.id, Profile: p.webProfile, URL: p.webURL})
		}

	case "status":
		var sb strings.Builder
		fmt.Fprintf(&sb, "\n[web] headless status\n")
		fmt.Fprintf(&sb, "%s\n", strings.Repeat("-", 50))
		if p.headlessPID != nil {
			fmt.Fprintf(&sb, "  state    : running\n")
			fmt.Fprintf(&sb, "  profile  : %s\n", p.headlessProfile)
			fmt.Fprintf(&sb, "  auth dir : %s\n", p.headlessProfileDir(p.headlessProfile))
		} else {
			fmt.Fprintf(&sb, "  state    : not running\n")
		}
		if bin := cdp.FindChromium(); bin != "" {
			fmt.Fprintf(&sb, "  chromium : %s\n", bin)
		} else {
			fmt.Fprintf(&sb, "  chromium : NOT FOUND — install one or set RYSH_CHROMIUM_PATH\n")
		}
		fmt.Fprintf(&sb, "%s\n", strings.Repeat("-", 50))
		fmt.Fprintf(&sb, "  note: headless auth is separate from the desktop app's browser —\n")
		fmt.Fprintf(&sb, "  log in once with ##web headless login <profile>\n\n")
		_ = p.pub.SendPaneRyshOutput(p.id, sb.String())

	default:
		_ = p.pub.SendPaneRyshOutput(p.id, fmt.Sprintf("[web] unknown headless op %q\n", m.Op))
	}
}

// headlessProfileDir is the Chromium user-data-dir for a profile's headless
// runs: a "headless" subdirectory of the shared browser-instances profile
// dir. Kept separate from the desktop app's Electron partition data (the
// storage formats are incompatible), but under the same root so it survives
// session cleanup with the rest of browser-instances.
func (p *PaneActor) headlessProfileDir(profile string) string {
	workDir := strings.TrimSpace(p.cfg.WorkingDirectory)
	if workDir == "" {
		if wd, err := os.Getwd(); err == nil {
			workDir = wd
		} else {
			workDir = "."
		}
	}
	return filepath.Join(browserinstance.ProfileDir(workDir, browserinstance.SanitizeProfile(profile)), "headless")
}
