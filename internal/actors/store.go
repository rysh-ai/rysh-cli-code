package actors

// Named-value stores — the shared machinery behind ##secret and ##variable.
//
// Both commands persist scope-namespaced named values under .rysh/<dir> (secrets
// → .rysh/secrets, variables → .rysh/variables) and resolve ${NAME} placeholders
// through the same tiered precedence:
//
//	session KV bucket  →  .rysh/<dir>/<scope>/<NAME>  →  rysh.config.yaml[<section>]  →  process env
//
// A secretStore is the secret flavour (SecretNAT-protected; the LLM never sees
// the real value); a variable store is the environment-variable flavour (plain
// config the LLM may see). The only differences are the on-disk directory, the
// session KV bucket, the config section, and — for secrets only — the SecretNAT
// known-tier sync run after every mutation. Everything else is shared here.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/nats-io/nats.go"
)

// secretNamePattern validates value names: a leading letter/underscore followed
// by letters, digits, or underscores — the same shape as an environment
// variable, so the names interchange cleanly across the resolution tiers. It
// also guarantees a name is a safe single-segment filename (no separators, no
// "..") for the persisted files under .rysh/<dir>.
var secretNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// secretRefPattern matches ${NAME} placeholders inside skill files and configs.
var secretRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// scopeUnsafePattern matches characters not allowed in a scope token. A scope is
// a workspace name (or tab id); anything containing path-or-key-unsafe
// characters is reduced to a safe token usable as both a NATS KV key segment and
// a directory name.
var scopeUnsafePattern = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// secretSource identifies which tier a resolved value came from. The tiers are
// the same for secrets and variables.
type secretSource string

const (
	secretSourceSession secretSource = "session" // the per-session JetStream KV bucket (workspace-scoped)
	secretSourcePersist secretSource = "persist" // a .rysh/<dir>/<scope>/<NAME> file
	secretSourceConfig  secretSource = "config"  // rysh.config.yaml [<section>].<scope>
	secretSourceEnv     secretSource = "env"     // the process environment (global)
)

// sanitizeScope turns a workspace name into a safe scope token for KV keys and
// directory names. Simple names pass through unchanged; unsafe characters are
// replaced and an empty name maps to "default".
func sanitizeScope(id string) string {
	if id == "" {
		return "default"
	}
	s := scopeUnsafePattern.ReplaceAllString(id, "-")
	if s == "" {
		return "default"
	}
	return s
}

// trimSecret strips leading and trailing whitespace (spaces and newlines) from a
// value while preserving internal spaces, so values such as Gmail app passwords
// ("abcd efgh ijkl mnop") survive intact. Applied uniformly at every resolution
// tier so a value loads the same regardless of which source it came from.
func trimSecret(v string) string { return strings.TrimSpace(v) }

// validSecretName reports whether name is a legal value/env identifier (and a
// safe persisted-file name).
func validSecretName(name string) bool { return secretNamePattern.MatchString(name) }

// secretKVKey builds the workspace-scoped KV key for a value: "<scope>/<name>".
func secretKVKey(scope, name string) string { return scope + "/" + name }

// storeKind captures the on-disk / display identity of a named-value store: the
// singular command label ("secret" / "variable") and the .rysh subdirectory
// ("secrets" / "variables"). Persistence (the file layout) depends only on the
// kind, so these helpers hang off storeKind rather than the full store.
type storeKind struct {
	label string // singular; the command verb and message tag, e.g. "secret"
	dir   string // subdir under .rysh, e.g. "secrets"
}

var (
	secretKind   = storeKind{label: "secret", dir: "secrets"}
	variableKind = storeKind{label: "variable", dir: "variables"}
)

// writeDir is the project-local root that "##<label> new" persists into
// (persistence is on by default; --no-persist skips it). It is relative to the
// daemon's working directory, mirroring how .rysh/agents and .rysh/humanoids are
// resolved. Per-scope values live in the <scope> subdirectory beneath it.
func (k storeKind) writeDir() string { return filepath.Join(".rysh", k.dir) }

