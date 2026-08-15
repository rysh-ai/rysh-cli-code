// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"strings"
	"sync"

	"github.com/rysh-ai/rysh-cli-code/internal/tools"
)

// ScopeKind identifies a level in the tool-visibility hierarchy. A tool enabled
// at a given scope is visible to AI executions whose pane falls under that scope.
// Widening order: Pane ⊂ Group ⊂ Lane ⊂ Tab ⊂ Global.
type ScopeKind int

const (
	ScopeGlobal ScopeKind = iota // session-wide (built-ins, startup-enabled integrations)
	ScopeTab                     // all panes in a tab (the default for ##integration enable)
	ScopeLane                    // all panes in a lane
	ScopeGroup                   // all panes in a pane-group
	ScopePane                    // a single pane
)

func (k ScopeKind) String() string {
	switch k {
	case ScopeGlobal:
		return "global"
	case ScopeTab:
		return "tab"
	case ScopeLane:
		return "lane"
	case ScopeGroup:
		return "panegroup"
	case ScopePane:
		return "pane"
	default:
		return "unknown"
	}
}

// ParseScope maps a user-supplied --scope token to a ScopeKind. An empty token
// defaults to ScopeTab (the documented default). "global" is intentionally not
// user-selectable — it is the daemon-wide default used at startup — so it is
// rejected here. Returns ok=false for unrecognized tokens.
func ParseScope(s string) (ScopeKind, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "tab":
		return ScopeTab, true
	case "lane":
		return ScopeLane, true
	case "panegroup", "group", "pg":
		return ScopeGroup, true
	case "pane":
		return ScopePane, true
	default:
		return ScopeGlobal, false
	}
}

// scopePrefix is the stable string prefix for a scope key (note: ScopeGroup
// uses "group" here, distinct from ScopeKind.String()'s user-facing "panegroup").
func scopePrefix(kind ScopeKind) string {
	switch kind {
	case ScopeTab:
		return "tab"
	case ScopeLane:
		return "lane"
	case ScopeGroup:
		return "group"
	case ScopePane:
		return "pane"
	default:
		return "global"
	}
}

func idForKind(kind ScopeKind, ids ScopeIDs) string {
	switch kind {
	case ScopeTab:
		return ids.TabID
	case ScopeLane:
		return ids.LaneID
	case ScopeGroup:
		return ids.GroupID
	case ScopePane:
		return ids.PaneID
	default:
		return ""
	}
}

// Hint encodes the scope ids into a compact opaque string carried on a prompt
// (MsgAgenticPrompt.ScopeHint), so an agent resolves tools against the invoking
// pane's scope. Format: "tab|lane|group|pane".
func (ids ScopeIDs) Hint() string {
	return strings.Join([]string{ids.TabID, ids.LaneID, ids.GroupID, ids.PaneID}, "|")
}

// ParseScopeHint is the inverse of ScopeIDs.Hint.
func ParseScopeHint(s string) ScopeIDs {
	p := strings.SplitN(s, "|", 4)
	var ids ScopeIDs
	if len(p) > 0 {
		ids.TabID = p[0]
	}
	if len(p) > 1 {
		ids.LaneID = p[1]
	}
	if len(p) > 2 {
		ids.GroupID = p[2]
	}
	if len(p) > 3 {
		ids.PaneID = p[3]
	}
	return ids
}

// GroupChainFor returns the registry chain a pane's group sits on
// (group→lane→tab→global), creating instances as needed. Agents inherit this as
// their parent so they see the integration tools enabled at the invoking pane's
// group/lane/tab/global — but not that pane's own pane-scoped or per-pane NATS
// tools.
func (s *ScopeRegistries) GroupChainFor(ids ScopeIDs) *tools.ToolRegistry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.groupReg(ids)
}

// ScopeKey returns a stable string identifying the scope instance selected by
// kind within ids (e.g. "lane:<laneID>", or "global"). Used as a display/tracking
// key by the forge/mcp managers.
func ScopeKey(kind ScopeKind, ids ScopeIDs) string {
	return ScopeKeyByID(kind, idForKind(kind, ids))
}

// ScopeKeyByID builds the same key from a scope kind and the bare instance id —
// used by actor teardown, which knows only its own id.
func ScopeKeyByID(kind ScopeKind, id string) string {
	if kind == ScopeGlobal || id == "" {
		return "global"
	}
	return scopePrefix(kind) + ":" + id
}

// ScopeIDs identifies the concrete scope instances an execution belongs to. Any
// field may be empty (e.g. a tab-only context), in which case that level is
// skipped and the next-wider scope becomes the parent.
type ScopeIDs struct {
	TabID   string
	LaneID  string
	GroupID string
	PaneID  string
}

