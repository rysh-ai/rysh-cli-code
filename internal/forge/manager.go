package forge

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ingest"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/runtime"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/toolpack"
)

// Manager owns the live, per-session set of enabled integrations and registers
// their tools into the shared agent registry. Like the MCP manager, it
// registers into the SHARED registry that panes/agents clone at creation, so
// Bootstrap (startup) reaches every pane; a live `##integration enable` reaches
// panes/agents created afterward.
type Manager struct {
	registry *sharedtools.ToolRegistry // global scope (default target)
	workDir  string
	redact   func(string) string

	mu      sync.Mutex
	active  map[string]*active
	storeMu sync.Mutex
}

// ScopeTarget tells the manager which registry to enable an integration into
// and a stable key identifying that scope instance (for display/teardown). The
// caller (which knows the scope hierarchy) resolves it; this keeps forge free of
// any dependency on the scope service. Use Manager.GlobalTarget for the default.
type ScopeTarget struct {
	Key      string                    // e.g. "global", "tab:<id>", "lane:<id>", "pane:<id>"
	Registry *sharedtools.ToolRegistry // where the integration's tools are registered
}

type active struct {
	def        Integration
	registered []string
	mode       toolpack.ExposureMode
	opCount    int
	err        string
	target     *sharedtools.ToolRegistry // the registry the tools were registered into
	scope      string                    // the ScopeTarget.Key they were enabled at
}

// activeKey is the m.active map key: integration name + scope. Keying by both
// (instead of name alone) lets the same integration be live at several scopes
// simultaneously. The 0x1f unit separator never occurs in a sanitized name or a
// scope key ("global", "tab:<uuid>", …).
func activeKey(name, scope string) string {
	if scope == "" {
		scope = "global"
	}
	return name + "\x1f" + scope
}

// Status is a snapshot of one integration for `##integration list`.
type Status struct {
	Name       string
	Source     string
	Enabled    bool
	Mode       string
	Scope      string // scope key the integration is enabled at ("" when disabled)
	ToolCount  int
	Operations int
	Error      string
	BaseURL    string
}

// NewManager creates a Manager registering into reg. redact (optional) is
// applied to tool output before it leaves the session.
func NewManager(reg *sharedtools.ToolRegistry, workDir string, redact func(string) string) *Manager {
	return &Manager{
		registry: reg,
		workDir:  workDir,
		redact:   redact,
		active:   map[string]*active{},
	}
}

// WorkDir returns the project root used for the store and spec files.
func (m *Manager) WorkDir() string { return m.workDir }

// GlobalTarget is the default scope target: the shared (session-wide) registry,
// the root of every pane's scope chain. Bootstrap and unscoped callers use it.
func (m *Manager) GlobalTarget() ScopeTarget {
	return ScopeTarget{Key: "global", Registry: m.registry}
}

// Bootstrap enables every persisted integration marked Enabled. Run once at
// startup, before panes clone the registry.
func (m *Manager) Bootstrap(ctx context.Context) {
	defs, err := LoadStore(m.workDir)
	if err != nil {
		slog.Warn("forge: failed to load integration store", "err", err)
		return
	}
	for _, d := range defs {
		if !d.Enabled {
			continue
		}
		// Only GLOBAL enablements re-apply at bootstrap — the per-scope instances
		// (tab/lane/group/pane) don't exist yet. Scoped enablements are replayed
		// by the owning actor when it restores (ReplayScope), keyed on the stable
		// scope-instance id.
		if d.Scope != "" && d.Scope != "global" {
			continue
		}
		if _, _, err := m.enable(ctx, d, false, m.GlobalTarget()); err != nil {
			slog.Warn("forge: bootstrap enable failed", "integration", d.Name, "err", err)
		} else {
			slog.Info("forge: integration enabled", "integration", d.Name)
		}
	}
}