// readDirs returns the ordered list of roots to search when resolving a
// persisted value: <base>/<dir> for each .rysh base dir (project-local ./.rysh
// first, then $HOME/.rysh). The scope is appended by the caller.
func (k storeKind) readDirs() []string {
	bases := ryshBaseDirs()
	dirs := make([]string, len(bases))
	for i, b := range bases {
		dirs[i] = filepath.Join(b, k.dir)
	}
	return dirs
}

// readPersisted returns the value of a persisted file for the given scope,
// searching the roots in priority order. Leading and trailing whitespace (spaces
// and newlines left by editors) is trimmed; internal spaces are preserved.
func (k storeKind) readPersisted(scope, name string) (string, bool) {
	if !validSecretName(name) {
		return "", false
	}
	for _, root := range k.readDirs() {
		if data, err := os.ReadFile(filepath.Join(root, scope, name)); err == nil {
			return strings.TrimSpace(string(data)), true
		}
	}
	return "", false
}

// writePersisted writes a value to .rysh/<dir>/<scope>/<NAME> with restrictive
// permissions (0600 file, 0700 dirs) and drops a protective .gitignore at the
// root so the plaintext values are never committed.
func (k storeKind) writePersisted(scope, name, value string) (string, error) {
	if !validSecretName(name) {
		return "", fmt.Errorf("invalid %s name %q", k.label, name)
	}
	root := k.writeDir()
	dir := filepath.Join(root, scope)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	k.ensureGitignore(root)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		return "", fmt.Errorf("write %s file: %w", k.label, err)
	}
	return path, nil
}

// ensureGitignore writes a .gitignore into the store root (best effort) so a
// project that tracks .rysh/ still never commits the plaintext values.
func (k storeKind) ensureGitignore(root string) {
	gi := filepath.Join(root, ".gitignore")
	if _, err := os.Stat(gi); err == nil {
		return
	}
	banner := fmt.Sprintf("# rysh %s — managed by ##%s; never commit these\n*\n!.gitignore\n", k.dir, k.label)
	_ = os.WriteFile(gi, []byte(banner), 0o600)
}

// namedStore resolves workspace-scoped named values (secrets or variables).
// Values are namespaced by workspace, and the underlying session KV bucket is
// itself per-session, so a value is effectively scoped per workspace per session.
// Lookups follow a fixed precedence within a scope:
//
//	session KV  →  .rysh/<dir>/<scope>  →  rysh.config.yaml[<section>].<scope>  →  process env (global)
//
// The environment tier is intentionally NOT workspace-scoped: OS env vars are
// process-global, so they remain a shared fallback across every workspace.
//
// The zero/nil store is safe to use: Get falls through to env and Expand returns
// its input unchanged. Set needs a session bucket OR --persist; Delete works on
// the session and persisted tiers (config/env are read-only).
type namedStore struct {
	kind storeKind
	kv   nats.KeyValue                // session bucket; nil when JetStream is unavailable
	cfg  map[string]map[string]string // config-declared, scope-namespaced values (scope→name→value)
}

// newNamedStore builds a store of the given kind over the (possibly nil) session
// KV bucket and the config-declared, scope-namespaced values. Config scope keys
// are sanitized so they match the runtime workspace-name scopes.
func newNamedStore(kind storeKind, kv nats.KeyValue, cfg map[string]map[string]string) *namedStore {
	norm := make(map[string]map[string]string, len(cfg))
	for scope, m := range cfg {
		tok := sanitizeScope(scope)
		if norm[tok] == nil {
			norm[tok] = make(map[string]string, len(m))
		}
		for k, v := range m {
			norm[tok][k] = v
		}
	}
	return &namedStore{kind: kind, kv: kv, cfg: norm}
}

// Set stores (or overwrites) a value in the given scope. When persist is true
// (the command default) the value is also written to .rysh/<dir>/<scope>/<NAME>
// (durable, project-local). It errors when the name is invalid, or when neither a
// session bucket nor persistence is available to store the value.
func (s *namedStore) Set(scope, name, value string, persist bool) error {
	if !validSecretName(name) {
		return fmt.Errorf("invalid %s name %q (use letters, digits and underscore; must not start with a digit)", s.kind.label, name)
	}
	stored := false
	if persist {
		if _, err := s.kind.writePersisted(scope, name, value); err != nil {
			return err
		}
		stored = true
	}
	if s != nil && s.kv != nil {
		if _, err := s.kv.Put(secretKVKey(scope, name), []byte(value)); err != nil {
			if !stored {
				return fmt.Errorf("store %s: %w", s.kind.label, err)
			}
			// Persisted to disk but the session put failed — non-fatal.
		} else {
			stored = true
		}
	}
	if !stored {
		return fmt.Errorf("the session %s store is unavailable (JetStream not active); drop --no-persist to write %s instead",
			s.kind.label, filepath.Join(s.kind.writeDir(), scope, name))
	}
	return nil
}

