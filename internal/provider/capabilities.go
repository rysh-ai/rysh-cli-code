package provider

// capabilities.go — the provider capability table (roadmap design 002 §3.3,
// openclaw_roadmap R3).
//
// Before this, capability was discovered ONLY by Go interface type-assertion
// (StreamingProvider, ModelEffortOverridable) scattered across call sites, and
// there was no way to ask "can this provider call tools?" at all. That gap made
// design 006 §4.4 unimplementable: a humanoid declaring `governance: human` on
// a provider without tool-calling would register draft-and-confirm tools that
// the model could never invoke, and then silently answer as if drafting had
// happened.
//
// The honest signal is structural, not a hardcoded name list: a provider can
// call tools exactly when it implements CompleteWithTools (the AgenticProvider
// interface). ClaudeCLI, ClaudeAPI and StaticProvider are Complete-only, so
// they report Tools=false and the degradation path fires. A provider that knows
// better about itself can override the defaults by implementing Reporter.

import (
	sharedprovider "github.com/rysh-ai/rysh-cli-shared/provider"
)

// Capabilities describes what a provider can do. Fields follow design 002 §3.3.
// Zero value = "knows nothing", which is the safe reading everywhere it is
// consulted: no tools, no streaming, no declared limits.
type Capabilities struct {
	Tools         bool // can accept tool specs and emit tool calls
	ParallelTools bool // can emit multiple tool calls in one turn
	Vision        bool // accepts image content blocks
	Streaming     bool // supports incremental delivery

	MaxContextTokens int
	MaxOutputTokens  int

	ToolChoiceModes []string // e.g. auto, any, none
}

// Reporter is the optional interface a provider implements when it wants to
// declare its own capabilities instead of accepting the structural defaults.
type Reporter interface {
	Caps() Capabilities
}

// unwrapper lets a decorator expose the provider it wraps so capability
// detection sees through it. sessionAgenticProvider implements this: without
// it, WithSessionDefaults would mask the real provider's capabilities behind
// the wrapper's own type.
type unwrapper interface {
	Unwrap() Provider
}

// Caps reports what p can do.
//
// Resolution order:
//  1. p declares its own capabilities (Reporter) — believed verbatim.
//  2. p is a decorator (unwrapper) — recurse into what it wraps.
//  3. structural: tool support iff p implements CompleteWithTools, plus
//     streaming iff it implements CompleteWithToolsStream (the legacy
//     StreamingProvider seam) OR it is a ChatProvider whose
//     ChatStreamSupported self-report says it streams — so
//     ChatProvider-native implementations (design 002 A2) are recognized
//     without having to also carry the legacy interface.
//
// A nil provider reports the zero value, so callers never need a nil check
// before asking.
func Caps(p Provider) Capabilities {
	if p == nil {
		return Capabilities{}
	}
	if r, ok := p.(Reporter); ok {
		return r.Caps()
	}
	if u, ok := p.(unwrapper); ok {
		if inner := u.Unwrap(); inner != nil {
			return Caps(inner)
		}
	}

	var c Capabilities
	if _, ok := p.(AgenticProvider); ok {
		// Structurally able to take tool specs and return tool calls. All three
		// shipped agentic providers (Claude, OpenAI-compatible incl. Ollama,
		// and the agentic mock) also emit parallel calls and accept the
		// standard tool-choice modes.
		c.Tools = true
		c.ParallelTools = true
		c.ToolChoiceModes = []string{"auto", "any", "none"}
	}
	if _, ok := p.(sharedprovider.StreamingProvider); ok {
		c.Streaming = true
	} else if cp, ok := p.(sharedprovider.ChatProvider); ok && sharedprovider.ChatStreamSupported(cp) {
		// ChatProvider-native streaming (no legacy StreamingProvider seam).
		// This runs on the already-unwrapped provider, so decorators are
		// seen through exactly as the legacy type-assert is.
		c.Streaming = true
	}
	return c
}

// SupportsTools is the question almost every caller actually has.
func SupportsTools(p Provider) bool { return Caps(p).Tools }
