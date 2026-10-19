// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// `##board copy` end to end in the daemon: a real recorder (board.ServeQueries)
// on an in-process bus, a stand-in TUI answering the clipboard hand-off, and
// the local clipboard swapped for a recorder.

// serveRecorder answers board queries from st for every board, as ABLA does.
func serveRecorder(t *testing.T, nc *nats.Conn, st *board.Store) {
	t.Helper()
	subs, err := board.ServeQueries(nc, func(string) *board.Store { return st }, nil)
	if err != nil {
		t.Fatalf("serve queries: %v", err)
	}
	t.Cleanup(func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	})
	_ = nc.Flush()
}

// fakeTUI answers MsgClipboardWrite like an attached terminal, recording the
// text it was handed.
type fakeTUI struct {
	mu   sync.Mutex
	got  []string
	fail string
}

func (f *fakeTUI) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

func attachFakeTUI(t *testing.T, nc *nats.Conn, fail string) *fakeTUI {
	t.Helper()
	f := &fakeTUI{fail: fail}
	codecs := msg.DefaultCodecRegistry()
	sub, err := nc.Subscribe(msg.ClipboardWriteSubject(), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		decoded, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		req := decoded.(*msg.MsgClipboardWrite)
		f.mu.Lock()
		f.got = append(f.got, req.Text)
		f.mu.Unlock()
		resp := &msg.MsgCLIResponse{OK: f.fail == "", Output: "terminal clipboard (OSC 52)", Error: f.fail}
		re := &msg.RequestEnvelope{ReplyTo: env.ReplyTo, NC: nc, Codecs: codecs}
		_ = re.Reply(resp)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	_ = nc.Flush()
	return f
}

// stubLocalClipboard replaces the host clipboard for the test.
func stubLocalClipboard(t *testing.T, fail error) *[]string {
	t.Helper()
	var got []string
	prev := writeLocalClipboard
	writeLocalClipboard = func(s string) error {
		if fail != nil {
			return fail
		}
		got = append(got, s)
		return nil
	}
	t.Cleanup(func() { writeLocalClipboard = prev })
	return &got
}

func copyFleetStore() *board.Store {
	st := board.New(0)
	root := func(pane, persona, text string, n int, ts int64) *msg.MsgBoardPost {
		p := msg.NewBoardPost(pane, persona, msg.BoardKindMilestone, text, ts)
		p.ThreadID = msg.MintThreadID(pane, n)
		return p
	}
	st.Apply(root("p314", "issue-314", "STATUS: codec wired", 1, 1_000))
	st.Apply(root("p31", "issue-31", "BLOCKED: waiting on CI", 1, 2_000))
	st.Apply(root("p314", "issue-314", "PR https://github.com/o/r/pull/"+strings.Repeat("7", 190), 2, 3_000))
	return st
}

func newCopyWorkspace(t *testing.T) (*WorkspaceActor, *nats.Conn) {
	t.Helper()
	w, nc := newBoardTestWorkspace(t)
	w.nc = nc
	return w, nc
}

func TestBoardCopyWorkerAndCountGoToTheTerminal(t *testing.T) {
	w, nc := newCopyWorkspace(t)
	serveRecorder(t, nc, copyFleetStore())
	tui := attachFakeTUI(t, nc, "")
	local := stubLocalClipboard(t, nil)

	var out strings.Builder
	// Positionals in any order: `1 issue-314` = `issue-314 1`.
	if err := w.handleBoardCommand(&out, "pA", []string{"copy", "1", "issue-314"}); err != nil {
		t.Fatalf("##board copy: %v\n%s", err, out.String())
	}
	got := tui.texts()
	if len(got) != 1 {
		t.Fatalf("the terminal was handed %d copies, want 1", len(got))
	}
	if !strings.HasSuffix(got[0], "\nPR https://github.com/o/r/pull/"+strings.Repeat("7", 190)) || strings.Contains(got[0], "STATUS") {
		t.Fatalf("copied %q; want issue-314's newest message, unwrapped", got[0])
	}
	if len(*local) != 0 {
		t.Fatal("the local fallback ran although a terminal took the copy")
	}
	if line := out.String(); !strings.HasPrefix(line, "[board] copied 1 of 2 messages from issue-314 to the clipboard") ||
		!strings.Contains(line, "OSC 52") {
		t.Fatalf("confirmation = %q", line)
	}
}

func TestBoardCopyByCountAndFallback(t *testing.T) {
	w, nc := newCopyWorkspace(t)
	serveRecorder(t, nc, copyFleetStore())
	local := stubLocalClipboard(t, nil)

	var out strings.Builder
	if err := w.handleBoardCommand(&out, "pA", []string{"copy", "2"}); err != nil {
		t.Fatalf("##board copy 2: %v\n%s", err, out.String())
	}
	if len(*local) != 1 || strings.Contains((*local)[0], "STATUS") || !strings.Contains((*local)[0], "BLOCKED") {
		t.Fatalf("copy 2 = %q; want the 2 newest messages", *local)
	}
	if !strings.HasPrefix(out.String(), "[board] copied 2 of 3 messages to the clipboard (local clipboard") {
		t.Fatalf("confirmation = %q", out.String())
	}

	// A terminal that refuses sends the copy to the fallback too.
	attachFakeTUI(t, nc, "no tty")
	out.Reset()
	if err := w.handleBoardCommand(&out, "pA", []string{"copy"}); err != nil {
		t.Fatalf("##board copy after a terminal refusal: %v", err)
	}
	if len(*local) != 2 || !strings.Contains((*local)[1], "STATUS") {
		t.Fatalf("local clipboard got %q", *local)
	}
}

// TestBoardCopyRefusesWithoutARecorder: nobody answering is refused loudly —
// nothing reaches any clipboard or file.
func TestBoardCopyRefusesWithoutARecorder(t *testing.T) {
	w, nc := newCopyWorkspace(t)
	tui := attachFakeTUI(t, nc, "")
	local := stubLocalClipboard(t, nil)
	wrote := stubOutput(t)

	for _, args := range [][]string{{"copy"}, {"output", "html"}} {
		var out strings.Builder
		if err := w.handleBoardCommand(&out, "pA", args); err == nil {
			t.Fatalf("##board %v with no recorder succeeded:\n%s", args, out.String())
		}
		if !strings.Contains(out.String(), "NOT AN EMPTY BOARD") {
			t.Fatalf("refusal = %q", out.String())
		}
	}
	if len(tui.texts()) != 0 || len(*local) != 0 || len(*wrote) != 0 {
		t.Fatal("something was copied or written although the recorder did not answer")
	}
}

func TestBoardCopyNoMessagesFromWorker(t *testing.T) {
	w, nc := newCopyWorkspace(t)
	serveRecorder(t, nc, copyFleetStore())
	local := stubLocalClipboard(t, nil)

	var out strings.Builder
	if err := w.handleBoardCommand(&out, "pA", []string{"copy", "issue-3"}); err == nil {
		t.Fatalf("an empty selection was copied:\n%s", out.String())
	}
	if len(*local) != 0 || strings.TrimSpace(out.String()) != "[board] no messages from issue-3" {
		t.Fatalf("refusal = %q, copied %q", out.String(), *local)
	}
}

func stubOutput(t *testing.T) *[]board.OutputRequest {
	t.Helper()
	var got []board.OutputRequest
	prev := writeBoardOutput
	writeBoardOutput = func(r board.OutputRequest) (string, error) {
		got = append(got, r)
		return "/tmp/board-out/x." + r.Format, nil
	}
	t.Cleanup(func() { writeBoardOutput = prev })
	return &got
}

func TestBoardOutputWritesTheSameItems(t *testing.T) {
	w, nc := newCopyWorkspace(t)
	serveRecorder(t, nc, copyFleetStore())
	wrote := stubOutput(t)

	var out strings.Builder
	if err := w.handleBoardCommand(&out, "pA", []string{"output", "pdf", "issue-314", "1", "--no-open"}); err != nil {
		t.Fatalf("##board output: %v\n%s", err, out.String())
	}
	if len(*wrote) != 1 {
		t.Fatalf("wrote %d files", len(*wrote))
	}
	req := (*wrote)[0]
	if req.Format != "pdf" || req.Open || req.Page.Worker != "issue-314" || len(req.Page.Picked) != 1 ||
		req.Page.Matched != 2 || req.Page.Total != 3 || req.Page.Board != "session" {
		t.Fatalf("request = %+v", req)
	}
	if out.String() != "[board] wrote 1 of 2 messages from issue-314 → /tmp/board-out/x.pdf\n" {
		t.Fatalf("confirmation = %q", out.String())
	}

	out.Reset()
	if err := w.handleBoardCommand(&out, "pA", []string{"output", "5"}); err == nil ||
		!strings.Contains(out.String(), "[board] output needs a format: html or pdf (e.g. ##board output 5 pdf)") {
		t.Fatalf("missing format = %q", out.String())
	}
	out.Reset()
	if err := w.handleBoardCommand(&out, "pA", []string{"output", "issue-314", "docx"}); err == nil ||
		!strings.Contains(out.String(), "unknown format") || !strings.Contains(out.String(), "##board copy [worker] [N]") {
		t.Fatalf("unknown format = %q; want the error and usage for both commands", out.String())
	}
	if len(*wrote) != 1 {
		t.Fatal("a refused output wrote a file")
	}
}

func TestBoardCopyBadArgsPrintUsageForBoth(t *testing.T) {
	w, nc := newCopyWorkspace(t)
	serveRecorder(t, nc, copyFleetStore())
	stubLocalClipboard(t, errors.New("no pbcopy"))

	for _, args := range [][]string{{"copy", "--agnet", "x"}, {"copy", "a", "b"}, {"copy", "1", "2"}} {
		var out strings.Builder
		if err := w.handleBoardCommand(&out, "pA", args); err == nil {
			t.Errorf("##board %v accepted", args)
		}
		if !strings.Contains(out.String(), "##board copy [worker] [N]") || !strings.Contains(out.String(), "##board output [worker] [N] html|pdf") {
			t.Errorf("##board %v usage = %q", args, out.String())
		}
	}
	var out strings.Builder
	if err := w.handleBoardCommand(&out, "pA", []string{"copy"}); err == nil ||
		!strings.Contains(out.String(), "local clipboard failed") {
		t.Fatalf("a failed clipboard write reported %v / %q", err, out.String())
	}
}

func TestBoardCopyIsDocumented(t *testing.T) {
	var help strings.Builder
	w := newDispatchTestWorkspace(t)
	w.ryshHelp(&help)
	for _, want := range []string{"##board copy [worker] [N]", "##board output [worker] [N] html|pdf"} {
		if !strings.Contains(help.String(), want) {
			t.Errorf("##help does not mention %q", want)
		}
	}
	var out strings.Builder
	_ = w.handleBoardCommand(&out, "pA", nil)
	if !strings.Contains(out.String(), "##board copy issue-314 3") || !strings.Contains(out.String(), "##board output 5 pdf") {
		t.Errorf("##board usage does not list copy/output:\n%s", out.String())
	}
}
