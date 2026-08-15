// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/nats-io/nats.go"

	sharedagentic "github.com/rysh-ai/rysh-cli-shared/agentic"
	"github.com/rysh-ai/rysh-cli-shared/secretnat"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/forge"
	"github.com/rysh-ai/rysh-cli-code/internal/mcp"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
	"github.com/rysh-ai/rysh-cli-code/internal/tools"
)

// Setup holds the initialized agentic components ready to be attached to a pane.
type Setup struct {
	Provider     provider.AgenticProvider
	ToolRegistry *tools.ToolRegistry
	AgenticCfg   config.AgenticConfig
	SystemPrompt string
	// SystemPromptNoEnv is SystemPrompt without the env block. Panes use it as
	// their base prompt and inject a fresh env block per turn (with the live
	// shell cwd) via the actor's env-block provider, instead of the static,
	// daemon-cwd env block baked into SystemPrompt. Falls back to SystemPrompt
	// when empty (e.g. ApplyPrompts was never called, as in some tests).
	SystemPromptNoEnv string
	BgSessions        *tools.BackgroundSessionManager
	// MCP manages external Model Context Protocol servers whose tools are
	// registered into ToolRegistry. Tools registered here (e.g. at startup via
	// BootstrapMCP) are visible to every pane/agent that later clones the
	// registry.
	MCP *mcp.Manager

	// Forge manages generated API integrations (OpenAPI→tool-pack). Like MCP, its
	// tools register into ToolRegistry; BootstrapForge enables persisted
	// integrations at startup so they reach every pane/agent.
	Forge *forge.Manager

	// Scopes owns the per-scope-instance tool registries (tab/lane/group/pane),
	// chained pane→group→lane→tab→global with ToolRegistry as the global root.
	// Per-pane registries are built via Scopes.PaneChain; scoped ##integration /
	// ##mcp enablement registers into Scopes.RegistryFor(scope, ids).
	Scopes *ScopeRegistries

	// Prompts holds the externalised prompt content (loaded from
	// rysh-cli-agent-prompts/ by main.go). Nil = fall back to in-package
	// constants in prompts.go. Set via ApplyPrompts after NewSetup.
	Prompts *Prompts

	// workDir is the working directory at Setup-time; cached so ApplyPrompts
	// and per-pane factories can build env blocks without re-os.Getwd().
	workDir string

	// PromptReloader is installed by main.go at startup. When invoked
	// (e.g. via SIGHUP or the ##agent reload-prompts workspace command),
	// it re-reads the layered prompt store from disk and returns a fresh
	// Prompts to apply. The workspace command handler calls Reload() which
	// uses this. Follow-up 2b.
	PromptReloader func() *Prompts

	// Metrics is the in-process metrics sink shared across the orchestrator
	// hierarchy (one per Setup, threaded into every orchestrator created
	// through this Setup). Follow-up 3b.
	Metrics sharedagentic.MetricsSink

	// SecretNAT is the process-wide SecretNAT / ReSet manager (reversible
	// secret translation between rysh and the LLM provider). One isolated
	// Session per pane/agent/humanoid is bound at actor creation. The
	// WorkspaceActor pushes known secrets (##secret store) into it; the
	// ##snat / ##rst command surface reads and mutates it. Never nil after
	// NewSetup/NewSetupAlwaysOn — disabled state is carried inside.
	SecretNAT *secretnat.Manager

	// SessionLLM is the runtime-mutable session default model/effort behind
	// the ##llm command. Provider is wrapped to consult it per call, so a
	// switch applies to every existing pane/agent on its next request.
	// Explicit seats (recipe/config step & judge models) still win. Never nil
	// after NewSetup/NewSetupAlwaysOn.
	SessionLLM *provider.SessionDefaults
}

// Reload re-runs the installed PromptReloader closure and applies the
// fresh Prompts via ApplyPrompts. Returns the newly-applied Prompts so
// the caller can broadcast MsgReloadPrompts to active actors. Returns
// nil when no reloader is installed (the host hasn't wired up
// SetPromptReloader).
//
// Follow-up 2b.
func (s *Setup) Reload() *Prompts {
	if s == nil || s.PromptReloader == nil {
		return nil
	}
	p := s.PromptReloader()
	s.ApplyPrompts(p, s.workDir)
	return s.Prompts
}

