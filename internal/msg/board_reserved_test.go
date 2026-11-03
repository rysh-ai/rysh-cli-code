// SPDX-License-Identifier: Apache-2.0

package msg

import "testing"

// Design 030 §9.6 Q1 (D-25): `fleet` and `session` are reserved. They stay
// ordinary, postable boards (agents post to `--board fleet` today); reserved
// means no fleet may take them as its name or board.
func TestW1ReservedBoardIDs(t *testing.T) {
	for _, id := range []string{"", "session", "SESSION", "fleet", " Fleet "} {
		if !IsReservedBoardID(id) {
			t.Errorf("IsReservedBoardID(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"fleets", "fleet-a", "root", "epic-07"} {
		if IsReservedBoardID(id) {
			t.Errorf("IsReservedBoardID(%q) = true, want false", id)
		}
	}
	if FleetBoardID != "fleet" {
		t.Errorf("FleetBoardID = %q, want fleet (D-25)", FleetBoardID)
	}
	// Still legal board ids: posting to them must keep working.
	for _, id := range []string{"fleet", "session"} {
		if err := ValidateBoardID(id); err != nil {
			t.Errorf("ValidateBoardID(%q) = %v; a reserved board is still a board", id, err)
		}
	}
}
