package provider

import (
	"strings"

	sharedprovider "github.com/rysh-ai/rysh-cli-shared/provider"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// Type aliases — these ARE the rysh-shared/provider types.
// ClaudeAgenticProvider satisfies AgenticProvider via structural typing.
type AgenticProvider = sharedprovider.AgenticProvider
type ClaudeAgenticProvider = sharedprovider.ClaudeAgenticProvider
type ConversationTurn = sharedprovider.ConversationTurn
type ToolSpec = sharedprovider.ToolSpec
type ToolCallRequest = sharedprovider.ToolCallRequest
type TextBlock = sharedprovider.TextBlock
type AgenticResponse = sharedprovider.AgenticResponse
type StopReason = sharedprovider.StopReason
type Usage = sharedprovider.Usage

// StopReason constants re-declared (Go constants cannot be type-aliased).
const (
	StopReasonEndTurn   StopReason = sharedprovider.StopReasonEndTurn
	StopReasonToolUse   StopReason = sharedprovider.StopReasonToolUse
	StopReasonMaxTokens StopReason = sharedprovider.StopReasonMaxTokens
	StopReasonStopSeq   StopReason = sharedprovider.StopReasonStopSeq
)

// NewClaudeAgenticProvider creates a provider from CLI config, adapting the
// config struct to the shared provider's constructor signature.
func NewClaudeAgenticProvider(cfg config.Config) *ClaudeAgenticProvider {
	apiURL := cfg.APIURL
	if apiURL == "" {
		apiURL = config.DefaultAnthropicAPIURL
	}
	model := cfg.DefaultModel
	if model == "" {
		// Track the engine's default rather than keeping a local copy of the
		// id: rysh-server's copy of this same string silently rotted two
		// releases behind before anyone noticed.
		model = sharedprovider.DefaultClaudeModel
	}
	// maxTokens <= 0 lets the shared constructor pick the per-model default
	// (DefaultMaxTokensForModel), so we don't hard-code a budget here.
	p := sharedprovider.NewClaudeAgenticProvider(cfg.APIKey, apiURL, model, cfg.MaxTokens)
	// Extended thinking: adaptive mode (Claude 4.6+). Assistant turns record
	// their thinking blocks and the provider replays them, so tool-use loops
	// keep working with thinking enabled.
	if cfg.Thinking {
		p.SetThinking(sharedprovider.AdaptiveThinkingConfig())
	}
	return p
}

// GeminiAPIURL is Gemini's OpenAI-compatible Chat Completions surface. It
// speaks the same dialect (Bearer auth, /chat/completions, function tools) as
// OpenAI and Ollama, so the one shared adapter covers all three (design 002
// §3.2) and Gemini needs no dedicated HTTP client.
const GeminiAPIURL = "https://generativelanguage.googleapis.com/v1beta/openai"

// geminiDefaultModel is used when neither config nor ##pane provider pins one.
const geminiDefaultModel = "gemini-2.5-flash"

// NewOpenAIAgenticProvider builds an OpenAI-compatible agentic provider from CLI
// config (design 002). Covers OpenAI, local Ollama (air-gapped mode) and Gemini
// (via its OpenAI-compat endpoint); the dialect is translated at the edge so
// the agentic loop stays neutral.
func NewOpenAIAgenticProvider(cfg config.Config) AgenticProvider {
	name, apiURL, model := openAIFamilyDefaults(cfg.ProviderName, cfg.APIURL, cfg.DefaultModel)
	return sharedprovider.NewOpenAIAgenticProvider(name, cfg.APIKey, apiURL, model, cfg.MaxTokens)
}

// openAIFamilyDefaults resolves the (family, endpoint, model) triple for an
// OpenAI-compatible selection, filling in each family's own defaults for
// whatever the config did not choose.
//
// The subtle part is what counts as "did not choose" for the endpoint. Every
// Config starts with Anthropic's host in api_url, because the default provider
// is Claude — so an EMPTY api_url is not the only unset state, and treating the
// inherited Anthropic default as a deliberate override is what sent
// `provider.name: openai` to api.anthropic.com and killed every turn on a bare
// 404. That host answers, so the failure read as a broken model rather than a
// misrouted request. The ##pane / ##humanoid override paths dodge it by
// blanking APIURL before they construct; a config-level selection has no such
// step, which is why the check belongs here.
func openAIFamilyDefaults(providerName, configuredURL, configuredModel string) (name, apiURL, model string) {
	name = strings.ToLower(strings.TrimSpace(providerName))
	apiURL, model = configuredURL, configuredModel
	if apiURL == config.DefaultAnthropicAPIURL {
		apiURL = ""
	}
	switch name {
	case "ollama":
		if apiURL == "" {
			apiURL = "http://127.0.0.1:11434/v1"
		}
		if model == "" {
			model = "llama3.1"
		}
	case "gemini":
		if apiURL == "" {
			apiURL = GeminiAPIURL
		}
		if model == "" {
			model = geminiDefaultModel
		}
	default:
		name = "openai"
		if apiURL == "" {
			apiURL = "https://api.openai.com/v1"
		}
		if model == "" {
			model = "gpt-4o"
		}
	}
	return name, apiURL, model
}

// RequiresAPIKey reports whether the named provider needs an API key before it
// can serve real completions.
//
// Local providers (ollama) talk to a loopback endpoint and are authenticated by
// nothing at all — requiring a key for them is what silently demoted the
// air-gapped path to the mock provider (design 002 §3.5). claude-cli is keyless
// for a different reason: the `claude` binary carries its own login. Callers
// must consult this instead of testing cfg.APIKey directly.
func RequiresAPIKey(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "ollama", "claude-cli":
		return false
	default:
		return true
	}
}

// KnownProviderNames lists the selectable provider names — the canonical
// spellings shown in errors and help (IsKnownProviderName additionally accepts
// the Claude-family aliases). Keep sorted.
func KnownProviderNames() []string {
	return []string{"anthropic", "claude-cli", "gemini", "ollama", "openai"}
}

// IsKnownProviderName reports whether name selects a real provider through
// NewAgenticProvider/New. Unknown names still construct (they fall to the
// Claude default branch), but runtime selection surfaces (##pane provider,
// ##humanoid provider) must reject them so a typo cannot silently pin a pane
// to the wrong provider.
func IsKnownProviderName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "anthropic", "claude", "claude-agentic", "claude-cli", "gemini", "ollama", "openai":
		return true
	}
	return false
}

// NewAgenticProvider selects the agentic provider by cfg.ProviderName:
// openai/ollama/gemini → OpenAI-compatible; claude-cli → the Complete-only CLI
// adapter (Caps().Tools=false); anything else (default) → Claude API.
// This is the single seam the agentic setup uses so a new provider needs no
// orchestrator changes.
func NewAgenticProvider(cfg config.Config) AgenticProvider {
	switch strings.ToLower(strings.TrimSpace(cfg.ProviderName)) {
	case "openai", "ollama", "gemini":
		return NewOpenAIAgenticProvider(cfg)
	case "claude-cli":
		return NewClaudeCLIAgenticProvider(cfg)
	default: // "", "claude", "anthropic", "claude-agentic"
		return NewClaudeAgenticProvider(cfg)
	}
}