// ApplyPrompts replaces the system prompt with one assembled from the
// externalised prompt files: default → project memory (RYSH.md) → env block
// → todo guidance. It also pushes the sub-agent and compaction prompt
// overrides into the rysh-shared agentic package (where the orchestrator and
// compaction code consume them).
//
// Production main.go calls this once at startup after NewSetup/NewSetupAlwaysOn.
// Tests typically skip it so they exercise the fallback consts.
func (s *Setup) ApplyPrompts(p *Prompts, workDir string) {
	if s == nil {
		return
	}
	if workDir == "" {
		workDir = s.workDir
	}
	s.workDir = workDir
	s.Prompts = p

	// Re-assemble the system prompt from scratch so ApplyPrompts is
	// idempotent: callers can re-apply (e.g. tests, or live reloads) and the
	// result depends only on the current Prompts + workDir.
	base := appendProjectMemory(p.defaultPrompt(), workDir)
	withTodo := func(x string) string {
		if td := p.todoGuidance(); td != "" {
			return x + "\n\n" + td
		}
		return x
	}

	// SystemPrompt keeps the static (Setup-time workDir) env block — used by
	// headless agents/humanoids and any direct consumer.
	full := base
	if env := buildEnvBlock(p.envBlockTemplate(), workDir); env != "" {
		full = full + "\n\n" + env
	}
	s.SystemPrompt = withTodo(full)

	// SystemPromptNoEnv omits the env block; panes inject a live one per turn.
	s.SystemPromptNoEnv = withTodo(base)

	p.ApplySharedOverrides()
}

// BuildLiveEnvBlock renders the env-block template for workDir using the
// current Prompts. Panes call this per turn with their live shell cwd so the
// env block tracks `cd`. Returns "" if no Prompts/template is configured.
func (s *Setup) BuildLiveEnvBlock(workDir string) string {
	if s == nil || s.Prompts == nil {
		return ""
	}
	return buildEnvBlock(s.Prompts.envBlockTemplate(), workDir)
}

// providerUsable reports whether the configured provider can serve real
// completions, and therefore whether the agentic setup may use it instead of
// falling back to the mock provider.
//
// A provider is usable when it has a key, or when it needs no key at all.
// Testing cfg.APIKey alone silently demoted keyless local providers (ollama) to
// "mock-agentic", which made the documented air-gapped command
// `RYSH_PROVIDER=ollama rysh` return a canned string instead of running the
// local model.
func providerUsable(cfg config.Config) bool {
	// Replay mode (design 009 record/replay): recorded turns need no key at
	// all — the transcript IS the provider.
	if provider.ReplayDirFromEnv() != "" {
		return true
	}
	return cfg.APIKey != "" || !provider.RequiresAPIKey(cfg.ProviderName)
}

// ProviderUsable exposes providerUsable for pre-flight checks by headless
// callers (`rysh run`, design 009): a run against the mock fallback provider
// would end_turn immediately and read as a successful "done", so CI mode must
// refuse to start when this is false.
func ProviderUsable(cfg config.Config) bool {
	return providerUsable(cfg)
}

// buildAgenticProvider selects the Setup's provider, folding in the design-009
// record/replay seam. Precedence:
//
//  1. RYSH_REPLAY_DIR set → the replay provider serving the recorded
//     transcript (deterministic, key-free; session-default model switching is
//     meaningless with no live model, so the wrapper is skipped).
//  2. No usable live provider → the mock fallback (interactive sessions only;
//     `rysh run` refuses it via ProviderUsable).
//  3. The live provider, wrapped for ##llm session defaults; with
//     RYSH_RECORD_DIR set the OUTERMOST wrapper records every interaction, so
//     the transcript holds exactly what was served (post model-override).
//
// A recording directory that cannot be prepared fails the provider closed
// (every call errors) rather than silently running unrecorded: the caller
// asked for a transcript to replay later, and a green-but-unrecorded run
// would only move the failure to replay time.
func buildAgenticProvider(cfg config.Config, sessionLLM *provider.SessionDefaults) provider.AgenticProvider {
	if dir := provider.ReplayDirFromEnv(); dir != "" {
		return provider.NewReplay(dir)
	}
	if !providerUsable(cfg) {
		return &staticAgenticProvider{name: "mock-agentic"}
	}
	prov := provider.WithSessionDefaults(provider.NewAgenticProvider(cfg), sessionLLM)
	if dir := provider.RecordDirFromEnv(); dir != "" {
		rec, err := provider.NewRecording(prov, dir)
		if err != nil {
			slog.Error("record: cannot prepare transcript dir; failing the provider closed", "dir", dir, "err", err)
			return &failingAgenticProvider{err: err}
		}
		prov = rec
	}
	return prov
}

