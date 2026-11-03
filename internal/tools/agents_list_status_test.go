// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

func TestPaneListStatusShowsNeedsYou(t *testing.T) {
	cases := []struct {
		name string
		pane domain.PaneSnapshot
		want string
	}{
		{"empty status defaults to idle", domain.PaneSnapshot{}, "idle"},
		{"own status kept", domain.PaneSnapshot{Status: "running"}, "running"},
		{"needs you with reason", domain.PaneSnapshot{Status: "running", Meta: map[string]string{
			domain.PaneMetaAttention: domain.AttentionNeedsYou, domain.PaneMetaAttentionReason: domain.AttentionReasonPrompt,
		}}, "needs you (prompt)"},
		{"needs you without reason", domain.PaneSnapshot{Meta: map[string]string{
			domain.PaneMetaAttention: domain.AttentionNeedsYou,
		}}, "needs you"},
	}
	for _, c := range cases {
		if got := paneListStatus(c.pane); got != c.want {
			t.Errorf("%s: paneListStatus = %q, want %q", c.name, got, c.want)
		}
	}
}

// A remote pane (##remote, design 070) is tagged with its host, and shows the
// working/idle state its remote rysh observed.
func TestPaneListStatusTagsRemoteHost(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]string
		want string
	}{
		{"remote working", map[string]string{domain.PaneMetaRemoteHost: "devbox", domain.PaneMetaActivity: domain.PaneActivityWorking}, "working host:devbox"},
		{"remote idle", map[string]string{domain.PaneMetaRemoteHost: "devbox", domain.PaneMetaActivity: domain.PaneActivityIdle}, "idle host:devbox"},
		{"remote needs you", map[string]string{domain.PaneMetaRemoteHost: "devbox",
			domain.PaneMetaAttention: domain.AttentionNeedsYou, domain.PaneMetaAttentionReason: domain.AttentionReasonPrompt}, "needs you (prompt) host:devbox"},
	}
	for _, c := range cases {
		if got := paneListStatus(domain.PaneSnapshot{Meta: c.meta}); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
