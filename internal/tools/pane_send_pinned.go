// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// PinnedPaneSendTool is pane_send for a humanoid: while pinned() names a pane
// (the chat thread is pinned with /use), only that pane may be sent to. With
// no pin it is the plain tool. Name and approval are pane_send's.
type PinnedPaneSendTool struct {
	inner  *PaneSendTool
	pinned func() string
}

// NewPinnedPaneSendTool wraps inner; pinned returns the current turn's pinned
// pane id, or "" when no pin applies.
func NewPinnedPaneSendTool(inner *PaneSendTool, pinned func() string) *PinnedPaneSendTool {
	return &PinnedPaneSendTool{inner: inner, pinned: pinned}
}

func (t *PinnedPaneSendTool) Spec() ToolSpec { return t.inner.Spec() }

func (t *PinnedPaneSendTool) RequiresApproval(p json.RawMessage) bool {
	return t.inner.RequiresApproval(p)
}

func (t *PinnedPaneSendTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	if pin := t.pinned(); pin != "" {
		var p PaneSendParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid pane_send params: %w", err)
		}
		if p.PaneID != pin {
			return ErrOutput(ErrKindValidation, fmt.Sprintf(
				"this chat is pinned to pane %s; pane_send may reach only the pinned pane (/use off ends the pin)", pin)), nil
		}
	}
	return t.inner.Execute(ctx, params)
}