// ScopeRegistries owns one tools.ToolRegistry per live scope instance and wires
// them into parent chains that mirror the actor tree:
//
//	pane:{id} → group:{id} → lane:{id} → tab:{id} → global
//
// A pane's per-run Clone() flattens this whole chain (see rysh-shared
// ToolRegistry.Clone), so a tool registered at, say, a lane registry is visible
// to every pane in that lane on its next prompt — and to no other lane. Tool
// registration/unregistration is performed by the forge/mcp managers against the
// registry returned by RegistryFor; this service only owns the instances and
// their parent wiring.
//
// It is a plain service (not an actor): its maps are guarded by mu because it is
// reached from the WorkspaceActor goroutine (enable/disable), pane creation, and
// per-run clones concurrently. The ToolRegistry instances are independently
// thread-safe.
type ScopeRegistries struct {
	mu     sync.Mutex
	global *tools.ToolRegistry
	tab    map[string]*tools.ToolRegistry
	lane   map[string]*tools.ToolRegistry
	group  map[string]*tools.ToolRegistry
	pane   map[string]*tools.ToolRegistry
}

// NewScopeRegistries creates a service whose global scope is the shared registry
// (built-ins + startup-enabled integrations live there, exactly as today).
func NewScopeRegistries(global *tools.ToolRegistry) *ScopeRegistries {
	return &ScopeRegistries{
		global: global,
		tab:    map[string]*tools.ToolRegistry{},
		lane:   map[string]*tools.ToolRegistry{},
		group:  map[string]*tools.ToolRegistry{},
		pane:   map[string]*tools.ToolRegistry{},
	}
}

// Global returns the session-wide registry (== the shared ToolRegistry).
func (s *ScopeRegistries) Global() *tools.ToolRegistry { return s.global }

// --- chain builders (callers must hold s.mu) ---

func (s *ScopeRegistries) tabReg(id string) *tools.ToolRegistry {
	if id == "" {
		return s.global
	}
	r := s.tab[id]
	if r == nil {
		r = tools.NewChildRegistry(s.global)
		s.tab[id] = r
	}
	return r
}

func (s *ScopeRegistries) laneReg(ids ScopeIDs) *tools.ToolRegistry {
	if ids.LaneID == "" {
		return s.tabReg(ids.TabID)
	}
	r := s.lane[ids.LaneID]
	if r == nil {
		r = tools.NewChildRegistry(s.tabReg(ids.TabID))
		s.lane[ids.LaneID] = r
	}
	return r
}

func (s *ScopeRegistries) groupReg(ids ScopeIDs) *tools.ToolRegistry {
	if ids.GroupID == "" {
		return s.laneReg(ids)
	}
	r := s.group[ids.GroupID]
	if r == nil {
		r = tools.NewChildRegistry(s.laneReg(ids))
		s.group[ids.GroupID] = r
	}
	return r
}

func (s *ScopeRegistries) paneReg(ids ScopeIDs) *tools.ToolRegistry {
	if ids.PaneID == "" {
		return s.groupReg(ids)
	}
	r := s.pane[ids.PaneID]
	if r == nil {
		r = tools.NewChildRegistry(s.groupReg(ids))
		s.pane[ids.PaneID] = r
	}
	return r
}

// RegistryFor returns (creating on first use, along with any missing ancestors)
// the registry for the scope instance identified by kind within ids. The forge/
// mcp managers register a tool's executors into this registry to enable it at
// that scope.
func (s *ScopeRegistries) RegistryFor(kind ScopeKind, ids ScopeIDs) *tools.ToolRegistry {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case ScopeGlobal:
		return s.global
	case ScopeTab:
		return s.tabReg(ids.TabID)
	case ScopeLane:
		return s.laneReg(ids)
	case ScopeGroup:
		return s.groupReg(ids)
	case ScopePane:
		return s.paneReg(ids)
	default:
		return s.global
	}
}

// PaneChain returns a fresh per-pane registry parented at group→lane→tab→global,
// stored under the pane id. Callers (CreateLLMPromptExecutionActor) register the
// per-pane NATS tools into it; the orchestrator clones it per run, flattening the
// whole chain live. Creating fresh each call ensures a re-created pane does not
// inherit a previous incarnation's per-pane tools.
func (s *ScopeRegistries) PaneChain(ids ScopeIDs) *tools.ToolRegistry {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := tools.NewChildRegistry(s.groupReg(ids))
	if ids.PaneID != "" {
		s.pane[ids.PaneID] = r
	}
	return r
}

// Close forgets a scope instance's registry. The actor that owns the scope
// (Tab/Lane/PaneGroup/Pane) calls this when it stops; because actor teardown
// cascades, each descendant also calls Close, so no stale parent links survive.
// Tool unregistration is the manager's responsibility (it tracks what it enabled
// at each scope and Unregisters before/around Close); this only drops the
// instance so a later same-id scope starts clean.
func (s *ScopeRegistries) Close(kind ScopeKind, id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case ScopeTab:
		delete(s.tab, id)
	case ScopeLane:
		delete(s.lane, id)
	case ScopeGroup:
		delete(s.group, id)
	case ScopePane:
		delete(s.pane, id)
	}
}