// getScoped resolves a value within a single scope, trying only the
// scope-namespaced tiers (session bucket → .rysh/<dir>/<scope> → config[scope]).
// It does NOT fall back to the process environment — that global tier is applied
// once, after every scope, by Get and GetLayered. Every tier's value has its
// surrounding whitespace trimmed via trimSecret; internal spaces are preserved.
func (s *namedStore) getScoped(scope, name string) (string, secretSource, bool) {
	if s == nil {
		return "", "", false
	}
	if s.kv != nil {
		if e, err := s.kv.Get(secretKVKey(scope, name)); err == nil {
			return trimSecret(string(e.Value())), secretSourceSession, true
		}
	}
	if v, ok := s.kind.readPersisted(scope, name); ok {
		return v, secretSourcePersist, true // already trimmed by readPersisted
	}
	if m, ok := s.cfg[scope]; ok {
		if v, ok := m[name]; ok {
			return trimSecret(v), secretSourceConfig, true
		}
	}
	return "", "", false
}

// Get resolves a value by name within a single scope, returning its value, the
// tier it came from, and whether it was found. Precedence: session bucket →
// .rysh/<dir>/<scope> → config[scope] → environment (global).
func (s *namedStore) Get(scope, name string) (string, secretSource, bool) {
	if v, src, ok := s.getScoped(scope, name); ok {
		return v, src, true
	}
	if v, ok := os.LookupEnv(name); ok {
		return trimSecret(v), secretSourceEnv, true
	}
	return "", "", false
}

// getLayeredScoped resolves across an ordered list of scopes (most specific
// first), trying each scope's session → persist → config tiers, WITHOUT the
// process-environment fallback. It backs both GetLayered (which adds env) and the
// combined secret+variable resolver (which applies env once after both stores).
func (s *namedStore) getLayeredScoped(scopes []string, name string) (value string, src secretSource, scope string, ok bool) {
	seen := map[string]bool{}
	for _, sc := range scopes {
		if sc == "" || seen[sc] {
			continue
		}
		seen[sc] = true
		if v, src, found := s.getScoped(sc, name); found {
			return v, src, sc, true
		}
	}
	return "", "", "", false
}

// GetLayered resolves a value across an ordered list of scopes (most specific
// first, e.g. tab → workspace), trying each scope's session → persist → config
// tiers, then falling back once to the global process environment. It returns the
// value, the tier it came from, the scope that supplied it ("" for the env tier),
// and whether it was found.
func (s *namedStore) GetLayered(scopes []string, name string) (value string, src secretSource, scope string, ok bool) {
	if v, src, sc, found := s.getLayeredScoped(scopes, name); found {
		return v, src, sc, true
	}
	if v, ok := os.LookupEnv(name); ok {
		return trimSecret(v), secretSourceEnv, "", true
	}
	return "", "", "", false
}

// Delete removes a value from the writable tiers of a scope — the session bucket
// and the persisted .rysh/<dir>/<scope> file. The config and environment tiers
// are read-only. The booleans report which writable tiers actually held it.
func (s *namedStore) Delete(scope, name string) (removedSession, removedPersist bool, err error) {
	if !validSecretName(name) {
		return false, false, fmt.Errorf("invalid %s name %q", s.kind.label, name)
	}
	for _, root := range s.kind.readDirs() {
		path := filepath.Join(root, scope, name)
		if _, statErr := os.Stat(path); statErr == nil {
			if rmErr := os.Remove(path); rmErr != nil {
				return removedSession, removedPersist, fmt.Errorf("delete %s file: %w", s.kind.label, rmErr)
			}
			removedPersist = true
		}
	}
	if s != nil && s.kv != nil {
		key := secretKVKey(scope, name)
		if _, getErr := s.kv.Get(key); getErr == nil {
			if delErr := s.kv.Delete(key); delErr != nil {
				return removedSession, removedPersist, fmt.Errorf("delete %s: %w", s.kind.label, delErr)
			}
			removedSession = true
		}
	}
	return removedSession, removedPersist, nil
}

