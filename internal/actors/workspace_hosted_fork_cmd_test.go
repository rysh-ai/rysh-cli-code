// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/hosted"
)

// ##hosted fork end to end against a fake server: the request, the saved
// forks, the fleet group up; then stopping the last fork downs the group.
func TestHostedForkCommandAgainstAFakeServer(t *testing.T) {
	var mu sync.Mutex
	state := map[string]string{"aaaa000000000001": "running", "aaaa000000000002": "running"}
	var forkBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		base := "/api/workspaces/ws1/hosted-runs"
		run := func(id string) hosted.Run {
			x := forkRun(id, state[id])
			return x
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == base+"/"+forkParentID+"/fork":
			_ = json.NewDecoder(r.Body).Decode(&forkBody)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"runs": []hosted.Run{run("aaaa000000000001"), run("aaaa000000000002")}})
		case r.Method == http.MethodGet && r.URL.Path == base:
			_ = json.NewEncoder(w).Encode(map[string]any{"runs": []hosted.Run{
				{ID: forkParentID, State: "running", Name: "work"}, run("aaaa000000000001"), run("aaaa000000000002")}})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, base+"/"):
			id := strings.TrimPrefix(r.URL.Path, base+"/")
			state[id] = "stopped"
			_ = json.NewEncoder(w).Encode(hosted.Run{ID: id, State: "stopped"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("RYSH_HOSTED_DIR", t.TempDir())
	var sent []fleet.Update
	old := hostedFleetSend
	hostedFleetSend = func(_ *WorkspaceActor, u fleet.Update) error { sent = append(sent, u); return nil }
	t.Cleanup(func() { hostedFleetSend = old })

	w := &WorkspaceActor{}
	w.cfg = config.Config{}
	w.cfg.Upstream.URL, w.cfg.Upstream.APIKey, w.cfg.Upstream.Workspace = srv.URL, "fake-workspace-key-for-tests", "ws1"
	var out strings.Builder
	if err := w.handleHostedCommand(&out, []string{"fork", forkParentID, "-n", "2", "--no-attach"}); err != nil {
		t.Fatalf("fork: %v\n%s", err, out.String())
	}
	if forkBody["count"] != float64(2) {
		t.Fatalf("body = %v", forkBody)
	}
	text := out.String()
	for _, want := range []string{"2 fork(s) of hosted run " + forkParentID, "aaaa000000000001", "aaaa000000000002", "fork-" + forkParentID, "shell-only"} {
		if !strings.Contains(text, want) {
			t.Errorf("output misses %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "fake-workspace-key-for-tests") {
		t.Fatal("the workspace key was printed")
	}
	if len(sent) != 2 || sent[0].Op != fleet.OpRegister || sent[1].State != fleet.StateUp {
		t.Fatalf("fleet updates = %+v", sent)
	}
	st := hosted.Store{Dir: mustHostedDir(t)}
	if saved, _ := st.List(); len(saved) != 2 {
		t.Fatalf("saved forks = %d", len(saved))
	}

	// Stop one fork: the other is alive, the group stays up.
	sent = nil
	out.Reset()
	if err := w.handleHostedCommand(&out, []string{"stop", "aaaa000000000001"}); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 0 {
		t.Fatalf("group touched while a fork is alive: %+v", sent)
	}
	// Stop the last: the group goes down.
	if err := w.handleHostedCommand(&out, []string{"stop", "aaaa000000000002"}); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0].State != fleet.StateDown || sent[0].Name != "fork-"+forkParentID {
		t.Fatalf("after the last fork: %+v", sent)
	}
}

func mustHostedDir(t *testing.T) string {
	t.Helper()
	d, err := hostedStateDir()
	if err != nil {
		t.Fatal(err)
	}
	return d
}
