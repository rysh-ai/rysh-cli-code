// SPDX-License-Identifier: Apache-2.0

package msg

import (
	"encoding/json"
	"testing"
)

func TestEmailSummaryCarriesReferences(t *testing.T) {
	b, _ := json.Marshal(EmailSummary{UID: 1, References: []string{"<a@x>", "<b@x>"}})
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	refs, ok := got["references"].([]any)
	if !ok || len(refs) != 2 {
		t.Fatalf("EmailSummary JSON %s has no references list", b)
	}
}
