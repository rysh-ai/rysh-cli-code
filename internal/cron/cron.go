// Package cron holds the pure scheduling domain for rysh's in-daemon cron
// service: the Job model, schedule parsing/validation (via robfig/cron), and
// next-run computation. It has no actor or NATS dependencies so it is fully
// unit-testable; the actor-side store, ticker, and job firing live in
// internal/actors/workspace_cron.go.
package cron

import (
	"fmt"
	"strings"
	"time"

	robfig "github.com/robfig/cron/v3"
)

// MaxRunHistory caps the per-job ring buffer of recent run records.
const MaxRunHistory = 20

// MaxJobs caps the number of jobs a session may hold.
const MaxJobs = 100

// Run is one recorded firing of a job.
type Run struct {
	At     time.Time `json:"at"`
	Status string    `json:"status"` // ok | error | skipped
	Note   string    `json:"note,omitempty"`
}

// Job is a scheduled rysh input injection.
type Job struct {
	ID       string `json:"id"`
	Name     string `json:"name"`     // unique handle for ##cron commands
	Schedule string `json:"schedule"` // "0 9 * * *", "@every 15m", "@daily"
	Timezone string `json:"timezone,omitempty"`
	Target   string `json:"target"` // pane id/name, or "active"
	Mode     string `json:"mode"`   // shell | prompt | rysh (inferred if empty)
	Input    string `json:"input"`  // text to inject (##…, @agent…, prompt, shell)
	Enabled  bool   `json:"enabled"`

	NextRun    time.Time `json:"next_run"`
	LastRun    time.Time `json:"last_run,omitempty"`
	LastStatus string    `json:"last_status,omitempty"`
	RunCount   int       `json:"run_count"`
	Runs       []Run     `json:"runs,omitempty"`

	// FailStreak counts consecutive failures; the actor auto-disables a job
	// after AutoDisableAfter consecutive failures so a broken job can't fire
	// forever.
	FailStreak int `json:"fail_streak,omitempty"`
}

// AutoDisableAfter is the consecutive-failure count at which a job is
// auto-disabled.
const AutoDisableAfter = 5

// namePattern-equivalent: a job name is a single token of letters, digits,
// hyphens, underscores. Kept simple so names are safe as command args and KV
// map keys.
func validName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// buildSpec prepends a CRON_TZ prefix when a timezone is set, which robfig's
// standard parser honours.
func buildSpec(schedule, tz string) string {
	schedule = strings.TrimSpace(schedule)
	if tz = strings.TrimSpace(tz); tz != "" {
		return "CRON_TZ=" + tz + " " + schedule
	}
	return schedule
}

// ParseSchedule parses a cron spec (5-field standard, or @every/@daily/…
// descriptors) with an optional timezone.
func ParseSchedule(schedule, tz string) (robfig.Schedule, error) {
	if strings.TrimSpace(schedule) == "" {
		return nil, fmt.Errorf("empty schedule")
	}
	sched, err := robfig.ParseStandard(buildSpec(schedule, tz))
	if err != nil {
		return nil, fmt.Errorf("invalid schedule %q: %w", schedule, err)
	}
	return sched, nil
}

// Validate checks a job's name, schedule, and input at creation time.
func Validate(name, schedule, tz, input string) error {
	if !validName(name) {
		return fmt.Errorf("invalid job name %q (letters, digits, - and _ only, ≤64 chars)", name)
	}
	if strings.TrimSpace(input) == "" {
		return fmt.Errorf("empty input — a job must inject some command or prompt")
	}
	// Guard against self-scheduling loops: a job that runs ##cron.
	if strings.HasPrefix(strings.TrimSpace(input), "##cron") {
		return fmt.Errorf("a cron job cannot itself run ##cron")
	}
	if _, err := ParseSchedule(schedule, tz); err != nil {
		return err
	}
	return nil
}

// ComputeNext sets NextRun to the first fire time strictly after `from`.
// Returns an error if the schedule no longer parses (e.g. corrupted KV).
func (j *Job) ComputeNext(from time.Time) error {
	sched, err := ParseSchedule(j.Schedule, j.Timezone)
	if err != nil {
		return err
	}
	j.NextRun = sched.Next(from)
	return nil
}

// Due reports whether the job should fire at time `now`: enabled and its
// NextRun has arrived.
func (j *Job) Due(now time.Time) bool {
	return j.Enabled && !j.NextRun.IsZero() && !j.NextRun.After(now)
}

// RecordRun appends a run record (ring-buffered), updates last-run bookkeeping,
// and maintains the failure streak.
func (j *Job) RecordRun(r Run) {
	j.LastRun = r.At
	j.LastStatus = r.Status
	j.RunCount++
	if r.Status == "error" {
		j.FailStreak++
	} else {
		j.FailStreak = 0
	}
	j.Runs = append(j.Runs, r)
	if len(j.Runs) > MaxRunHistory {
		j.Runs = j.Runs[len(j.Runs)-MaxRunHistory:]
	}
}

// InferMode returns the effective input mode: the explicit Mode, else inferred
// from the input prefix (## / @ → those are handled by the dispatcher
// regardless of mode, so default to "prompt" for bare text is wrong for
// shell; we default bare text to "shell" to match a terminal's expectation).
func (j *Job) InferMode() string {
	if j.Mode != "" {
		return j.Mode
	}
	t := strings.TrimSpace(j.Input)
	switch {
	case strings.HasPrefix(t, "##"), strings.HasPrefix(t, "@"):
		// Prefix-dispatched by the workspace regardless of mode; "rysh" keeps
		// the ## echo/history behaviour tidy.
		return "rysh"
	default:
		return "shell"
	}
}
