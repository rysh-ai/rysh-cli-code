// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// Design 066 S3: a sandboxed agent pane says so in its border title.
func TestPaneBorderTitleShowsSandbox(t *testing.T) {
	var m Model
	p := domain.PaneSnapshot{ID: "p1", Title: "brave-otter", GivenName: "worker",
		Meta: map[string]string{"agent.sandbox": "box"}}
	if got := m.paneBorderTitle(p); !strings.HasSuffix(got, " | [sbx:box]") {
		t.Fatalf("title = %q", got)
	}
	p.Meta = nil
	if got := m.paneBorderTitle(p); strings.Contains(got, "sbx") {
		t.Fatalf("unsandboxed title = %q", got)
	}
}
