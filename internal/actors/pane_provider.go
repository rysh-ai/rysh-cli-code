package actors

// pane_provider.go — the pane side of `##pane provider` (design 002 §3.4).
//
// The workspace command validates the name and sends MsgPaneSetProvider; this
// file applies it: build the override provider, swap it into the pane's
// PaneOverride holder (consulted per call by the executor's decorator, so it
// takes effect on the pane's NEXT agentic prompt — no executor respawn), and
// mark the pane dirty so the selection persists in the pane's KV record and
// survives detach/attach. This differs deliberately from `##humanoid
// provider`, which is in-memory only and waits for the next executor spawn.

import (
	"log/slog"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

// handleSetProviderOverride applies (or clears — empty Provider) the runtime
// provider override. Runs on the actor goroutine.
func (p *PaneActor) handleSetProviderOverride(m *msg.MsgPaneSetProvider) {
	name := strings.ToLower(strings.TrimSpace(m.Provider))
	if name == "" || name == "default" {
		p.providerOverride = ""
		p.providerOverrideModel = ""
		if p.providerHolder != nil {
			p.providerHolder.Set(nil)
		}
		if p.agSetup != nil && p.agSetup.Provider != nil {
			p.providerName = p.agSetup.Provider.Name()
		}
		p.kvDirty = true
		slog.Info("pane: provider override cleared", "pane", p.id, "provider", p.providerName)
		return
	}
	if !provider.IsKnownProviderName(name) {
		// The workspace command already rejects unknown names; this guards the
		// raw NATS path so a bad message cannot pin the pane to the Claude
		// default branch under a misleading name.
		slog.Warn("pane: ignoring unknown provider override", "pane", p.id, "provider", name)
		return
	}
	p.providerOverride = name
	p.providerOverrideModel = strings.TrimSpace(m.Model)
	p.installProviderOverride()
	p.kvDirty = true
}

// installProviderOverride builds the override provider and swaps it into the
// holder. Key rules mirror the humanoid's setupForProvider: a cross-family
// selection gets the family's conventional env key and the family's own
// default endpoint/model — cfg.APIKey belongs to the config provider and must
// never leak into another provider's requests. A same-family selection keeps
// the config credentials and only re-pins the model.
func (p *PaneActor) installProviderOverride() {
	if p.providerHolder == nil || p.providerOverride == "" {
		return
	}
	cfg := p.cfg
	family := providerFamily(p.providerOverride)
	if family != providerFamily(cfg.ProviderName) {
		cfg.ProviderName = family
		cfg.APIURL = ""
		cfg.APIKey = providerKeyFromEnv(family)
		cfg.DefaultModel = ""
	}
	if p.providerOverrideModel != "" {
		cfg.DefaultModel = p.providerOverrideModel
	}
	prov := provider.NewAgenticProvider(cfg)
	p.providerHolder.Set(prov)
	p.providerName = prov.Name()
	slog.Info("pane: provider override installed",
		"pane", p.id, "provider", prov.Name(), "model", cfg.DefaultModel)
}
