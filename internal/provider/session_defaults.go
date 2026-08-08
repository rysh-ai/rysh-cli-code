package provider

import (
	"context"
	"sync/atomic"

	sharedprovider "github.com/rysh-ai/rysh-cli-shared/provider"
)

// SessionDefaults is the runtime-mutable session-wide model/effort default
// behind the ##llm command. One instance lives on the agentic Setup; the
// session provider decorator (WithSessionDefaults) consults it PER CALL, so
// a ##llm switch takes effect on the next request of every already-running
// pane/agent/humanoid — no restart, no new actors.
//
// Precedence stays: explicit seats (recipe/config step & judge models, which
// arrive via WithModelEffort) > this session default > the provider's
// configured model (rysh.config.yaml provider.model > built-in).
type SessionDefaults struct {
	v atomic.Pointer[sessionSeat]
}

// sessionSeat is the whole session selection in ONE atomic value: the model,
// the effort, and — for a cross-family switch — the provider that serves them.
// Keeping the provider in the same seat is what stops a reader pairing the
// model from one ##llm switch with the provider from another.
type sessionSeat struct {
	model, effort string
	// prov is set only when the session switched to a DIFFERENT provider
	// family than the configured one; nil means "keep the configured
	// provider and just re-pin its model".
	prov AgenticProvider
}

// NewSessionDefaults returns an empty holder (no session override).
func NewSessionDefaults() *SessionDefaults { return &SessionDefaults{} }

// Set installs the session default on the CONFIGURED provider. Empty strings
// clear the respective field; Set("", "") clears the override entirely,
// including any cross-family provider a previous SetProvider installed.
func (s *SessionDefaults) Set(model, effort string) {
	s.SetProvider(model, effort, nil)
}

// SetProvider installs the session default AND the provider that serves it —
// the cross-family case, where `##llm use openai/sol` on an anthropic session
// must change which client the request goes to, not merely which model id
// rides on the existing one. A nil prov keeps the configured provider.
func (s *SessionDefaults) SetProvider(model, effort string, prov AgenticProvider) {
	if model == "" && effort == "" && prov == nil {
		s.v.Store(nil)
		return
	}
	s.v.Store(&sessionSeat{model: model, effort: effort, prov: prov})
}

// Get returns the current session default ("" when unset).
func (s *SessionDefaults) Get() (model, effort string) {
	if seat := s.v.Load(); seat != nil {
		return seat.model, seat.effort
	}
	return "", ""
}

// Provider returns the session's cross-family provider, or nil when the
// session runs on the configured one.
func (s *SessionDefaults) Provider() AgenticProvider {
	if seat := s.v.Load(); seat != nil {
		return seat.prov
	}
	return nil
}

// WithSessionDefaults wraps an agentic provider so every call resolves the
// session default at request time. Identity when the provider cannot override
// models (the static mock) or defaults is nil.
func WithSessionDefaults(inner AgenticProvider, defaults *SessionDefaults) AgenticProvider {
	mo, ok := inner.(sharedprovider.ModelEffortOverridable)
	if !ok || defaults == nil {
		return inner
	}
	return &sessionAgenticProvider{inner: inner, overridable: mo, defaults: defaults}
}

// sessionAgenticProvider is the dynamic decorator. It implements the same
// capability surface the executor and orchestrator type-assert against:
// AgenticProvider, StreamingProvider, ModelEffortOverridable, and
// MaxTokensOverridable.
type sessionAgenticProvider struct {
	inner       AgenticProvider
	overridable sharedprovider.ModelEffortOverridable
	defaults    *SessionDefaults
}

// active returns the provider the session currently runs on — the cross-family
// one when ##llm switched families, else the configured provider this
// decorator wraps.
func (p *sessionAgenticProvider) active() AgenticProvider {
	if prov := p.defaults.Provider(); prov != nil {
		return prov
	}
	return p.inner
}

