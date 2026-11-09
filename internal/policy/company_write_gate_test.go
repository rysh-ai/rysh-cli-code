// SPDX-License-Identifier: Apache-2.0

package policy

import "testing"

// CD5 C-C2 (security-lead): the company write tools always stop for a human,
// with or without a policy file, and no permissive rule can un-gate them.
func TestCompanyWriteToolsAreAlwaysGated(t *testing.T) {
	for _, tool := range []string{"company_enquiry_update", "company_order_status"} {
		var nilPolicy *Policy
		if act, id := nilPolicy.EvaluateToolApproval(tool, nil); act != ApprovalGate || id == "" {
			t.Errorf("%s with no policy: %v %q, want a gate", tool, act, id)
		}
		p := &Policy{AutoApprove: []string{"*"}, Loaded: true}
		if act, _ := p.EvaluateToolApproval(tool, nil); act != ApprovalGate {
			t.Errorf("%s with auto_approve '*': %v, want a gate", tool, act)
		}
	}
	// The read tools are untouched.
	var nilPolicy *Policy
	if act, _ := nilPolicy.EvaluateToolApproval("company_ledger", nil); act != ApprovalDefault {
		t.Errorf("company_ledger: %v, want the default", act)
	}
}