// ReplayScope re-enables any persisted integration that was enabled at scopeKey,
// into target. Called by a Tab/Lane/PaneGroup/Pane actor when it restores with a
// known (stable) scope-instance id, so scoped enablement survives detach/attach
// and restart. No-op for the global scope (handled at Bootstrap).
func (m *Manager) ReplayScope(ctx context.Context, scopeKey string, target ScopeTarget) {
	if scopeKey == "" || scopeKey == "global" {
		return
	}
	defs, err := LoadStore(m.workDir)
	if err != nil {
		return
	}
	for _, d := range defs {
		if !d.Enabled || d.Scope != scopeKey {
			continue
		}
		if _, _, err := m.enable(ctx, d, false, target); err != nil {
			slog.Warn("forge: replay-scope enable failed", "integration", d.Name, "scope", scopeKey, "err", err)
		} else {
			slog.Info("forge: integration replayed at scope", "integration", d.Name, "scope", scopeKey)
		}
	}
}

// Enable loads, registers (into target), and persists an integration as enabled.
func (m *Manager) Enable(ctx context.Context, def Integration, target ScopeTarget) (int, toolpack.ExposureMode, error) {
	if err := def.Validate(); err != nil {
		return 0, "", err
	}
	return m.enable(ctx, def, true, target)
}

// EnableByName enables a previously-added integration by name into target.
func (m *Manager) EnableByName(ctx context.Context, name string, target ScopeTarget) (int, toolpack.ExposureMode, error) {
	defs, err := LoadStore(m.workDir)
	if err != nil {
		return 0, "", err
	}
	for _, d := range defs {
		if d.Name == name {
			return m.enable(ctx, d, true, target)
		}
	}
	return 0, "", fmt.Errorf("no integration named %q (add it with: rysh forge add)", name)
}

func (m *Manager) enable(_ context.Context, def Integration, persist bool, target ScopeTarget) (int, toolpack.ExposureMode, error) {
	if target.Registry == nil {
		target = m.GlobalTarget()
	}
	specPath := def.SpecFile
	if !filepath.IsAbs(specPath) {
		specPath = filepath.Join(m.workDir, specPath)
	}
	spec, err := os.ReadFile(specPath)
	if err != nil {
		return 0, "", fmt.Errorf("read spec %s: %w", def.SpecFile, err)
	}

	if def.source() == SourceGraphQL {
		return m.enableGraphQL(def, spec, persist, target)
	}

	api, err := ingest.OpenAPI(spec)
	if err != nil {
		return 0, "", err
	}

	baseURL := def.BaseURL
	if baseURL == "" {
		baseURL = api.BaseURL()
	}
	cred := runtime.Credential{}
	if def.CredentialEnv != "" {
		cred.APIKey = os.Getenv(def.CredentialEnv)
	}
	if def.BasicUserEnv != "" {
		cred.Username = os.Getenv(def.BasicUserEnv)
	}
	if def.BasicPassEnv != "" {
		cred.Password = os.Getenv(def.BasicPassEnv)
	}
	opts := runtime.Options{Redact: m.redact}
	if err := applyAuthOptions(def, &opts); err != nil {
		return 0, "", err
	}
	exec := runtime.NewHTTPExecutor(baseURL, api.Auth, cred, opts)

	pol := def.Policy
	if pol.Prefix == "" {
		pol.Prefix = toolpack.SanitizeName(def.Name) + "_"
	}

	tp := toolpack.Build(api, pol.Prefix)

	// Retract any prior registration of this integration AT THIS SAME SCOPE (a
	// re-enable at the same scope). Enabling at a DIFFERENT scope is additive —
	// the integration can be live at several scopes at once (keyed by name+scope).
	key := activeKey(def.Name, target.Key)
	m.mu.Lock()
	if prior := m.active[key]; prior != nil {
		for _, n := range prior.registered {
			prior.target.Unregister(n)
		}
	}
	m.mu.Unlock()

	names, mode := tp.Register(target.Registry, api, exec, pol)

	m.mu.Lock()
	m.active[key] = &active{def: def, registered: names, mode: mode, opCount: len(api.Operations), target: target.Registry, scope: target.Key}
	m.mu.Unlock()

	if persist {
		def.Enabled = true
		def.Scope = target.Key
		if err := m.persist(def); err != nil {
			slog.Warn("forge: persist enable failed", "integration", def.Name, "err", err)
		}
	}
	return len(names), mode, nil
}

