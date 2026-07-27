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

type sessionSeat struct{ model, effort string }

// NewSessionDefaults returns an empty holder (no session override).
func NewSessionDefaults() *SessionDefaults { return &SessionDefaults{} }

// Set installs the session default. Empty strings clear the respective field;
// Set("", "") clears the override entirely.
func (s *SessionDefaults) Set(model, effort string) {
	if model == "" && effort == "" {
		s.v.Store(nil)
		return
	}
	s.v.Store(&sessionSeat{model: model, effort: effort})
}

// Get returns the current session default ("" when unset).
func (s *SessionDefaults) Get() (model, effort string) {
	if seat := s.v.Load(); seat != nil {
		return seat.model, seat.effort
	}
	return "", ""
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

// resolved returns the provider to use for one call: the inner provider when
// no session default is set, otherwise a per-call model/effort variant.
func (p *sessionAgenticProvider) resolved() AgenticProvider {
	model, effort := p.defaults.Get()
	if model == "" && effort == "" {
		return p.inner
	}
	return p.overridable.WithModelEffort(model, effort)
}

func (p *sessionAgenticProvider) Name() string { return p.inner.Name() }

// Unwrap exposes the wrapped provider so capability detection (Caps) sees the
// real provider rather than this decorator's own type. Without it the wrapper
// would mask tool/streaming support behind itself.
func (p *sessionAgenticProvider) Unwrap() Provider { return p.inner }

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
	return p.overridable.WithModelEffort(model, effort)
}
