// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestMeetingJoinValidationAndControlParsing(t *testing.T) {
	good := []string{"join", "https://meet.google.com/abc-defg-hij", "--public-url", "https://meeting.example", "--consent", "--session", "test"}
	c, sess, _, err := parseMeetingArgs(good)
	if err != nil || sess != "test" || !c.Consent {
		t.Fatal(c, err)
	}
	for _, args := range [][]string{
		{"join", "https://meet.google.com/abc-defg-hij", "--public-url", "https://meeting.example"},
		{"join", "https://evil.test/abc-defg-hij", "--public-url", "https://meeting.example", "--consent"},
		{"join", "https://meet.google.com/abc-defg-hij", "--public-url", "http://meeting.example", "--consent"},
		{"join", "https://meet.google.com/abc-defg-hij", "--public-url", "https://meeting.example", "--listen", "0.0.0.0:8791", "--consent"},
		{"status", "--api-key", "secret"}, {"approve"}, {"doctor", "--tts", "unknown"},
		{"join", "https://meet.google.com/abc-defg-hij", "--public-url", "https://meeting.example", "--consent", "--auto-speak"},
		{"join", "https://meet.google.com/abc-defg-hij", "--public-url", "https://meeting.example", "--consent", "--allow", "anyone,Bob"},
		{"join", "https://meet.google.com/abc-defg-hij", "--public-url", "https://meeting.example", "--consent", "--allow", "Ann,,Bob"},
	} {
		if _, _, _, err := parseMeetingArgs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if c.Approve || len(c.Allow) != 1 || c.Allow[0] != "host" {
		t.Fatal("default join must be hands-free, host only:", c)
	}
	c, _, _, err = parseMeetingArgs(append(good, "--allow", "host, Halil Ağın", "--approve", "--auto-speak"))
	if err != nil || !c.Approve || !c.AutoSpeak || len(c.Allow) != 2 || c.Allow[1] != "Halil Ağın" {
		t.Fatal(c, err)
	}
	c, _, _, err = parseMeetingArgs([]string{"approve", "turn-1"})
	if err != nil || c.TurnID != "turn-1" {
		t.Fatal(c, err)
	}
}