// resolved returns the provider to use for one call: the active provider with
// the session's model/effort applied when it can take them.
//
// The override is applied to the ACTIVE provider, not to the configured one.
// Applying it to p.overridable unconditionally is what would send an OpenAI
// model id down the Anthropic client on a cross-family switch.
func (p *sessionAgenticProvider) resolved() AgenticProvider {
	model, effort := p.defaults.Get()
	target := p.active()
	if model == "" && effort == "" {
		return target
	}
	if mo, ok := target.(sharedprovider.ModelEffortOverridable); ok {
		return mo.WithModelEffort(model, effort)
	}
	return target
}

// Name reports the ACTIVE provider's name, so attribution, usage accounting
// and the status line follow a cross-family switch instead of still claiming
// the configured provider.
func (p *sessionAgenticProvider) Name() string { return p.active().Name() }

// Unwrap exposes the active provider so capability detection (Caps) sees the
// real provider rather than this decorator's own type. Without it the wrapper
// would mask tool/streaming support behind itself — and after a cross-family
// switch it would report the capabilities of the provider no longer serving
// the session.
func (p *sessionAgenticProvider) Unwrap() Provider { return p.active() }

func (p *sessionAgenticProvider) Complete(ctx context.Context, prompt string) (string, error) {
	return p.resolved().Complete(ctx, prompt)
}

func (p *sessionAgenticProvider) CompleteWithTools(
	ctx context.Context,
	conversation []sharedprovider.ConversationTurn,
	tools []sharedprovider.ToolSpec,
	systemPrompt string,
) (*sharedprovider.AgenticResponse, error) {
	return p.resolved().CompleteWithTools(ctx, conversation, tools, systemPrompt)
}

// CompleteWithToolsStream satisfies sharedprovider.StreamingProvider. When the
// resolved provider cannot stream (never the case for the Claude provider),
// it degrades to the non-streaming call.
func (p *sessionAgenticProvider) CompleteWithToolsStream(
	ctx context.Context,
	conversation []sharedprovider.ConversationTurn,
	tools []sharedprovider.ToolSpec,
	systemPrompt string,
	cb sharedprovider.StreamCallback,
) (*sharedprovider.AgenticResponse, error) {
	r := p.resolved()
	if streamer, ok := r.(sharedprovider.StreamingProvider); ok {
		return streamer.CompleteWithToolsStream(ctx, conversation, tools, systemPrompt, cb)
	}
	return r.CompleteWithTools(ctx, conversation, tools, systemPrompt)
}

// WithMaxTokens satisfies sharedprovider.MaxTokensOverridable — the seam
// behind ChatRequest.MaxTokens. Mirroring the secretnat wrapper, the cap is
// forwarded to the wrapped provider and the result is RE-WRAPPED with the
// same defaults holder, so a ##llm session default keeps applying to the
// capped variant. When the wrapped provider lacks the seam, nil is returned
// per the MaxTokensOverridable contract so the ChatProvider path errors
// loudly instead of silently dropping the requested cap.
func (p *sessionAgenticProvider) WithMaxTokens(maxTokens int) AgenticProvider {
	mt, ok := p.inner.(sharedprovider.MaxTokensOverridable)
	if !ok {
		return nil
	}
	capped := mt.WithMaxTokens(maxTokens)
	if capped == nil {
		return nil
	}
	// Re-wrap with the same holder: the capped variant must keep resolving the
	// session seat per call, including a cross-family provider, which the
	// re-wrap preserves because the holder — not this wrapper — owns it.
	return WithSessionDefaults(capped, p.defaults)
}

// WithModelEffort satisfies sharedprovider.ModelEffortOverridable — the seam
// explicit seats (recipe do/step model, finalizer, judge) come through.
// Explicit fields win; empty fields fall back to the session default, so a
// recipe that only pins the model still inherits a ##llm session effort.
func (p *sessionAgenticProvider) WithModelEffort(model, effort string) AgenticProvider {
	sm, se := p.defaults.Get()
	if model == "" {
		model = sm
	}
	if effort == "" {
		effort = se
	}
	// Route through the ACTIVE provider: an explicit seat that pins only the
	// effort must still land on the family the session switched to.
	if mo, ok := p.active().(sharedprovider.ModelEffortOverridable); ok {
		return mo.WithModelEffort(model, effort)
	}
	return p.overridable.WithModelEffort(model, effort)
}
