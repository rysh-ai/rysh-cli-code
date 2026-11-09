// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/ansakeys"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// `ansa keys` — press named keys in ONE pane (`rysh ansa keys`, `##ansa keys`).
//
// Why it exists (2026-11-01): a claude hung mid-turn with seven messages queued
// behind it. One Esc in its pane would have freed it, and nothing could press
// it: `ansa send` types text and submits it, tab-msg never touches the PTY,
// and `##ansa interrupt` is ESC-only and runs on the `##` path, which focuses
// and echoes into a pane. This is the general form, with a silent agent door.
//
// THE GUARD, and why it has this shape. Keys in another pane can approve a
// permission prompt (Enter), change claude's permission mode (shift+tab), or
// end a turn or a process (ctrl+c, ctrl+d). So:
//
//   - ONE target, resolved exactly: a pane id, a unique @given-name, or a
//     unique claude.session_id. No fleet, no wildcard, no list. Zero or two
//     matches are refusals, never a guess.
//   - SAFE keys (esc, arrows, home/end, page up/down, backspace, delete,
//     ctrl+u) need nothing more. None of them can submit, confirm or signal.
//   - GUARDED keys (enter, tab, shift+tab, every other ctrl+<letter>) need
//     --force, and the pane must answer a liveness probe. --force is required
//     whatever the pane runs: "it is a claude" is not a safety signal, because
//     a claude is exactly where Enter approves a tool call.
//   - EVERY send is audited before the first key is written, to
//     <session dir>/<session>/ansa-keys-audit.jsonl, and a send that cannot be
//     audited is not sent. Refusals are audited too (best effort). The line
//     holds who (--as), the target, and the canonical key names; never text,
//     and never a word the caller typed that was not a known key name.
//
// What --as is NOT: authentication. Like `ansa send`, it is the caller's own
// statement of who it is. The session bus is the trust boundary; anything
// that can reach it can already type into any pane through `ansa send`.
//
// What a receipt does NOT claim: that the key had an effect. Nothing
// acknowledges a raw key; the receipt says the keys were published.

// ansaKeysAuditFile is the per-session audit log of key sends.
const ansaKeysAuditFile = "ansa-keys-audit.jsonl"

// ansaKeyGap separates key presses. Each press is its own PTY write: a lone
// ESC followed at once by other bytes reads as an escape sequence (or alt+key)
// to the program, and a TUI that sees a burst may take it as a paste.
var ansaKeyGap = 80 * time.Millisecond

// ansaKeysRequest is what either door hands the edge.
type ansaKeysRequest struct {
	Door          string // "cli" or "##", for the audit line
	From          string // --as; optional
	To            string // pane id or @given-name
	ClaudeSession string // claude.session_id meta value
	Keys          []string
	Force         bool
}

// ansaKeysOutcome is the receipt plus what the caller should be told.
type ansaKeysOutcome struct {
	Result  *msg.MsgAnsaRouteResult
	Keys    string // canonical key names, "" if they did not parse
	Program string // the pane's foreground program, if the probe answered
	Probed  bool   // the pane answered the liveness probe
}

// ansaKeysAuditEntry is one line of ansa-keys-audit.jsonl.
type ansaKeysAuditEntry struct {
	Time    time.Time `json:"time"`
	Door    string    `json:"door,omitempty"`
	From    string    `json:"from,omitempty"`
	Ref     string    `json:"ref,omitempty"`    // what the caller addressed
	Target  string    `json:"target,omitempty"` // the resolved pane id
	Persona string    `json:"persona,omitempty"`
	Keys    string    `json:"keys,omitempty"`
	Force   bool      `json:"force,omitempty"`
	Program string    `json:"program,omitempty"`
	Status  string    `json:"status"` // "sent" or "refused"
	Code    string    `json:"code,omitempty"`
}

