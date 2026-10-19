// SPDX-License-Identifier: Apache-2.0

package tui

import "testing"

// emailRailWindow keeps the rail cursor visible when the account has more
// folders than the rail has rows (Gmail label lists run long).
func TestEmailRailWindow(t *testing.T) {
	rows := []string{"r0", "r1", "r2", "r3", "r4", "r5"}
	for _, tc := range []struct {
		sel, h    int
		wantFirst string
		wantLen   int
	}{
		{0, 3, "r0", 6},  // cursor in the first screenful: no scroll (joinRows clips)
		{2, 3, "r0", 6},  // still visible
		{3, 3, "r1", 5},  // one past the screen: scroll so r3 is the last visible row
		{5, 3, "r3", 3},  // the last folder
		{5, 10, "r0", 6}, // everything fits
		{4, 0, "r0", 6},  // no height: leave alone
	} {
		got := emailRailWindow(rows, tc.sel, tc.h)
		if len(got) != tc.wantLen || got[0] != tc.wantFirst {
			t.Errorf("emailRailWindow(sel=%d, h=%d) = %v, want first %q len %d", tc.sel, tc.h, got, tc.wantFirst, tc.wantLen)
			continue
		}
		if tc.h > 0 && tc.sel-(len(rows)-len(got)) >= tc.h {
			t.Errorf("sel=%d h=%d: cursor row is outside the visible window %v", tc.sel, tc.h, got)
		}
	}
}
