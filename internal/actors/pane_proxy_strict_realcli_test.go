// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package actors

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/creack/pty"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/proxy"
)

// `[proxy] strict` (design 022 §8.2) fired at a REAL vendor CLI.
//
// Everything under it had unit coverage — the signal reaches a process group
// (pane_proxy_strict_test.go), the latch fires once (internal/proxy), the notice
// reads correctly — and none of it had ever been pointed at a binary somebody
// actually runs. These two tests do that, with the real `claude`, a real PTY, a
// real internal/proxy and the production decision functions.
//
// Strict acts on NEGATIVE evidence — "a known CLI ran past GraceWindow and I
// never saw a governed request from it" — so there are two cases, and the second
// is the one that protects users:
//
//	TestStrictStopsARealUngovernedClaude  — escaped CLI ⇒ its process group dies
//	TestStrictSparesARealGovernedClaude   — governed CLI ⇒ it must NOT die
//
// Both are gated on RYSH_TEST_REAL_CLI (an absolute path to the binary) because
// they cost a real CLI startup and depend on a machine that has one installed.
//
// Both pass. Two things they surfaced on the way, recorded because a green test
// is not the same as a safe feature:
//
//   - The second test is a COIN FLIP on a loaded host, not a guarantee (F-14).
//     Cold-start-to-first-request for `claude` 2.1.224 was measured at 9.6s
//     in-process, and at 15.9s / 17.2s / 23.1s / 24.3s against a separate
//     upstream process at load average ~110. GraceWindow is 20s, so the run that
//     took 23.1s would have had a perfectly governed CLI stopped before it could
//     produce the evidence that spares it.
//   - The name strict reports for the CLI here is "2.1.224", not "claude"
//     (F-15) — the native installer's binary IS the version number, so p_comm is
//     the version. The stop still happens, because nothing in the detection path
//     consults the CLI profile table; see TestStrictStopsAnUnknownLongRunningProgram
//     and F-13.
//
// # Re-measured 2026-08-12 against `claude` 2.1.228 (E8/T8)
//
// The numbers above were taken on 2.1.224; `~/.local/bin/claude` now points at
// 2.1.228, so they were re-run rather than trusted. Command, from a rysh-cli
// worktree:
//
//	RYSH_TEST_REAL_CLI=/Users/halilagin/.local/bin/claude GOWORK=off \
//	  go test ./internal/actors/ -run '^TestStrict…$' -v -count=1 -timeout 15m
//
// Six runs, three of each test, on an 8-core host carrying eight concurrent
// agent fleets — i.e. the loaded condition F-14 describes, and then some:
//
//   - Stop case: 3/3 PASS at load 56–70. Strict fired at 20.118s / 20.120s /
//     20.274s against a 20s GraceWindow, the CLI's process group was gone every
//     run, and every run ended governed=0 escaped=3 — so each one really was an
//     escaped CLI mid-conversation and not an idle one.
//   - Spare case: 3/3 PASS at load 128 / 140 / 152. First governed request at
//     4.691s / 6.535s / 2.859s.
//
// F-14 therefore did NOT reproduce, and the margin moved the right way: on the
// same in-process basis, 2.1.224 needed 9.6s and 2.1.228 needs 2.9–6.5s, at
// 13× the load. That is not the same as F-14 being closed. This test measures
// exec → first request against an in-process httptest upstream; the 23.1s and
// 24.3s samples that made F-14 a coin flip were taken against a SEPARATE
// upstream process, a condition nothing here exercises. F-14's worst case is
// untested today, not refuted.
//
// F-15 reproduced exactly, with the version moved on: strict named the CLI
// "2.1.228" in all six runs (`cli=2.1.228` in the log line, `name="2.1.228"`
// from processName). Every literal `2.1.224` in the F-15 backlog row is stale.
//
// F-13 also still reproduces on 2.1.228 — run with RYSH_TEST_KNOWN_FAILING=1 it
// stopped `sleep 120`, a program with no profile, and told the user to run
// `##proxy check sleep`.

