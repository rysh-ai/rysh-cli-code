// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/cron"
	"github.com/rysh-ai/rysh-cli-code/internal/trigger"
)

// ---------------------------------------------------------------------------
// Inbound webhook triggers (gap06)
//
// A trigger is a named, fixed input (an @agent prompt or a plain prompt) that a
// signed POST to http://127.0.0.1:<port>/t/<name> fires. The HTTP goroutine
// only authenticates (internal/trigger.Handler); the fire itself is delivered
// into the WorkspaceActor mailbox as a triggerFireMsg and runs through
// fireScheduledInput — the same path as a cron job, so approvals, the
// governance proxy, SecretNAT, the sandbox and fail-closed policy apply
// exactly as for typed input. Triggers persist to the workspace KV under
// triggerKVKey with cron's fail-safe load; each secret lives in
// .rysh/triggers/<name>.secret (0600) and is never printed.
// ---------------------------------------------------------------------------

// triggerKVKey is the workspace-KV key holding the trigger state.
const triggerKVKey = "trigger.state"

// triggerFireTimeout bounds how long a webhook waits for the mailbox to take
// the fire. A fire the mailbox has not started by then is abandoned for good
// (it never runs later); one it has started is waited for to the end.
const triggerFireTimeout = 10 * time.Second

// triggerState is what persists: the triggers and the listen address ("" =
// listener off), so a daemon restart re-binds.
type triggerState struct {
	Listen   string             `json:"listen,omitempty"`
	Triggers []*trigger.Trigger `json:"triggers"`
}

// triggerService owns the session's triggers and the listener.
type triggerService struct {
	state triggerState
	// loaded mirrors cronScheduler.loaded: until the stored state has been
	// read (or confirmed absent), nothing is persisted over it.
	loaded bool
	server *trigger.Server
}

// triggerFireMsg carries one authenticated fire into the mailbox. claim
// decides, once, who owns it: the mailbox (fireClaimed) or the waiter that
// gave up (fireAbandoned).
type triggerFireMsg struct {
	req   trigger.FireRequest
	reply chan trigger.FireResult
	claim atomic.Int32
}

const (
	fireClaimed   = 1
	fireAbandoned = 2
)

// triggerDir is where trigger secrets live.
func (w *WorkspaceActor) triggerDir() string {
	return filepath.Join(w.cfg.RyshDir, "triggers")
}

// startTriggers restores the trigger state and, if it was listening, re-binds.
func (w *WorkspaceActor) startTriggers() {
	w.triggers = &triggerService{}
	if err := w.triggerLoad(); err != nil {
		slog.Warn("trigger: persisted triggers unavailable at start — retrying every minute; stored triggers will not be overwritten", "err", err)
		return
	}
	w.triggerRestoreListener()
}

// triggerTick retries a failed load; it rides the cron minute tick.
func (w *WorkspaceActor) triggerTick() {
	if w.triggers == nil || w.triggers.loaded {
		return
	}
	if err := w.triggerLoad(); err != nil {
		slog.Warn("trigger: persisted triggers still unavailable", "err", err)
		return
	}
	w.triggerRestoreListener()
}

func (w *WorkspaceActor) triggerRestoreListener() {
	if addr := w.triggers.state.Listen; addr != "" {
		if err := w.triggerStartListener(addr); err != nil {
			slog.Warn("trigger: could not re-bind the listener", "addr", addr, "err", err)
		}
	}
}

// stopTriggers closes the listener (idempotent).
func (w *WorkspaceActor) stopTriggers() {
	if w.triggers != nil && w.triggers.server != nil {
		_ = w.triggers.server.Close()
		w.triggers.server = nil
	}
}

func (w *WorkspaceActor) triggerLoad() error {
	if w.wKV == nil {
		w.triggers.loaded = true
		return nil
	}
	entry, err := w.wKV.Get(triggerKVKey)
	if errors.Is(err, nats.ErrKeyNotFound) {
		w.triggers.state = triggerState{}
		w.triggers.loaded = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", triggerKVKey, err)
	}
	var st triggerState
	if err := json.Unmarshal(entry.Value(), &st); err != nil {
		return fmt.Errorf("decode %s: %w", triggerKVKey, err)
	}
	w.triggers.state = st
	w.triggers.loaded = true
	return nil
}

func (w *WorkspaceActor) triggerPersist() {
	if w.wKV == nil || w.triggers == nil || !w.triggers.loaded {
		return
	}
	if data, err := json.Marshal(w.triggers.state); err == nil {
		_, _ = w.wKV.Put(triggerKVKey, data)
	}
}