// Expand replaces every ${NAME} reference in in with its resolved value for the
// given scope. Unresolved references are left verbatim so non-value ${...} text
// survives.
func (s *namedStore) Expand(scope, in string) string {
	if s == nil {
		return in
	}
	return secretRefPattern.ReplaceAllStringFunc(in, func(match string) string {
		name := secretRefPattern.FindStringSubmatch(match)[1]
		if v, _, ok := s.Get(scope, name); ok {
			return v
		}
		return match
	})
}

// expandFunc returns Expand bound to a scope as a standalone, nil-safe function.
func (s *namedStore) expandFunc(scope string) func(string) string {
	return func(in string) string { return s.Expand(scope, in) }
}

// ExpandLayered replaces every ${NAME} reference in in with its resolved value,
// looking the name up across the ordered scopes (most specific first) and then
// the global environment. Unresolved references are left verbatim.
func (s *namedStore) ExpandLayered(scopes []string, in string) string {
	if s == nil {
		return in
	}
	return secretRefPattern.ReplaceAllStringFunc(in, func(match string) string {
		name := secretRefPattern.FindStringSubmatch(match)[1]
		if v, _, _, ok := s.GetLayered(scopes, name); ok {
			return v
		}
		return match
	})
}

// secretInfo is one row of the "##secret/##variable list" output. Sources holds
// every tier the value exists in, highest precedence first (session → persist →
// config): the value resolves from Sources[0]; any further entries are shadowed
// copies (e.g. the persisted file behind a session value).
type secretInfo struct {
	Name    string
	Sources []secretSource
}

// Source returns the tier the value resolves from (the highest-precedence tier
// it exists in).
func (i secretInfo) Source() secretSource { return i.Sources[0] }

// sourcesLabel renders the tier list for display, e.g. "session, persist".
func (i secretInfo) sourcesLabel() string {
	parts := make([]string, len(i.Sources))
	for j, s := range i.Sources {
		parts[j] = string(s)
	}
	return strings.Join(parts, ", ")
}

// List returns the names of all values visible in a scope (session, persisted,
// and config tiers), sorted, each tagged with EVERY tier it exists in,
// precedence-ordered (session shadowing persist shadowing config) — so a value
// stored with the default persistence shows as "session, persist". Environment
// variables are global and not enumerated, but still resolve via Get.
func (s *namedStore) List(scope string) []secretInfo {
	seen := map[string][]secretSource{}
	add := func(name string, src secretSource) {
		for _, existing := range seen[name] {
			if existing == src {
				return // e.g. the same persisted name in both ./.rysh and ~/.rysh
			}
		}
		seen[name] = append(seen[name], src)
	}
	prefix := scope + "/"
	if s != nil && s.kv != nil {
		if keys, err := s.kv.Keys(); err == nil {
			for _, k := range keys {
				if strings.HasPrefix(k, prefix) {
					add(strings.TrimPrefix(k, prefix), secretSourceSession)
				}
			}
		}
	}
	// Guarded like the two tiers around it. This block dereferenced s.kind
	// unconditionally while the session tier above and the config tier below
	// both check s != nil, so a nil store panicked here instead of returning
	// an empty list. Found by staticcheck SA5011, which inferred the
	// inconsistency from those neighbouring checks.
	if s != nil {
		for _, root := range s.kind.readDirs() {
			entries, err := os.ReadDir(filepath.Join(root, scope))
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() || !validSecretName(e.Name()) {
					continue
				}
				add(e.Name(), secretSourcePersist)
			}
		}
	}
	if s != nil {
		for name := range s.cfg[scope] {
			add(name, secretSourceConfig)
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]secretInfo, len(names))
	for i, name := range names {
		out[i] = secretInfo{Name: name, Sources: seen[name]}
	}
	return out
}

// ---------------------------------------------------------------------------
// Workspace/tab scope resolution (shared by ##secret and ##variable)
// ---------------------------------------------------------------------------

