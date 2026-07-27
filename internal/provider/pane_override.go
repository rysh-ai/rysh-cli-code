package provider

// pane_override.go — the `##pane provider` seam (design 002 §3.4).
//
// A pane's executor (LLMPromptExecutionActor) binds one AgenticProvider at
// spawn, so switching provider by respawning would drop executor state and
// delay the change. Instead — exactly like the ##llm session default — the
// switch is a decorator consulted PER CALL: the PaneActor installs or clears
// the override in a PaneOverride holder, and the pane's very next agentic
// prompt uses it. No respawn.
//
// Precedence (design 002 §3.4): the explicit per-pane selection beats the base
// provider INCLUDING its ##llm session-default wrapper (explicit beats
// session-wide), and loses only to skill-frontmatter seats, which never route
// through a pane's provider.

import (
	"context"
	"fmt"
	"sync/atomic"

	sharedprovider "github.com/rysh-ai/rysh-cli-shared/provider"
)

// PaneOverride holds one pane's runtime provider override. Set/Get are
// atomic: Set runs on the pane's actor goroutine, Get on the executor's.
type PaneOverride struct {
	v atomic.Pointer[paneOverrideSlot]
}

// paneOverrideSlot boxes the interface value (atomic.Pointer needs a concrete
// type).
type paneOverrideSlot struct{ p AgenticProvider }

// NewPaneOverride returns an empty holder (no override — base provider rules).
func NewPaneOverride() *PaneOverride { return &PaneOverride{} }

// Set installs the override provider; nil clears it.
func (o *PaneOverride) Set(p AgenticProvider) {
	if p == nil {
		o.v.Store(nil)
		return
	}
	o.v.Store(&paneOverrideSlot{p: p})
}

// Get returns the current override, or nil when none is set.
func (o *PaneOverride) Get() AgenticProvider {
	if s := o.v.Load(); s != nil {
		return s.p
	}
	return nil
}

// WithPaneOverride wraps base so every call resolves the pane override first.
// Identity when the holder is nil.
func WithPaneOverride(base AgenticProvider, o *PaneOverride) AgenticProvider {
	if o == nil {
		return base
	}
	return &paneAgenticProvider{base: base, override: o}
}

// paneAgenticProvider is the dynamic decorator. Like sessionAgenticProvider it
// implements the full surface the executor type-asserts against:
// AgenticProvider, StreamingProvider, ModelEffortOverridable,
// MaxTokensOverridable — plus Unwrap for the capability table.
type paneAgenticProvider struct {
	base     AgenticProvider
	override *PaneOverride
	// maxTokens > 0 is a per-request output cap installed via WithMaxTokens.
	// It is applied to the EFFECTIVE provider at call time, so the pane
	// override keeps governing routing even on a capped variant.
	maxTokens int
}

// effective returns the provider serving the next call, applying the
// per-request max-tokens cap (when one is set) to whichever provider the
// override holder resolves to right now.
func (p *paneAgenticProvider) effective() AgenticProvider {
	eff := p.base
	if ov := p.override.Get(); ov != nil {
		eff = ov
	}
	if p.maxTokens <= 0 {
		return eff
	}
	if mt, ok := eff.(sharedprovider.MaxTokensOverridable); ok {
		if capped := mt.WithMaxTokens(p.maxTokens); capped != nil {
			return capped
		}
	}
	// The effective provider changed to one without the seam AFTER
	// WithMaxTokens validated the cap (override swapped between request
	// admission and the call). Fail the call loudly rather than silently
	// sending it uncapped.
	return &uncappableAgentic{name: eff.Name(), maxTokens: p.maxTokens}
}

func (p *paneAgenticProvider) Name() string { return p.effective().Name() }

// Unwrap exposes the EFFECTIVE provider so Caps() reflects what will actually
// serve the next prompt — a claude-cli override must read Tools=false even
// while the base provider supports tools, and vice versa after clearing.
func (p *paneAgenticProvider) Unwrap() Provider { return p.effective() }

