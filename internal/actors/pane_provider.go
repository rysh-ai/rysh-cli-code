// SPDX-License-Identifier: Apache-2.0

package actors

// pane_provider.go — the pane side of `##pane provider` (design 002 §3.4) and
// of the model hierarchy (session > workspace > tab > lane > stack > pane; see
// model_scope.go).
//
// The workspace command validates the name and sends MsgPaneSetProvider; this
// file applies it: build the override provider, swap it into the pane's
// PaneOverride holder (consulted per call by the executor's decorator, so it
// takes effect on the pane's NEXT agentic prompt — no executor respawn), and
// mark the pane dirty so the selection persists in the pane's KV record and
// survives detach/attach. This differs deliberately from `##humanoid
// provider`, which is in-memory only and waits for the next executor spawn.
//
// Two slots, not one. A message carrying Scope "" or "pane" is the pane's OWN
// selection: persisted, and outranking everything above. Any broader Scope is
// INHERITED from the nearest enclosing bound scope: applied live but never
// persisted, since the WorkspaceActor recomputes it on every re-bind. The pane
// runs on `own ?? inherited`, so a lane- or stack-wide switch reaches every
// pane that has not chosen for itself and leaves the ones that have alone.

import (
	"log/slog"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

// handleSetProviderOverride applies (or clears — empty Provider) a runtime
// provider/model selection at one scope. Runs on the actor goroutine.
func (p *PaneActor) handleSetProviderOverride(m *msg.MsgPaneSetProvider) {
	name := strings.ToLower(strings.TrimSpace(m.Provider))
	inherited := isInheritedScope(m.Scope)

	if name == "" || name == "default" {
		if inherited {
			p.inheritedProvider, p.inheritedModel, p.inheritedScope = "", "", ""
		} else {
			p.providerOverride, p.providerOverrideModel = "", ""
			p.kvDirty = true
		}
		p.applyEffectiveProvider()
		slog.Info("pane: provider override cleared",
			"pane", p.id, "scope", modelScopeLabel(m.Scope), "provider", p.providerName)
		return
	}
	if !provider.IsKnownProviderName(name) {
		// The workspace command already rejects unknown names; this guards the
		// raw NATS path so a bad message cannot pin the pane to the Claude
		// default branch under a misleading name.
		slog.Warn("pane: ignoring unknown provider override", "pane", p.id, "provider", name)
		return
	}
	if inherited {
		p.inheritedProvider = name
		p.inheritedModel = strings.TrimSpace(m.Model)
		p.inheritedScope = m.Scope
	} else {
		p.providerOverride = name
		p.providerOverrideModel = strings.TrimSpace(m.Model)
		p.kvDirty = true
	}
	p.applyEffectiveProvider()
}

// isInheritedScope reports whether a MsgPaneSetProvider targets the inherited
// slot. "" and "pane" are the pane's own selection (design note: "" keeps
// every pre-existing sender — `##pane provider`, the raw NATS path — writing
// the pane's own slot exactly as before).
func isInheritedScope(scope string) bool {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "", string(scopePane):
		return false
	default:
		return true
	}
}

func modelScopeLabel(scope string) string {
	if s := strings.TrimSpace(scope); s != "" {
		return s
	}
	return string(scopePane)
}

// effectiveProviderSelection returns the provider/model pair serving this
// pane, and the scope it came from. The pane's own selection wins; an
// inherited one applies only in its absence.
func (p *PaneActor) effectiveProviderSelection() (providerName, model, scope string) {
	if p.providerOverride != "" {
		return p.providerOverride, p.providerOverrideModel, string(scopePane)
	}
	if p.inheritedProvider != "" {
		return p.inheritedProvider, p.inheritedModel, modelScopeLabel(p.inheritedScope)
	}
	return "", "", ""
}

// applyEffectiveProvider recomputes the holder from the two slots: it installs
// the winning selection, or clears the holder when neither slot binds one, in
// which case the pane falls through to the session provider (and so to any
// ##llm session default, applied by the session decorator per call).
func (p *PaneActor) applyEffectiveProvider() {
	name, model, _ := p.effectiveProviderSelection()
	if name == "" {
		if p.providerHolder != nil {
			p.providerHolder.Set(nil)
		}
		if p.agSetup != nil && p.agSetup.Provider != nil {
			p.providerName = p.agSetup.Provider.Name()
		}
		return
	}
	p.installProviderOverride(name, model)
}

// providerOverrideConfig is the config a pane override is constructed from.
// Key rules mirror the humanoid's setupForProvider: a cross-family selection
// gets the family's OWN key — resolved through the workspace's ##secret store
// for this pane's tab, then the environment — and the family's own default
// endpoint/model, because cfg.APIKey belongs to the config provider and must
// never leak into another provider's requests. A same-family selection keeps
// the config credentials and only re-pins the model.
//
// Split out from installProviderOverride because the resolved key is not
// readable back off a constructed provider, and which key lands here is exactly
// what needs pinning down.
func (p *PaneActor) providerOverrideConfig(name, model string) config.Config {
	cfg := p.cfg
	family := providerFamily(name)
	if family != providerFamily(cfg.ProviderName) {
		cfg.ProviderName = family
		cfg.APIURL = ""
		cfg.APIKey = p.secrets.providerAPIKey(p.tabID, family)
		cfg.DefaultModel = ""
	}
	if model != "" {
		cfg.DefaultModel = model
	}
	return cfg
}

// installProviderOverride builds the override provider and swaps it into the
// holder.
func (p *PaneActor) installProviderOverride(name, model string) {
	if p.providerHolder == nil || name == "" {
		return
	}
	cfg := p.providerOverrideConfig(name, model)
	prov := provider.NewAgenticProvider(cfg)
	p.providerHolder.Set(prov)
	p.providerName = prov.Name()
	slog.Info("pane: provider override installed",
		"pane", p.id, "provider", prov.Name(), "model", cfg.DefaultModel)
}
