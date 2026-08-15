// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// TestSpendStatus covers the status-bar spend indicator (design 003 §3.5):
// hidden at zero/no-warn, shows the dollar figure otherwise, and reflects the
// ceiling warning.
func TestSpendStatus(t *testing.T) {
	// Nothing spent, no ceiling near limit → hidden (keeps the footer clean).
	m := Model{snapshot: domain.WorkspaceSnapshot{SpendMicroUSD: 0, SpendWarn: false}}
	if got := m.spendStatus(); got != "" {
		t.Fatalf("spendStatus at zero = %q, want empty", got)
	}

	// Spend present → shows the dollar figure.
	m = Model{snapshot: domain.WorkspaceSnapshot{SpendMicroUSD: 1_420_000}}
	if got := m.spendStatus(); !strings.Contains(got, "$1.42") {
		t.Fatalf("spendStatus = %q, want to contain $1.42", got)
	}

	// A ceiling warning surfaces the indicator even at zero cost (e.g. unpriced
	// model with token spend near a ceiling).
	m = Model{snapshot: domain.WorkspaceSnapshot{SpendMicroUSD: 0, SpendWarn: true}}
	if got := m.spendStatus(); got == "" {
		t.Fatal("spendStatus with a ceiling warning should not be empty")
	}
}
