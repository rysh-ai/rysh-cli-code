// SPDX-License-Identifier: Apache-2.0

package domain

import "testing"

// Only an explicit "off" pauses a pane's web stream. A typo, "false" or "0"
// must NOT silently freeze a pane in the browser.
func TestWebSyncPaused(t *testing.T) {
	for _, c := range []struct {
		meta map[string]string
		want bool
	}{
		{nil, false},
		{map[string]string{}, false},
		{map[string]string{PaneMetaWebSync: "on"}, false},
		{map[string]string{PaneMetaWebSync: "false"}, false},
		{map[string]string{PaneMetaWebSync: "of"}, false},
		{map[string]string{PaneMetaWebSync: "off"}, true},
		{map[string]string{PaneMetaWebSync: " OFF "}, true},
	} {
		if got := WebSyncPaused(c.meta); got != c.want {
			t.Errorf("WebSyncPaused(%v) = %v, want %v", c.meta, got, c.want)
		}
	}
}