func (w *WorkspaceActor) findTrigger(name string) *trigger.Trigger {
	if w.triggers == nil {
		return nil
	}
	for _, t := range w.triggers.state.Triggers {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// triggerStartListener binds the loopback listener. Fires go through
// w.triggerDispatch when set (tests), else through the mailbox.
func (w *WorkspaceActor) triggerStartListener(addr string) error {
	w.stopTriggers()
	fire := w.triggerDispatch
	if fire == nil {
		self, system := w.selfPID, w.actorSystem
		if self == nil || system == nil {
			return errors.New("the workspace mailbox is not running")
		}
		fire = w.triggerMailboxFire(func(m *triggerFireMsg) { system.Root.Send(self, m) }, triggerFireTimeout)
	}
	dir := w.triggerDir()
	h := &trigger.Handler{
		Secret: func(name string) ([]byte, bool) { return trigger.ReadSecret(dir, name) },
		Fire:   fire,
		Now:    w.cronNow,
	}
	srv, err := trigger.Listen(addr, h)
	if err != nil {
		return err
	}
	w.triggers.server = srv
	return nil
}

// triggerMailboxFire returns the HTTP side of a fire: hand the request to the
// mailbox via send and wait up to timeout for the mailbox to claim it. If the
// waiter gives up first it wins the claim, and the mailbox later records the
// fire as skipped without running it — a webhook that was answered "not
// fired" never fires afterwards. If the mailbox claimed it first, the fire is
// already running, so the waiter waits for its real outcome.
func (w *WorkspaceActor) triggerMailboxFire(send func(*triggerFireMsg), timeout time.Duration) func(trigger.FireRequest) trigger.FireResult {
	return func(req trigger.FireRequest) trigger.FireResult {
		m := &triggerFireMsg{req: req, reply: make(chan trigger.FireResult, 1)}
		send(m)
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case r := <-m.reply:
			return r
		case <-timer.C:
			if m.claim.CompareAndSwap(0, fireAbandoned) {
				return trigger.FireResult{Status: cron.StatusError, Note: "workspace busy — not fired"}
			}
			return <-m.reply
		}
	}
}

// handleTriggerFireMsg is the mailbox side: claim the fire, or record that the
// webhook already gave up on it.
func (w *WorkspaceActor) handleTriggerFireMsg(ctx actor.Context, m *triggerFireMsg) {
	if !m.claim.CompareAndSwap(0, fireClaimed) {
		if t := w.findTrigger(m.req.Name); t != nil {
			t.RecordRun(cron.Run{At: w.cronClock(), Status: cron.StatusSkipped,
				Note: "not fired: the webhook answer had already timed out"})
			w.triggerPersist()
		}
		return
	}
	m.reply <- w.handleTriggerFire(ctx, m.req)
}

// handleTriggerFire runs one fire on the mailbox goroutine and records it.
func (w *WorkspaceActor) handleTriggerFire(ctx actor.Context, req trigger.FireRequest) trigger.FireResult {
	if w.triggers == nil || !w.triggers.loaded {
		return trigger.FireResult{Status: cron.StatusError, Note: "triggers not loaded"}
	}
	t := w.findTrigger(req.Name)
	if t == nil {
		return trigger.FireResult{Status: cron.StatusError, Note: "unknown trigger"}
	}
	from := fmt.Sprintf("delivery %s, %d bytes", orDash(req.DeliveryID), req.BodyBytes)
	var res trigger.FireResult
	switch {
	case req.Skip != "":
		res = trigger.FireResult{Status: cron.StatusSkipped, Note: req.Skip}
	case !t.Enabled:
		res = trigger.FireResult{Status: cron.StatusSkipped, Note: "trigger disabled"}
	default:
		paneID := w.cronResolveTarget(t.Target)
		if paneID == "" {
			res = trigger.FireResult{Status: cron.StatusError, Note: "target pane not found: " + t.Target}
			break
		}
		status, note := w.fireScheduledInput(ctx, paneID, "trigger", t.Name, trigger.EffectiveMode(t.Mode, t.Input), t.Input)
		res = trigger.FireResult{Status: status, Note: note}
	}
	t.RecordRun(cron.Run{At: w.cronClock(), Status: res.Status, Note: truncateCron(from+" · "+res.Note, 200)})
	w.triggerPersist()
	return res
}

// ---------------------------------------------------------------------------
// ##trigger command
// ---------------------------------------------------------------------------

func (w *WorkspaceActor) handleTriggerCommand(out *strings.Builder, rawCmd string) {
	if w.triggers == nil {
		fmt.Fprintf(out, "\n[trigger] triggers are not available in this workspace\n")
		w.failRysh("triggers are not available in this workspace")
		return
	}
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rawCmd), "trigger"))
	fields := strings.Fields(rest)
	sub := ""
	if len(fields) > 0 {
		sub = fields[0]
	}
	if !w.triggers.loaded {
		if err := w.triggerLoad(); err != nil {
			fmt.Fprintf(out, "\n[trigger] the saved trigger list could not be read (%v) — nothing changed; retrying every minute\n", err)
			w.failRysh("saved triggers unreadable: %v", err)
			return
		}
		w.triggerRestoreListener()
	}
	needName := func(usage string) (string, bool) {
		if len(fields) < 2 {
			ryshWriter(out).UsageLineIn("trigger", usage)
			w.failRyshUsage("usage: %s", usage)
			return "", false
		}
		return fields[1], true
	}

	switch sub {
	case "", "list":
		w.triggerList(out)
	case "add":
		w.triggerAdd(out, strings.TrimSpace(strings.TrimPrefix(rest, "add")))
	case "show":
		if name, ok := needName("##trigger show <name>"); ok {
			w.triggerShow(out, name)
		}
	case "rm", "remove", "delete":
		if name, ok := needName("##trigger rm <name>"); ok {
			w.triggerRemove(out, name)
		}
	case "enable", "disable":
		if name, ok := needName(fmt.Sprintf("##trigger %s <name>", sub)); ok {
			w.triggerSetEnabled(out, name, sub == "enable")
		}
	case "rotate":
		if name, ok := needName("##trigger rotate <name>"); ok {
			w.triggerRotate(out, name)
		}
	case "logs":
		if name, ok := needName("##trigger logs <name> [N]"); ok {
			n := 10
			if len(fields) > 2 {
				if v, err := parseIntCron(fields[2]); err == nil && v > 0 {
					n = v
				}
			}
			w.triggerLogs(out, name, n)
		}
	case "listen":
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(trigger.DefaultPort))
		if len(fields) > 1 {
			addr = fields[1]
		}
		w.triggerListen(out, addr)
	case "stop":
		w.stopTriggers()
		w.triggers.state.Listen = ""
		w.triggerPersist()
		fmt.Fprintf(out, "\n[trigger] listener stopped\n")
	case "status":
		w.triggerStatus(out)
	case "help":
		w.triggerUsage(out)
	default:
		ryshWriter(out).UnknownIn("trigger", sub)
		w.failRyshUsage("unknown %s subcommand: %q", "trigger", sub)
		w.triggerUsage(out)
	}
}

