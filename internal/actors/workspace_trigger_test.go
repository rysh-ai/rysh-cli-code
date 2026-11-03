// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/cron"
	"github.com/rysh-ai/rysh-cli-code/internal/policy"
	"github.com/rysh-ai/rysh-cli-code/internal/trigger"
)

// newTriggerWorkspace is newCronFireWorkspace plus a loaded trigger service
// whose secrets live in a temp .rysh dir.
func newTriggerWorkspace(t *testing.T) *WorkspaceActor {
	w, _ := newTriggerWorkspaceSub(t)
	return w
}

func newTriggerWorkspaceSub(t *testing.T) (*WorkspaceActor, *nats.Subscription) {
	t.Helper()
	w, sub := newCronFireWorkspace(t)
	w.cfg.RyshDir = filepath.Join(t.TempDir(), ".rysh")
	w.policy = &policy.Policy{Loaded: true}
	w.triggers = &triggerService{loaded: true}
	t.Cleanup(w.stopTriggers)
	return w, sub
}

func triggerCmd(w *WorkspaceActor, cmd string) string {
	var out strings.Builder
	w.handleTriggerCommand(&out, cmd)
	return out.String()
}

func TestTriggerAddRefusesShellAndCommands(t *testing.T) {
	w := newTriggerWorkspace(t)
	for _, cmd := range []string{
		`trigger add t1 --mode shell rm -rf build`,
		`trigger add t2 ##secret list`,
		`trigger add t3 #deploy`,
		`trigger add t4 @@bot stop`,
		`trigger add ../t5 @a hi`,
	} {
		out := triggerCmd(w, cmd)
		if !strings.Contains(out, "[trigger]") || strings.Contains(out, "added") {
			t.Errorf("%q was not refused: %q", cmd, out)
		}
	}
	if n := len(w.triggers.state.Triggers); n != 0 {
		t.Fatalf("refused adds left %d trigger(s)", n)
	}
	if _, err := os.Stat(w.triggerDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused add created the secret dir (err %v)", err)
	}
}

// The secret is written 0600 and never appears in pane output (which is
// persisted); add and show print only its path.
func TestTriggerAddWritesSecretFileAndNeverPrintsIt(t *testing.T) {
	w := newTriggerWorkspace(t)
	out := triggerCmd(w, `trigger add gh-push @reviewer review the latest push`)
	if !strings.Contains(out, `added "gh-push"`) {
		t.Fatalf("add failed: %q", out)
	}
	path := trigger.SecretPath(w.triggerDir(), "gh-push")
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("secret file %v mode %v, want 0600", err, st)
	}
	secret, ok := trigger.ReadSecret(w.triggerDir(), "gh-push")
	if !ok {
		t.Fatal("secret not readable")
	}
	show := triggerCmd(w, `trigger show gh-push`)
	rot := triggerCmd(w, `trigger rotate gh-push`)
	rotated, _ := trigger.ReadSecret(w.triggerDir(), "gh-push")
	for name, text := range map[string]string{"add": out, "show": show, "rotate": rot} {
		if bytes.Contains([]byte(text), secret) || bytes.Contains([]byte(text), rotated) {
			t.Errorf("%s output contains the secret value", name)
		}
		if name != "rotate" && !strings.Contains(text, path) {
			t.Errorf("%s output does not point at the secret file", name)
		}
	}
	if bytes.Equal(secret, rotated) {
		t.Error("rotate kept the old secret")
	}
}

func addTrigger(t *testing.T, w *WorkspaceActor, cmd string) {
	t.Helper()
	if out := triggerCmd(w, cmd); !strings.Contains(out, "added") {
		t.Fatalf("add failed: %q", out)
	}
}

func TestTriggerFireRoutesAndRecords(t *testing.T) {
	w, sub := newTriggerWorkspaceSub(t)
	addTrigger(t, w, `trigger add standup write the standup note`)

	res := w.handleTriggerFire(nil, trigger.FireRequest{Name: "standup", DeliveryID: "d-7", BodyBytes: 12})

	if res.Status != cron.StatusOK {
		t.Fatalf("status = %q (%s), want ok", res.Status, res.Note)
	}
	if n := paneInputs(t, sub); n != 1 {
		t.Fatalf("delivered %d input(s), want 1", n)
	}
	tr := w.findTrigger("standup")
	if tr.RunCount != 1 || !strings.Contains(tr.Runs[0].Note, "d-7") || !strings.Contains(tr.Runs[0].Note, "12 bytes") {
		t.Fatalf("run record = %+v", tr.Runs)
	}
}