// NewSetup creates the agentic infrastructure from configuration.
// Returns nil if agentic mode is disabled.
func NewSetup(cfg config.Config, plKV nats.KeyValue) *Setup {
	agenticCfg := config.LoadAgenticConfig()
	if !agenticCfg.Enabled {
		return nil
	}

	// Create the agentic provider, wrapped so the ##llm session default
	// (SessionLLM) is consulted on every call.
	sessionLLM := provider.NewSessionDefaults()
	prov := buildAgenticProvider(cfg, sessionLLM)

	// Determine working directory
	workDir, err := os.Getwd()
	if err != nil {
		workDir = "."
	}

	// Create shared background session manager.
	bgSessions := tools.NewBackgroundSessionManager()

	// Create tool registry with all built-in tools
	registry := tools.NewToolRegistry()
	registry.Register("bash", tools.NewBashTool(workDir, agenticCfg.BashTimeout, agenticCfg.BlockedCommands))
	registry.Register("file_read", tools.NewFileReadTool(workDir))
	registry.Register("edit", tools.NewEditTool(workDir))
	registry.Register("file_write", tools.NewFileWriteTool(workDir))
	registry.Register("glob", tools.NewGlobTool(workDir))
	registry.Register("grep", tools.NewGrepTool(workDir))
	registry.Register("web_search", tools.NewWebSearchTool(cfg.BraveAPIKey))
	registry.Register("web_fetch", tools.NewWebFetchTool())
	registry.Register("monitor", tools.NewMonitorTool(workDir))
	registry.Register("sub_agent", tools.NewSubAgentTool())
	// grounding_report is intercepted by the orchestrator (like sub_agent):
	// it opens the enforced grounding gate or asks the user and pauses.
	registry.Register("grounding_report", tools.NewGroundingReportTool())
	// Git: only git_commit is a gated tool; read-only git (status/diff/log/show)
	// is done via bash (use `git diff --color=always` for coloured output).
	registry.Register("git_commit", tools.NewGitCommitTool(workDir))
	// Code intelligence (directory listing via bash: ls/tree/find)
	registry.Register("symbol_search", tools.NewSymbolSearchTool(workDir))
	// Environment (secret-redacting; process/port inspection via bash: ps/lsof)
	registry.Register("env_read", tools.NewEnvReadTool())
	// Testing (structured); lint/build are done via bash (go vet / go build)
	registry.Register("test_run", tools.NewTestRunTool(workDir))
	// Workspace tools (non-NATS; NATS-dependent tools are added per-pane in CreateLLMPromptExecutionActor)
	registry.Register("clipboard", tools.NewClipboardTool())
	registry.Register("project_notes", tools.NewProjectNotesTool(workDir))
	// Durable project memory (RYSH.md), auto-loaded into the system prompt.
	registry.Register("memory_edit", tools.NewMemoryEditTool(workDir))
	// Pipeline tools
	registry.Register("rysh_build_pipeline", tools.NewRyshBuildPipelineTool(workDir, plKV))
	// Background process tools (bash_session merges the former bash_output + kill_shell)
	registry.Register("bash_background", tools.NewBashBackgroundTool(workDir, bgSessions))
	registry.Register("bash_session", tools.NewBashSessionTool(bgSessions))
	// list_tools must be registered last so it sees all tools
	registry.Register("list_tools", tools.NewListToolsTool(registry))

	// MCP + Forge managers register external/generated tools into this same
	// shared registry at startup (BootstrapMCP / BootstrapForge), before any
	// pane clones it.
	mcpMgr := mcp.NewManager(registry, workDir)
	forgeMgr := forge.NewManager(registry, workDir, nil)

	// Load system prompt, then layer in durable project/user memory (RYSH.md).
	systemPrompt := loadSystemPrompt(cfg.SystemPrompt)
	systemPrompt = appendProjectMemory(systemPrompt, workDir)

	return &Setup{
		Provider:     prov,
		ToolRegistry: registry,
		Scopes:       NewScopeRegistries(registry),
		AgenticCfg:   agenticCfg,
		SystemPrompt: systemPrompt,
		BgSessions:   bgSessions,
		MCP:          mcpMgr,
		Forge:        forgeMgr,
		SecretNAT:    newSecretNATManager(cfg),
		SessionLLM:   sessionLLM,
	}
}

