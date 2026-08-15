// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"path"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/runtime"
)

// RunOp executes a forge-registered operation by name on the owner side of a
// forged-API share (Task 2, phase 2b). It is the *forge-origin-enforcing*
// runner: it only ever runs an operation this Manager registered (looked up in
// the live `active` set). A built-in / MCP / per-pane tool name — or any op the
// owner never enabled — fails the lookup and returns ErrKindMissing, so the
// invocation path can never reach a non-forge executor.
//
// The op runs with the owner's real credentials (the forge executor holds the
// upstream base URL + auth). Redaction and the allow/deny gate are applied by
// the caller (UpstreamShareActor) BEFORE and AFTER this call — RunOp itself
// performs no policy check beyond forge-origin, so it stays a pure primitive.
func (m *Manager) RunOp(ctx context.Context, opName string, args json.RawMessage) (*sharedtools.ToolOutput, error) {
	return m.RunOpWithAuth(ctx, opName, args, "")
}

// RunOpWithAuth is RunOp with an optional per-call bearer override (forged-API
// auth plan, Model B / delegated identity). When bearer is non-empty the op
// executes under the SUBSCRIBER's token for this one request: the token is
// injected via the request context and is never cached or persisted on the
// owner. An empty bearer is identical to RunOp (Model A / owner identity).
func (m *Manager) RunOpWithAuth(ctx context.Context, opName string, args json.RawMessage, bearer string) (*sharedtools.ToolOutput, error) {
	if bearer != "" {
		ctx = runtime.WithBearer(ctx, bearer)
	}
	m.mu.Lock()
	var target *sharedtools.ToolRegistry
	for _, a := range m.active {
		if a.target == nil {
			continue
		}
		for _, n := range a.registered {
			if n == opName {
				target = a.target
				break
			}
		}
		if target != nil {
			break
		}
	}
	m.mu.Unlock()

	if target == nil {
		return nil, fmt.Errorf("forge op %q is not enabled (forge-origin check failed)", opName)
	}
	ex, ok := target.Get(opName)
	if !ok {
		return nil, fmt.Errorf("forge op %q not found in its scope registry", opName)
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return ex.Execute(ctx, args)
}

// AllowSharedOp reports whether a remote subscriber may invoke op `opName`
// (with the given `mutating` flag) given the owner's allow / block glob lists.
// It encodes the "default-deny mutation" policy from the plan:
//
//   - The block list always wins: any match → denied.
//   - If the allow list is non-empty, the op MUST match it to be permitted
//     (an explicit owner opt-in for exactly the ops they choose to expose).
//   - If the allow list is empty, read ops (non-mutating) are permitted but
//     mutating ops are denied — the owner must opt mutations in explicitly.
//
// Patterns are shell-style globs over the op name (e.g. "weather_*",
// "*_getPet"); a malformed pattern simply never matches.
func AllowSharedOp(opName string, mutating bool, allow, block []string) bool {
	for _, g := range block {
		if globMatch(g, opName) {
			return false
		}
	}
	if len(allow) > 0 {
		for _, g := range allow {
			if globMatch(g, opName) {
				return true
			}
		}
		return false
	}
	// Empty allow list: reads flow, mutations are denied by default.
	return !mutating
}

func globMatch(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}
