// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package peercoord

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	pc "github.com/rysh-ai/rysh-cli-shared/peercoord"
	"github.com/rysh-ai/rysh-cli-code/internal/peer"
)

type coordServer struct {
	mu       sync.Mutex
	store    pc.Store
	events   []pc.Event
	receipts map[string]pc.Response
	drop     string
}

func newCoordServer(t *testing.T) (*coordServer, *httptest.Server) {
	t.Helper()
	s := &coordServer{store: pc.NewStore(), receipts: map[string]pc.Response{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			var req pc.Request
			if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
				t.Error(e)
				return
			}
			if cached, ok := s.receipts[req.ID]; ok && req.Op != "heartbeat" {
				cached.Replayed = true
				json.NewEncoder(w).Encode(cached)
				return
			}
			updated, resp, events, e := pc.Apply(s.store, pc.Auth{UserID: "test-user", WorkspaceRole: "owner", PeerToken: r.Header.Get("X-Rysh-Peer-Token"), ExecutionToken: r.Header.Get("X-Rysh-Execution-Token")}, req, time.Now())
			if e != nil {
				w.WriteHeader(409)
				known := e.(*pc.Error)
				json.NewEncoder(w).Encode(map[string]string{"code": known.Code, "error": known.Message})
				return
			}
			s.store = updated
			for _, ev := range events {
				ev.Sequence = int64(len(s.events) + 1)
				s.events = append(s.events, ev)
			}
			s.receipts[req.ID] = resp
			if s.drop == req.Op {
				s.drop = ""
				h, _, _ := w.(http.Hijacker).Hijack()
				h.Close()
				return
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/events") {
			var after int64
			fmt.Sscan(r.URL.Query().Get("after"), &after)
			page := pc.Page{Next: after, Events: []pc.Event{}}
			for _, ev := range s.events {
				if ev.Sequence > after {
					page.Events = append(page.Events, ev)
					page.Next = ev.Sequence
				}
			}
			json.NewEncoder(w).Encode(page)
			return
		}
		state := s.store.State
		state.ServerTime = time.Now()
		json.NewEncoder(w).Encode(state)
	}))
	t.Cleanup(server.Close)
	return s, server
}

type testHost struct {
	mu       sync.Mutex
	panes    map[string]string
	launches map[string]int
}