// NewSetupAlwaysOn creates an agentic Setup regardless of the Enabled flag.
// This is used when LLMActor has been replaced by LLMPromptExecutionActor and a Setup is
// always required for prompt handling.
func NewSetupAlwaysOn(cfg config.Config, plKV nats.KeyValue) *Setup {
	agenticCfg := config.LoadAgenticConfig()
	agenticCfg.Enabled = true

	// Session-default seam: see NewSetup.
	sessionLLM := provider.NewSessionDefaults()
	prov := buildAgenticProvider(cfg, sessionLLM)

	workDir, err := os.Getwd()
	if err != nil {
		workDir = "."
	}

	bgSessions := tools.NewBackgroundSessionManager()

	registry := tools.NewToolRegistry()
	registry.Register("bash", tools.NewBashTool(workDir, agenticCfg.BashTimeout, agenticCfg.BlockedCommands))
	registry.Register("file_read", tools.NewFileReadTool(workDir))
	registry.Register("edit", tools.NewEditTool(workDir))
	registry.Register("file_write", tools.NewFileWriteTool(workDir))
	registry.Register("glob", tools.NewGlobTool(workDir))
	registry.Register("grep", tools.NewGrepTool(workDir))
	registry.Register("web_search", tools.NewWebSearchTool(cfg.BraveAPIKey))
	registry.Register("web_fetch", tools.NewWebFetchTool())
	registry.Register("monitor", tools.NewMonitorTool(workDir))
	registry.Register("sub_agent", tools.NewSubAgentTool())
	// grounding_report is intercepted by the orchestrator (like sub_agent):
	// it opens the enforced grounding gate or asks the user and pauses.
	registry.Register("grounding_report", tools.NewGroundingReportTool())
	// Git: only git_commit is a gated tool; read-only git (status/diff/log/show)
	// is done via bash (use `git diff --color=always` for coloured output).
	registry.Register("git_commit", tools.NewGitCommitTool(workDir))
	// Code intelligence (directory listing via bash: ls/tree/find)
	registry.Register("symbol_search", tools.NewSymbolSearchTool(workDir))
	// Environment (secret-redacting; process/port inspection via bash: ps/lsof)
	registry.Register("env_read", tools.NewEnvReadTool())
	// Testing (structured); lint/build are done via bash (go vet / go build)
	registry.Register("test_run", tools.NewTestRunTool(workDir))
	// Workspace tools
	registry.Register("clipboard", tools.NewClipboardTool())
	registry.Register("project_notes", tools.NewProjectNotesTool(workDir))
	// Durable project memory (RYSH.md), auto-loaded into the system prompt.
	registry.Register("memory_edit", tools.NewMemoryEditTool(workDir))
	// Pipeline tools
	registry.Register("rysh_build_pipeline", tools.NewRyshBuildPipelineTool(workDir, plKV))
	// Background process tools (bash_session merges the former bash_output + kill_shell)
	registry.Register("bash_background", tools.NewBashBackgroundTool(workDir, bgSessions))
	registry.Register("bash_session", tools.NewBashSessionTool(bgSessions))
	registry.Register("list_tools", tools.NewListToolsTool(registry))

	mcpMgr := mcp.NewManager(registry, workDir)
	forgeMgr := forge.NewManager(registry, workDir, nil)

	systemPrompt := loadSystemPrompt(cfg.SystemPrompt)
	systemPrompt = appendProjectMemory(systemPrompt, workDir)

	return &Setup{
		Provider:     prov,
		ToolRegistry: registry,
		Scopes:       NewScopeRegistries(registry),
		AgenticCfg:   agenticCfg,
		SystemPrompt: systemPrompt,
		BgSessions:   bgSessions,
		MCP:          mcpMgr,
		Forge:        forgeMgr,
		SecretNAT:    newSecretNATManager(cfg),
		SessionLLM:   sessionLLM,
	}
}

// newSecretNATManager builds the process-wide SecretNAT / ReSet manager from
// the [snat] config section. An invalid custom detector pattern is dropped
// with a warning rather than failing daemon startup.
func newSecretNATManager(cfg config.Config) *secretnat.Manager {
	ttl, err := time.ParseDuration(cfg.SNAT.MappingTTL)
	if err != nil {
		ttl = 0 // "" or malformed → conversation lifetime
	}
	custom := make([]secretnat.CustomDetector, 0, len(cfg.SNAT.CustomDetectors))
	for _, c := range cfg.SNAT.CustomDetectors {
		custom = append(custom, secretnat.CustomDetector{Name: c.Name, Pattern: c.Pattern, Prefix: c.Prefix})
	}
	opts := secretnat.Options{
		Enabled:           cfg.SNAT.Enabled,
		Mode:              secretnat.ParseMode(cfg.SNAT.Mode),
		RestoreDisplay:    cfg.SNAT.RestoreDisplay,
		MappingTTL:        ttl,
		DisabledDetectors: cfg.SNAT.DisableDetectors,
		CustomDetectors:   custom,
	}
	m, err := secretnat.NewManager(opts)
	if err != nil {
		slog.Warn("secretnat: invalid custom detector config — continuing without custom detectors", "err", err)
		opts.CustomDetectors = nil
		m, _ = secretnat.NewManager(opts)
	}
	return m
}

