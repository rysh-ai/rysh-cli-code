package agentic

import (
	"encoding/json"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// browserActionPublisher adapts *msg.NATSPublisher to tools.BrowserActionPublisher
// so the shared browser_action tool can publish action requests without importing
// the msg package directly (which would cause a cross-module import conflict).
//
// It publishes a MsgBrowserActionRequest envelope to the pane's browser.request
// subject; a connected web-pane executor (the desktop app) runs the action and
// replies on browser.response, which the tool awaits.
type browserActionPublisher struct{ pub *msg.NATSPublisher }

func (b browserActionPublisher) SendBrowserAction(subject, requestID, action string, params json.RawMessage) error {
	return b.pub.Send(subject, &msg.MsgBrowserActionRequest{
		RequestID: requestID,
		Action:    action,
		Params:    params,
	})
}