func (h *testHost) call(_ context.Context, q HostRequest) HostResponse {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch q.Op {
	case "secret":
		return HostResponse{Value: "test-workspace-key"}
	case "inspect":
		v, ok := h.panes[q.Pane]
		return HostResponse{Exists: ok && (q.Execution == "" || v == q.Execution)}
	case "create":
		h.panes[q.Pane] = q.Execution
		return HostResponse{Exists: true}
	case "launch":
		h.launches[q.Execution]++
		return HostResponse{}
	}
	return HostResponse{Err: fmt.Errorf("unexpected host request")}
}
func testRuntime(t *testing.T, url, session, role string) (*Runtime, *testHost, string) {
	t.Helper()
	dir := t.TempDir()
	origin := uuid.NewString()
	host := &testHost{panes: map[string]string{origin: ""}, launches: map[string]int{}}
	c := peer.Connection{Enabled: true, URL: url, Workspace: uuid.NewString(), Board: "team", KeySecret: "TEST_KEY", SessionID: uuid.NewString()}
	if e := peer.Save(dir, session, c); e != nil {
		t.Fatal(e)
	}
	r := New(dir, session, host.call)
	r.probe = func(context.Context, string) error { return nil }
	if e := r.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(r.Close)
	verb := "join"
	if role == "leader" {
		verb = "init"
	}
	runCommand(t, r, origin, verb, "--name", role, "--runner", "codex", "--project", "app")
	repo := filepath.Join(t.TempDir(), "repo")
	os.MkdirAll(repo, 0700)
	ctx := context.Background()
	for _, args := range [][]string{{"init"}, {"config", "user.email", "peer@test.invalid"}, {"config", "user.name", "Peer test"}} {
		if _, e := git(ctx, repo, args...); e != nil {
			t.Fatal(e)
		}
	}
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0600)
	git(ctx, repo, "add", "README.md")
	if _, e := git(ctx, repo, "commit", "-m", "initial"); e != nil {
		t.Fatal(e)
	}
	runCommand(t, r, origin, "project", "add", "app", repo)
	return r, host, origin
}
func runCommand(t *testing.T, r *Runtime, pane string, args ...string) any {
	t.Helper()
	out, e := r.Command(context.Background(), "local", pane, args)
	if e != nil {
		t.Fatalf("%v: %v", args, e)
	}
	return out
}
func enrollRuntime(t *testing.T, r *Runtime, pane, name, leader string) {
	args := []string{"enroll", name, "--permission", "workspace", "--mode", "auto", "--max-runtime", "60"}
	if leader != "" {
		args = append(args, "--accept-leader", leader)
	}
	runCommand(t, r, pane, args...)
	runCommand(t, r, pane, "listen", "start")
	r.tick()
}
func firstBoard(r *Runtime) *Board {
	for _, b := range r.data.Boards {
		return b
	}
	return nil
}
func TestListenerAcquiresApprovedStandbyAfterLeaseExpiry(t *testing.T) {
	s, server := newCoordServer(t)
	leader, _, lp := testRuntime(t, server.URL, "one", "leader")
	standby, _, sp := testRuntime(t, server.URL, "two", "worker")
	registered := runCommand(t, standby, sp, "register", "--name", "backup", "--role", "leader", "--runner", "codex", "--project", "app").(pc.Response)
	enrollRuntime(t, standby, sp, "backup", "")
	runCommand(t, leader, lp, "leader", "standbys", "--agents", registered.Agent.ID, "--auto")
	leader.Close()
	leader.mu.Lock()
	leader.mu.Unlock()
	s.mu.Lock()
	s.store.State.Leadership.LeaseUntil = time.Now().Add(-time.Second)
	s.mu.Unlock()
	standby.tick()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store.State.Leadership.AgentID != registered.Agent.ID || s.store.State.Leadership.Term != 2 {
		t.Fatal("listener failed to acquire approved standby leadership")
	}
}
func TestSamePaneEndpointsRequireExplicitSender(t *testing.T) {
	_, server := newCoordServer(t)
	r, _, pane := testRuntime(t, server.URL, "one", "leader")
	runCommand(t, r, pane, "register", "--name", "second", "--runner", "codex", "--project", "app")
	_, err := r.Command(context.Background(), "local", pane, []string{"send", "--to", "one/second", "--", "hello"})
	if err == nil || !strings.Contains(err.Error(), "specify --agent") {
		t.Fatalf("ambiguous sender error: %v", err)
	}
	runCommand(t, r, pane, "send", "--agent", "leader", "--to", "one/second", "--", "hello")
}
func TestLocalSetupAndManualAdmissionCannotLaunchEarly(t *testing.T) {
	_, server := newCoordServer(t)
	leader, _, lp := testRuntime(t, server.URL, "one", "leader")
	worker, host, wp := testRuntime(t, server.URL, "two", "worker")
	worker.mu.Lock()
	worker.probe = func(context.Context, string) error { return fmt.Errorf("runner login required") }
	worker.mu.Unlock()
	_, err := worker.Command(context.Background(), "local", wp, []string{"enroll", "worker", "--accept-leader", "one/leader", "--permission", "workspace", "--mode", "auto"})
	if err == nil || !strings.Contains(err.Error(), "login required") {
		t.Fatalf("missing login enrollment: %v", err)
	}
	worker.mu.Lock()
	if firstBoard(worker).Endpoints["worker"].Enrolled {
		t.Fatal("missing login enabled execution")
	}
	worker.probe = func(context.Context, string) error { return nil }
	worker.mu.Unlock()
	runCommand(t, worker, wp, "enroll", "worker", "--accept-leader", "one/leader", "--permission", "workspace", "--mode", "manual")
	runCommand(t, worker, wp, "listen", "start")
	worker.tick()
	bad := runCommand(t, leader, lp, "task", "assign", "--to", "two/worker", "--project", "app", "--base", "missing-commit", "--criteria", "evidence", "--", "inspect").(pc.Response)
	_, err = worker.Command(context.Background(), "local", wp, []string{"task", "admit", bad.Task.ID})
	if err == nil {
		t.Fatal("unavailable base admitted")
	}
	good := runCommand(t, leader, lp, "task", "assign", "--to", "two/worker", "--project", "app", "--criteria", "evidence", "--", "inspect").(pc.Response)
	worker.tick()
	host.mu.Lock()
	if len(host.launches) != 0 {
		t.Fatal("manual enrollment launched without admission")
	}
	host.mu.Unlock()
	runCommand(t, worker, wp, "task", "admit", good.Task.ID)
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.launches) != 1 {
		t.Fatal("explicit manual admission failed")
	}
}
func TestCoordinationListenerClaimsOnceAndScopesReports(t *testing.T) {
	_, server := newCoordServer(t)
	leader, _, lp := testRuntime(t, server.URL, "one", "leader")
	worker, host, wp := testRuntime(t, server.URL, "two", "worker")
	enrollRuntime(t, leader, lp, "leader", "")
	enrollRuntime(t, worker, wp, "worker", "one/leader")
	assigned := runCommand(t, leader, lp, "task", "assign", "--to", "two/worker", "--project", "app", "--criteria", "test evidence", "--max-runtime", "60", "--", "inspect README").(pc.Response)
	worker.tick()
	worker.tick()
	b := firstBoard(worker)
	if len(b.Executions) != 1 {
		t.Fatalf("executions=%d error=%s", len(b.Executions), b.LastError)
	}
	var x *Execution
	for _, ex := range b.Executions {
		x = ex
	}
	host.mu.Lock()
	launches := host.launches[x.ID]
	host.mu.Unlock()
	if launches != 1 {
		t.Fatalf("launch count %d", launches)
	}
	q := Control{Execution: x.ID, Pane: x.PaneID, Token: x.Token, Op: "hello"}
	if _, e := Call(context.Background(), worker.socket, q); e != nil {
		t.Fatal(e)
	}
	if _, e := worker.Command(context.Background(), "local", x.PaneID, []string{"enroll", "worker", "--permission", "workspace"}); e == nil {
		t.Fatal("child changed enrollment")
	}
	if _, e := worker.Command(context.Background(), "local", wp, []string{"task", "result", assigned.Task.ID, "--", "forged"}); e == nil {
		t.Fatal("registration pane impersonated execution")
	}
	runCommand(t, worker, x.PaneID, "task", "result", assigned.Task.ID, "--result-json", `{"summary":"README inspected","tests":["present"]}`)
	runCommand(t, leader, lp, "task", "review", assigned.Task.ID, "--accept", "--", "verified")
	worker.tick()
	if b.State.Tasks[assigned.Task.ID].State != "completed" {
		t.Fatal("completion not shared")
	}
	// Explicit replay of a completed attempt never creates another worktree/pane.
	worker.tick()
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.launches[x.ID] != 1 {
		t.Fatal("duplicate launch")
	}
}
func TestCoordinationLostClaimReceiptNeverLaunchesTwice(t *testing.T) {
	s, server := newCoordServer(t)
	leader, _, lp := testRuntime(t, server.URL, "one", "leader")
	worker, host, wp := testRuntime(t, server.URL, "two", "worker")
	enrollRuntime(t, leader, lp, "leader", "")
	enrollRuntime(t, worker, wp, "worker", "one/leader")
	runCommand(t, leader, lp, "task", "assign", "--to", "two/worker", "--project", "app", "--criteria", "evidence", "--max-runtime", "60", "--", "inspect")
	s.mu.Lock()
	s.drop = "task.claim"
	s.mu.Unlock()
	worker.tick()
	worker.tick()
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.launches) != 0 {
		t.Fatal("uncertain claim launched a child")
	}
	b := firstBoard(worker)
	if len(b.Executions) != 1 {
		t.Fatal("claim journal missing")
	}
	for _, x := range b.Executions {
		if x.Phase != "needs_reconciliation" {
			t.Fatalf("phase %s", x.Phase)
		}
	}
}
func TestCoordinationWrongLeaderAndOrdinaryMessagesDoNotLaunch(t *testing.T) {
	_, server := newCoordServer(t)
	leader, _, lp := testRuntime(t, server.URL, "one", "leader")
	worker, host, wp := testRuntime(t, server.URL, "two", "worker")
	enrollRuntime(t, leader, lp, "leader", "")
	enrollRuntime(t, worker, wp, "worker", "one/leader")
	runCommand(t, leader, lp, "send", "--to", "two/worker", "--", "run arbitrary shell text")
	worker.tick()
	host.mu.Lock()
	n := len(host.launches)
	host.mu.Unlock()
	if n != 0 {
		t.Fatal("chat launched worker")
	}
	worker.mu.Lock()
	firstBoard(worker).Endpoints["worker"].AcceptedLeaders = nil
	worker.mu.Unlock()
	runCommand(t, leader, lp, "task", "assign", "--to", "two/worker", "--project", "app", "--criteria", "evidence", "--max-runtime", "60", "--", "inspect")
	worker.tick()
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.launches) != 0 {
		t.Fatal("unapproved leader launched worker")
	}
}
func TestRunnerUsesStructuredInputAndExplicitPermissions(t *testing.T) {
	for _, runner := range []string{"claude", "codex"} {
		for _, permission := range []string{"review", "workspace"} {
			args, in, e := RunnerArgs(RunnerSpec{Runner: runner, Permission: permission, Binary: "/usr/local/bin/rysh", Socket: "/tmp/rysh-peer-test/control.sock"}, "", "literal $(touch /tmp/NO) prompt")
			if e != nil {
				t.Fatal(e)
			}
			if in == nil {
				t.Fatal("prompt is not separate input")
			}
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "dangerously") || strings.Contains(joined, "$(touch") {
				t.Fatal("unsafe runner args")
			}
			if runner == "codex" && permission == "review" && (!strings.Contains(joined, `extends=":read-only"`) || !strings.Contains(joined, `unix_sockets={"/tmp/rysh-peer-test/control.sock"="allow"}`) || !strings.Contains(joined, "features.network_proxy=true") || strings.Contains(joined, "sandbox_mode=")) {
				t.Fatal("review must retain read-only files and allow only the local control socket")
			}
		}
	}
}