// secretWorkspaceScope resolves the scope token and human label for this
// workspace. Values are namespaced by workspace name, and the session KV bucket
// is itself per-session, so a value is effectively scoped per workspace per
// session. The workspace name falls back to the session name, then to "default".
func (w *WorkspaceActor) secretWorkspaceScope() (scope, label string) {
	name := w.workspaceName
	if name == "" {
		name = w.sessionName
	}
	if name == "" {
		return "default", "default"
	}
	return sanitizeScope(name), name
}

// secretTabScopeID returns the tab-scope token for the tab containing paneID,
// falling back to the active tab. Returns "" when no tab is resolvable. Tab-scoped
// values are the most-specific layer, overriding workspace values.
func (w *WorkspaceActor) secretTabScopeID(paneID string) string {
	t := w.findPaneTab(paneID)
	if t == nil {
		t = w.currentTab()
	}
	if t == nil {
		return ""
	}
	return sanitizeScope(t.id)
}

// secretScopeChain returns the ordered scope tokens used to resolve ${NAME} when
// a skill file is loaded from paneID: the pane's tab scope (most specific) then
// the workspace scope (the shared default). The global environment is appended by
// GetLayered. The tab scope is omitted when it is empty or equals the workspace
// scope, so a single-scope chain is returned in the common case.
func (w *WorkspaceActor) secretScopeChain(paneID string) []string {
	wsScope, _ := w.secretWorkspaceScope()
	tabScope := w.secretTabScopeID(paneID)
	if tabScope == "" || tabScope == wsScope {
		return []string{wsScope}
	}
	return []string{tabScope, wsScope}
}

// secretScopeLabel renders a resolved scope token from GetLayered as a friendly
// label: the environment, the workspace, or a named tab.
func (w *WorkspaceActor) secretScopeLabel(token, wsScope, wsLabel string) string {
	switch {
	case token == "":
		return "environment"
	case token == wsScope:
		return fmt.Sprintf("workspace %q", wsLabel)
	default:
		for _, t := range w.tabs {
			if sanitizeScope(t.id) == token {
				name := t.title
				if name == "" {
					name = t.id
				}
				return fmt.Sprintf("tab %q", name)
			}
		}
		return token
	}
}

// splitTabFlag pulls a "--tab <value>" / "--tab=<value>" / "-t <value>" flag out
// of a ##secret/##variable argument list, returning the tab value (empty when
// absent) and the remaining arguments with the flag removed. The flag selects
// tab-level storage; without it, values default to the workspace scope.
func splitTabFlag(parts []string) (tabArg string, rest []string) {
	for i := 0; i < len(parts); i++ {
		a := parts[i]
		switch {
		case a == "--tab" || a == "-t":
			if i+1 < len(parts) {
				tabArg = parts[i+1]
				i++ // consume the value
			}
		case strings.HasPrefix(a, "--tab="):
			tabArg = strings.TrimPrefix(a, "--tab=")
		default:
			rest = append(rest, a)
		}
	}
	return tabArg, rest
}

// tabHint returns " --tab <tab>" when a tab is targeted, for echoing back in the
// empty-list hint so the suggested command stays scoped to that tab.
func tabHint(tabArg string) string {
	if tabArg == "" {
		return ""
	}
	return " --tab " + tabArg
}

// ---------------------------------------------------------------------------
// Shared ##secret / ##variable subcommand handler
// ---------------------------------------------------------------------------

// namedStoreCmd bundles a store with its command identity so the one subcommand
// handler serves both ##secret and ##variable. afterMut, when set, runs after a
// successful set/delete mutation — for secrets it re-syncs the SecretNAT known
// tier; for variables it is nil.
type namedStoreCmd struct {
	store    *namedStore
	afterMut func()
}

// label is the singular command verb / message tag ("secret" or "variable").
func (c namedStoreCmd) label() string { return c.store.kind.label }

