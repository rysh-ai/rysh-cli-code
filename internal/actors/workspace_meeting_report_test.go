// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
)

// reportRig is the agents rig with a rysh dir, a fake LLM and a LiveKit room
// the test can speak into.
func newReportRig(t *testing.T) (*meetingAgentsRig, chan meeting.LiveKitEvent, func() []string) {
	h := newMeetingAgentsRig(t, nil)
	h.w.cfg.RyshDir = t.TempDir()
	h.w.cfg.SessionName = "macbook-report"
	events := make(chan meeting.LiveKitEvent, 8)
	meeting.DialLiveKit = func(context.Context, string, string, meeting.LiveKitDialOptions) (meeting.LiveKitRoom, error) {
		return &agentsRoom{events: events}, nil
	}
	var mu sync.Mutex
	var prompts []string
	old := meetingReportExec
	t.Cleanup(func() { meetingReportExec = old })
	meetingReportExec = func(*WorkspaceActor) (meeting.Execute, error) {
		return func(_ context.Context, prompt string) (string, error) {
			mu.Lock()
			prompts = append(prompts, prompt)
			mu.Unlock()
			if strings.Contains(prompt, "<notes>\n") {
				return `{"overview":"Standup.","blockers":[{"owner":"Guest","item":"TURN ports"}],"assignments":[{"who":"Halil","what":"Open the ports","when":"today"}]}`, nil
			}
			return `{"topics":[{"title":"TURN"}]}`, nil
		}, nil
	}
	return h, events, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), prompts...)
	}
}

// runReport issues `##meeting report …` and waits for its (async) result.
func (h *meetingAgentsRig) runReport(t *testing.T, args ...string) string {
	t.Helper()
	var out strings.Builder
	h.w.ryshFail = nil
	h.w.meetingReportCommand(&out, "p1", args)
	if h.w.ryshFail != nil {
		return "FAIL " + out.String()
	}
	return h.nextNotice(t)
}

func (h *meetingAgentsRig) nextNotice(t *testing.T) string {
	t.Helper()
	select {
	case n := <-h.notices:
		if !strings.HasPrefix(n, "p1:") {
			t.Fatalf("notice went to the wrong pane: %q", n)
		}
		return n
	case <-time.After(15 * time.Second):
		t.Fatal("no result")
	}
	return ""
}

func TestMeetingReportLiveKitEndToEnd(t *testing.T) {
	h, events, prompts := newReportRig(t)
	id := h.register(t, meeting.Command{PaneID: "mp", Room: agentsTestRoom, Server: h.server})

	if got := h.runReport(t, id, "on"); !strings.Contains(got, "--consent") || !strings.Contains(got, "never saved") {
		t.Fatalf("without consent: %q", got)
	}
	got := h.runReport(t, id, "on", "--consent")
	if !strings.Contains(got, "reporter listening (bot joining)") {
		t.Fatalf("on: %q", got)
	}
	if strings.Join(h.tokens, ",") != "reporter" {
		t.Fatalf("agent-token for %v", h.tokens)
	}
	for deadline := time.Now().Add(15 * time.Second); h.metaOf("mp", "meeting.reporter") != "in_call"; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("reporter phase %q", h.metaOf("mp", "meeting.reporter"))
		}
	}
	events <- meeting.LiveKitEvent{Kind: meeting.LiveKitUtterance, Identity: agentsTestOwner, Name: "Halil", Text: "I will open the TURN ports today"}
	events <- meeting.LiveKitEvent{Kind: meeting.LiveKitUtterance, Identity: "guest-1", Name: "Guest", Text: "I am blocked on TURN"}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if st := h.runReport(t, id); strings.Contains(st, "2 segments heard") && strings.Contains(st, "speakers Halil, Guest") {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("status %q", st)
		}
	}
	// `##meeting list` marks the reporter.
	h.w.meetingAsync("p1", meeting.Command{Action: "list"}, false)
	if l := h.nextNotice(t); !strings.Contains(l, ", reporter") {
		t.Fatalf("list %q", l)
	}

	// remove agent … reporter is report off: the reporter leaves and the
	// report is announced in the pane that started it.
	if got := h.run(t, "remove", "agent", id, "reporter"); !strings.Contains(got, "being written") {
		t.Fatalf("off: %q", got)
	}
	done := h.nextNotice(t)
	path := filepath.Join(h.w.cfg.RyshDir, "meetings", id, "report.md")
	if !strings.Contains(done, "report written: "+path) || !strings.Contains(done, "1 blockers") || !strings.Contains(done, "1 assignments") {
		t.Fatalf("done notice %q", done)
	}
	if h.metaOf("mp", "meeting.report") != path || h.metaOf("mp", "meeting.reporter") != "left" {
		t.Fatalf("meta %v", h.meta)
	}
	md, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(md), "| Halil | Open the ports | today |") || !strings.Contains(string(md), "session `macbook-report`") {
		t.Fatalf("report %v:\n%s", err, md)
	}
	if strings.Contains(string(md), "I am blocked on TURN") {
		t.Fatal("transcript text in the report file")
	}
	var sawSpeakers bool
	for _, p := range prompts() {
		sawSpeakers = sawSpeakers || strings.Contains(p, "] Guest: I am blocked on TURN")
	}
	if !sawSpeakers {
		t.Fatalf("prompts %q", prompts())
	}
	if st := h.runReport(t, id); !strings.Contains(st, "no reporter") || !strings.Contains(st, "report: "+path) {
		t.Fatalf("status after off %q", st)
	}
}

