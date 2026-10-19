// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

func PeerCommand(store *session.Store, sessionName, asPaneID string, args []string) (*msg.MsgCLIResponse, error) {
	c, err := connect(store, sessionName)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return cliRequest(c, &msg.MsgCLIPeerCommand{AsPaneID: asPaneID, Args: args})
}
