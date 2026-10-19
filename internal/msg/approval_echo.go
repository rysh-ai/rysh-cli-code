// SPDX-License-Identifier: Apache-2.0

package msg

import "fmt"

// QuestionEchoLabel is the pane echo for an answer to a question — an
// approval request of ApprovalTypeQuestion, which only the ask_user tool sends.
// A question is not a gate, so its answer is never echoed as approved or
// rejected (F-92). It mirrors what ask_user hands the model: yes/yes_always is
// "yes" (or the typed text), no is a refusal to answer, no_with_explanation is
// the typed text, and a choice is the option picked. Used by the web door and
// the TUI so the two cannot drift.
func QuestionEchoLabel(decision ApprovalDecision, reason string, choiceIdx int) string {
	switch decision {
	case DecisionChoiceSelected:
		label := fmt.Sprintf("chose option %d", choiceIdx+1)
		if reason != "" {
			label += ": " + reason
		}
		return label
	case DecisionNo:
		return "declined"
	case DecisionYes, DecisionYesAlways:
		if reason == "" {
			return "answered: yes"
		}
	}
	return "answered: " + reason
}
