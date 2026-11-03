// SPDX-License-Identifier: Apache-2.0

package domain

import "testing"

func TestPaneNeedsYouReadsOnlyTheExactState(t *testing.T) {
	cases := []struct {
		name       string
		meta       map[string]string
		wantReason string
		wantOK     bool
	}{
		{"nil meta", nil, "", false},
		{"no key", map[string]string{"fleet.role": "worker"}, "", false},
		{"needs you, prompt", map[string]string{PaneMetaAttention: AttentionNeedsYou, PaneMetaAttentionReason: AttentionReasonPrompt}, AttentionReasonPrompt, true},
		{"needs you, approval", map[string]string{PaneMetaAttention: AttentionNeedsYou, PaneMetaAttentionReason: AttentionReasonApproval}, AttentionReasonApproval, true},
		{"needs you, no reason", map[string]string{PaneMetaAttention: AttentionNeedsYou}, "", true},
		{"other value", map[string]string{PaneMetaAttention: "maybe"}, "", false},
	}
	for _, c := range cases {
		reason, ok := PaneNeedsYou(c.meta)
		if reason != c.wantReason || ok != c.wantOK {
			t.Errorf("%s: PaneNeedsYou = (%q, %v), want (%q, %v)", c.name, reason, ok, c.wantReason, c.wantOK)
		}
	}
}

func TestAttentionMetaKeysAreRecognised(t *testing.T) {
	for _, k := range []string{PaneMetaAttention, PaneMetaAttentionReason, PaneMetaAttentionSince} {
		if !IsAttentionMetaKey(k) {
			t.Errorf("IsAttentionMetaKey(%q) = false", k)
		}
	}
	for _, k := range []string{"", "fleet.role", PaneMetaWebSync, "attention2"} {
		if IsAttentionMetaKey(k) {
			t.Errorf("IsAttentionMetaKey(%q) = true", k)
		}
	}
}
