// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
)

// ForgedAPIProxy is a subscriber-side stand-in for one of a remote owner's
// forged-API operations (Task 2). The subscriber's agent sees it as a normal
// tool; calling it forwards an `invoke_op` request to the owner, who runs the
// real operation and returns a (redacted) result.
//
//   - phase 2a / inert (invoke == nil): registration and discovery work
//     end-to-end (the op appears in the subscriber pane's tool list) but Execute
//     returns a "discovery-only" message instead of performing a remote call.
//   - phase 2b / active (invoke != nil): Execute delegates to the invoke callback,
//     which performs the remote round-trip. The callback is supplied by the
//     PaneActor and carries the NATS plumbing, so this package stays free of any
//     NATS / message dependency.
//
// There is no per-call approval for shared forged APIs (the owner's allow-list is
// the gate), so RequiresApproval always returns false.
type ForgedAPIProxy struct {
	shareID string
	spec    ToolSpec
	// invoke performs the remote call (op name, raw args) → result. Nil ⇒ inert.
	invoke func(ctx context.Context, op string, args json.RawMessage) (*ToolOutput, error)
}

// NewForgedAPIProxy builds an active (phase 2b) proxy for one shared op. Passing
// a nil invoke yields the inert (phase 2a) behaviour; prefer NewInertForgedAPIProxy
// for that to make the intent explicit.
func NewForgedAPIProxy(shareID, name, description, schema string, mutating bool, invoke func(ctx context.Context, op string, args json.RawMessage) (*ToolOutput, error)) *ForgedAPIProxy {
	sc := json.RawMessage(schema)
	if len(sc) == 0 {
		sc = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return &ForgedAPIProxy{
		shareID: shareID,
		invoke:  invoke,
		spec: ToolSpec{
			Name:             name,
			Description:      "[shared-api] " + description,
			Parameters:       sc,
			RequiresApproval: false,
		},
	}
}

// NewInertForgedAPIProxy builds a phase-2a (inert) proxy for one shared op.
func NewInertForgedAPIProxy(shareID, name, description, schema string, mutating bool) *ForgedAPIProxy {
	return NewForgedAPIProxy(shareID, name, description, schema, mutating, nil)
}

// Spec returns the forwarded operation spec.
func (p *ForgedAPIProxy) Spec() ToolSpec { return p.spec }

// RequiresApproval is always false — shared forged APIs have no per-call approval.
func (p *ForgedAPIProxy) RequiresApproval(json.RawMessage) bool { return false }

// Execute delegates to the remote invoke callback, or returns a discovery-only
// message when inert.
func (p *ForgedAPIProxy) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	if p.invoke == nil {
		return ErrOutput(ErrKindPermissionDenied,
			"forged-API sharing is discovery-only for this share: remote invocation is not enabled"), nil
	}
	return p.invoke(ctx, p.spec.Name, params)
}