func (w *WorkspaceActor) triggerUsage(out *strings.Builder) {
	ryshWriter(out).Usage(
		"##trigger add <name> [--pane <id|name|active>] <@agent prompt | prompt>",
		"##trigger list | show <name> | logs <name> [N]",
		"##trigger enable <name> | disable <name> | rm <name> | rotate <name>",
		"##trigger listen [127.0.0.1:<port>] | stop | status   loopback listener (default port 23320)",
		"fire: POST /t/<name> with X-Rysh-Signature-256: sha256=<HMAC-SHA256(secret, body)>",
		"      (GitHub's X-Hub-Signature-256 works too); the body is never passed to the agent",
		"triggers fire while the session daemon is alive (running or detached)",
	)
}

// triggerSignPy prints the hex HMAC-SHA256 of stdin keyed by the file named in
// argv[1] (the secret file, never the secret).
const triggerSignPy = `import hmac,hashlib,sys;print(hmac.new(open(sys.argv[1],"rb").read().strip(),sys.stdin.buffer.read(),hashlib.sha256).hexdigest())`

// triggerURL is the local URL for a trigger, or "" while not listening.
func (w *WorkspaceActor) triggerURL(name string) string {
	if w.triggers.server == nil {
		return ""
	}
	return "http://" + w.triggers.server.Addr() + "/t/" + name
}