// BootstrapMCP connects to MCP servers persisted in <workDir>/.rysh/mcp.json and
// registers their tools into the shared registry. It must run at daemon startup,
// before panes clone the registry, so MCP tools reach every pane and agent. It
// is a no-op when MCP is unavailable or no servers are configured.
func (s *Setup) BootstrapMCP(ctx context.Context) {
	if s == nil || s.MCP == nil {
		return
	}
	s.MCP.Bootstrap(ctx)
}

// BootstrapForge enables Forge integrations persisted in <workDir>/.rysh/forge
// that are marked enabled, registering their tools into the shared registry
// before panes clone it. No-op when Forge is unavailable.
func (s *Setup) BootstrapForge(ctx context.Context) {
	if s == nil || s.Forge == nil {
		return
	}
	s.Forge.Bootstrap(ctx)
}

// CreateLLMPromptExecutionActor creates a new LLMPromptExecutionActor for the given pane.
// Per-pane NATS-dependent tools are registered into a cloned registry before
// spawning the actor, so the shared OrchestratorActor (which clones it again
// per run) inherits all tools without needing NATS access itself.
//
// paneOverride, when non-nil, is the pane's `##pane provider` holder (design
// 002 §3.4): the executor's provider is wrapped to consult it per call, so an
// override installed later applies to the pane's next prompt without
// respawning this actor.
func (s *Setup) CreateLLMPromptExecutionActor(
	ids ScopeIDs,
	cfg config.Config,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
	kvStore nats.KeyValue,
	paneOverride *provider.PaneOverride,
) *LLMPromptExecutionActor {
	paneID := ids.PaneID
	// Build the pane's registry as a live child of its scope chain
	// (pane → group → lane → tab → global) via the ScopeRegistries service, then
	// add the per-pane NATS tools. Reading the chain live (rather than
	// snapshotting it) means tools enabled/disabled via ##integration / ##mcp at
	// any scope take effect on the pane's next prompt — no new pane or restart.
	// With empty group/lane/tab ids this is exactly a child of global, identical
	// to the prior behavior.
	perPaneRegistry := s.Scopes.PaneChain(ids)
	perPaneRegistry.Register("ask_user", tools.NewAskUserTool(nc, pub, paneID))
	perPaneRegistry.Register("pane_inspect", tools.NewPaneInspectTool(nc, cfg.SessionName))
	perPaneRegistry.Register("pane_send", tools.NewPaneSendTool(nc, cfg.SessionName))
	// Browser automation: lets the AI drive a web-mode pane's embedded browser
	// (rendered by the desktop app). The tool publishes to the pane's
	// browser.request subject and awaits browser.response; the connected web pane
	// executor runs the action and replies. Only useful for panes with an active
	// web view, but harmless otherwise (the call times out if no executor answers).
	perPaneRegistry.Register("browser_action", tools.NewBrowserActionTool(
		paneID, nc, browserActionPublisher{pub: pub},
		msg.T("pane", paneID, "browser", "request"),
		msg.T("pane", paneID, "browser", "response"),
	))
	// Phase 4: agent discovery and history tools
	codecs := pub.Codecs()
	perPaneRegistry.Register("agents_list", tools.NewAgentsListTool(nc, cfg.SessionName, codecs))
	perPaneRegistry.Register("session_history", tools.NewSessionHistoryTool(nc, cfg.SessionName))
	// Context tools need JetStream — extract from NATS connection.
	if js, err := nc.JetStream(); err == nil {
		perPaneRegistry.Register("context", tools.NewContextTool(js))
		// Phase 3: todo tool (per-pane, JetStream-backed)
		perPaneRegistry.Register("todo", tools.NewTodoTool(js, paneID))
	}
	// Re-register list_tools last so it sees all per-pane additions.
	perPaneRegistry.Register("list_tools", tools.NewListToolsTool(perPaneRegistry))

	maxIterations := s.AgenticCfg.MaxIterations
	if maxIterations <= 0 {
		maxIterations = 50
	}

	// Panes use the env-block-free base prompt and inject a fresh env block per
	// turn (with the live shell cwd) via SetEnvBlockProvider, wired by the
	// PaneActor where the cwd resolver lives. Fall back to SystemPrompt when
	// ApplyPrompts hasn't populated the no-env variant.
	basePrompt := s.SystemPromptNoEnv
	if basePrompt == "" {
		basePrompt = s.SystemPrompt
	}

	prov := s.Provider
	if paneOverride != nil {
		prov = provider.WithPaneOverride(s.Provider, paneOverride)
	}

	llm := sharedagentic.NewLLMPromptExecutionActor(
		paneID,
		maxIterations,
		pub,
		nc,
		prov,
		perPaneRegistry,
		basePrompt,
		"", // no pipeline output subject for normal panes
		"", // no chat output routing for normal panes
		kvStore,
	)
	// Follow-up 3b: thread the shared metrics sink so every orchestrator
	// this actor spawns records LLM / tool / compaction events.
	if s.Metrics != nil {
		llm.SetMetricsSink(s.Metrics)
	}
	// Grounding: panes default to the ENFORCED gate (read the codebase like a
	// human — read-only tools until grounding_report) unless config overrides.
	llm.SetGroundingMode(s.paneGroundingMode())
	// SecretNAT / ReSet: bind this pane's isolated translation session. The
	// actor wraps every leg provider and threads the session into every
	// orchestrator it spawns.
	if s.SecretNAT != nil {
		llm.SetSecretNAT(s.SecretNAT.Session(paneID))
	}
	return llm
}

