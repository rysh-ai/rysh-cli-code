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

// maxRememberedApprovalTypes bounds approvalTypes. An entry is removed when the
// web door answers it, but a request answered elsewhere (the TUI, a timeout)
// never comes back here, so past this size the OLDEST entries are evicted. The
// map used to be cleared wholesale instead, which wiped a question asked a
// moment earlier along with the stale gates (F-92). A question now loses its
// type only if this many newer requests arrive while it is still pending; then
// its answer is echoed with the gate labels (what ask_user hands the model is
// unaffected).
const maxRememberedApprovalTypes = 1024

// rememberApprovalType records the type of a request being forwarded to
// clients (F-92).
func (s *Server) rememberApprovalType(requestID string, t msg.ApprovalType) {
	if requestID == "" {
		return
	}
	s.approvalTypesMu.Lock()
	defer s.approvalTypesMu.Unlock()
	if s.approvalTypes == nil {
		s.approvalTypes = make(map[string]msg.ApprovalType)
	}
	if _, seen := s.approvalTypes[requestID]; !seen {
		s.approvalOrder = append(s.approvalOrder, requestID)
	}
	s.approvalTypes[requestID] = t
	// Evict oldest first. IDs already taken are still queued; skip them.
	for len(s.approvalTypes) > maxRememberedApprovalTypes {
		oldest := s.approvalOrder[0]
		s.approvalOrder = s.approvalOrder[1:]
		delete(s.approvalTypes, oldest)
	}
	// Taken IDs linger in the queue; compact it before it outgrows the map.
	if len(s.approvalOrder) > 2*maxRememberedApprovalTypes {
		live := make([]string, 0, len(s.approvalTypes))
		for _, id := range s.approvalOrder {
			if _, ok := s.approvalTypes[id]; ok {
				live = append(live, id)
			}
		}
		s.approvalOrder = live
	}
}

// takeApprovalType returns and forgets the type recorded for requestID, or ""
// when the request was never forwarded by this server.
func (s *Server) takeApprovalType(requestID string) msg.ApprovalType {
	s.approvalTypesMu.Lock()
	defer s.approvalTypesMu.Unlock()
	t := s.approvalTypes[requestID]
	delete(s.approvalTypes, requestID)
	return t
}
