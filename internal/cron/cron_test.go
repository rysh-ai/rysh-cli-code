package cron

import (
	"testing"
	"time"
)

func TestParseSchedule(t *testing.T) {
	good := []string{"0 9 * * *", "*/5 * * * *", "@every 15m", "@daily", "@hourly", "30 8 * * 1-5"}
	for _, s := range good {
		if _, err := ParseSchedule(s, ""); err != nil {
			t.Errorf("schedule %q should parse: %v", s, err)
		}
	}
	bad := []string{"", "not a cron", "60 * * * *", "* * * *"}
	for _, s := range bad {
		if _, err := ParseSchedule(s, ""); err == nil {
			t.Errorf("schedule %q should fail", s)
		}
	}
	// Timezone prefix is honoured.
	if _, err := ParseSchedule("0 9 * * *", "America/New_York"); err != nil {
		t.Errorf("tz schedule should parse: %v", err)
	}
	if _, err := ParseSchedule("0 9 * * *", "Not/AZone"); err == nil {
		t.Error("bad tz should fail")
	}
}

func TestValidate(t *testing.T) {
	if err := Validate("ig-discover", "0 9 * * *", "", "##auto web run ig-discover"); err != nil {
		t.Errorf("valid job rejected: %v", err)
	}
	// Bad names.
	for _, n := range []string{"", "has space", "bad/slash", "toooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooolong"} {
		if err := Validate(n, "@daily", "", "x"); err == nil {
			t.Errorf("name %q should be invalid", n)
		}
	}
	// Empty input.
	if err := Validate("j", "@daily", "", "   "); err == nil {
		t.Error("empty input should be invalid")
	}
	// Self-scheduling loop guard.
	if err := Validate("j", "@daily", "", "##cron add other @daily x"); err == nil {
		t.Error("a job that runs ##cron should be rejected")
	}
	// Bad schedule surfaces.
	if err := Validate("j", "nope", "", "x"); err == nil {
		t.Error("bad schedule should be rejected")
	}
}

func TestComputeNextAndDue(t *testing.T) {
	j := &Job{Name: "j", Schedule: "0 9 * * *", Enabled: true}
	base := time.Date(2026, 7, 8, 8, 0, 0, 0, time.UTC)
	if err := j.ComputeNext(base); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC)
	if !j.NextRun.Equal(want) {
		t.Errorf("next = %v, want %v", j.NextRun, want)
	}
	// Not due before, due at/after.
	if j.Due(time.Date(2026, 7, 8, 8, 59, 0, 0, time.UTC)) {
		t.Error("should not be due before next run")
	}
	if !j.Due(want) {
		t.Error("should be due at next run")
	}
	// Disabled is never due.
	j.Enabled = false
	if j.Due(want) {
		t.Error("disabled job must not be due")
	}
}

func TestRecordRunRingAndFailStreak(t *testing.T) {
	j := &Job{Name: "j"}
	for i := 0; i < MaxRunHistory+5; i++ {
		j.RecordRun(Run{At: time.Now(), Status: "ok"})
	}
	if len(j.Runs) != MaxRunHistory {
		t.Errorf("ring buffer len = %d, want %d", len(j.Runs), MaxRunHistory)
	}
	if j.RunCount != MaxRunHistory+5 {
		t.Errorf("run count = %d", j.RunCount)
	}
	if j.FailStreak != 0 {
		t.Errorf("ok runs should reset streak, got %d", j.FailStreak)
	}
	j.RecordRun(Run{At: time.Now(), Status: "error"})
	j.RecordRun(Run{At: time.Now(), Status: "error"})
	if j.FailStreak != 2 {
		t.Errorf("fail streak = %d, want 2", j.FailStreak)
	}
	j.RecordRun(Run{At: time.Now(), Status: "ok"})
	if j.FailStreak != 0 {
		t.Errorf("ok should reset streak, got %d", j.FailStreak)
	}
}

func TestInferMode(t *testing.T) {
	cases := map[string]string{
		"##auto web run x":        "rysh",
		"@tango-reporter do it":   "rysh",
		"echo hello":              "shell",
		"summarize the last hour": "shell",
	}
	for input, want := range cases {
		j := &Job{Input: input}
		if got := j.InferMode(); got != want {
			t.Errorf("InferMode(%q) = %q, want %q", input, got, want)
		}
	}
	// Explicit mode wins.
	j := &Job{Input: "echo x", Mode: "prompt"}
	if j.InferMode() != "prompt" {
		t.Errorf("explicit mode ignored: %q", j.InferMode())
	}
}