// paneGroundingMode resolves the grounding mode for panes: config value when
// set, otherwise enforced (the pane default).
func (s *Setup) paneGroundingMode() string {
	switch s.AgenticCfg.Grounding {
	case sharedagentic.GroundingOff, sharedagentic.GroundingPrompt, sharedagentic.GroundingEnforced:
		return s.AgenticCfg.Grounding
	}
	return sharedagentic.GroundingEnforced
}

// agentGroundingMode resolves the grounding mode for headless agents and
// humanoids: advisory by default (chat users shouldn't get a forced grep
// ritual), off when config disables grounding entirely. A configured
// "enforced" still maps to advisory here — enforcing the gate on headless
// actors risks bricking channel conversations.
func (s *Setup) agentGroundingMode() string {
	if s.AgenticCfg.Grounding == sharedagentic.GroundingOff {
		return sharedagentic.GroundingOff
	}
	return sharedagentic.GroundingPrompt
}

// CreateAgentLLMPromptExecutionActor creates a new LLMPromptExecutionActor for an autonomous agent.
// Unlike CreateLLMPromptExecutionActor (which is for panes), this accepts a custom system
// prompt and chatOutputPaneID for routing output to a pane's chat buffer.
func (s *Setup) CreateAgentLLMPromptExecutionActor(
	agentName string,
	cfg config.Config,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
	systemPrompt string,
	chatOutputPaneID string,
) *LLMPromptExecutionActor {
	// Link to the shared registry as a live parent and add per-agent NATS tools
	// into the child, so ##integration / ##mcp enable/disable take effect on the
	// agent's next prompt without re-creating it.
	perAgentRegistry := tools.NewChildRegistry(s.ToolRegistry)
	// Agents get the same tools as panes (minus ask_user since they're headless).
	perAgentRegistry.Register("pane_inspect", tools.NewPaneInspectTool(nc, cfg.SessionName))
	perAgentRegistry.Register("pane_send", tools.NewPaneSendTool(nc, cfg.SessionName))
	codecs := pub.Codecs()
	perAgentRegistry.Register("agents_list", tools.NewAgentsListTool(nc, cfg.SessionName, codecs))
	perAgentRegistry.Register("session_history", tools.NewSessionHistoryTool(nc, cfg.SessionName))
	// rysh_command (design 008 RA3): the only tool that reaches the "##" handler
	// in WorkspaceActor. pane_send publishes straight to the pane inbox and so
	// cannot run system commands — without this, tabs/panes/agents/share are
	// unreachable for a headless agent or a humanoid driven from a chat app.
	perAgentRegistry.Register("rysh_command", tools.NewRyshCommandTool(nc, cfg.SessionName, chatOutputPaneID))
	if js, err := nc.JetStream(); err == nil {
		perAgentRegistry.Register("context", tools.NewContextTool(js))
	}
	perAgentRegistry.Register("list_tools", tools.NewListToolsTool(perAgentRegistry))

	maxIterations := s.AgenticCfg.MaxIterations
	if maxIterations <= 0 {
		maxIterations = 50
	}

	// Use the agent's custom system prompt, falling back to default if empty.
	sp := systemPrompt
	if sp == "" {
		sp = s.SystemPrompt
	}

	llm := sharedagentic.NewLLMPromptExecutionActor(
		agentName, // paneID field is used for NATS subject construction
		maxIterations,
		pub,
		nc,
		s.Provider,
		perAgentRegistry,
		sp,
		"", // no pipeline output
		chatOutputPaneID,
		nil, // agents don't have pane KV store
	)
	llm.SetAgentName(agentName) // attribute usage to this agent (design 003 "by agent")
	if s.Metrics != nil {
		llm.SetMetricsSink(s.Metrics)
	}
	// Scope inheritance: per prompt, re-point the agent's registry parent to the
	// invoking pane's group chain (from the prompt's ScopeHint), so the agent sees
	// exactly the integration/MCP tools enabled at that pane's scope. Empty hint ⇒
	// global (agent invoked outside any pane).
	llm.SetScopeResolver(s.agentScopeResolver())
	// Grounding: agents/humanoids get the ADVISORY protocol (prompt mode).
	llm.SetGroundingMode(s.agentGroundingMode())
	// SecretNAT / ReSet: agents get their own isolated translation session,
	// keyed by agent name (the actor's subject id).
	if s.SecretNAT != nil {
		llm.SetSecretNAT(s.SecretNAT.Session(agentName))
	}
	return llm
}