func (w *WorkspaceActor) triggerAdd(out *strings.Builder, argstr string) {
	const usage = "##trigger add <name> [--pane <id|name|active>] <@agent prompt | prompt>"
	name, rest := nextToken(argstr)
	if name == "" {
		ryshWriter(out).UsageLineIn("trigger", usage)
		w.failRyshUsage("usage: %s", usage)
		return
	}
	target := "active"
	mode := ""
	for {
		rest = strings.TrimSpace(rest)
		flag, after := nextToken(rest)
		if flag == "--pane" || flag == "-p" {
			target, rest = nextToken(after)
			continue
		}
		if flag == "--mode" || flag == "-m" {
			mode, rest = nextToken(after)
			continue
		}
		break
	}
	input := strings.TrimSpace(rest)
	if err := trigger.Validate(name, mode, input); err != nil {
		fmt.Fprintf(out, "\n[trigger] %v\n", err)
		w.failRysh("%v", err)
		return
	}
	if w.findTrigger(name) != nil {
		fmt.Fprintf(out, "\n[trigger] a trigger named %q already exists — rm it first or pick another name\n", name)
		w.failRysh("a trigger named %q already exists", name)
		return
	}
	if len(w.triggers.state.Triggers) >= trigger.MaxTriggers {
		fmt.Fprintf(out, "\n[trigger] trigger limit reached (%d)\n", trigger.MaxTriggers)
		w.failRysh("trigger limit reached (%d)", trigger.MaxTriggers)
		return
	}
	if err := trigger.WriteSecret(w.triggerDir(), name); err != nil {
		fmt.Fprintf(out, "\n[trigger] could not write the secret file: %v\n", err)
		w.failRysh("could not write the secret file: %v", err)
		return
	}
	t := &trigger.Trigger{ID: uuid.New().String(), Name: name, Target: target, Mode: mode,
		Input: input, Enabled: true, CreatedAt: w.cronClock()}
	w.triggers.state.Triggers = append(w.triggers.state.Triggers, t)
	w.triggerPersist()

	fmt.Fprintf(out, "\n[trigger] added %q\n", name)
	w.triggerDetails(out, t)
}

// triggerDetails prints a trigger's settings and how to call it. The secret is
// shown only as its file path: pane output is persisted.
func (w *WorkspaceActor) triggerDetails(out *strings.Builder, t *trigger.Trigger) {
	secret := trigger.SecretPath(w.triggerDir(), t.Name)
	fmt.Fprintf(out, "  input   : %s\n", t.Input)
	fmt.Fprintf(out, "  target  : %s\n", t.Target)
	fmt.Fprintf(out, "  enabled : %v\n", t.Enabled)
	fmt.Fprintf(out, "  secret  : %s (0600; never shown here)\n", secret)
	url := w.triggerURL(t.Name)
	if url == "" {
		fmt.Fprintf(out, "  url     : listener off — ##trigger listen, then POST /t/%s\n", t.Name)
		url = fmt.Sprintf("http://127.0.0.1:%d/t/%s", trigger.DefaultPort, t.Name)
	} else {
		fmt.Fprintf(out, "  url     : %s\n", url)
	}
	// The signing helper reads the secret from the file inside python, so the
	// value never appears in any process's argv.
	fmt.Fprintf(out, "  try     : BODY='{}'; curl -sS -X POST -d \"$BODY\" -H \"%s: sha256=$(printf %%s \"$BODY\" | python3 -c %s %s)\" %s\n",
		trigger.SignatureHeader, shellQuote(triggerSignPy), shellQuote(secret), url)
}

func (w *WorkspaceActor) triggerList(out *strings.Builder) {
	if len(w.triggers.state.Triggers) == 0 {
		fmt.Fprintf(out, "\n[trigger] no triggers — ##trigger add <name> <@agent prompt>\n")
		return
	}
	listen := "off"
	if w.triggers.server != nil {
		listen = w.triggers.server.Addr()
	}
	fmt.Fprintf(out, "\n[trigger] triggers (%d) · listener: %s\n", len(w.triggers.state.Triggers), listen)
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 72))
	for _, t := range w.triggers.state.Triggers {
		state := "on"
		if !t.Enabled {
			state = "off"
		}
		fmt.Fprintf(out, "  %-16s [%s] runs:%d  last:%s  %s\n",
			t.Name, state, t.RunCount, orDash(t.LastStatus), truncateCron(t.Input, 36))
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 72))
}

