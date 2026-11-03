// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadNotify(t *testing.T, body string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rysh.config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadFrom(path)
}

// gap02 (G6): the daemon-side needs-you notifier is opt-in. With no sink it
// is off, and the defaults are 60s / 5m.
func TestNotifyNeedsYouDefaultsOff(t *testing.T) {
	n := loadNotify(t, "ui:\n  shell: bash\n").NotifyNeedsYou
	if n.Enabled() {
		t.Fatalf("enabled with no sink: %+v", n)
	}
	if n.After != 60*time.Second || n.Cooldown != 5*time.Minute {
		t.Fatalf("defaults = %v / %v, want 60s / 5m", n.After, n.Cooldown)
	}
}

func TestNotifyNeedsYouLoadsSinksAndDurations(t *testing.T) {
	n := loadNotify(t, `notify:
  needs_you:
    after: 90s
    cooldown: 10m
    webhook: ${RYSH_NOTIFY_URL}
    command: ["notify-helper", "--topic", "a b"]
`).NotifyNeedsYou
	if !n.Enabled() {
		t.Fatal("not enabled with two sinks")
	}
	if n.After != 90*time.Second || n.Cooldown != 10*time.Minute {
		t.Fatalf("durations = %v / %v", n.After, n.Cooldown)
	}
	if n.Webhook != "${RYSH_NOTIFY_URL}" {
		t.Fatalf("webhook = %q (kept as the reference; resolved at start)", n.Webhook)
	}
	if strings.Join(n.Command, "|") != "notify-helper|--topic|a b" {
		t.Fatalf("command = %q", n.Command)
	}
	if len(n.Problems) != 0 {
		t.Fatalf("problems = %v", n.Problems)
	}
}

func TestNotifyNeedsYouAfterZeroMeansAtOnce(t *testing.T) {
	n := loadNotify(t, "notify:\n  needs_you:\n    after: 0s\n    command: [\"x\"]\n").NotifyNeedsYou
	if n.After != 0 {
		t.Fatalf("after = %v, want 0", n.After)
	}
}

// A typo must neither stop the daemon nor be silent: the default is kept and
// the problem is reported.
func TestNotifyNeedsYouBadDurationKeepsDefaultAndSaysSo(t *testing.T) {
	n := loadNotify(t, "notify:\n  needs_you:\n    after: soon\n    cooldown: -1m\n    command: [\"x\"]\n").NotifyNeedsYou
	if n.After != 60*time.Second || n.Cooldown != 5*time.Minute {
		t.Fatalf("durations = %v / %v, want the defaults", n.After, n.Cooldown)
	}
	if len(n.Problems) != 2 {
		t.Fatalf("problems = %v, want 2", n.Problems)
	}
}

func TestNotifyNeedsYouEmptyCommandIsNoSink(t *testing.T) {
	n := loadNotify(t, "notify:\n  needs_you:\n    command: []\n    webhook: \"  \"\n").NotifyNeedsYou
	if n.Enabled() {
		t.Fatalf("enabled with empty sinks: %+v", n)
	}
}

// gap02 slice 2: "task done" alerts are off by default and turn on with
// notify.needs_you.done_after; they need a sink like every alert.
func TestNotifyDoneAfterOffByDefaultAndLoads(t *testing.T) {
	if n := loadNotify(t, "notify:\n  needs_you:\n    command: [\"x\"]\n").NotifyNeedsYou; n.DoneAfter != 0 {
		t.Fatalf("done_after default = %v, want 0 (off)", n.DoneAfter)
	}
	n := loadNotify(t, "notify:\n  needs_you:\n    done_after: 3m\n    command: [\"x\"]\n").NotifyNeedsYou
	if n.DoneAfter != 3*time.Minute {
		t.Fatalf("done_after = %v, want 3m", n.DoneAfter)
	}
	n = loadNotify(t, "notify:\n  needs_you:\n    done_after: later\n    command: [\"x\"]\n").NotifyNeedsYou
	if n.DoneAfter != 0 || len(n.Problems) != 1 || !strings.Contains(n.Problems[0], "done_after") {
		t.Fatalf("bad done_after: %v / %v", n.DoneAfter, n.Problems)
	}
	if n := loadNotify(t, "notify:\n  needs_you:\n    done_after: 3m\n").NotifyNeedsYou; n.Enabled() {
		t.Fatal("done_after alone is no sink")
	}
}

// E33 slice E1 (design 086): naming a humanoid is a sink of its own.
func TestNotifyNeedsYouAssistantAloneEnables(t *testing.T) {
	n := loadNotify(t, "notify:\n  needs_you:\n    assistant: \" assistant \"\n").NotifyNeedsYou
	if !n.Enabled() || n.Assistant != "assistant" {
		t.Fatalf("assistant sink not loaded: %+v", n)
	}
}

// E33 Q-E33-2 (root ruling 2026-10-28): setup turns the chat alert on only
// where no needs-you block exists yet; an existing one is never touched.
func TestSetNeedsYouAssistantIfUnset(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "rysh.config.yaml")
	if err := os.WriteFile(p, []byte("# keep me\nrysh:\n  session_name: work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := SetNeedsYouAssistantIfUnset(p, "assistant")
	if err != nil || !set {
		t.Fatalf("set=%v err=%v, want written", set, err)
	}
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), "# keep me") || !strings.Contains(string(raw), "session_name: work") {
		t.Fatalf("existing content lost:\n%s", raw)
	}
	if n := LoadFrom(p).NotifyNeedsYou; n.Assistant != "assistant" {
		t.Fatalf("assistant = %q", n.Assistant)
	}

	for _, existing := range []string{
		"notify:\n  needs_you:\n    webhook: https://example.invalid/x\n",
		"notify:\n  needs_you: {}\n",
	} {
		if err := os.WriteFile(p, []byte(existing), 0o644); err != nil {
			t.Fatal(err)
		}
		set, err := SetNeedsYouAssistantIfUnset(p, "assistant")
		raw, _ := os.ReadFile(p)
		if err != nil || set || string(raw) != existing {
			t.Fatalf("existing needs_you block changed: set=%v err=%v\n%s", set, err, raw)
		}
	}
}
