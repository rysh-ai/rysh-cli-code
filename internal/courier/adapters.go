// SPDX-License-Identifier: Apache-2.0

package courier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/agentenv"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// snapshotTimeout bounds one look at a recipient pane.
const snapshotTimeout = 3 * time.Second

// SnapshotVia reads a pane's foreground program, pgid and meta over the bus —
// the one look the courier and a listener both take before every delivery.
func SnapshotVia(pub *msg.NATSPublisher, paneID string) (Target, error) {
	reply, err := pub.Request(msg.T("pane", paneID, "snapshot"),
		&msg.MsgGetPaneSnapshot{LayoutOnly: true}, snapshotTimeout)
	if err != nil {
		return Target{}, err
	}
	snap, ok := reply.(*msg.MsgPaneSnapshotReply)
	if !ok {
		return Target{}, fmt.Errorf("unexpected reply %T", reply)
	}
	s := snap.Snapshot
	return Target{PaneID: paneID, Program: s.Program, PGID: s.ProgramPGID, Meta: s.Meta}, nil
}

// Dispatch performs one delivery over a planned route. rysh-native is the only
// route that touches a pane subject, and it is the pane's prompt INBOX — the
// same non-PTY door fleetctl's native kickoff uses — never rawinput.
func Dispatch(ctx context.Context, pub *msg.NATSPublisher, r Route, t Target, text string) error {
	switch r.Adapter {
	case RouteClaudeInbox:
		return SendClaudeInbox(ctx, r.Address, text)
	case RouteCodexQueue:
		bin, err := CodexBin()
		if err != nil {
			return fmt.Errorf("codex not found: %v", err)
		}
		return SendCodexQueue(ctx, bin, r.Address, text)
	case RouteRyshNative:
		return pub.Send(msg.T("pane", t.PaneID, "inbox"), &msg.MsgPaneExecPrompt{Prompt: text})
	}
	return fmt.Errorf("unknown route %q", r.Adapter)
}

// SendClaudeInbox writes one user message to a claude session's inbox socket.
//
// The wire format is Claude Code's own (cross-session messaging): one JSON line,
// {"type":"user","message":{"role":"user","content":"..."}}. The optional
// {"type":"auth"} line is omitted — it is optional on macOS and Linux, and its
// token is only exported to the session's own children, which the daemon is
// not. Fleet panes run with crossSessionInbound=accept, so the message is
// delivered rather than held behind an approval dialog in the composer.
//
// Claude closes a connection that sends no complete line within 30s, so the
// message is fully built before dialling, as its docs ask.
func SendClaudeInbox(ctx context.Context, socket, text string) error {
	line, err := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": text},
	})
	if err != nil {
		return err
	}
	line = append(line, '\n')

	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return fmt.Errorf("dial %s: %w", socket, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(dl)
	}
	if _, err := conn.Write(line); err != nil {
		return fmt.Errorf("write %s: %w", socket, err)
	}
	return nil
}

// CodexBin is the codex executable: $RYSH_CODEX_BIN, else `codex` on PATH.
func CodexBin() (string, error) {
	if b := strings.TrimSpace(os.Getenv("RYSH_CODEX_BIN")); b != "" {
		return b, nil
	}
	return exec.LookPath("codex")
}

// SendCodexQueue queues a user turn on a running codex thread:
// `codex queue --thread <id> --message <text>`. The interactive TUI attached
// to that thread runs it at its next idle point. Success is codex's own
// receipt ("Queued message … for thread …"), not merely exit 0.
func SendCodexQueue(ctx context.Context, bin, thread, text string) error {
	cmd := exec.CommandContext(ctx, bin, "queue", "--thread", thread, "--message", text)
	// codex is not yet verified for the pattern scrub (ORDER-64): exact names only.
	cmd.Env = agentenv.FilterStatic(os.Environ())
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	got := strings.TrimSpace(out.String())
	if err != nil {
		return fmt.Errorf("codex queue: %v: %s", err, lastLine(got))
	}
	if !strings.Contains(got, "Queued message") {
		return fmt.Errorf("codex queue gave no receipt: %s", lastLine(got))
	}
	return nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
