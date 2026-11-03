// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Pane agent profile (design 093 §12.3 H1): the pane side.
//
// The profile lives in pane meta (domain.PaneMetaAgentTools /
// domain.PaneMetaAgentPromptFile), which rides the pane's own durable KV
// record. The pane turns it into a MsgSetPaneAgentProfile for its LLM actor:
//
//   - when the LLM actor is spawned (pane start, and so every daemon restart),
//     right after Spawn — the message is in the actor's mailbox before its
//     inbox subscription exists, so it lands before the first turn can;
//   - whenever a profile key changes (`##pane profile set|clear`).
//
// It is sent to the actor's PID, not over NATS, for that ordering guarantee.

// maxProfilePromptBytes bounds a profile prompt read from disk.
const maxProfilePromptBytes = 64 << 10

// paneProfileDir resolves the pane-profiles directory: the agentic Setup's
// (the daemon's workspace root) when there is one, else <base>/.rysh/pane-profiles.
// The workspace (writer) and the pane (reader) both resolve it this way, so the
// pane can demand the EXACT path it expects instead of trusting meta.
func paneProfileDir(s *agentic.Setup, base string) string {
	if d := s.PaneProfileDir(); d != "" {
		return d
	}
	if strings.TrimSpace(base) == "" {
		base = "."
	}
	if abs, err := filepath.Abs(base); err == nil {
		base = abs
	}
	return filepath.Join(base, ".rysh", domain.PaneProfileDirName)
}

// paneProfilePromptPath is the only path a pane's profile prompt may have.
func paneProfilePromptPath(dir, paneID string) string {
	return filepath.Join(dir, paneID+".md")
}

// paneAgentProfileFromMeta builds the profile message from pane meta. The
// prompt file must be exactly want (paneProfilePromptPath); a different path,
// a symlink, a missing/unreadable/oversized file is an error, and the
// allow-list still applies without it (fail closed on tools: a broken prompt
// never widens the tool set).
func paneAgentProfileFromMeta(meta map[string]string, want string) (*msg.MsgSetPaneAgentProfile, error) {
	m := &msg.MsgSetPaneAgentProfile{AllowTools: domain.ParseAgentTools(meta[domain.PaneMetaAgentTools])}
	path := strings.TrimSpace(meta[domain.PaneMetaAgentPromptFile])
	if path == "" {
		return m, nil
	}
	prompt, err := readProfilePrompt(path, want)
	if err != nil {
		return m, err
	}
	m.SystemPrompt = prompt
	return m, nil
}

// readProfilePrompt reads a profile prompt file (e20 W1). Only the exact path
// want is accepted — <workspace>/.rysh/pane-profiles/<pane-id>.md, computed by
// the reader, never taken on trust from meta (which a MsgPaneSetMeta published
// straight on the pane inbox, or a hand-edited KV record, could set). The file
// and its directory must not be symlinks; the file is Lstat'ed, opened, and the
// opened file must be the same one (no swap between the check and the read).
func readProfilePrompt(path, want string) (string, error) {
	name := filepath.Base(path)
	if want == "" || filepath.Clean(path) != filepath.Clean(want) {
		return "", fmt.Errorf("profile prompt file %q is not this pane's profile file", name)
	}
	if di, err := os.Lstat(filepath.Dir(path)); err != nil || !di.IsDir() || di.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("profile prompt directory for %q is not a plain directory", name)
	}
	li, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("profile prompt file: %w", err)
	}
	if li.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("profile prompt file %q is a symlink: refused", name)
	}
	if !li.Mode().IsRegular() {
		return "", fmt.Errorf("profile prompt file %q is not a regular file", name)
	}
	if li.Size() > maxProfilePromptBytes {
		return "", fmt.Errorf("profile prompt file %q is larger than %d bytes", name, maxProfilePromptBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("profile prompt file: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !os.SameFile(li, fi) {
		return "", fmt.Errorf("profile prompt file %q changed while being read: refused", name)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxProfilePromptBytes+1))
	if err != nil {
		return "", fmt.Errorf("profile prompt file: %w", err)
	}
	if len(b) > maxProfilePromptBytes {
		return "", fmt.Errorf("profile prompt file %q is larger than %d bytes", name, maxProfilePromptBytes)
	}
	return string(b), nil
}

// applyAgentProfile sends the pane's current profile (possibly empty, which
// restores the defaults) to its LLM actor.
func (p *PaneActor) applyAgentProfile(ctx actor.Context) {
	if ctx == nil || p.llmPromptExecPID == nil {
		return
	}
	m, err := paneAgentProfileFromMeta(p.meta,
		paneProfilePromptPath(paneProfileDir(p.agSetup, p.cfg.WorkingDirectory), p.id))
	if err != nil {
		slog.Warn("pane: agent profile prompt not applied", "pane", p.id, "err", err)
	}
	if len(m.AllowTools) > 0 || m.SystemPrompt != "" {
		slog.Info("pane: agent profile applied", "pane", p.id, "tools", m.AllowTools, "prompt_bytes", len(m.SystemPrompt))
	}
	ctx.Send(p.llmPromptExecPID, m)
}

// hasAgentProfileMeta reports whether meta holds any profile key.
func hasAgentProfileMeta(meta map[string]string) bool {
	return strings.TrimSpace(meta[domain.PaneMetaAgentTools]) != "" ||
		strings.TrimSpace(meta[domain.PaneMetaAgentPromptFile]) != ""
}

// reapplyAgentProfileOnSpawn re-sends a stored profile to a just-spawned LLM
// actor (pane start, daemon restart). A pane without profile meta sends
// nothing, so its actor starts exactly as before.
func (p *PaneActor) reapplyAgentProfileOnSpawn(ctx actor.Context) {
	if hasAgentProfileMeta(p.meta) {
		p.applyAgentProfile(ctx)
	}
}