// handleNamedStoreSubcommand implements the shared ##secret / ##variable command
// surface. Values live at two scopes: the WORKSPACE (the default) and a TAB.
// Within each scope they fill ${NAME} placeholders when agent/humanoid skill
// files load, resolving session bucket → .rysh/<dir>/<scope> → config[<scope>].
// When a skill is loaded from a pane the lookup is layered: the pane's tab scope
// is tried first (most specific), then the workspace scope, then the global
// environment — so a tab can override a shared workspace default.
//
// Storage defaults to the workspace; a "--tab <name|index|id>" flag targets a
// specific tab instead. Persistence to .rysh/<dir> is ON by default; --no-persist
// keeps the value in the session store only.
func (w *WorkspaceActor) handleNamedStoreSubcommand(out *strings.Builder, paneID string, parts []string, c namedStoreCmd) {
	label := c.label()
	// A --tab flag (anywhere in the args) selects tab-level storage; without it,
	// values default to this workspace's scope.
	tabArg, parts := splitTabFlag(parts)
	wsScope, wsLabel := w.secretWorkspaceScope()

	// scope/scopeDesc is the single scope that new/set/add, list and delete
	// operate on (workspace by default, or the named tab with --tab).
	scope := wsScope
	scopeDesc := fmt.Sprintf("workspace %q", wsLabel)
	if tabArg != "" {
		t := w.resolveTabArg(tabArg)
		if t == nil {
			fmt.Fprintf(out, "\n[%s] tab not found: %s  (see ##tab list)\n", label, tabArg)
			w.failRysh("tab not found: %s  (see ##tab list)", tabArg)
			return
		}
		tabLabel := t.title
		if tabLabel == "" {
			tabLabel = t.id
		}
		scope = sanitizeScope(t.id)
		scopeDesc = fmt.Sprintf("tab %q", tabLabel)
	}

	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "new", "set", "add":
		// Separate the persistence flags (allowed anywhere) from the positional
		// NAME and VALUE arguments. Persistence is ON by default: values are
		// written to .rysh/<dir>/<scope>/<NAME> unless --no-persist is given.
		// --persist/-p is still accepted as an explicit no-op for compatibility.
		persist := true
		var pos []string
		for _, a := range parts[1:] {
			if a == "--persist" || a == "-p" {
				persist = true
				continue
			}
			if a == "--no-persist" {
				persist = false
				continue
			}
			pos = append(pos, a)
		}
		if len(pos) < 2 {
			ryshWriter(out).UsageLineIn(label, fmt.Sprintf("##%s %s <NAME> <VALUE> [--no-persist] [--tab <tab>]", label, sub))
			w.failRyshUsage("usage: %s", fmt.Sprintf("##%s %s <NAME> <VALUE> [--no-persist] [--tab <tab>]", label, sub))
			return
		}
		name := pos[0]
		value := strings.Join(pos[1:], " ")
		if err := c.store.Set(scope, name, value, persist); err != nil {
			w.failRysh("%v", err)
			fmt.Fprintf(out, "\n[%s] %v\n", label, err)
			return
		}
		if c.afterMut != nil {
			c.afterMut()
		}
		if persist {
			fmt.Fprintf(out, "\n[%s] stored %q in %s (session + persisted to %s)\n",
				label, name, scopeDesc, filepath.Join(c.store.kind.writeDir(), scope, name))
		} else {
			fmt.Fprintf(out, "\n[%s] stored %q in %s (session store only)\n", label, name, scopeDesc)
		}

	case "list", "ls", "":
		infos := c.store.List(scope)
		fmt.Fprintf(out, "\n[%s] %s %ss (%d)\n", label, scopeDesc, label, len(infos))
		fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
		if len(infos) == 0 {
			fmt.Fprintf(out, "  (none in this %s — add one with ##%s new <NAME> <VALUE>%s)\n",
				strings.SplitN(scopeDesc, " ", 2)[0], label, tabHint(tabArg))
		} else {
			for _, in := range infos {
				fmt.Fprintf(out, "  %-28s  [%s]\n", in.Name, in.sourcesLabel())
			}
		}
		fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
		fmt.Fprintf(out, "  storage: session -> persist (%s/<scope>) -> config[<scope>]\n", c.store.kind.writeDir())
		fmt.Fprintf(out, "  a name tagged with several tiers resolves from the first listed\n")
		fmt.Fprintf(out, "  skill resolution from a pane: tab -> workspace -> environment (global)\n")

	case "get", "show":
		if len(parts) < 2 {
			ryshWriter(out).UsageLineIn(label, fmt.Sprintf("##%s get <NAME> [--tab <tab>]", label))
			w.failRyshUsage("usage: %s", fmt.Sprintf("##%s get <NAME> [--tab <tab>]", label))
			return
		}
		name := parts[1]
		// Reflect what a skill loaded from this pane would actually resolve for
		// this store: tab (the --tab override, else the pane's tab) → workspace → env.
		chain := []string{scope, wsScope}
		if tabArg == "" {
			chain = w.secretScopeChain(paneID)
		}
		v, src, fromScope, ok := c.store.GetLayered(chain, name)
		if !ok {
			fmt.Fprintf(out, "\n[%s] %q is not visible from this pane (tab/workspace session/persist/config) or the environment\n", label, name)
			w.failRysh("%s %q is not visible from this pane or the environment", label, name)
			return
		}
		fmt.Fprintf(out, "\n[%s] %s = %s   [%s, %s]\n", label, name, v, src, w.secretScopeLabel(fromScope, wsScope, wsLabel))

	case "delete", "rm", "del":
		if len(parts) < 2 {
			ryshWriter(out).UsageLineIn(label, fmt.Sprintf("##%s delete <NAME> [--tab <tab>]", label))
			w.failRyshUsage("usage: %s", fmt.Sprintf("##%s delete <NAME> [--tab <tab>]", label))
			return
		}
		name := parts[1]
		removedSession, removedPersist, err := c.store.Delete(scope, name)
		if err != nil {
			w.failRysh("%v", err)
			fmt.Fprintf(out, "\n[%s] %v\n", label, err)
			return
		}
		if c.afterMut != nil {
			c.afterMut()
		}
		switch {
		case removedSession && removedPersist:
			fmt.Fprintf(out, "\n[%s] deleted %q from %s (session + persisted file)\n", label, name, scopeDesc)
		case removedSession:
			fmt.Fprintf(out, "\n[%s] deleted %q from %s (session store)\n", label, name, scopeDesc)
		case removedPersist:
			fmt.Fprintf(out, "\n[%s] deleted persisted %s file for %q in %s\n", label, label, name, scopeDesc)
		default:
			fmt.Fprintf(out, "\n[%s] no session or persisted %s named %q in %s (config/env tiers are read-only)\n", label, label, name, scopeDesc)
			w.failRysh("no session or persisted %s named %q in %s", label, name, scopeDesc)
		}

	case "help":
		namedStoreHelp(out, c.store.kind)

	default:
		ryshWriter(out).UnknownIn(label, sub)
		w.failRyshUsage("unknown %s subcommand: %q", label, sub)
		namedStoreHelp(out, c.store.kind)
	}
}