func TestTriggerFireUnderBlockedPolicyIsBlocked(t *testing.T) {
	w, sub := newTriggerWorkspaceSub(t)
	addTrigger(t, w, `trigger add gh-push @reviewer review the push`)
	w.policy = blockedPolicy()

	res := w.handleTriggerFire(nil, trigger.FireRequest{Name: "gh-push"})

	if res.Status != cron.StatusBlocked {
		t.Fatalf("status = %q, want blocked", res.Status)
	}
	if n := paneInputs(t, sub); n != 0 {
		t.Fatalf("a blocked fire delivered %d input(s)", n)
	}
	if got := w.findTrigger("gh-push").LastStatus; got != cron.StatusBlocked {
		t.Fatalf("recorded %q, want blocked", got)
	}
}

func TestTriggerDisabledAndSkippedRouteNothing(t *testing.T) {
	w, sub := newTriggerWorkspaceSub(t)
	addTrigger(t, w, `trigger add nightly summarise the day`)

	if res := w.handleTriggerFire(nil, trigger.FireRequest{Name: "nightly", Skip: "rate limited"}); res.Status != cron.StatusSkipped {
		t.Fatalf("skip request status = %q", res.Status)
	}
	triggerCmd(w, `trigger disable nightly`)
	if res := w.handleTriggerFire(nil, trigger.FireRequest{Name: "nightly"}); res.Status != cron.StatusSkipped {
		t.Fatalf("disabled status = %q", res.Status)
	}
	if n := paneInputs(t, sub); n != 0 {
		t.Fatalf("skipped fires delivered %d input(s)", n)
	}
	if got := w.findTrigger("nightly").RunCount; got != 2 {
		t.Fatalf("run count = %d, want 2 skipped runs", got)
	}
}

func TestTriggerRemoveDeletesSecret(t *testing.T) {
	w := newTriggerWorkspace(t)
	addTrigger(t, w, `trigger add tmp @a hi`)
	triggerCmd(w, `trigger rm tmp`)
	if w.findTrigger("tmp") != nil {
		t.Fatal("trigger still listed after rm")
	}
	if _, ok := trigger.ReadSecret(w.triggerDir(), "tmp"); ok {
		t.Fatal("secret still on disk after rm")
	}
}

// Like cron.jobs: an unreadable stored state is never overwritten.
func TestTriggerAddRefusedWhileStateUnreadable(t *testing.T) {
	kv := &cronKV{value: []byte(`{"triggers":[{"name":"keep"}]}`),
		getErrs: []error{errors.New("nats: timeout"), errors.New("nats: timeout")}}
	w := newTriggerWorkspace(t)
	w.wKV = kv
	w.startTriggers() // first error

	out := triggerCmd(w, `trigger add other @a hi`) // second error

	if !strings.Contains(out, "could not be read") {
		t.Fatalf("add not refused: %q", out)
	}
	if kv.puts != 0 {
		t.Fatalf("refused add wrote the state (%d puts)", kv.puts)
	}
}

func TestTriggerStatePersistsAndReloads(t *testing.T) {
	kv := &cronKV{}
	w := newTriggerWorkspace(t)
	w.wKV = kv
	addTrigger(t, w, `trigger add gh-push @reviewer review`)
	triggerCmd(w, `trigger disable gh-push`)

	w2 := newTriggerWorkspace(t)
	w2.wKV = kv
	w2.startTriggers()
	tr := w2.findTrigger("gh-push")
	if tr == nil || tr.Enabled || tr.Input != "@reviewer review" {
		t.Fatalf("reloaded trigger = %+v", tr)
	}
}

func TestTriggerListenRefusesNonLoopback(t *testing.T) {
	w := newTriggerWorkspace(t)
	out := triggerCmd(w, `trigger listen 0.0.0.0:0`)
	if !strings.Contains(out, "loopback only") || w.triggers.server != nil {
		t.Fatalf("non-loopback listen not refused: %q", out)
	}
	if w.triggers.state.Listen != "" {
		t.Fatalf("refused listen was persisted: %q", w.triggers.state.Listen)
	}
}

