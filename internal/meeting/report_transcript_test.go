// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// H-61 (045 D5): the reporter keeps no raw transcript unless the owner turns
// it on for the meeting, with its own consent naming the retention.

func TestReporterKeepsTheTranscriptOnlyWithItsOwnConsent(t *testing.T) {
	h := newJoinRig(t)
	llm := &fakeLLM{}
	events := reportEvents(h.m)
	id := register(t, h.m, Command{PaneID: "p9", Room: existingRoom, Server: h.base}).ID
	dir := filepath.Join(t.TempDir(), id)
	keys := Credentials{RyshAPIKey: "rysh_testkey", LiveKit: LiveKitCredentials{STTKey: "dg"}}
	on := Command{Action: "report_on", ID: id, Consent: true, KeepTranscript: true, ReportDir: dir, Origin: "op"}

	// The reporter's own --consent does not cover keeping the transcript.
	r := h.m.Do(on, keys, llm.exec)
	if !strings.Contains(r.Error, "--transcript-consent") || !strings.Contains(r.Error, "kept until the owner deletes it") || len(h.rs.agents) != 0 {
		t.Fatalf("keep-transcript without its consent %+v (agents %v)", r, h.rs.agents)
	}
	on.Consent, on.TranscriptConsent = false, true
	if r = h.m.Do(on, keys, llm.exec); r.Error != "" || r.Report == nil {
		t.Fatalf("report on %+v", r)
	}
	transcript := filepath.Join(dir, TranscriptFile)
	if r.Report.Transcript != transcript {
		t.Fatalf("status transcript %q, want %q", r.Report.Transcript, transcript)
	}
	s := reporterOf(t, h.m, id)
	s.poll()
	h.room.events <- LiveKitEvent{Kind: LiveKitUtterance, Identity: ownerID, Name: "Halil", Text: "Yesterday I finished the room invites. rawphrase-one"}
	h.room.events <- LiveKitEvent{Kind: LiveKitUtterance, Identity: "guest-7", Text: "I am blocked on TURN ports. rawphrase-two"}
	h.room.events <- LiveKitEvent{Kind: LiveKitUtterance, Identity: "agent-reporter", Name: "reporter", Text: "my own voice rawphrase-three"}
	waitFor(t, func() bool { return s.reportStatus().Segments == 2 })
	if r := h.m.Do(Command{Action: "leave", ID: id}, Credentials{}, nil); r.Error != "" {
		t.Fatalf("leave %+v", r)
	}
	ev := nextEvent(t, events)
	if ev.Error != "" || ev.Path == "" {
		t.Fatalf("%+v", ev)
	}

	b, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatalf("no transcript kept: %v", err)
	}
	if !bytes.Contains(b, []byte("] Halil: Yesterday I finished the room invites. rawphrase-one\n")) ||
		!bytes.Contains(b, []byte("] guest-7: I am blocked on TURN ports. rawphrase-two\n")) || bytes.Contains(b, []byte("rawphrase-three")) {
		t.Fatalf("transcript:\n%s", b)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, transcript: 0o600, ev.Path: 0o600} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s: %v %v, want %v", path, info.Mode().Perm(), err, want)
		}
	}
	md, _ := os.ReadFile(ev.Path)
	if bytes.Contains(md, []byte("rawphrase")) || !bytes.Contains(md, []byte(transcriptKeptFooter)) || bytes.Contains(md, []byte(transcriptNotKeptFooter)) {
		t.Fatalf("report:\n%s", md)
	}
}

// Off (the default): no transcript file, and the footer says it was not kept.
func TestReporterWithoutOptInKeepsNoTranscript(t *testing.T) {
	llm := &fakeLLM{}
	r, events, dir := newTestTaker(t, llm)
	say(r, "Ada", "we agreed on the budget rawphrase")
	r.finish("the meeting ended")
	ev := nextEvent(t, events)
	md, _ := os.ReadFile(ev.Path)
	if !bytes.Contains(md, []byte(transcriptNotKeptFooter)) || bytes.Contains(md, []byte(transcriptKeptFooter)) {
		t.Fatalf("report:\n%s", md)
	}
	if _, err := os.Stat(filepath.Join(dir, TranscriptFile)); !os.IsNotExist(err) {
		t.Fatalf("transcript file without the opt-in: %v", err)
	}
	if st := r.status(); st.Transcript != "" {
		t.Fatalf("status names a transcript: %+v", st)
	}
	noTranscriptOnDisk(t, dir)
}

// A report recovered after a crash still says the transcript was kept.
func TestRecoveredReportSaysTheTranscriptWasKept(t *testing.T) {
	smallChunks(t, 50)
	llm := &fakeLLM{}
	dir := filepath.Join(t.TempDir(), "m-abcdef")
	r, err := newReportTaker(ReportConfig{MeetingID: "m-abcdef", Platform: PlatformZoom, Dir: dir, Summarize: llm.exec, KeepTranscript: true})
	if err != nil {
		t.Fatal(err)
	}
	say(r, "Ada", "we agreed on the budget rawphrase")
	r.summarizeDue(true)
	r.abort() // the daemon died
	r.wait(5 * time.Second)
	ev, err := RecoverReport(dir, llm.exec)
	if err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(ev.Path)
	if !bytes.Contains(md, []byte(transcriptKeptFooter)) {
		t.Fatalf("report:\n%s", md)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, TranscriptFile)); !bytes.Contains(b, []byte("] Ada: we agreed on the budget rawphrase\n")) {
		t.Fatalf("transcript:\n%s", b)
	}
}
