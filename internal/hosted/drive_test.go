// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Public drive API client (boat gaps #5/#6) and `rysh run --hosted`
// (RunCI). Every test runs against an httptest server; no network.

const (
	drvPane = "0f8e2a4c-1b3d-4e5f-8a9b-0c1d2e3f4a5b"
	drvKey  = "test-only-not-a-credential"
	drvLLM  = "test-only-llm-not-a-key"
)

// fakeRunServer is a scripted hosted-runs server.
type fakeRunServer struct {
	t  *testing.T
	mu sync.Mutex
	// prompt answers POST …/prompt; nil means {"status":"done","exit_code":0,"output":"all good"}.
	prompt func(body map[string]any) (int, string)
	// events are written after the connect snapshot, one per element; a
	// "close" element ends the stream.
	events    chan string
	calls     []string
	stopped   int
	startBody map[string]any
	authBad   int
}

func newFakeRunServer(t *testing.T) (*fakeRunServer, *httptest.Server) {
	f := &fakeRunServer{t: t, events: make(chan string, 32)}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeRunServer) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeRunServer) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+drvKey {
		f.mu.Lock()
		f.authBad++
		f.mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	base := "/api/workspaces/ws1/hosted-runs"
	run := testRun()
	switch {
	case r.Method == "POST" && r.URL.Path == base:
		f.record("start")
		_ = json.NewDecoder(r.Body).Decode(&f.startBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(run)
	case r.Method == "GET" && r.URL.Path == base+"/"+run.ID+"/panes":
		f.record("panes")
		_ = json.NewEncoder(w).Encode(map[string]any{"panes": []Pane{{ID: drvPane, Title: "main", Status: "idle"}}})
	case r.Method == "POST" && r.URL.Path == base+"/"+run.ID+"/prompt":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.record(fmt.Sprintf("prompt %v %v %v", body["pane"], body["text"], body["wait_seconds"]))
		code, out := 200, `{"status":"done","exit_code":0,"output":"all good"}`
		if f.prompt != nil {
			code, out = f.prompt(body)
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(out))
	case r.Method == "POST" && r.URL.Path == base+"/"+run.ID+"/interrupt":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.record(fmt.Sprintf("interrupt %v", body["pane"]))
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"sent","pane":"` + drvPane + `"}`))
	case r.Method == "GET" && r.URL.Path == base+"/"+run.ID+"/events":
		f.record("events")
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "retry: 3000\n: connected\n\nevent: run\ndata: {\"id\":%q,\"state\":\"running\"}\n\nevent: panes\ndata: {\"panes\":[{\"id\":%q,\"title\":\"main\",\"status\":\"idle\",\"needs_you\":false}]}\n\n", run.ID, drvPane)
		fl.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-f.events:
				if ev == "close" {
					return
				}
				fmt.Fprint(w, ev)
				fl.Flush()
			}
		}
	case r.Method == "DELETE" && r.URL.Path == base+"/"+run.ID:
		f.record("stop")
		f.mu.Lock()
		f.stopped++
		f.mu.Unlock()
		run.State = "stopped"
		_ = json.NewEncoder(w).Encode(run)
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeRunServer) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func drvClient(srv *httptest.Server) *Client {
	return &Client{Conn: Conn{URL: srv.URL, APIKey: drvKey, WorkspaceID: "ws1"}}
}

func paneEvent(status string, needsYou bool) string {
	return fmt.Sprintf("event: panes\ndata: {\"panes\":[{\"id\":%q,\"title\":\"main\",\"status\":%q,\"needs_you\":%v}]}\n\n", drvPane, status, needsYou)
}

func TestClientPromptInterruptPanes(t *testing.T) {
	f, srv := newFakeRunServer(t)
	c := drvClient(srv)
	ctx := context.Background()
	panes, err := c.Panes(ctx, testRun().ID)
	if err != nil || len(panes) != 1 || panes[0].ID != drvPane {
		t.Fatalf("panes = %+v err = %v", panes, err)
	}
	res, err := c.Prompt(ctx, testRun().ID, drvPane, "fix it", 20)
	if err != nil || res.Status != "done" || res.Output != "all good" {
		t.Fatalf("prompt = %+v err = %v", res, err)
	}
	if err := c.Interrupt(ctx, testRun().ID, drvPane); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.callList(), "|")
	if got != "panes|prompt "+drvPane+" fix it 20|interrupt "+drvPane {
		t.Fatalf("calls = %s", got)
	}
	// Bad ids never reach the server.
	if _, err := c.Prompt(ctx, "../x", drvPane, "x", 1); err == nil {
		t.Fatal("a bad run id was sent")
	}
	if err := c.Interrupt(ctx, testRun().ID, "@x"); err == nil {
		t.Fatal("a bad pane id was sent")
	}
	if len(f.callList()) != 3 {
		t.Fatalf("calls = %v", f.callList())
	}
}

func TestClientEventsParsesTheStream(t *testing.T) {
	f, srv := newFakeRunServer(t)
	c := drvClient(srv)
	f.events <- "id: 3\nevent: needs_you\ndata: {\"pane\":\"" + drvPane + "\",\n\n" // not JSON: kept as text
	f.events <- "event: needs_you\ndata: {\"pane\":\"" + drvPane + "\",\"attention\":\"approval\"}\n\n"
	f.events <- "close"
	var got []Event
	err := c.Events(context.Background(), testRun().ID, func(e Event) error {
		got = append(got, e)
		return nil
	})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var names []string
	for _, e := range got {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "run,panes,needs_you,needs_you" {
		t.Fatalf("events = %v", names)
	}
	var ny struct{ Pane, Attention string }
	if err := json.Unmarshal(got[3].Data, &ny); err != nil || ny.Pane != drvPane || ny.Attention != "approval" {
		t.Fatalf("needs_you = %s (%v)", got[3].Data, err)
	}
	if got[2].ID != "3" {
		t.Fatalf("id = %q", got[2].ID)
	}
}

func runCI(t *testing.T, c *Client, o RunOptions, ctx context.Context) (RunResult, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	if o.Timeout == 0 {
		o.Timeout = 5 * time.Second
	}
	if o.Provider == "" {
		o.Provider, o.LLMKey = "anthropic", drvLLM
	}
	res := RunCI(ctx, c, o, &out, &errOut)
	all := out.String() + errOut.String()
	if strings.Contains(all, drvKey) || strings.Contains(all, drvLLM) {
		t.Fatalf("a key reached the output:\n%s", all)
	}
	return res, out.String(), errOut.String()
}

func TestRunCIDoneStopsTheRun(t *testing.T) {
	f, srv := newFakeRunServer(t)
	res, out, _ := runCI(t, drvClient(srv), RunOptions{Prompt: "fix the test"}, context.Background())
	if res.ExitCode != ExitDone || res.Status != "done" || res.RunID != testRun().ID {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(out, "all good") {
		t.Fatalf("stdout = %q", out)
	}
	// The events stream opens beside the prompt, so its call may land before
	// or after the stop; check the order of the other calls only.
	var ordered []string
	for _, c := range f.callList() {
		if c != "events" {
			ordered = append(ordered, c)
		}
	}
	calls := strings.Join(ordered, "|")
	if calls != "start|panes|prompt "+drvPane+" fix the test 50|stop" {
		t.Fatalf("calls = %s", calls)
	}
	if f.startBody["llm_provider"] != "anthropic" || f.startBody["llm_api_key"] != drvLLM {
		t.Fatalf("start body = %v", f.startBody)
	}
	if f.authBad != 0 {
		t.Fatalf("%d calls without the key", f.authBad)
	}
}

func TestRunCIMapsPromptOutcomes(t *testing.T) {
	for status, want := range map[string]int{"gate_blocked": ExitGateBlocked, "error": ExitError, "partial": ExitPartial, "budget": ExitBudget} {
		f, srv := newFakeRunServer(t)
		code := map[string]int{"gate_blocked": 3, "error": 1, "partial": 2, "budget": 4}[status]
		f.prompt = func(map[string]any) (int, string) {
			return 200, fmt.Sprintf(`{"status":%q,"exit_code":%d}`, status, code)
		}
		res, _, _ := runCI(t, drvClient(srv), RunOptions{Prompt: "x"}, context.Background())
		if res.ExitCode != want || f.stopped != 1 {
			t.Errorf("%s: exit %d (want %d), stopped %d", status, res.ExitCode, want, f.stopped)
		}
	}
}

func TestRunCILongTurnWaitsForThePaneToGoIdle(t *testing.T) {
	f, srv := newFakeRunServer(t)
	f.prompt = func(map[string]any) (int, string) {
		f.events <- paneEvent("querying provider", false)
		return 200, `{"status":"timeout","exit_code":5}`
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		f.events <- paneEvent("idle", false)
	}()
	res, _, errOut := runCI(t, drvClient(srv), RunOptions{Prompt: "long job"}, context.Background())
	if res.ExitCode != ExitDone || res.Status != "done" {
		t.Fatalf("result = %+v stderr=%s", res, errOut)
	}
	if f.stopped != 1 {
		t.Fatalf("stopped = %d", f.stopped)
	}
}

func TestRunCINeedsYouIsGateBlocked(t *testing.T) {
	f, srv := newFakeRunServer(t)
	f.prompt = func(map[string]any) (int, string) {
		f.events <- paneEvent("querying provider", false)
		f.events <- "event: needs_you\ndata: {\"pane\":\"" + drvPane + "\",\"attention\":\"approval\"}\n\n"
		return 200, `{"status":"timeout","exit_code":5}`
	}
	res, _, errOut := runCI(t, drvClient(srv), RunOptions{Prompt: "x"}, context.Background())
	if res.ExitCode != ExitGateBlocked || !strings.Contains(errOut, "approval") {
		t.Fatalf("result = %+v stderr = %s", res, errOut)
	}
	if f.stopped != 1 {
		t.Fatalf("stopped = %d", f.stopped)
	}
}

func TestRunCIRunEndedIsAnError(t *testing.T) {
	f, srv := newFakeRunServer(t)
	f.prompt = func(map[string]any) (int, string) {
		f.events <- "event: end\ndata: {\"id\":\"x\",\"state\":\"expired\",\"reason\":\"max lifetime\"}\n\n"
		return 200, `{"status":"timeout","exit_code":5}`
	}
	res, _, errOut := runCI(t, drvClient(srv), RunOptions{Prompt: "x"}, context.Background())
	if res.ExitCode != ExitError || !strings.Contains(errOut, "max lifetime") {
		t.Fatalf("result = %+v stderr = %s", res, errOut)
	}
}

func TestRunCITimeoutAndCancelStopTheRun(t *testing.T) {
	f, srv := newFakeRunServer(t)
	f.prompt = func(map[string]any) (int, string) { return 200, `{"status":"timeout","exit_code":5}` }
	res, _, _ := runCI(t, drvClient(srv), RunOptions{Prompt: "x", Timeout: 200 * time.Millisecond}, context.Background())
	if res.ExitCode != ExitTimeout || f.stopped != 1 {
		t.Fatalf("timeout: %+v stopped=%d", res, f.stopped)
	}

	f2, srv2 := newFakeRunServer(t)
	f2.prompt = func(map[string]any) (int, string) { return 200, `{"status":"timeout","exit_code":5}` }
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }() // Ctrl-C
	res2, _, errOut := runCI(t, drvClient(srv2), RunOptions{Prompt: "x", Timeout: time.Minute}, ctx)
	if res2.ExitCode != ExitError || res2.Status != "cancelled" || f2.stopped != 1 {
		t.Fatalf("cancel: %+v stopped=%d stderr=%s", res2, f2.stopped, errOut)
	}
}

func TestRunCIKeepLeavesTheRunAndJSONLine(t *testing.T) {
	f, srv := newFakeRunServer(t)
	res, out, _ := runCI(t, drvClient(srv), RunOptions{Prompt: "x", Keep: true, JSON: true}, context.Background())
	if res.ExitCode != ExitDone || f.stopped != 0 {
		t.Fatalf("keep: %+v stopped=%d", res, f.stopped)
	}
	var line struct {
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
		RunID    string `json:"run_id"`
		Output   string `json:"output"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &line); err != nil || line.Status != "done" || line.RunID != testRun().ID || line.Output != "all good" {
		t.Fatalf("json line = %q (%v)", out, err)
	}
}

func TestRunCIStartFailureIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"run quota reached"}`))
	}))
	defer srv.Close()
	res, _, errOut := runCI(t, drvClient(srv), RunOptions{Prompt: "x"}, context.Background())
	if res.ExitCode != ExitError || !strings.Contains(errOut, "quota") {
		t.Fatalf("result = %+v stderr = %s", res, errOut)
	}
}