func TestCoordinationRestartPreservesUnconfirmedTurnAndInbox(t *testing.T) {
	_, server := newCoordServer(t)
	r, host, pane := testRuntime(t, server.URL, "one", "leader")
	enrollRuntime(t, r, pane, "leader", "")
	runCommand(t, r, pane, "leader", "objective", "--", "inspect existing tasks")
	r.tick()
	b := firstBoard(r)
	if len(b.Executions) != 1 {
		t.Fatalf("leader not started: %s", b.LastError)
	}
	var x *Execution
	for _, ex := range b.Executions {
		x = ex
	}
	q := Control{Execution: x.ID, Pane: x.PaneID, Token: x.Token, Op: "hello"}
	if _, e := Call(context.Background(), r.socket, q); e != nil {
		t.Fatal(e)
	}
	q.Op = "next"
	turn, e := Call(context.Background(), r.socket, q)
	if e != nil || turn.DeliveryID == "" {
		t.Fatalf("turn: %+v %v", turn, e)
	}
	r.Close()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not close")
	}
	restarted := New(r.root, r.session, host.call)
	restarted.probe = func(context.Context, string) error { return nil }
	if e = restarted.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(restarted.Close)
	restarted.tick()
	q.Op = "hello"
	if _, e = Call(context.Background(), restarted.socket, q); e != nil {
		t.Fatal(e)
	}
	q.Op = "next"
	if _, e = Call(context.Background(), restarted.socket, q); e == nil {
		t.Fatal("unconfirmed turn was automatically replayed")
	}
	resumed := firstBoard(restarted).Executions[x.ID]
	if resumed.DeliveryID != turn.DeliveryID || len(resumed.Delivery) != 1 {
		t.Fatal("durable delivery was lost")
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.launches[x.ID] != 1 {
		t.Fatal("restart duplicated the leader process")
	}
}

