// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"

	sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"

	"github.com/rysh-ai/rysh-cli-code/internal/proxy"
)

// root-order-044 #8: `##proxy status` shows the workspace spend cap, marked as
// an estimate, and nothing when there is none.
func TestProxyStatusSpendCapLine(t *testing.T) {
	got := spendCapStatusLine(sharedmsg.GatewaySpendCap{Period: "month", CapMicroUSD: 50_000_000, SpentMicroUSD: 12_340_000}, true)
	want := "  spend cap   $12.34 of $50.00 this month (UTC), estimated from rysh's price table\n"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	got = spendCapStatusLine(sharedmsg.GatewaySpendCap{Period: "day", CapMicroUSD: 1_000_000, SpentMicroUSD: 0}, true)
	if !strings.Contains(got, "$0.00 of $1.00 today (UTC)") {
		t.Fatalf("day line = %q", got)
	}
	if got := spendCapStatusLine(sharedmsg.GatewaySpendCap{}, false); got != "" {
		t.Fatalf("no cap rendered %q", got)
	}
}

func TestProxyStatusNoLedgerNoSpendCapLine(t *testing.T) {
	w := &WorkspaceActor{proxyServer: proxy.New(nil, nil, nil, nil, false)}
	var out strings.Builder
	w.renderProxyStatus(&out)
	if strings.Contains(out.String(), "spend cap") {
		t.Fatalf("spend cap shown with no governance:\n%s", out.String())
	}
}
