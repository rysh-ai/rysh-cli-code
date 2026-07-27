package actors

import "testing"

// TestParseReplayPlayArgs covers the ##replay play flag surface (design 006
// §3.2): --pane / --from / --speed parse into their specs, --here selects the
// v1 in-pane playback, and anything unknown or valueless is an error — a
// mistyped flag must not silently play the whole session at 1x.
func TestParseReplayPlayArgs(t *testing.T) {
	pane, from, speed, here, err := parseReplayPlayArgs(nil)
	if err != nil || pane != "" || from != "" || speed != "" || here {
		t.Fatalf("no args = (%q,%q,%q,%v,%v), want empties", pane, from, speed, here, err)
	}

	pane, from, speed, here, err = parseReplayPlayArgs([]string{"--pane", "p1", "--from", "90s", "--speed", "4"})
	if err != nil || pane != "p1" || from != "90s" || speed != "4" || here {
		t.Fatalf("full args = (%q,%q,%q,%v,%v), want (p1,90s,4,false,nil)", pane, from, speed, here, err)
	}

	// --here keeps the v1 in-pane behavior (same spelling tolerance as
	// ##new grid's --here).
	for _, spelling := range []string{"--here", "-here", "here"} {
		_, _, _, here, err = parseReplayPlayArgs([]string{spelling})
		if err != nil || !here {
			t.Fatalf("%s = (here=%v, err=%v), want (true, nil)", spelling, here, err)
		}
	}

	if _, _, _, _, err := parseReplayPlayArgs([]string{"--speed"}); err == nil {
		t.Fatal("valueless --speed: want error")
	}
	if _, _, _, _, err := parseReplayPlayArgs([]string{"--spede", "4"}); err == nil {
		t.Fatal("unknown flag: want error")
	}
}