func TestTaskCommandCapabilityCannotUseSupervisorOrOtherPane(t *testing.T) {
	_, server := newCoordServer(t)
	r, _, pane := testRuntime(t, server.URL, "one", "leader")
	enrollRuntime(t, r, pane, "leader", "")
	runCommand(t, r, pane, "leader", "objective", "--", "inspect")
	r.tick()
	var x *Execution
	for _, ex := range firstBoard(r).Executions {
		x = ex
	}
	q := Control{Op: "command", Execution: x.ID, Pane: x.PaneID, Token: x.CommandToken, Args: []string{"state", "--json"}}
	if _, e := Call(context.Background(), r.socket, q); e != nil {
		t.Fatal(e)
	}
	q.Op = "hello"
	if _, e := Call(context.Background(), r.socket, q); e == nil {
		t.Fatal("command token gained supervisor authority")
	}
	q.Op = "command"
	q.Pane = pane
	if _, e := Call(context.Background(), r.socket, q); e == nil {
		t.Fatal("command token impersonated registration pane")
	}
	q.Pane = x.PaneID
	q.Args = []string{"enroll", "leader", "--permission", "workspace"}
	if _, e := Call(context.Background(), r.socket, q); e == nil {
		t.Fatal("task token changed enrollment")
	}
	q.Args = []string{"connect", "https://example.com", "--key-secret", "X"}
	if _, e := Call(context.Background(), r.socket, q); e == nil {
		t.Fatal("task token changed connection")
	}
}

