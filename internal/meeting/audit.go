// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// AuditEntry is one tool call made from a meeting turn (founder decision
// 2026-09-28: agents use all their tools with no approval; the allowlist,
// kill switch and this log are the controls). It never carries tool output,
// secret values or meeting transcript beyond the tool's own target label.
//
// The audit log is the RECORD of what ran, sub-agent calls (every depth)
// included: each call has exactly one "started" and one terminal entry. The
// meeting wall activity
// (ReportActivity: "thinking with …", "running …") is best-effort LIVE
// progress, NOT a record: labels for a turn that already ended may never
// appear, and it must not be cited as evidence of what ran.
type AuditEntry struct {
	Time    time.Time `json:"time"`
	Meeting string    `json:"meeting,omitempty"` // Recall bot id
	// MeetingID is the registry id (m-xxxxxx, design 043) of the meeting
	// the turn ran in.
	MeetingID string `json:"meeting_id,omitempty"`
	Turn      string `json:"turn"`
	Agent     string `json:"agent"`
	Speaker   string `json:"speaker"` // display name: a label, not authentication
	Tool      string `json:"tool"`
	Target    string `json:"target,omitempty"` // redacted, ≤ 160 runes
	Status    string `json:"status"`           // started | ok | error | blocked_by_policy | incomplete (started, never ended: the turn stopped first)
}

type turnKey struct{}

type turnInfo struct {
	turn, agent, speaker string
	record               func(AuditEntry)
}

// withTurn binds RecordToolCall and TurnSpeaker for one turn. Unlike
// activity, records from a stale (stopped) turn are kept: a tool that
// finishes after "stop" still acted, and the log must say so.
func (s *Service) withTurn(ctx context.Context, p *turn) context.Context {
	return context.WithValue(ctx, turnKey{}, turnInfo{turn: p.id, agent: p.agent, speaker: p.speaker, record: s.recordTool})
}

// TurnSpeaker returns the participant label of the current meeting turn.
func TurnSpeaker(ctx context.Context) string {
	t, _ := ctx.Value(turnKey{}).(turnInfo)
	return t.speaker
}

// RecordToolCall appends a tool call to the meeting audit log. It is a no-op
// outside a meeting turn. target is redacted and clipped here.
func RecordToolCall(ctx context.Context, tool, target, status string) {
	t, ok := ctx.Value(turnKey{}).(turnInfo)
	if !ok || t.record == nil {
		return
	}
	t.record(AuditEntry{Time: time.Now().UTC(), Turn: t.turn, Agent: t.agent, Speaker: clip(t.speaker, 80), Tool: clip(tool, 80), Target: clip(Redact(target), 160), Status: status})
}

// WithAuditRecorder binds RecordToolCall to f outside a Service (tests,
// other executors).
func WithAuditRecorder(ctx context.Context, agent, speaker string, f func(AuditEntry)) context.Context {
	return context.WithValue(ctx, turnKey{}, turnInfo{agent: agent, speaker: speaker, record: f})
}

var auditFileMu sync.Mutex

func (s *Service) recordTool(e AuditEntry) {
	s.mu.Lock()
	e.Meeting, e.MeetingID = s.botID, s.cfg.ID
	if e.Status != "started" {
		s.toolCalls++
	}
	path := s.cfg.AuditPath
	s.mu.Unlock()
	if path == "" {
		return
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	auditFileMu.Lock()
	defer auditFileMu.Unlock()
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

var (
	redactAssign = regexp.MustCompile(`(?i)([A-Za-z0-9_.-]*(password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential|auth)[A-Za-z0-9_.-]*\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)`)
	redactBearer = regexp.MustCompile(`(?i)(bearer|token|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	redactPrefix = regexp.MustCompile(`(sk-|ghp_|gho_|github_pat_|xox[abpr]-|AKIA|whsec_|glpat-)[A-Za-z0-9_\-]{6,}`)
	redactLong   = regexp.MustCompile(`[A-Za-z0-9+/_\-]{32,}={0,2}`)
	redactURL    = regexp.MustCompile(`://[^/\s:@]+:[^@\s]+@`)
)

// Redact masks credential-looking parts of a tool target: sensitive
// KEY=value assignments, bearer tokens, well-known token prefixes, URL
// userinfo and long opaque strings. Best effort; tool targets are labels,
// not payloads, and SecretNAT placeholders pass through unchanged.
func Redact(v string) string {
	v = redactBearer.ReplaceAllString(v, "${1} <redacted>")
	v = redactAssign.ReplaceAllString(v, "${1}<redacted>")
	v = redactPrefix.ReplaceAllString(v, "<redacted>")
	v = redactURL.ReplaceAllString(v, "://<redacted>@")
	v = redactLong.ReplaceAllString(v, "<redacted>")
	return strings.TrimSpace(v)
}