func (p *paneAgenticProvider) Complete(ctx context.Context, prompt string) (string, error) {
	return p.effective().Complete(ctx, prompt)
}

func (p *paneAgenticProvider) CompleteWithTools(
	ctx context.Context,
	conversation []sharedprovider.ConversationTurn,
	tools []sharedprovider.ToolSpec,
	systemPrompt string,
) (*sharedprovider.AgenticResponse, error) {
	return p.effective().CompleteWithTools(ctx, conversation, tools, systemPrompt)
}

// CompleteWithToolsStream satisfies sharedprovider.StreamingProvider,
// degrading to the non-streaming call when the effective provider cannot
// stream — now only the claude-cli adapter, since the OpenAI-compatible
// provider gained streaming (design 002, rysh-shared openai_streaming.go).
func (p *paneAgenticProvider) CompleteWithToolsStream(
	ctx context.Context,
	conversation []sharedprovider.ConversationTurn,
	tools []sharedprovider.ToolSpec,
	systemPrompt string,
	cb sharedprovider.StreamCallback,
) (*sharedprovider.AgenticResponse, error) {
	eff := p.effective()
	if streamer, ok := eff.(sharedprovider.StreamingProvider); ok {
		return streamer.CompleteWithToolsStream(ctx, conversation, tools, systemPrompt, cb)
	}
	return eff.CompleteWithTools(ctx, conversation, tools, systemPrompt)
}

// WithModelEffort satisfies sharedprovider.ModelEffortOverridable — explicit
// seats (recipe/judge models) resolve against the provider active at seat
// time. A non-overridable effective provider (claude-cli) ignores the seat,
// matching the executor's own fallback for such providers.
func (p *paneAgenticProvider) WithModelEffort(model, effort string) AgenticProvider {
	eff := p.effective()
	if mo, ok := eff.(sharedprovider.ModelEffortOverridable); ok {
		return mo.WithModelEffort(model, effort)
	}
	return eff
}

// WithMaxTokens satisfies sharedprovider.MaxTokensOverridable — the seam
// behind ChatRequest.MaxTokens. Mirroring the secretnat wrapper, the cap is
// forwarded to the wrapped provider and the result stays INSIDE this
// decorator (the cap is stored and applied per call by effective()), so the
// pane override keeps governing routing on the capped variant. When the
// effective provider lacks the seam, nil is returned per the
// MaxTokensOverridable contract so the ChatProvider path errors loudly
// instead of silently dropping the requested cap.
func (p *paneAgenticProvider) WithMaxTokens(maxTokens int) AgenticProvider {
	mt, ok := p.effective().(sharedprovider.MaxTokensOverridable)
	if !ok {
		return nil
	}
	if mt.WithMaxTokens(maxTokens) == nil {
		return nil
	}
	if maxTokens <= 0 {
		// <= 0 keeps the receiver's cap, matching the interface contract.
		maxTokens = p.maxTokens
	}
	return &paneAgenticProvider{base: p.base, override: p.override, maxTokens: maxTokens}
}

// uncappableAgentic is the loud-failure fallback for the race where the pane
// override is swapped to a seam-less provider after WithMaxTokens validated
// the cap: every call errors instead of silently going out uncapped.
type uncappableAgentic struct {
	name      string
	maxTokens int
}

func (u *uncappableAgentic) Name() string { return u.name }

func (u *uncappableAgentic) err() error {
	return fmt.Errorf(
		"provider %q cannot honor the per-request max_tokens cap %d (pane override changed to a provider without the seam)",
		u.name, u.maxTokens)
}

func (u *uncappableAgentic) Complete(context.Context, string) (string, error) {
	return "", u.err()
}

func (u *uncappableAgentic) CompleteWithTools(
	context.Context, []sharedprovider.ConversationTurn, []sharedprovider.ToolSpec, string,
) (*sharedprovider.AgenticResponse, error) {
	return nil, u.err()
}
