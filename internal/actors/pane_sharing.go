// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"strings"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ---------------------------------------------------------------------------
// Remote upstream sharing
// ---------------------------------------------------------------------------

func (p *PaneActor) startSharing(ctx actor.Context) {
	if p.sharing {
		p.appendOutput("\n[rysh] pane is already shared\n")
		return
	}
	if !p.cfg.Upstream.Enabled || p.cfg.Upstream.URL == "" {
		p.appendOutput("\n[rysh] upstream not configured; set the upstream: block in rysh.config.yaml\n")
		return
	}

	upstream := NewRemoteUpstreamActor(
		p.id, p.title, p.sessionName,
		p.cfg.Upstream, p.pub, p.nc,
	)
	props := actor.PropsFromProducer(func() actor.Actor { return upstream })
	p.remoteUpstreamPID = ctx.Spawn(props)
	p.sharing = true
	p.upstreamURL = p.cfg.Upstream.URL
	p.upstreamConnected = upstream.Connected()
	p.kvDirty = true
	p.appendOutput(fmt.Sprintf("\n[rysh] pane sharing started (upstream: %s)\n", p.upstreamURL))
}

func (p *PaneActor) stopSharing(ctx actor.Context) {
	if !p.sharing {
		p.appendOutput("\n[rysh] pane is not being shared\n")
		return
	}
	if p.remoteUpstreamPID != nil {
		ctx.Stop(p.remoteUpstreamPID)
		p.remoteUpstreamPID = nil
	}
	p.sharing = false
	p.upstreamURL = ""
	p.upstreamConnected = false
	p.kvDirty = true
	p.appendOutput("\n[rysh] pane sharing stopped\n")
}

// Sharing returns whether this pane is currently sharing to the upstream.
func (p *PaneActor) Sharing() bool {
	return p.sharing
}

// ---------------------------------------------------------------------------
// Share restrictions
// ---------------------------------------------------------------------------

// validShareModes lists the mode names accepted by disable/enable commands.
var validShareModes = map[string]bool{"sh": true, "ai": true, "rysh": true, "chat": true}

// handleShareDisableMode adds a mode to the disabled list (deduplicated).
func (p *PaneActor) handleShareDisableMode(mode string) {
	if !validShareModes[mode] {
		_ = p.pub.SendPaneRyshOutput(p.id,
			fmt.Sprintf("[rysh] error: invalid mode %q (valid: sh, ai, rysh, chat)\n", mode))
		return
	}
	// Check for dedup.
	for _, m := range p.shareRestrictions.DisabledModes {
		if m == mode {
			_ = p.pub.SendPaneRyshOutput(p.id,
				fmt.Sprintf("[rysh] mode %q is already disabled\n", mode))
			return
		}
	}
	// Ensure at least one mode remains enabled.
	allModes := []string{"sh", "ai", "rysh", "chat"}
	willBeDisabled := append(append([]string{}, p.shareRestrictions.DisabledModes...), mode)
	if len(willBeDisabled) >= len(allModes) {
		_ = p.pub.SendPaneRyshOutput(p.id,
			"[rysh] error: cannot disable all modes -- at least one must remain enabled\n")
		return
	}
	p.shareRestrictions.DisabledModes = willBeDisabled
	p.kvDirty = true
	p.publishRestrictions()
	_ = p.pub.SendPaneRyshOutput(p.id,
		fmt.Sprintf("[rysh] disabled mode %q for remote users\n", mode))
}

// handleShareEnableMode removes a mode from the disabled list.
func (p *PaneActor) handleShareEnableMode(mode string) {
	if !validShareModes[mode] {
		_ = p.pub.SendPaneRyshOutput(p.id,
			fmt.Sprintf("[rysh] error: invalid mode %q (valid: sh, ai, rysh, chat)\n", mode))
		return
	}
	filtered := make([]string, 0, len(p.shareRestrictions.DisabledModes))
	found := false
	for _, m := range p.shareRestrictions.DisabledModes {
		if m == mode {
			found = true
			continue
		}
		filtered = append(filtered, m)
	}
	if !found {
		_ = p.pub.SendPaneRyshOutput(p.id,
			fmt.Sprintf("[rysh] mode %q is not disabled\n", mode))
		return
	}
	p.shareRestrictions.DisabledModes = filtered
	p.kvDirty = true
	p.publishRestrictions()
	_ = p.pub.SendPaneRyshOutput(p.id,
		fmt.Sprintf("[rysh] enabled mode %q for remote users\n", mode))
}

// publishRestrictions sends a MsgShareRestrictionsUpdated to the pane's
// restrictions topic so UpstreamShareActor (and remote subscribers) can
// pick up the change.
func (p *PaneActor) publishRestrictions() {
	// File browsing is always enabled; publish it as such regardless of the
	// stored zero value so UpstreamShareActor / subscribers see a consistent flag.
	r := p.shareRestrictions
	r.AllowFileBrowse = true
	_ = p.pub.Send(msg.T("pane", p.id, "restrictions"), &msg.MsgShareRestrictionsUpdated{
		PaneID:       p.id,
		Restrictions: r,
	})
}

// showRestrictions formats and publishes the current restrictions via rysh output.
func (p *PaneActor) showRestrictions() {
	var sb strings.Builder
	paneLabel := p.id
	if len(paneLabel) > 8 {
		paneLabel = paneLabel[:8]
	}
	fmt.Fprintf(&sb, "\n[rysh] share restrictions for pane %s:\n", paneLabel)

	if len(p.shareRestrictions.DisabledModes) == 0 {
		sb.WriteString("  disabled modes: none\n")
	} else {
		fmt.Fprintf(&sb, "  disabled modes: %s\n", strings.Join(p.shareRestrictions.DisabledModes, ", "))
	}

	switch {
	case len(p.shareRestrictions.ShellAllowList) > 0:
		fmt.Fprintf(&sb, "  shell: allow-list [%s]\n", strings.Join(p.shareRestrictions.ShellAllowList, ", "))
	case len(p.shareRestrictions.ShellForbidList) > 0:
		fmt.Fprintf(&sb, "  shell: forbid-list [%s]\n", strings.Join(p.shareRestrictions.ShellForbidList, ", "))
	default:
		sb.WriteString("  shell: unrestricted\n")
	}

	// File browsing is always enabled for an active share.
	fmt.Fprintf(&sb, "  file browse: enabled (allow-absolute: %v)\n", p.shareRestrictions.AllowAbsolute)
	_ = p.pub.SendPaneRyshOutput(p.id, sb.String())
}