// realCLIPath returns the binary under test, or skips.
func realCLIPath(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("RYSH_TEST_REAL_CLI")
	if bin == "" {
		t.Skip("RYSH_TEST_REAL_CLI unset — set it to an absolute path (e.g. " +
			"$HOME/.local/bin/claude) to run the real-binary strict proofs")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("RYSH_TEST_REAL_CLI=%s: %v", bin, err)
	}
	if _, ok := proxy.ProfileFor(bin); !ok {
		t.Skipf("no CLI profile for %q — strict only acts on known CLIs", bin)
	}
	return bin
}

// strictRig is a pane running a real CLI under a real governance proxy, with
// strict on.
type strictRig struct {
	paneID    string
	pane      *PaneActor
	pty       *os.File
	shell     *exec.Cmd
	shellPgid int

	srv          *proxy.Server
	governedReqs *int64                // requests that reached rysh's proxy
	escapedReqs  *int64                // requests that reached the provider directly
	holdEscape   func(d time.Duration) // make the escaped provider sit on its reply

	escapeURL string // a "provider" rysh cannot see
	proxyURL  string // rysh's proxy, pane-scoped, as pane_shell.go injects it

	notices func() string // everything published to the pane
}

func newStrictRig(t *testing.T) *strictRig {
	t.Helper()

	body := `{"id":"msg_strict","type":"message","role":"assistant",` +
		`"content":[{"type":"text","text":"ok"}],"model":"claude-opus-5",` +
		`"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`

	var governed, escaped int64
	var hold atomic.Int64 // nanoseconds the escape upstream sits on a response
	// Released before the servers are closed, so a held handler cannot make
	// httptest.Server.Close block on a connection whose client is already dead.
	teardown := make(chan struct{})
	reply := func(counter *int64, hold *atomic.Int64) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(counter, 1)
			if hold != nil {
				if d := time.Duration(hold.Load()); d > 0 {
					select {
					case <-time.After(d):
					case <-r.Context().Done(): // the CLI died
						return
					case <-teardown:
						return
					}
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}
	}
	// The "provider": a CLI that reaches this one escaped the proxy. Using a
	// stub rather than api.anthropic.com keeps the test off the network and off
	// anyone's bill; from rysh's side the two are identical, because all rysh
	// can observe is the ABSENCE of its own traffic.
	//
	// It can be told to sit on the response, which is what makes the stop case
	// honest: against an instant stub, real `claude --print` completes in about
	// twelve seconds and never reaches the twenty-second grace window at all, so
	// the test would pass or fail on how fast the machine was. Holding the
	// response leaves the CLI demonstrably mid-conversation with a provider rysh
	// cannot see at the moment strict makes its decision.
	escape := httptest.NewServer(reply(&escaped, &hold))
	t.Cleanup(escape.Close)
	// Registered after escape.Close, so it runs BEFORE it (cleanups are LIFO).
	t.Cleanup(func() { close(teardown) })
	// The upstream behind rysh's proxy. Always prompt: the governed case is
	// about the CLI producing evidence, not about how long a turn takes.
	upstream := httptest.NewServer(reply(&governed, nil))
	t.Cleanup(upstream.Close)

	ns, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true,
	})
	if err != nil {
		t.Fatalf("nats: %v", err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Fatal("nats not ready")
	}
	t.Cleanup(ns.Shutdown)
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	t.Cleanup(nc.Close)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())

	paneID := "strict-real-cli-pane"

	// Everything rysh prints into the pane, captured off the wire. The notices
	// are the user-visible half of strict mode, so asserting on them is
	// asserting on what the operator actually sees.
	var mu sync.Mutex
	var seen strings.Builder
	// Subscribed on ">" rather than the pane's own subject: the session prefix
	// is process-global (msg.SetSessionPrefix), so a subject built here can
	// silently stop matching what the publisher builds.
	sub, err := nc.Subscribe(">", func(m *nats.Msg) {
		mu.Lock()
		seen.Write(m.Data)
		seen.WriteByte('\n')
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	srv := proxy.New(pub, nil, nil, map[string]string{"anthropic": upstream.URL}, false)
	base, err := srv.Start("")
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	t.Cleanup(srv.Stop)
	srv.SetStrict(true)

	// A real interactive shell on a real PTY: the foreground process group the
	// whole mechanism reads comes from this fd's TIOCGPGRP.
	shell := exec.Command("/bin/bash", "--norc", "--noprofile", "-i")
	f, err := pty.Start(shell)
	if err != nil {
		t.Skipf("pty.Start unavailable: %v", err)
	}
	t.Cleanup(func() { _ = shell.Process.Kill(); _ = f.Close() })
	go func() {
		b := make([]byte, 4096)
		for {
			if _, e := f.Read(b); e != nil {
				return
			}
		}
	}()

	shellPgid := processPgid(shell.Process.Pid)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && foregroundPgrp(f) != shellPgid {
		time.Sleep(20 * time.Millisecond)
	}
	if fg := foregroundPgrp(f); fg != shellPgid {
		t.Fatalf("shell never reached its prompt (fg=%d shellPgid=%d)", fg, shellPgid)
	}

	return &strictRig{
		paneID:       paneID,
		srv:          srv,
		pane:         &PaneActor{id: paneID, ptyFile: f, shellPgid: shellPgid, pub: pub},
		pty:          f,
		shell:        shell,
		shellPgid:    shellPgid,
		governedReqs: &governed,
		escapedReqs:  &escaped,
		holdEscape:   func(d time.Duration) { hold.Store(int64(d)) },
		escapeURL:    escape.URL,
		proxyURL:     base + "/anthropic/" + paneID,
		notices: func() string {
			mu.Lock()
			defer mu.Unlock()
			return seen.String()
		},
	}
}

// launch types a one-shot run of the CLI into the pane's shell, pointed at
// baseURL. The parent session's CLAUDE_* variables are stripped so the child is
// a plain CLI run and not a nested Claude Code session.
func (r *strictRig) launch(t *testing.T, bin, baseURL string) {
	t.Helper()
	cmd := "env -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT -u CLAUDE_CODE_SESSION_ID " +
		"-u CLAUDE_CODE_CHILD_SESSION -u CLAUDE_CODE_MESSAGING_SOCKET " +
		"-u CLAUDE_CODE_EXECPATH -u CLAUDE_PID -u CLAUDE_EFFORT " +
		"ANTHROPIC_BASE_URL=" + baseURL + " " +
		"ANTHROPIC_API_KEY=rysh-strict-test-dummy " +
		bin + " --print 'reply with the single word ok'\n"
	if _, err := r.pty.Write([]byte(cmd)); err != nil {
		t.Fatalf("write to pty: %v", err)
	}
}

// waitForCLI blocks until something other than the shell owns the terminal and
// returns its process group.
func (r *strictRig) waitForCLI(t *testing.T, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if fg := foregroundPgrp(r.pty); fg > 0 && fg != r.shellPgid {
			return fg
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no foreground program appeared within %s", within)
	return -1
}

// tick runs exactly what pane_shell.go's read loop runs on every burst of PTY
// output: re-read the foreground group, tell the proxy about it, and let the
// latch decide. Nothing here is test-only logic.
func (r *strictRig) tick(lastFg *int) {
	fg := foregroundPgrp(r.pty)
	if fg != *lastFg {
		r.pane.noteForegroundProgram(fg)
		*lastFg = fg
	}
	r.pane.emitUnproxiedWarning()
}

// groupMembers lists the live pids in a process group, so the assertions can
// name what died rather than asserting that a function returned nil.
func groupMembers(pgid int) string {
	out, err := exec.Command("ps", "-o", "pid=,pgid=,comm=", "-g", fmt.Sprint(pgid)).Output()
	if err != nil {
		return "(none)"
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var kept []string
	for _, l := range lines {
		if s := strings.TrimSpace(l); s != "" {
			kept = append(kept, s)
		}
	}
	if len(kept) == 0 {
		return "(none)"
	}
	return strings.Join(kept, " | ")
}

// TestStrictStopsARealUngovernedClaude is the stop case: a real `claude` whose
// traffic goes somewhere rysh cannot see must be stopped once it runs past the
// grace window, and the whole process GROUP must go — a surviving child is a
// process still holding a provider connection.
func TestStrictStopsARealUngovernedClaude(t *testing.T) {
	bin := realCLIPath(t)
	rig := newStrictRig(t)

	// The escaped provider sits on its reply, so the CLI is still mid-request
	// when the grace window expires rather than having completed and exited.
	rig.holdEscape(90 * time.Second)

	// Pointed at the stub "provider", NOT at rysh's proxy: this is an escaped
	// CLI, byte for byte what an operator turns strict mode on to catch.
	rig.launch(t, bin, rig.escapeURL)
	cliPgid := rig.waitForCLI(t, 60*time.Second)
	t.Logf("claude foreground process group %d: %s", cliPgid, groupMembers(cliPgid))
	if !processGroupAlive(cliPgid) {
		t.Fatalf("the CLI's group %d was already gone", cliPgid)
	}

	// Do not start judging until the CLI has demonstrably reached the provider
	// rysh cannot see. Without this the test could pass against an idle CLI,
	// which is the ambiguity strict mode is criticised for — not the escape it
	// claims to catch.
	escapeDeadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(escapeDeadline) && atomic.LoadInt64(rig.escapedReqs) == 0 {
		time.Sleep(100 * time.Millisecond)
	}
	if n := atomic.LoadInt64(rig.escapedReqs); n == 0 {
		t.Fatalf("the CLI never reached the escaped provider, so it was idle "+
			"rather than escaped and this run proves nothing (group: %s)",
			groupMembers(cliPgid))
	} else {
		t.Logf("CLI is on the wire to a provider rysh cannot see: %d request(s)", n)
	}

	started := time.Now()
	lastFg := -1
	deadline := started.Add(proxy.GraceWindow + 60*time.Second)
	var stoppedAt time.Duration
	nextLog := time.Duration(0)
	for time.Now().Before(deadline) {
		rig.tick(&lastFg)
		if el := time.Since(started); el >= nextLog {
			nextLog = el + 5*time.Second
			t.Logf("t=%3.0fs fg=%d name=%q state=%s alive=%v governed=%d escaped=%d",
				el.Seconds(), foregroundPgrp(rig.pty), processName(foregroundPgrp(rig.pty)),
				rig.srv.PaneGovernanceState(rig.paneID), processGroupAlive(cliPgid),
				atomic.LoadInt64(rig.governedReqs), atomic.LoadInt64(rig.escapedReqs))
		}
		if strings.Contains(rig.notices(), "strict: stopping") && stoppedAt == 0 {
			stoppedAt = time.Since(started)
			t.Logf("strict fired at %s (GraceWindow=%s); group %d now: %s",
				stoppedAt.Round(time.Millisecond), proxy.GraceWindow,
				cliPgid, groupMembers(cliPgid))
		}
		if stoppedAt != 0 && !processGroupAlive(cliPgid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	notices := rig.notices()
	if !strings.Contains(notices, "has been running with no governed request") {
		t.Fatalf("no unproxied warning was published; pane saw:\n%s", notices)
	}
	if !strings.Contains(notices, "strict: stopping") {
		t.Fatalf("strict was on and the CLI ran past %s ungoverned, but nothing "+
			"was stopped; pane saw:\n%s", proxy.GraceWindow, notices)
	}
	if processGroupAlive(cliPgid) {
		t.Fatalf("process group %d survived strict mode: %s", cliPgid, groupMembers(cliPgid))
	}
	if n := atomic.LoadInt64(rig.governedReqs); n != 0 {
		t.Fatalf("this run was supposed to be UNGOVERNED but %d request(s) "+
			"reached rysh's proxy — the test proved nothing", n)
	}
	t.Logf("stopped at %s · governed requests=%d escaped requests=%d · group after: %s",
		stoppedAt.Round(time.Millisecond),
		atomic.LoadInt64(rig.governedReqs), atomic.LoadInt64(rig.escapedReqs),
		groupMembers(cliPgid))
}

// TestStrictSparesARealGovernedClaude is the case that protects users from
// strict mode: a real `claude` whose traffic DOES reach rysh's proxy must be
// left alone. An implementation that stops it has turned a governance feature
// into an outage for the exact users who configured it correctly.
//
// This test is LOAD-DEPENDENT — see F-14 (not F-13) and the file header. It
// asserts a property the code does not actually guarantee: that a governed CLI
// produces its first request inside GraceWindow. Whether it does is a race
// between the CLI's cold start and a 20s timer.
//
// On 2.1.224 that race was close enough to lose — 23.1s and 24.3s samples
// against a separate upstream process, versus a 20s window. On 2.1.228,
// 2026-08-12, it was won three times out of three at load 128–152, with the
// first governed request at 4.691s / 6.535s / 2.859s. A green run here is
// evidence about this host, this CLI build and this load, and not a proof that
// strict cannot stop a correctly-governed CLI.
func TestStrictSparesARealGovernedClaude(t *testing.T) {
	bin := realCLIPath(t)
	rig := newStrictRig(t)

	// Pointed at rysh's proxy exactly as pane_shell.go injects it.
	rig.launch(t, bin, rig.proxyURL)
	cliPgid := rig.waitForCLI(t, 30*time.Second)
	t.Logf("claude foreground process group %d: %s", cliPgid, groupMembers(cliPgid))

	started := time.Now()
	lastFg := -1
	var firstGoverned, stoppedAt time.Duration
	// Long enough to outlast a cold start on a loaded host AND to leave the
	// grace window well behind, so "not stopped" means it survived the verdict
	// rather than that the verdict had not been reached yet.
	deadline := started.Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		rig.tick(&lastFg)
		if firstGoverned == 0 && atomic.LoadInt64(rig.governedReqs) > 0 {
			firstGoverned = time.Since(started)
			t.Logf("first governed request reached the proxy at %s (GraceWindow=%s)",
				firstGoverned.Round(time.Millisecond), proxy.GraceWindow)
		}
		if stoppedAt == 0 && strings.Contains(rig.notices(), "strict: stopping") {
			stoppedAt = time.Since(started)
		}
		// Done once the CLI has been governed and has outlived the window.
		if firstGoverned != 0 && time.Since(started) > proxy.GraceWindow+15*time.Second {
			break
		}
		if !processGroupAlive(cliPgid) && firstGoverned == 0 && stoppedAt == 0 {
			break // it exited on its own; the assertions below judge that
		}
		time.Sleep(100 * time.Millisecond)
	}

	if n := atomic.LoadInt64(rig.governedReqs); n == 0 {
		t.Fatalf("no request ever reached rysh's proxy in %s, so this run says "+
			"nothing about the governed case; pane saw:\n%s",
			time.Since(started).Round(time.Second), rig.notices())
	}
	t.Logf("governed requests=%d · first at %s · escaped=%d",
		atomic.LoadInt64(rig.governedReqs), firstGoverned.Round(time.Millisecond),
		atomic.LoadInt64(rig.escapedReqs))

	if notices := rig.notices(); strings.Contains(notices, "strict: stopping") {
		t.Fatalf("F-13: strict stopped a GOVERNED claude at %s — its first "+
			"governed request landed at %s but GraceWindow is %s, so the "+
			"evidence that would have spared it arrived after the verdict.\n"+
			"pane saw:\n%s",
			stoppedAt.Round(time.Millisecond), firstGoverned.Round(time.Millisecond),
			proxy.GraceWindow, notices)
	}
}

// launchRaw types an arbitrary command into the pane's shell.
func (r *strictRig) launchRaw(t *testing.T, line string) {
	t.Helper()
	if _, err := r.pty.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("write to pty: %v", err)
	}
}

// TestStrictStopsAnUnknownLongRunningProgram is finding F-13 written as an
// executable statement of what SHOULD happen.
//
// Design 022 §4.4 scopes the detector to a *known agent CLI*: "When a known
// agent CLI is the pane's foreground process and no governed request arrives
// within a grace window, warn once." internal/proxy/compat.go maintains the
// profile table that defines "known", and internal/proxy/wirecheck consults it.
// The detection path does not. PaneActor.noteForegroundProgram passes whatever
// processName returned straight to Server.NoteForeground, govWatch stores any
// non-empty string, and emitUnproxiedWarning acts on it — so under
// `[proxy] strict` the SIGTERM lands on any foreground program that runs longer
// than GraceWindow without producing governed traffic. A build, a test run, an
// ssh session, an editor.
//
// Observed, with `sleep 120` as the pane's foreground program:
//
//	sleep pgid=46191 processName="sleep" profileKnown=false
//	t= 20s state=unproxied?
//	WARN proxy strict: stopped an ungoverned CLI pane=dbg-pane cli=sleep pgid=46191
//	group died at t=21.4s
//	[proxy] warning: 'sleep' has been running with no governed request …
//	Confirm with `##proxy check sleep`
//
// Skipped by default: it asserts the fixed behaviour and therefore fails until
// the gate lands, and a permanently red test in the default suite trains people
// to ignore red. Run it with RYSH_TEST_KNOWN_FAILING=1.
//
// The fix is not the one-line gate it looks like, which is why this is filed
// rather than patched here: adding `if _, ok := proxy.ProfileFor(binary); !ok`
// would ALSO stop rysh detecting the real `claude`, whose process name on a
// native install is "2.1.224" and not "claude" (F-15). The two have to be fixed
// together.
func TestStrictStopsAnUnknownLongRunningProgram(t *testing.T) {
	if os.Getenv("RYSH_TEST_KNOWN_FAILING") == "" {
		t.Skip("F-13 is open — strict stops any long-running foreground program, " +
			"not only a known agent CLI. Set RYSH_TEST_KNOWN_FAILING=1 to run this " +
			"regression target; it fails until the known-CLI gate lands.")
	}
	rig := newStrictRig(t)

	rig.launchRaw(t, "sleep 120")
	pgid := rig.waitForCLI(t, 30*time.Second)
	name := processName(pgid)
	if _, known := proxy.ProfileFor(name); known {
		t.Fatalf("processName(%d)=%q is a known CLI — pick a program rysh has no "+
			"profile for, or this test asserts nothing", pgid, name)
	}
	t.Logf("foreground program pgid=%d name=%q knownCLI=false: %s",
		pgid, name, groupMembers(pgid))

	started := time.Now()
	lastFg := -1
	for time.Since(started) < proxy.GraceWindow+15*time.Second {
		rig.tick(&lastFg)
		time.Sleep(100 * time.Millisecond)
	}

	notices := rig.notices()
	if strings.Contains(notices, "strict: stopping") || !processGroupAlive(pgid) {
		t.Fatalf("F-13: strict stopped %q, which is not an agent CLI and has no "+
			"profile in internal/proxy/compat.go. Any build, test run or editor "+
			"that occupies a pane for %s is killed the same way.\ngroup now: %s\npane saw:\n%s",
			name, proxy.GraceWindow, groupMembers(pgid), notices)
	}
	if strings.Contains(notices, "no governed request") {
		t.Fatalf("F-13: strict warned about %q, a program rysh has no profile for; "+
			"the notice even tells the user to run `##proxy check %s`.\npane saw:\n%s",
			name, name, notices)
	}
}