func TestMeetingReportCommandRefusals(t *testing.T) {
	h, _, _ := newReportRig(t)
	id := h.register(t, meeting.Command{PaneID: "mp", URL: agentsTestMeet})
	for _, args := range [][]string{{}, {id, "on", "--allow", "host"}, {id, "bogus"}, {id, "off", "extra"}} {
		if got := h.runReport(t, args...); !strings.HasPrefix(got, "FAIL") || !strings.Contains(got, "report <id>") {
			t.Fatalf("%v: %q", args, got)
		}
	}
	if got := h.runReport(t, "nope"); !strings.HasPrefix(got, "FAIL") || !strings.Contains(got, "not a meeting id") {
		t.Fatalf("%q", got)
	}
	if got := h.runReport(t, "m-0000ff"); !strings.HasPrefix(got, "FAIL") || !strings.Contains(got, "no meeting m-0000ff") {
		t.Fatalf("%q", got)
	}
	if got := h.run(t, "add", "agent", id, "reporter", "--allow", "host"); !strings.HasPrefix(got, "FAIL") || !strings.Contains(got, "--allow does not apply") {
		t.Fatalf("%q", got)
	}
	// Web: the reporter follows the join rules (public URL, keys) but
	// needs no speech key.
	if got := h.run(t, "add", "agent", id, "reporter", "--consent"); !strings.Contains(got, "--public-url") {
		t.Fatalf("no public URL: %q", got)
	}
	if got := h.runReport(t, id, "off"); !strings.Contains(got, "has no reporter") {
		t.Fatalf("%q", got)
	}
	if got := h.runReport(t, id, "now"); !strings.Contains(got, "has no reporter") {
		t.Fatalf("%q", got)
	}
	if got := h.runReport(t, id, "write"); !strings.Contains(got, "no reporter notes to recover") {
		t.Fatalf("%q", got)
	}
}

// H-61 (045 D5): `--keep-transcript` turns on keeping the raw transcript for
// one meeting; it needs `--transcript-consent`, whose wording names the
// retention, and the reporter's own --consent does not stand in for it.
func TestMeetingReportKeepTranscriptOptIn(t *testing.T) {
	h, events, _ := newReportRig(t)
	id := h.register(t, meeting.Command{PaneID: "mp", Room: agentsTestRoom, Server: h.server})

	if got := h.runReport(t, id, "on", "--consent", "--keep-transcript"); !strings.Contains(got, "--transcript-consent") || !strings.Contains(got, "kept until the owner deletes it") {
		t.Fatalf("without the transcript consent: %q", got)
	}
	if len(h.tokens) != 0 {
		t.Fatalf("a refused reporter asked for a token: %v", h.tokens)
	}
	if got := h.runReport(t, id, "on", "--transcript-consent"); !strings.HasPrefix(got, "FAIL") || !strings.Contains(got, "--keep-transcript") {
		t.Fatalf("consent without the opt-in: %q", got)
	}
	transcript := filepath.Join(h.w.cfg.RyshDir, "meetings", id, meeting.TranscriptFile)
	got := h.runReport(t, id, "on", "--keep-transcript", "--transcript-consent")
	if !strings.Contains(got, "reporter listening") || !strings.Contains(got, "transcript kept: "+transcript) {
		t.Fatalf("on: %q", got)
	}
	for deadline := time.Now().Add(15 * time.Second); h.metaOf("mp", "meeting.reporter") != "in_call"; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("reporter phase %q", h.metaOf("mp", "meeting.reporter"))
		}
	}
	events <- meeting.LiveKitEvent{Kind: meeting.LiveKitUtterance, Identity: "guest-1", Name: "Guest", Text: "I am blocked on TURN"}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if st := h.runReport(t, id); strings.Contains(st, "1 segments heard") {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("status %q", st)
		}
	}
	if got := h.runReport(t, id, "off"); !strings.Contains(got, "being written") {
		t.Fatalf("off: %q", got)
	}
	h.nextNotice(t)
	b, err := os.ReadFile(transcript)
	if err != nil || !strings.Contains(string(b), "] Guest: I am blocked on TURN\n") {
		t.Fatalf("transcript %v:\n%s", err, b)
	}
	md, _ := os.ReadFile(filepath.Join(h.w.cfg.RyshDir, "meetings", id, "report.md"))
	if !strings.Contains(string(md), "kept until the owner deletes it") || strings.Contains(string(md), "I am blocked on TURN") {
		t.Fatalf("report:\n%s", md)
	}
}
