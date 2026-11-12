// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/hosted"
)

// C4 (rr-order-154): `##hosted board` shows the workspace's sandboxes as one
// board group (hosted.RenderBoard), marking the runs this machine is attached to.
func TestHostedBoard(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	api := &fakeHostedAPI{runs: []hosted.Run{
		{ID: "423b54f6fd36fa94", Name: "w", State: "running", Size: "large", ExpiresAt: now.Add(10 * time.Minute)},
	}}
	out, err := hostedBoard(context.Background(), api, map[string]bool{"423b54f6fd36fa94": true}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1 awake (4 units)", "large", "attached"} {
		if !strings.Contains(out, want) {
			t.Errorf("board lacks %q:\n%s", want, out)
		}
	}
	if !slices.Contains(hostedForms, "##hosted board") {
		t.Error("usage does not name ##hosted board")
	}
}