func TestMeetingAgentFlagsParsing(t *testing.T) {
	join := []string{"join", "https://meet.google.com/abc-defg-hij", "--public-url", "https://meeting.example", "--consent", "--agent", "scribe,code-reviewer"}
	c, _, _, err := parseMeetingArgs(join)
	if err != nil || len(c.Agents) != 2 || c.Agents[0] != "scribe" || c.Agents[1] != "code-reviewer" {
		t.Fatal(c, err)
	}
	c, _, _, err = parseMeetingArgs([]string{"attach", "scribe"})
	if err != nil || c.Action != "attach" || c.Agent != "scribe" {
		t.Fatal(c, err)
	}
	for _, args := range [][]string{
		append(join[:5:5], "--agent", "rysh"),
		append(join[:5:5], "--agent", "a1,b"),
		append(join[:5:5], "--agent", "aa,bb,cc,dd,ee"),
		{"attach"}, {"detach", "--json"}, {"attach", "scribe", "planner"}, {"detach", "hey-bot"},
	} {
		if _, _, _, err := parseMeetingArgs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestMeetingHostParsing(t *testing.T) {
	c, sess, _, err := parseMeetingArgs([]string{"host", "--room", "standup", "--allow", "host,alice", "--agent", "scribe", "--session", "s1"})
	if err != nil || c.Action != "host" || c.Room != "standup" || sess != "s1" || len(c.Allow) != 2 || c.Allow[1] != "alice" || len(c.Agents) != 1 || c.URL != "" {
		t.Fatal(c, err)
	}
	if c, _, _, err := parseMeetingArgs([]string{"host"}); err != nil || c.Room != "" {
		t.Fatal(c, err)
	}
	for _, args := range [][]string{
		{"host", "https://meet.google.com/abc-defg-hij"},
		{"host", "--room", "../etc"},
		{"host", "--public-url", "https://meeting.example"},
		{"host", "--consent"},
		{"host", "--zoom-obf"},
		{"join", "https://meet.google.com/abc-defg-hij", "--public-url", "https://meeting.example", "--consent", "--room", "x"},
	} {
		if _, _, _, err := parseMeetingArgs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestMeetingHostServerParsing(t *testing.T) {
	c, _, _, err := parseMeetingArgs([]string{"host", "--server", "https://rysh.ai", "--allow", "host,0b6f3c5e-1111-4222-8333-944455556666"})
	if err != nil || c.Server != "https://rysh.ai" || len(c.Allow) != 2 {
		t.Fatal(c, err)
	}
	for _, args := range [][]string{
		{"host", "--server", "http://rysh.ai"},
		{"host", "--server", "https://u:p@rysh.ai"},
		{"join", "https://meet.google.com/abc-defg-hij", "--public-url", "https://meeting.example", "--consent", "--server", "https://rysh.ai"},
	} {
		if _, _, _, err := parseMeetingArgs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestMeetingIDAndListParsing(t *testing.T) {
	c, _, _, err := parseMeetingArgs([]string{"status", "--id", "m-3f9a2c"})
	if err != nil || c.ID != "m-3f9a2c" || c.Action != "status" {
		t.Fatal(c, err)
	}
	c, _, _, err = parseMeetingArgs([]string{"attach", "scribe", "--id", "m-3f9a2c"})
	if err != nil || c.ID != "m-3f9a2c" || c.Agent != "scribe" {
		t.Fatal(c, err)
	}
	// join into a pane meeting: its URL comes from the registry.
	c, _, _, err = parseMeetingArgs([]string{"join", "--id", "m-3f9a2c", "--public-url", "https://meeting.example", "--consent"})
	if err != nil || c.ID != "m-3f9a2c" || c.URL != "" {
		t.Fatal(c, err)
	}
	c, _, _, err = parseMeetingArgs([]string{"list", "--json"})
	if err != nil || c.Action != "list" {
		t.Fatal(c, err)
	}
	for _, args := range [][]string{
		{"status", "--id", "3f9a2c"}, {"status", "--id", "m-3F9A2C"}, {"leave", "--id", "m-3f9a2c0"},
		{"list", "extra"}, {"list", "--id", "m-3f9a2c"}, {"host", "--id", "m-3f9a2c"}, {"doctor", "--id", "m-3f9a2c"},
		{"join", "--public-url", "https://meeting.example", "--consent"},
		{"register"}, {"unregister"},
	} {
		if _, _, _, err := parseMeetingArgs(args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

// Design 048: --tts/--stt are validated here, but an unset one reaches the
// daemon empty so MEETING_TTS / MEETING_STT can fill it there.
func TestMeetingSpeechProviderFlags(t *testing.T) {
	c, _, _, err := parseMeetingArgs([]string{"host", "--tts", "elevenlabs", "--stt", "elevenlabs"})
	if err != nil || c.TTS != "elevenlabs" || c.STT != "elevenlabs" {
		t.Fatal(c, err)
	}
	c, _, _, err = parseMeetingArgs([]string{"host"})
	if err != nil || c.TTS != "" || c.STT != "" || c.Minutes != 60 {
		t.Fatal(c, err)
	}
	c, _, _, err = parseMeetingArgs([]string{"join", "https://meet.google.com/abc-defg-hij", "--public-url", "https://meeting.example", "--consent", "--tts", "elevenlabs"})
	if err != nil || c.TTS != "elevenlabs" || c.STT != "" {
		t.Fatal(c, err)
	}
	c, _, _, err = parseMeetingArgs([]string{"doctor", "--tts", "elevenlabs", "--stt", "elevenlabs"})
	if err != nil || c.TTS != "elevenlabs" || c.STT != "elevenlabs" {
		t.Fatal(c, err)
	}
	for _, args := range [][]string{
		{"host", "--tts", "google"}, {"host", "--stt", "openai"}, {"doctor", "--stt", "openai"},
		{"join", "https://meet.google.com/abc-defg-hij", "--public-url", "https://meeting.example", "--consent", "--tts", "eleven"},
	} {
		if _, _, _, err := parseMeetingArgs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	c, _, _, err = parseMeetingArgs([]string{"host", "--tts", "elevenlabs", "--stt", "elevenlabs", "--lang", "KMR"})
	if err != nil || c.Lang != "kmr" {
		t.Fatal(c, err)
	}
	if _, _, _, err := parseMeetingArgs([]string{"host", "--tts", "deepgram", "--lang", "kmr"}); err == nil {
		t.Fatal("deepgram voice accepted for kmr")
	}
}