// agentScopeResolver returns the per-prompt scope resolver shared by agents and
// humanoids: it maps a ScopeHint to the parent registry their tool registry
// should chain to for that run.
func (s *Setup) agentScopeResolver() func(string) *tools.ToolRegistry {
	return func(hint string) *tools.ToolRegistry {
		if hint == "" || s.Scopes == nil {
			return s.ToolRegistry // global
		}
		return s.Scopes.GroupChainFor(ParseScopeHint(hint))
	}
}

// The former per-channel constructors (CreateHumanoidLLMWithEmailTools /
// WhatsAppTools / SlackTools) are gone: they were dead code, and the email/
// whatsapp variants built UNGATED send tools — exactly the regression the
// shared requireApprovedDraft gate exists to prevent. Every humanoid toolset
// now goes through CreateHumanoidLLMWithChannelTools below.

// HumanoidChannelTools declares which channel toolsets a humanoid should get.
//
// It exists because a humanoid can have MORE THAN ONE channel contact. The
// previous per-channel constructors were selected by a switch, so a humanoid
// configured with both email (human-governed) and Slack received the email
// toolset and NO slack_* tools at all — it could not draft, let alone send.
// Registering whichever adapters are present removes that whole class of bug.
type HumanoidChannelTools struct {
	WhatsApp *channels.WhatsAppAdapter
	Email    *channels.EmailAdapter
	Slack    *channels.SlackAdapter
	Drafts   *channels.DraftStore

	// SlackHumanGoverned / EmailHumanGoverned / WhatsAppHumanGoverned gate
	// slack_send, email_send and whatsapp_send (and whatsapp_send_template):
	// when one reports true, only an owner-approved draft may leave on that
	// channel. Read per call so a runtime `##humanoid governance` switch
	// takes effect immediately. All three share one gate implementation —
	// requireApprovedDraft in internal/tools.
	SlackHumanGoverned    func() bool
	EmailHumanGoverned    func() bool
	WhatsAppHumanGoverned func() bool
}