// enableGraphQL registers GraphQL tools (schema + query) for a graphql source.
// GraphQL is exposed via two constant-footprint tools rather than one per field.
func (m *Manager) enableGraphQL(def Integration, spec []byte, persist bool, target ScopeTarget) (int, toolpack.ExposureMode, error) {
	if target.Registry == nil {
		target = m.GlobalTarget()
	}
	api, err := ingest.GraphQL(spec)
	if err != nil {
		return 0, "", err
	}
	endpoint := def.BaseURL
	if endpoint == "" {
		endpoint = api.BaseURL()
	}
	if endpoint == "" || endpoint == "/graphql" {
		return 0, "", fmt.Errorf("graphql integration %q needs a full endpoint URL; set it with --base-url", def.Name)
	}
	cred := runtime.Credential{}
	if def.CredentialEnv != "" {
		cred.APIKey = os.Getenv(def.CredentialEnv)
	}
	if def.BasicUserEnv != "" {
		cred.Username = os.Getenv(def.BasicUserEnv)
	}
	if def.BasicPassEnv != "" {
		cred.Password = os.Getenv(def.BasicPassEnv)
	}
	opts := runtime.Options{Redact: m.redact}
	if err := applyAuthOptions(def, &opts); err != nil {
		return 0, "", err
	}
	exec := runtime.NewGraphQLExecutor(endpoint, api.Auth, cred, opts)

	pol := def.Policy
	if pol.Prefix == "" {
		pol.Prefix = toolpack.SanitizeName(def.Name) + "_"
	}

	key := activeKey(def.Name, target.Key)
	m.mu.Lock()
	if prior := m.active[key]; prior != nil {
		for _, n := range prior.registered {
			prior.target.Unregister(n)
		}
	}
	m.mu.Unlock()

	names := toolpack.RegisterGraphQL(target.Registry, api, exec, pol)

	m.mu.Lock()
	m.active[key] = &active{def: def, registered: names, mode: "graphql", opCount: len(api.Operations), target: target.Registry, scope: target.Key}
	m.mu.Unlock()

	if persist {
		def.Enabled = true
		def.Scope = target.Key
		if err := m.persist(def); err != nil {
			slog.Warn("forge: persist enable failed", "integration", def.Name, "err", err)
		}
	}
	return len(names), "graphql", nil
}

// Disable unregisters an integration's tools at EVERY scope it is enabled at and
// persists Enabled=false.
func (m *Manager) Disable(name string) error {
	m.mu.Lock()
	var acts []*active
	for k, a := range m.active {
		if a.def.Name == name {
			acts = append(acts, a)
			delete(m.active, k)
		}
	}
	m.mu.Unlock()
	for _, act := range acts {
		reg := act.target
		if reg == nil {
			reg = m.registry
		}
		for _, n := range act.registered {
			reg.Unregister(n)
		}
	}
	// Persist Enabled=false (keep the definition so it can be re-enabled).
	defs, err := LoadStore(m.workDir)
	if err == nil {
		for i := range defs {
			if defs[i].Name == name {
				defs[i].Enabled = false
				defs[i].Scope = ""
				_ = m.saveStore(defs)
				break
			}
		}
	}
	if len(acts) == 0 {
		return fmt.Errorf("integration %q is not enabled", name)
	}
	return nil
}