func (w *WorkspaceActor) triggerShow(out *strings.Builder, name string) {
	t := w.findTrigger(name)
	if t == nil {
		fmt.Fprintf(out, "\n[trigger] no trigger named %q\n", name)
		w.failRysh("no trigger named %q", name)
		return
	}
	fmt.Fprintf(out, "\n[trigger] %q\n", t.Name)
	w.triggerDetails(out, t)
	fmt.Fprintf(out, "  last run: %s (%s) · runs: %d\n", fmtCronTime(t.LastRun), orDash(t.LastStatus), t.RunCount)
}

func (w *WorkspaceActor) triggerRemove(out *strings.Builder, name string) {
	for i, t := range w.triggers.state.Triggers {
		if t.Name == name {
			if err := trigger.RemoveSecret(w.triggerDir(), name); err != nil {
				fmt.Fprintf(out, "\n[trigger] could not remove the secret file: %v — trigger kept\n", err)
				w.failRysh("could not remove the secret file: %v", err)
				return
			}
			w.triggers.state.Triggers = append(w.triggers.state.Triggers[:i], w.triggers.state.Triggers[i+1:]...)
			w.triggerPersist()
			fmt.Fprintf(out, "\n[trigger] removed %q and its secret\n", name)
			return
		}
	}
	fmt.Fprintf(out, "\n[trigger] no trigger named %q\n", name)
	w.failRysh("no trigger named %q", name)
}

func (w *WorkspaceActor) triggerSetEnabled(out *strings.Builder, name string, enabled bool) {
	t := w.findTrigger(name)
	if t == nil {
		fmt.Fprintf(out, "\n[trigger] no trigger named %q\n", name)
		w.failRysh("no trigger named %q", name)
		return
	}
	t.Enabled = enabled
	w.triggerPersist()
	if enabled {
		fmt.Fprintf(out, "\n[trigger] enabled %q\n", name)
	} else {
		fmt.Fprintf(out, "\n[trigger] disabled %q — POSTs answer 409 and are logged as skipped\n", name)
	}
}

func (w *WorkspaceActor) triggerRotate(out *strings.Builder, name string) {
	if w.findTrigger(name) == nil {
		fmt.Fprintf(out, "\n[trigger] no trigger named %q\n", name)
		w.failRysh("no trigger named %q", name)
		return
	}
	if err := trigger.WriteSecret(w.triggerDir(), name); err != nil {
		fmt.Fprintf(out, "\n[trigger] could not rotate the secret: %v\n", err)
		w.failRysh("could not rotate the secret: %v", err)
		return
	}
	fmt.Fprintf(out, "\n[trigger] rotated the secret of %q — %s; the old one no longer verifies\n",
		name, trigger.SecretPath(w.triggerDir(), name))
}

func (w *WorkspaceActor) triggerLogs(out *strings.Builder, name string, n int) {
	t := w.findTrigger(name)
	if t == nil {
		fmt.Fprintf(out, "\n[trigger] no trigger named %q\n", name)
		w.failRysh("no trigger named %q", name)
		return
	}
	if len(t.Runs) == 0 {
		fmt.Fprintf(out, "\n[trigger] %q has not fired yet\n", name)
		return
	}
	runs := t.Runs
	if len(runs) > n {
		runs = runs[len(runs)-n:]
	}
	fmt.Fprintf(out, "\n[trigger] %q — last %d run(s)\n", name, len(runs))
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
	for _, r := range runs {
		fmt.Fprintf(out, "  %s  %-7s %s\n", fmtCronTime(r.At), r.Status, r.Note)
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
}

func (w *WorkspaceActor) triggerListen(out *strings.Builder, addr string) {
	norm, err := trigger.LoopbackAddr(addr)
	if err != nil {
		fmt.Fprintf(out, "\n[trigger] %v\n", err)
		w.failRysh("%v", err)
		return
	}
	if err := w.triggerStartListener(norm); err != nil {
		fmt.Fprintf(out, "\n[trigger] %v\n", err)
		w.failRysh("%v", err)
		return
	}
	w.triggers.state.Listen = norm
	w.triggerPersist()
	fmt.Fprintf(out, "\n[trigger] listening on http://%s/t/<name> (loopback only; expose it through your own tunnel)\n", w.triggers.server.Addr())
}

func (w *WorkspaceActor) triggerStatus(out *strings.Builder) {
	if w.triggers.server == nil {
		fmt.Fprintf(out, "\n[trigger] listener off · %d trigger(s) — ##trigger listen\n", len(w.triggers.state.Triggers))
		return
	}
	fmt.Fprintf(out, "\n[trigger] listening on http://%s · %d trigger(s)\n", w.triggers.server.Addr(), len(w.triggers.state.Triggers))
}