// namedStoreHelp writes the ##secret / ##variable usage block.
func namedStoreHelp(out *strings.Builder, kind storeKind) {
	label := kind.label
	fmt.Fprintf(out, "\n[%s] usage (%ss default to the WORKSPACE; --tab targets a TAB):\n", label, label)
	fmt.Fprintf(out, "  ##%s new <NAME> <VALUE> [--no-persist] [--tab <tab>]  store a %s (alias: set);\n", label, label)
	fmt.Fprintf(out, "                                          persisted to %s/<scope>/<NAME> by default;\n", kind.writeDir())
	fmt.Fprintf(out, "                                          --no-persist keeps it in the session store only\n")
	fmt.Fprintf(out, "  ##%s list [--tab <tab>]              list the workspace's (or a tab's) %ss\n", label, label)
	fmt.Fprintf(out, "  ##%s get <NAME> [--tab <tab>]        resolve a %s as a skill here would\n", label, label)
	fmt.Fprintf(out, "  ##%s delete <NAME> [--tab <tab>]     remove a session + persisted %s\n", label, label)
	fmt.Fprintf(out, "  --tab takes a tab name, 1-based index, or id (see ##tab list); without it the\n")
	fmt.Fprintf(out, "  workspace scope is used. workspace %ss are shared by every tab and persist\n", label)
	fmt.Fprintf(out, "  per session. ${NAME} in agent/humanoid skills resolves tab -> workspace ->\n")
	fmt.Fprintf(out, "  environment (global), so a tab can override a shared workspace default.\n\n")
}