// Keys presses keys in a pane addressed BY ID (the router invariant: names are
// resolved at the edge). base carries the audit fields the edge knows. audit
// must succeed before the first key is written.
func (r *ansaRouter) Keys(base ansaKeysAuditEntry, paneID string, specs []string, force bool,
	audit func(ansaKeysAuditEntry) error) *ansaKeysOutcome {
	presses, err := ansakeys.Parse(specs)
	if err != nil {
		return &ansaKeysOutcome{Result: msg.AnsaRefusal(msg.AnsaErrBadKey, "%v", err)}
	}
	out := &ansaKeysOutcome{Keys: ansakeys.Canonical(presses)}

	to := strings.TrimSpace(paneID)
	if to == "" {
		out.Result = msg.AnsaRefusal(msg.AnsaErrNoTarget, "no target: name a pane id")
		return out
	}
	panes, err := r.tr.Panes()
	if err != nil {
		out.Result = msg.AnsaRefusal(msg.AnsaErrDirectory,
			"cannot enumerate the session's panes, so %q can be neither found nor ruled out: %v", to, err)
		return out
	}
	target, found := ansaPaneByID(panes, to)
	if !found {
		if ansaNameExists(panes, to) {
			out.Result = msg.AnsaRefusal(msg.AnsaErrNotAnID,
				"%q is a pane NAME, not a pane id — resolve @name at the edge and send the id", to)
			return out
		}
		out.Result = msg.AnsaRefusal(msg.AnsaErrUnknownTarget, "no pane with id %q in this session", to)
		return out
	}

	guarded := ansakeys.GuardedNames(presses)
	if len(guarded) > 0 && !force {
		out.Result = msg.AnsaRefusal(msg.AnsaErrNeedsForce,
			"%s can submit, confirm or signal something in pane %s (%s); nothing was sent. "+
				"Add --force if that is what you mean",
			strings.Join(guarded, ", "), target.ID, ansaPersona(target))
		return out
	}

	// The probe tells the receipt what owns the PTY. For safe keys a silent
	// pane is not a refusal: a hung pane is the one that needs Esc, and Esc to
	// a pane that has exited is a no-op (see Interrupt). Guarded keys are
	// another matter: they are only sent to a pane that answers.
	program, perr := r.tr.Probe(target.ID)
	out.Probed = perr == nil
	if out.Probed {
		out.Program = program
	}
	if len(guarded) > 0 && !out.Probed {
		out.Result = msg.AnsaRefusal(msg.AnsaErrUnreachable,
			"pane %s (%s) did not answer a liveness probe, and guarded keys (%s) are only sent "+
				"to a pane that answers; nothing was sent: %v",
			target.ID, ansaPersona(target), strings.Join(guarded, ", "), perr)
		return out
	}

	entry := base
	entry.Time = time.Now().UTC()
	entry.Target = target.ID
	entry.Persona = ansaPersona(target)
	entry.Keys = out.Keys
	entry.Force = force
	entry.Program = out.Program
	entry.Status = "sent"
	if err := audit(entry); err != nil {
		out.Result = msg.AnsaRefusal(msg.AnsaErrAudit,
			"the key send could not be written to the audit log, so nothing was sent: %v", err)
		return out
	}

	unlock := r.lockLane(target.ID)
	err = r.tr.SendKeys(target.ID, ansakeys.Writes(presses))
	unlock()
	if err != nil {
		out.Result = msg.AnsaRefusal(msg.AnsaErrPublishFailed,
			"publishing keys to pane %s failed (some may have been sent): %v", target.ID, err)
		return out
	}
	out.Result = &msg.MsgAnsaRouteResult{OK: true, TargetPaneID: target.ID, TargetPersona: ansaPersona(target)}
	return out
}

// ansaKeysAtEdge resolves the target the caller named, then hands the router
// an id. Both doors call this; refusals are audited here, best effort.
func ansaKeysAtEdge(r *ansaRouter, req ansaKeysRequest, audit func(ansaKeysAuditEntry) error) *ansaKeysOutcome {
	to := strings.TrimSpace(req.To)
	sid := strings.TrimSpace(req.ClaudeSession)
	base := ansaKeysAuditEntry{Door: req.Door, From: clipRunes(strings.TrimSpace(req.From), 64)}
	switch {
	case sid != "":
		base.Ref = "claude-session:" + clipRunes(sid, 80)
	default:
		base.Ref = clipRunes(to, 80)
	}

	out := ansaKeysResolveAndSend(r, req, to, sid, base, audit)

	if !out.Result.OK {
		e := base
		e.Time = time.Now().UTC()
		e.Target = out.Result.TargetPaneID
		e.Keys = out.Keys
		e.Force = req.Force
		e.Program = out.Program
		e.Status = "refused"
		e.Code = out.Result.Code
		_ = audit(e)
	}
	slog.Info("ansa keys", "door", base.Door, "from", base.From, "ref", base.Ref,
		"target", out.Result.TargetPaneID, "keys", out.Keys, "force", req.Force,
		"ok", out.Result.OK, "code", out.Result.Code)
	return out
}

func ansaKeysResolveAndSend(r *ansaRouter, req ansaKeysRequest, to, sid string, base ansaKeysAuditEntry,
	audit func(ansaKeysAuditEntry) error) *ansaKeysOutcome {
	// Keys first: a bad key name is the caller's mistake and is reported
	// before the session is even looked at.
	presses, err := ansakeys.Parse(req.Keys)
	if err != nil {
		return &ansaKeysOutcome{Result: msg.AnsaRefusal(msg.AnsaErrBadKey, "%v", err)}
	}
	keys := ansakeys.Canonical(presses)
	refuse := func(res *msg.MsgAnsaRouteResult) *ansaKeysOutcome {
		return &ansaKeysOutcome{Result: res, Keys: keys}
	}
	if to != "" && sid != "" {
		return refuse(msg.AnsaRefusal(msg.AnsaErrNoTarget,
			"name ONE target: a pane id, an @given-name, or --claude-session, not two"))
	}
	if to == "" && sid == "" {
		return refuse(msg.AnsaRefusal(msg.AnsaErrNoTarget,
			"no target: name a pane id, an @given-name, or --claude-session <id>"))
	}
	panes, err := r.tr.Panes()
	if err != nil {
		return refuse(msg.AnsaRefusal(msg.AnsaErrDirectory,
			"cannot enumerate the session's panes, so the target can be neither found nor ruled out: %v", err))
	}
	var target ansaPane
	var refusal *msg.MsgAnsaRouteResult
	if sid != "" {
		target, refusal = ansaResolveClaudeSession(panes, sid)
	} else {
		target, refusal = ansaResolveTarget(panes, to)
	}
	if refusal != nil {
		return refuse(refusal)
	}
	return r.Keys(base, target.ID, req.Keys, req.Force, audit)
}

