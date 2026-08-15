// SPDX-License-Identifier: Apache-2.0

// Answering a gated tool from a non-TUI client (E-12b).
//
// A tool call awaiting approval blocks in the orchestrator's waitForApproval
// until a MsgApprovalResponse arrives on pane.<paneid>.approval.response. The
// `approval_response` ws command (server.go) is the only thing that publishes
// one on behalf of the desktop UI, the browser UI and the phone.
//
//	client → server: approval_response {pane_id,request_id,decision,reason}
//	server → client: approval_error    {pane_id,request_id,error}
//
// The error direction exists because the phone has no console: an approval that
// is silently dropped looks exactly like one that is merely slow, while the
// tool blocks for five minutes and is then rejected.
package web

import (
	"encoding/json"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// validApprovalDecision reports whether the decision is one the orchestrator
// actually acts on (rysh-shared/msg: ApprovalDecision).
//
// This is not defensive tidiness. The two ends read an unknown value
// differently — the pane echo in server.go calls anything that is not a
// rejection "approved", while the orchestrator's switch takes its default
// branch and skips the tool — so an unknown string produces a pane that says
// approved and a tool that never ran. Forwarding it is the bug; refusing it at
// the door is the fix.
func validApprovalDecision(decision string) bool {
	switch msg.ApprovalDecision(decision) {
	case msg.DecisionYes, msg.DecisionYesAlways, msg.DecisionNo,
		msg.DecisionNoWithExplanation, msg.DecisionChoiceSelected:
		return true
	}
	return false
}

// pushApprovalError tells the client its answer was not delivered. Mirrors
// pushWebPaneError: broadcast, since the answering client is whichever one has
// the dialog open.
func (s *Server) pushApprovalError(paneID, requestID, errMsg string) {
	data, err := json.Marshal(map[string]interface{}{
		"type": "approval_error",
		"data": map[string]interface{}{
			"pane_id":    paneID,
			"request_id": requestID,
			"error":      errMsg,
		},
	})
	if err != nil {
		return
	}
	s.hub.sendAll(data)
}