// List returns a sorted snapshot of all known integrations (persisted + active).
func (m *Manager) List() []Status {
	defs, _ := LoadStore(m.workDir)
	m.mu.Lock()
	defer m.mu.Unlock()

	byName := map[string]Status{}
	for _, d := range defs {
		byName[d.Name] = Status{Name: d.Name, Source: d.source(), Enabled: d.Enabled, BaseURL: d.BaseURL}
	}
	// An integration may be active at several scopes (multi-scope). Aggregate by
	// name: sum the registered tool counts, union the scope keys.
	scopeSets := map[string]map[string]bool{}
	for _, act := range m.active {
		name := act.def.Name
		st := byName[name]
		st.Name = name
		st.Source = act.def.source()
		st.Enabled = true
		st.Mode = string(act.mode)
		st.ToolCount += len(act.registered)
		st.Operations = act.opCount
		if act.err != "" {
			st.Error = act.err
		}
		if st.BaseURL == "" {
			st.BaseURL = act.def.BaseURL
		}
		byName[name] = st
		if scopeSets[name] == nil {
			scopeSets[name] = map[string]bool{}
		}
		if act.scope != "" {
			scopeSets[name][act.scope] = true
		}
	}
	for name, set := range scopeSets {
		keys := make([]string, 0, len(set))
		for s := range set {
			keys = append(keys, s)
		}
		sort.Strings(keys)
		st := byName[name]
		st.Scope = strings.Join(keys, ",")
		byName[name] = st
	}
	out := make([]Status, 0, len(byName))
	for _, s := range byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Tools returns the registered tool names for an enabled integration, unioned
// across every scope it is enabled at (deduplicated).
func (m *Manager) Tools(name string) ([]string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	found := false
	for _, a := range m.active {
		if a.def.Name != name {
			continue
		}
		found = true
		for _, n := range a.registered {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	if !found {
		return nil, false
	}
	return out, true
}

// SharedOp describes one forge-registered operation for Task 2 forged-API sharing.
type SharedOp struct {
	Name        string
	Description string
	Schema      string // JSON schema text (the op's input schema)
	Mutating    bool
}

// APIOps returns the op specs for the integration named `name`, deduped across
// every scope it is enabled at. This is the name-keyed counterpart of SharedOps
// (which is scope-keyed): forged-API SHARING is by integration name, not by a
// pane/tab scope, so this is what `##forge share api <name>` exposes. Empty when
// the integration is not enabled anywhere (it must be enabled to be runnable).
func (m *Manager) APIOps(name string) []SharedOp {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	var out []SharedOp
	for _, a := range m.active {
		if a.def.Name != name || a.target == nil {
			continue
		}
		for _, opName := range a.registered {
			ex, ok := a.target.Get(opName)
			if !ok {
				continue
			}
			sp := ex.Spec()
			if seen[sp.Name] {
				continue
			}
			seen[sp.Name] = true
			out = append(out, SharedOp{
				Name:        sp.Name,
				Description: sp.Description,
				Schema:      string(sp.Parameters),
				Mutating:    sp.RequiresApproval,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SharedOps returns the specs of forge-registered operations enabled at any of the
// given scope keys (e.g. "global", "tab:<id>", "pane:<id>"). This is the
// forge-origin filter for forged-API sharing: ONLY operations this manager
// registered are returned — built-in / MCP / per-pane tools are excluded by
// construction (they live in different registries / managers). Mutating comes from
// the op's RequiresApproval flag (forge sets it for non-GET/HEAD operations).
func (m *Manager) SharedOps(scopeKeys map[string]bool) []SharedOp {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SharedOp
	// An integration can be active at several scopes (multi-scope); when a share's
	// scope chain matches more than one of them, the op would otherwise appear
	// multiple times. Dedup by tool name.
	seen := map[string]bool{}
	for _, a := range m.active {
		if a.target == nil || !scopeKeys[a.scope] {
			continue
		}
		for _, name := range a.registered {
			ex, ok := a.target.Get(name)
			if !ok {
				continue
			}
			sp := ex.Spec()
			if seen[sp.Name] {
				continue
			}
			seen[sp.Name] = true
			out = append(out, SharedOp{
				Name:        sp.Name,
				Description: sp.Description,
				Schema:      string(sp.Parameters),
				Mutating:    sp.RequiresApproval,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Remove disables and deletes an integration definition (and its stored spec).
func (m *Manager) Remove(name string) error {
	_ = m.Disable(name)
	defs, err := LoadStore(m.workDir)
	if err != nil {
		return err
	}
	defs, removed := remove(defs, name)
	if err := m.saveStore(defs); err != nil {
		return err
	}
	_ = os.RemoveAll(SpecDir(m.workDir, name))
	if !removed {
		return fmt.Errorf("no integration named %q", name)
	}
	return nil
}

// UnregisterScope retracts every active integration that was enabled at the
// given scope key (e.g. "lane:<id>") and forgets it. Called when the scope
// instance (tab/lane/group/pane) is torn down so its tools don't linger.
func (m *Manager) UnregisterScope(scopeKey string) {
	if scopeKey == "" || scopeKey == "global" {
		return // never auto-retract the session-wide scope
	}
	m.mu.Lock()
	var drop []string
	for k, a := range m.active {
		if a.scope == scopeKey {
			reg := a.target
			if reg == nil {
				reg = m.registry
			}
			for _, n := range a.registered {
				reg.Unregister(n)
			}
			drop = append(drop, k)
		}
	}
	for _, k := range drop {
		delete(m.active, k)
	}
	m.mu.Unlock()
}

// Close unregisters all active integration tools.
func (m *Manager) Close() {
	m.mu.Lock()
	acts := make([]*active, 0, len(m.active))
	for _, a := range m.active {
		acts = append(acts, a)
	}
	m.active = map[string]*active{}
	m.mu.Unlock()
	for _, a := range acts {
		reg := a.target
		if reg == nil {
			reg = m.registry
		}
		for _, n := range a.registered {
			reg.Unregister(n)
		}
	}
}

// LiveAPI ingests an enabled/known integration's spec into the IR (used by the
// CLI `rysh forge generate`/diff paths).
func LoadSpecAPI(workDir string, def Integration) (*ir.API, error) {
	specPath := def.SpecFile
	if !filepath.IsAbs(specPath) {
		specPath = filepath.Join(workDir, specPath)
	}
	spec, err := os.ReadFile(specPath)
	if err != nil {
		return nil, err
	}
	switch def.source() {
	case SourceGraphQL:
		return ingest.GraphQL(spec)
	case SourceGRPC:
		return ingest.GRPC(spec)
	default:
		return ingest.OpenAPI(spec)
	}
}

func (m *Manager) persist(def Integration) error {
	m.storeMu.Lock()
	defer m.storeMu.Unlock()
	defs, err := LoadStore(m.workDir)
	if err != nil {
		return err
	}
	defs = upsert(defs, def)
	return SaveStore(m.workDir, defs)
}

func (m *Manager) saveStore(defs []Integration) error {
	m.storeMu.Lock()
	defer m.storeMu.Unlock()
	return SaveStore(m.workDir, defs)
}

// applyAuthOptions builds a TokenManager from the integration's Auth config (if
// it describes a token flow) and wires its Token/Refresh callbacks into the
// executor Options. No-op when Auth is unset or static apiKey/basic (handled by
// the executor's InjectAuth via CredentialEnv). Forged-API auth plan, Phase A.
func applyAuthOptions(def Integration, opts *runtime.Options) error {
	if def.Auth == nil || !def.Auth.IsTokenFlow() {
		return nil
	}
	tm, err := runtime.NewTokenManager(*def.Auth, nil)
	if err != nil {
		return fmt.Errorf("integration %q auth: %w", def.Name, err)
	}
	if tm == nil {
		return nil
	}
	opts.TokenProvider = tm.Token
	opts.TokenRefresher = tm.Refresh
	opts.AuthHeader = def.Auth.Header
	opts.AuthScheme = def.Auth.Scheme
	return nil
}
