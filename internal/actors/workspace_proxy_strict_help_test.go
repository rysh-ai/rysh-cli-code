// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/proxy"
)

// `##proxy status` states what strict mode covers, and no more
// (root-order-039). The proxy server is never started: the status only reads
// its configuration.
func TestProxyStatusStatesWhatStrictCovers(t *testing.T) {
	w := &WorkspaceActor{proxyServer: proxy.New(nil, nil, nil, nil, false)}
	w.cfg.Proxy.Strict = true
	var out strings.Builder
	w.renderProxyStatus(&out)

	want := "  strict      ON — an ungoverned agent CLI is stopped, not just warned\n" +
		"              covers the allowed CLIs at their known install locations, by real executable;\n" +
		"              not an anti-evasion boundary: a copy elsewhere is unknown and not watched\n"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("##proxy status does not state strict's coverage exactly.\nwant:\n%s\ngot:\n%s", want, out.String())
	}
}
