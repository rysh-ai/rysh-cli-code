package actors

import "testing"

// TestParseReplayPlayArgs covers the ##replay play flag surface (design 006
// §3.2): --pane / --from / --speed parse into their specs, and anything
// unknown or valueless is an error — a mistyped flag must not silently play
// the whole session at 1x.
func TestParseReplayPlayArgs(t *testing.T) {
	pane, from, speed, err := parseReplayPlayArgs(nil)
	if err != nil || pane != "" || from != "" || speed != "" {
		t.Fatalf("no args = (%q,%q,%q,%v), want empties", pane, from, speed, err)
	}

	pane, from, speed, err = parseReplayPlayArgs([]string{"--pane", "p1", "--from", "90s", "--speed", "4"})
	if err != nil || pane != "p1" || from != "90s" || speed != "4" {
		t.Fatalf("full args = (%q,%q,%q,%v), want (p1,90s,4,nil)", pane, from, speed, err)
	}

	if _, _, _, err := parseReplayPlayArgs([]string{"--speed"}); err == nil {
		t.Fatal("valueless --speed: want error")
	}
	if _, _, _, err := parseReplayPlayArgs([]string{"--spede", "4"}); err == nil {
		t.Fatal("unknown flag: want error")
	}
}
