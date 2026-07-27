package actors

// workspace_pane_provider.go — `##pane provider [name [model]]` (design 002
// §3.4 runtime selection), symmetric with `##humanoid provider` but pane-side
// persistent: the selection is stored in the pane's KV record and applies to
// the pane's next agentic prompt without an executor respawn.

import (
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

// cmdPaneProvider handles the command and returns the message to send to the
// active pane's inbox, or nil when nothing should be sent (show / usage /
// validation paths). The caller owns the publish, so this handler stays
// unit-testable without NATS.
func (w *WorkspaceActor) cmdPaneProvider(out *strings.Builder, paneID string, args []string) *msg.MsgPaneSetProvider {
	if paneID == "" {
		fmt.Fprintf(out, "\n[rysh] no active pane\n")
		return nil
	}
	if len(args) == 0 {
		fmt.Fprint(out, paneProviderStatus(w.findPaneSnapshot(paneID), w.cfg.ProviderName))
		return nil
	}
	name := strings.ToLower(strings.TrimSpace(args[0]))
	switch name {
	case "default", "clear", "reset":
		fmt.Fprintf(out, "\n[rysh] pane provider override cleared — back to the session provider (%s)\n",
			providerFamily(w.cfg.ProviderName))
		fmt.Fprintf(out, "[rysh] applies to the next agentic prompt in this pane\n")
		return &msg.MsgPaneSetProvider{}
	}
	if !provider.IsKnownProviderName(name) {
		fmt.Fprintf(out, "\n[rysh] unknown provider %q — valid: %s (or \"default\" to clear)\n",
			name, strings.Join(provider.KnownProviderNames(), ", "))
		return nil
	}
	model := ""
	if len(args) > 1 {
		model = args[1]
	}
	fmt.Fprintf(out, "\n[rysh] pane provider set to %q", name)
	if model != "" {
		fmt.Fprintf(out, " (model %s)", model)
	}
	fmt.Fprintf(out, "\n[rysh] applies to the next agentic prompt in this pane (no respawn needed); persisted with the pane across detach/attach\n")
	// The selection would 401 without credentials; say so now rather than on
	// the next prompt. Never echo the key itself — presence only.
	family := providerFamily(name)
	if provider.RequiresAPIKey(family) &&
		family != providerFamily(w.cfg.ProviderName) &&
		providerKeyFromEnv(family) == "" {
		fmt.Fprintf(out, "[rysh] warning: no API key found for %s (set %s in the daemon environment) — prompts will fail until it is set\n",
			name, providerKeyEnvHint(family))
	}
	return &msg.MsgPaneSetProvider{Provider: name, Model: model}
}

// paneProviderStatus renders the no-args view: the provider that will serve
// the pane's next agentic prompt, plus the override/session split.
func paneProviderStatus(snap *domain.PaneSnapshot, sessionProvider string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n[rysh] pane provider\n")
	if snap == nil {
		fmt.Fprintf(&b, "  effective : %s (session provider; pane state unavailable)\n",
			providerFamily(sessionProvider))
		return b.String()
	}
	effective := snap.ProviderName
	if effective == "" {
		effective = providerFamily(sessionProvider)
	}
	fmt.Fprintf(&b, "  effective : %s\n", effective)
	if snap.ProviderOverride != "" {
		fmt.Fprintf(&b, "  override  : %s", snap.ProviderOverride)
		if snap.ProviderOverrideModel != "" {
			fmt.Fprintf(&b, " (model %s)", snap.ProviderOverrideModel)
		}
		fmt.Fprintf(&b, "\n  session   : %s (restore with ##pane provider default)\n",
			providerFamily(sessionProvider))
	} else {
		fmt.Fprintf(&b, "  override  : none (set with ##pane provider <name> [model])\n")
	}
	return b.String()
}

// providerKeyEnvHint names the conventional key variable(s) for a provider
// family — the counterpart of providerKeyFromEnv, for error/warning text.
func providerKeyEnvHint(family string) string {
	switch family {
	case "openai":
		return "OPENAI_API_KEY"
	case "gemini":
		return "GEMINI_API_KEY (or GOOGLE_API_KEY)"
	default:
		return "ANTHROPIC_API_KEY"
	}
}

// findPaneSnapshot fetches the live snapshot of one pane in the active tab
// (the ##pane info lookup, extracted). Nil when the tab or pane cannot be
// resolved.
func (w *WorkspaceActor) findPaneSnapshot(paneID string) *domain.PaneSnapshot {
	tab := w.currentTab()
	if tab == nil {
		return nil
	}
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		return nil
	}
	for _, lane := range tabSnap.Lanes {
		for _, g := range lane.PaneGroups {
			for i := range g.Panes {
				if g.Panes[i].ID == paneID {
					return &g.Panes[i]
				}
			}
		}
	}
	return nil
}