// ansaResolveClaudeSession finds the ONE pane whose claude.session_id meta is
// sid. Zero matches and two matches are both refusals; two can happen when a
// session was resumed in a second pane or a stamp went stale.
func ansaResolveClaudeSession(panes []ansaPane, sid string) (ansaPane, *msg.MsgAnsaRouteResult) {
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return ansaPane{}, msg.AnsaRefusal(msg.AnsaErrNoTarget, "no claude session id given")
	}
	var matches []ansaPane
	for _, p := range panes {
		if strings.TrimSpace(p.meta(metaClaudeSessionID)) == sid {
			matches = append(matches, p)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return ansaPane{}, msg.AnsaRefusal(msg.AnsaErrUnknownTarget,
			"no pane in this session carries %s=%s", metaClaudeSessionID, sid)
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.ID)
		}
		sort.Strings(ids)
		res := msg.AnsaRefusal(msg.AnsaErrAmbiguousTarget,
			"%d panes carry %s=%s (%s); nothing was sent — re-send to one pane id",
			len(ids), metaClaudeSessionID, sid, strings.Join(ids, ", "))
		res.Candidates = ids
		return ansaPane{}, res
	}
}

// ansaSendKeys publishes one MsgRawKeyInput per key press, ansaKeyGap apart.
func ansaSendKeys(pub *msg.NATSPublisher, paneID string, writes [][]byte) error {
	subject := msg.T("pane", paneID, "rawinput")
	for i, w := range writes {
		if i > 0 {
			time.Sleep(ansaKeyGap)
		}
		if err := pub.Send(subject, &msg.MsgRawKeyInput{PaneID: paneID, Data: w}); err != nil {
			return err
		}
	}
	return nil
}

func (t *natsAnsaTransport) SendKeys(paneID string, writes [][]byte) error {
	return ansaSendKeys(t.pub, paneID, writes)
}

func (t *workspaceAnsaTransport) SendKeys(paneID string, writes [][]byte) error {
	return ansaSendKeys(t.w.pub, paneID, writes)
}

// ansaKeysReceipt is the success text both doors print.
func ansaKeysReceipt(out *ansaKeysOutcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "keys sent to %s (pane %s): %s", out.Result.TargetPersona, out.Result.TargetPaneID, out.Keys)
	if out.Program != "" {
		fmt.Fprintf(&b, " [running %s]", out.Program)
	}
	b.WriteString("\n")
	if !out.Probed {
		b.WriteString("note: the pane did not answer a liveness probe; the keys were published, " +
			"but nothing acknowledges a raw key\n")
	}
	return b.String()
}

// parseAnsaKeysArgs reads `##ansa keys` arguments:
//
//	<@name|pane-id> [--force] [--] <key>...
//	--claude-session <id> [--force] [--] <key>...
func parseAnsaKeysArgs(args []string) (ansaKeysRequest, error) {
	var req ansaKeysRequest
	var rest []string
	for i := 0; i < len(args); i++ {
		a := strings.TrimSpace(args[i])
		switch {
		case a == "":
		case a == "--":
			rest = append(rest, args[i+1:]...)
			i = len(args)
		case a == "--force":
			req.Force = true
		case a == "--claude-session":
			if i+1 >= len(args) || strings.HasPrefix(strings.TrimSpace(args[i+1]), "-") {
				return req, fmt.Errorf("--claude-session needs a session id")
			}
			req.ClaudeSession = strings.TrimSpace(args[i+1])
			i++
		case strings.HasPrefix(a, "--claude-session="):
			req.ClaudeSession = strings.TrimSpace(strings.TrimPrefix(a, "--claude-session="))
			if req.ClaudeSession == "" {
				return req, fmt.Errorf("--claude-session needs a session id")
			}
		case strings.HasPrefix(a, "-"):
			return req, fmt.Errorf("unknown flag %q to ansa keys (keys go to ONE pane: no --fleet, no lists)", a)
		default:
			rest = append(rest, a)
		}
	}
	if req.ClaudeSession == "" {
		if len(rest) == 0 {
			return req, fmt.Errorf("ansa keys needs a target: a pane id, an @given-name, or --claude-session <id>")
		}
		req.To, rest = rest[0], rest[1:]
	}
	if len(rest) == 0 {
		return req, fmt.Errorf("ansa keys needs at least one key, e.g. esc")
	}
	req.Keys = rest
	return req, nil
}

var ansaKeysAuditMu sync.Mutex

// appendAnsaKeysAudit appends one JSON line, creating the file 0600.
func appendAnsaKeysAudit(path string, e ansaKeysAuditEntry) error {
	if path == "" {
		return fmt.Errorf("no session directory")
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	ansaKeysAuditMu.Lock()
	defer ansaKeysAuditMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}