func TestManagedTurnBudgetAndManualTakeover(t *testing.T) {
	_, server := newCoordServer(t)
	r, _, pane := testRuntime(t, server.URL, "one", "leader")
	enrollRuntime(t, r, pane, "leader", "")
	runCommand(t, r, pane, "leader", "objective", "--", "inspect")
	r.tick()
	var x *Execution
	for _, ex := range firstBoard(r).Executions {
		x = ex
	}
	r.mu.Lock()
	firstBoard(r).Endpoints["leader"].MaxBudgetUSD = 1
	r.mu.Unlock()
	q := Control{Execution: x.ID, Pane: x.PaneID, Token: x.Token, Op: "hello"}
	if _, e := Call(context.Background(), r.socket, q); e != nil {
		t.Fatal(e)
	}
	q.Op = "next"
	turn, e := Call(context.Background(), r.socket, q)
	if e != nil || turn.BudgetRemaining != 1 {
		t.Fatalf("budget %+v %v", turn, e)
	}
	q.Op = "finish"
	q.DeliveryID = turn.DeliveryID
	q.CostUSD = .4
	if _, e = Call(context.Background(), r.socket, q); e != nil {
		t.Fatal(e)
	}
	if _, e = Call(context.Background(), r.socket, q); e != nil {
		t.Fatal(e)
	} // receipt replay must not double charge
	q.Op = "input"
	q.Text = "local correction"
	q.DeliveryID = ""
	if _, e = Call(context.Background(), r.socket, q); e == nil {
		t.Fatal("manual input did not require takeover")
	}
	q.Op = "pause"
	Call(context.Background(), r.socket, q)
	q.Op = "input"
	manual, e := Call(context.Background(), r.socket, q)
	if e != nil || manual.Prompt != q.Text || manual.BudgetRemaining != .6 {
		t.Fatalf("manual %+v %v", manual, e)
	}
	q.Op = "finish"
	q.DeliveryID = manual.DeliveryID
	q.CostUSD = .6
	Call(context.Background(), r.socket, q)
	q.Op = "ping"
	ping, e := Call(context.Background(), r.socket, q)
	if e != nil || !ping.Stop {
		t.Fatalf("budget did not stop execution: %+v %v", ping, e)
	}
}