// CreateHumanoidLLMWithChannelTools builds a humanoid's LLM actor with every
// requested channel toolset registered, plus the matching governance prompts.
func (s *Setup) CreateHumanoidLLMWithChannelTools(
	name string,
	cfg config.Config,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
	systemPrompt string,
	chatOutputPaneID string,
	ct HumanoidChannelTools,
) *LLMPromptExecutionActor {
	perRegistry := tools.NewChildRegistry(s.ToolRegistry)
	perRegistry.Register("pane_inspect", tools.NewPaneInspectTool(nc, cfg.SessionName))
	perRegistry.Register("pane_send", tools.NewPaneSendTool(nc, cfg.SessionName))
	codecs := pub.Codecs()
	perRegistry.Register("agents_list", tools.NewAgentsListTool(nc, cfg.SessionName, codecs))
	perRegistry.Register("session_history", tools.NewSessionHistoryTool(nc, cfg.SessionName))
	if js, err := nc.JetStream(); err == nil {
		perRegistry.Register("context", tools.NewContextTool(js))
	}

	sp := systemPrompt
	if sp == "" {
		sp = s.SystemPrompt
	}

	if ct.WhatsApp != nil {
		perRegistry.Register("whatsapp_list", tools.NewWhatsAppListTool(ct.WhatsApp))
		perRegistry.Register("whatsapp_read", tools.NewWhatsAppReadTool(ct.WhatsApp))
		perRegistry.Register("whatsapp_draft", tools.NewWhatsAppDraftTool(ct.Drafts))
		perRegistry.Register("whatsapp_send", tools.NewWhatsAppSendTool(ct.WhatsApp, ct.Drafts, ct.WhatsAppHumanGoverned))
		perRegistry.Register("whatsapp_send_template", tools.NewWhatsAppSendTemplateTool(ct.WhatsApp, ct.WhatsAppHumanGoverned))
		sp += "\n\n" + s.Prompts.whatsappGovernance()
	}
	if ct.Email != nil {
		perRegistry.Register("email_list", tools.NewEmailListTool(ct.Email))
		perRegistry.Register("email_read", tools.NewEmailReadTool(ct.Email))
		perRegistry.Register("email_draft", tools.NewEmailDraftTool(ct.Drafts))
		perRegistry.Register("email_send", tools.NewEmailSendTool(ct.Email, ct.Drafts, ct.EmailHumanGoverned))
		perRegistry.Register("email_attach", tools.NewEmailAttachTool(ct.Drafts))
		sp += "\n\n" + s.Prompts.emailGovernance()
	}
	if ct.Slack != nil {
		perRegistry.Register("slack_list", tools.NewSlackListTool(ct.Slack))
		perRegistry.Register("slack_read", tools.NewSlackReadTool(ct.Slack))
		perRegistry.Register("slack_draft", tools.NewSlackDraftTool(ct.Drafts))
		perRegistry.Register("slack_send", tools.NewSlackSendTool(ct.Slack, ct.Drafts, ct.SlackHumanGoverned))
		sp += "\n\n" + s.Prompts.slackGovernance()
	}
	perRegistry.Register("list_tools", tools.NewListToolsTool(perRegistry))

	maxIterations := s.AgenticCfg.MaxIterations
	if maxIterations <= 0 {
		maxIterations = 50
	}

	llm := sharedagentic.NewLLMPromptExecutionActor(
		name,
		maxIterations,
		pub,
		nc,
		s.Provider,
		perRegistry,
		sp,
		"",
		chatOutputPaneID,
		nil, // humanoids don't have pane KV store
	)
	llm.SetAgentName(name) // attribute usage to this humanoid (design 003 "by agent")
	if s.Metrics != nil {
		llm.SetMetricsSink(s.Metrics)
	}
	llm.SetScopeResolver(s.agentScopeResolver())
	llm.SetGroundingMode(s.agentGroundingMode())
	return llm
}

// loadSystemPrompt reads a system-prompt override from disk if cfg.SystemPrompt
// points to one; otherwise it returns the in-package fallback. NewSetup callers
// can still override the default by setting RYSH_SYSTEM_PROMPT / system_prompt
// in the config. The Prompts struct supersedes this for the higher-level
// assembly performed by ApplyPrompts.
func loadSystemPrompt(path string) string {
	if path == "" {
		return fallbackDefaultPrompt
	}
	if !filepath.IsAbs(path) {
		wd, err := os.Getwd()
		if err != nil {
			return fallbackDefaultPrompt
		}
		path = filepath.Join(wd, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fallbackDefaultPrompt
	}
	return string(data)
}

// staticAgenticProvider is a fallback that doesn't make real API calls.
type staticAgenticProvider struct {
	name string
}

func (s *staticAgenticProvider) Name() string { return s.name }

func (s *staticAgenticProvider) Complete(_ context.Context, prompt string) (string, error) {
	return "Agentic mode requires an API key. Set RYSH_API_KEY or configure api_key in rysh.config.yaml.", nil
}

func (s *staticAgenticProvider) CompleteWithTools(
	_ context.Context,
	_ []provider.ConversationTurn,
	_ []provider.ToolSpec,
	_ string,
) (*provider.AgenticResponse, error) {
	return &provider.AgenticResponse{
		TextBlocks: []provider.TextBlock{{Text: "Agentic mode requires an API key. Set RYSH_API_KEY or configure api_key in rysh.config.yaml."}},
		StopReason: provider.StopReasonEndTurn,
	}, nil
}

// failingAgenticProvider errors every call with a fixed cause. Used when a
// requested recording transcript cannot be prepared: the run must fail loudly
// rather than run green-but-unrecorded (design 009 record/replay).
type failingAgenticProvider struct {
	err error
}

func (f *failingAgenticProvider) Name() string { return "record-failed" }

func (f *failingAgenticProvider) Complete(_ context.Context, _ string) (string, error) {
	return "", f.err
}

func (f *failingAgenticProvider) CompleteWithTools(
	_ context.Context,
	_ []provider.ConversationTurn,
	_ []provider.ToolSpec,
	_ string,
) (*provider.AgenticResponse, error) {
	return nil, f.err
}
