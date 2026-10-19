// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"net"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 WO-7 (s2), founder ruling Q9: the agent `email_read` tool must PEEK.
//
// fetchMessage — BODY[HEADER] BODY[TEXT], non-PEEK — has two callers:
//   - the human reader (TUI / desktop, humanoid.go → ReadEmailIn): a person
//     opening a mail marks it read. Intended M1 behaviour; it stays.
//   - the agent tool (internal/tools/email_read.go → ReadEmail): an LLM agent
//     reading a mail is not the owner reading it, so it must not mark it read.

// fakeSessionAdapter is an EmailAdapter whose one dial reaches a
// seen-tracking fake server holding uids.
func fakeSessionAdapter(t *testing.T, uids []int) (*EmailAdapter, func(uid int) bool, func() []string) {
	t.Helper()
	conn, isSeen, sent := seenTrackingIMAPConn(t, uids, true)
	e := &EmailAdapter{
		config:     msg.ChannelConfig{EmailConfig: &msg.EmailChannelConfig{IMAPHost: "fake", Username: "me@x.io", Password: "pw"}},
		shortByUID: map[int]string{},
		uidByShort: map[string]int{},
		dialFn: func(string) (net.Conn, error) {
			return conn, nil
		},
	}
	return e, isSeen, sent
}

func fetchesOf(cmds []string) []string {
	var out []string
	for _, c := range cmds {
		if strings.Contains(strings.ToUpper(c), " FETCH ") {
			out = append(out, c)
		}
	}
	return out
}

func isPeekOnly(fetch string) bool {
	return !strings.Contains(strings.ReplaceAll(strings.ToUpper(fetch), "BODY.PEEK[", ""), "BODY[")
}

func TestAgentEmailReadPeeksAndNeverMarksRead(t *testing.T) {
	t.Chdir(t.TempDir()) // shortIDForUID persists .rysh/channel-state relative to cwd
	e, isSeen, sent := fakeSessionAdapter(t, []int{41})

	// ReadEmail is what the email_read tool calls (internal/tools/email_read.go).
	d, err := e.ReadEmail(41)
	if err != nil {
		t.Fatalf("ReadEmail: %v", err)
	}
	if d.Subject != "hi 41" || d.Body != "hello" {
		t.Errorf("detail = %q / %q, want hi 41 / hello", d.Subject, d.Body)
	}
	f := fetchesOf(sent())
	if len(f) != 1 || !isPeekOnly(f[0]) {
		t.Errorf("agent read FETCH = %q, want exactly one BODY.PEEK fetch", f)
	}
	if isSeen(41) {
		t.Error("UID 41 is \\Seen on the server after the agent's email_read")
	}
}

// The difference is deliberate: a human opening a mail in the reader marks it
// read (M1). Only the U-6/H-5 read-only guard turns this into PEEK.
func TestHumanReaderOpenStillMarksRead(t *testing.T) {
	t.Chdir(t.TempDir())
	e, isSeen, sent := fakeSessionAdapter(t, []int{41})

	if _, err := e.ReadEmailIn("", 41); err != nil {
		t.Fatalf("ReadEmailIn: %v", err)
	}
	f := fetchesOf(sent())
	if len(f) != 1 || isPeekOnly(f[0]) {
		t.Errorf("reader-open FETCH = %q, want exactly one non-PEEK BODY[...] fetch", f)
	}
	if !isSeen(41) {
		t.Error("UID 41 is not \\Seen after the human opened it; M1 reader-open marks read")
	}
}