// End to end on 127.0.0.1: listen, a signed POST fires the trigger and records
// it; a bad signature does not; stop closes the listener.
func TestTriggerListenEndToEnd(t *testing.T) {
	w, sub := newTriggerWorkspaceSub(t)
	var mu sync.Mutex // stands in for the mailbox: one fire at a time
	w.triggerDispatch = func(req trigger.FireRequest) trigger.FireResult {
		mu.Lock()
		defer mu.Unlock()
		return w.handleTriggerFire(nil, req)
	}
	addTrigger(t, w, `trigger add gh-push write the push summary`)
	if out := triggerCmd(w, `trigger listen 127.0.0.1:0`); !strings.Contains(out, "listening on") {
		t.Fatalf("listen failed: %q", out)
	}
	url := "http://" + w.triggers.server.Addr() + "/t/gh-push"
	secret, _ := trigger.ReadSecret(w.triggerDir(), "gh-push")

	post := func(sig string) int {
		body := []byte(`{"after":"abc"}`)
		req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		req.Header.Set(trigger.SignatureHeader, sig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	if code := post("sha256=00"); code != http.StatusUnauthorized {
		t.Fatalf("bad signature code = %d, want 401", code)
	}
	if code := post(trigger.Sign(secret, []byte(`{"after":"abc"}`))); code != http.StatusAccepted {
		t.Fatalf("signed code = %d, want 202", code)
	}
	mu.Lock()
	runs := w.findTrigger("gh-push").RunCount
	mu.Unlock()
	if runs != 1 {
		t.Fatalf("run count = %d, want 1 (the bad signature must not record)", runs)
	}
	if n := paneInputs(t, sub); n != 1 {
		t.Fatalf("delivered %d input(s), want 1", n)
	}
	triggerCmd(w, `trigger stop`)
	if w.triggers.server != nil || w.triggers.state.Listen != "" {
		t.Fatal("stop left the listener on")
	}
}

// LEAD-CHANGES (4): a webhook whose answer timed out must not fire later. The
// waiter and the mailbox race for the fire with one compare-and-swap.
func TestTriggerFireAbandonedAfterTimeoutNeverRuns(t *testing.T) {
	w, sub := newTriggerWorkspaceSub(t)
	addTrigger(t, w, `trigger add slow write the summary`)
	var queued *triggerFireMsg
	fire := w.triggerMailboxFire(func(m *triggerFireMsg) { queued = m }, 20*time.Millisecond)

	res := fire(trigger.FireRequest{Name: "slow"})
	if res.Status == cron.StatusOK {
		t.Fatalf("a fire that never reached the mailbox answered ok")
	}
	w.handleTriggerFireMsg(nil, queued) // the mailbox gets to it late

	if n := paneInputs(t, sub); n != 0 {
		t.Fatalf("an abandoned fire still delivered %d input(s)", n)
	}
	tr := w.findTrigger("slow")
	if tr.RunCount != 1 || tr.Runs[0].Status != cron.StatusSkipped {
		t.Fatalf("abandoned fire record = %+v, want one skipped run", tr.Runs)
	}
}

// When the mailbox claims the fire first, the waiter waits for its answer.
func TestTriggerFireClaimedByMailboxIsAnswered(t *testing.T) {
	w := newTriggerWorkspace(t)
	addTrigger(t, w, `trigger add quick write the summary`)
	fire := w.triggerMailboxFire(func(m *triggerFireMsg) {
		go w.handleTriggerFireMsg(nil, m)
	}, 5*time.Second)

	if res := fire(trigger.FireRequest{Name: "quick"}); res.Status != cron.StatusOK {
		t.Fatalf("status = %q (%s), want ok", res.Status, res.Note)
	}
}

// LEAD-CHANGES (4b): the `try` helper reads the secret from its file (never
// argv) and must produce the signature the server verifies.
func TestTriggerTryHelperSignsLikeTheServer(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	w := newTriggerWorkspace(t)
	addTrigger(t, w, `trigger add sig @a hi`)
	path := trigger.SecretPath(w.triggerDir(), "sig")
	body := []byte(`{"after":"abc"}`)
	cmd := exec.Command(py, "-I", "-c", triggerSignPy, path)
	cmd.Stdin = bytes.NewReader(body)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper failed: %v", err)
	}
	secret, _ := trigger.ReadSecret(w.triggerDir(), "sig")
	if got := "sha256=" + strings.TrimSpace(string(out)); !trigger.Verify(secret, body, got) {
		t.Fatal("the try helper's signature does not verify")
	}
	if strings.Contains(triggerSignPy, string(secret)) {
		t.Fatal("helper source carries the secret")
	}
}
