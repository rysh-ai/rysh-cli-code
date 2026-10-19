// SPDX-License-Identifier: Apache-2.0

package msg

// MsgCLIPeerCommand is the silent native-agent door to the peer board. A
// sender is explicit; it never inherits the human's focused pane.
type MsgCLIPeerCommand struct {
	AsPaneID string   `json:"as_pane_id,omitempty"`
	Args     []string `json:"args"`
}
