// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The status count of audited tool calls must never run ahead of the audit
// file: a call is counted only once its line is in the log. Before this fix
// recordTool counted first and appended after, so a reader that waited on
// ToolCalls and then read the file could miss the newest line — the ~1/20
// flake in TestMeetingAgentToolTurnAuditAndStop (E17 report; E9 fix).
//
// The file write is held back by holding auditFileMu. While it is held, the
// count must stay at 0.
func TestRecordToolCountsOnlyAfterTheAuditLineIsWritten(t *testing.T) {
	s, _, _ := fixture(t, func(context.Context, string) (string, error) { return "", nil })
	audit := filepath.Join(t.TempDir(), "sess", "meeting-audit.jsonl")
	s.mu.Lock()
	s.cfg.AuditPath = audit
	s.mu.Unlock()

	auditFileMu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.recordTool(AuditEntry{Turn: "t1", Agent: "scribe", Tool: "bash", Target: "git push", Status: "ok"})
	}()
	time.Sleep(50 * time.Millisecond)
	if n := s.status(false).ToolCalls; n != 0 {
		auditFileMu.Unlock()
		<-done
		t.Fatalf("ToolCalls = %d while the audit line is not yet written; want 0", n)
	}
	auditFileMu.Unlock()
	<-done

	b, err := os.ReadFile(audit)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(b)), "\n"); len(lines) != 1 {
		t.Fatalf("audit holds %d lines, want 1", len(lines))
	}
	if n := s.status(false).ToolCalls; n != 1 {
		t.Fatalf("ToolCalls = %d after the write, want 1", n)
	}
}
