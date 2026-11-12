// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/hosted"
)

func (f *fakeHostedAPI) SaveSnapshot(context.Context, string, string) (hosted.Snapshot, error) {
	return hosted.Snapshot{}, errors.New("fakeHostedAPI does not snapshot")
}
func (f *fakeHostedAPI) ListSnapshots(context.Context) ([]hosted.Snapshot, error) { return nil, nil }
func (f *fakeHostedAPI) DeleteSnapshot(context.Context, string) error             { return nil }

func TestParseHostedUpFrom(t *testing.T) {
	o, err := parseHostedUp([]string{"--from", "aaaa000000000001", "--no-attach"})
	if err != nil || o.From != "aaaa000000000001" || !o.NoAttach {
		t.Fatalf("opts = %+v %v", o, err)
	}
	if _, err := parseHostedUp([]string{"--from"}); err == nil {
		t.Fatal("--from without a value accepted")
	}
}

func TestParseHostedSnapshot(t *testing.T) {
	o, err := parseHostedSnapshot([]string{forkParentID, "--name", "base"})
	if err != nil || o.Run != forkParentID || o.Name != "base" {
		t.Fatalf("opts = %+v %v", o, err)
	}
	if o, err = parseHostedSnapshot([]string{"--name", "base"}); err != nil || o.Run != "" {
		t.Fatalf("no run id: %+v %v", o, err)
	}
	for _, bad := range [][]string{nil, {forkParentID}, {"--name"}, {"--name", "Bad Name"}, {"--bogus", "x"}, {"a1", "b2", "--name", "x"}} {
		if _, err := parseHostedSnapshot(bad); err == nil {
			t.Errorf("parseHostedSnapshot(%v) accepted", bad)
		}
	}
}

// ##hosted snapshot / snapshots / snapshot delete / up --from against a fake server.
func TestHostedSnapshotCommandsAgainstAFakeServer(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var startBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		base := "/api/workspaces/ws1"
		switch {
		case r.Method == "POST" && r.URL.Path == base+"/hosted-runs/"+forkParentID+"/snapshots":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hosted.Snapshot{ID: "aaaa000000000001", Name: "base", SourceRunID: forkParentID})
		case r.Method == "GET" && r.URL.Path == base+"/hosted-snapshots":
			_ = json.NewEncoder(w).Encode(map[string]any{"snapshots": []hosted.Snapshot{{ID: "aaaa000000000001", Name: "base", SourceRunID: forkParentID}}})
		case r.Method == "DELETE" && r.URL.Path == base+"/hosted-snapshots/aaaa000000000001":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "POST" && r.URL.Path == base+"/hosted-runs":
			_ = json.NewDecoder(r.Body).Decode(&startBody)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(forkRun("bbbb000000000001", "running"))
		case r.Method == "GET" && r.URL.Path == base+"/hosted-runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"runs": []hosted.Run{{ID: forkParentID, State: "running", Name: "work"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("RYSH_HOSTED_DIR", t.TempDir())
	w := &WorkspaceActor{}
	w.cfg = config.Config{}
	w.cfg.Upstream.URL, w.cfg.Upstream.APIKey, w.cfg.Upstream.Workspace = srv.URL, "fake-workspace-key-for-tests", "ws1"
	var out strings.Builder
	run := func(args ...string) string {
		t.Helper()
		out.Reset()
		if err := w.handleHostedCommand(&out, args); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}
	if got := run("snapshot", "--name", "base"); !strings.Contains(got, "aaaa000000000001") || !strings.Contains(got, "base") {
		t.Fatalf("snapshot output:\n%s", got)
	}
	if got := run("snapshots"); !strings.Contains(got, "aaaa000000000001") || !strings.Contains(got, "base") {
		t.Fatalf("snapshots output:\n%s", got)
	}
	if got := run("up", "--from", "aaaa000000000001", "--no-attach"); !strings.Contains(got, "bbbb000000000001") {
		t.Fatalf("up --from output:\n%s", got)
	}
	if startBody["from_snapshot"] != "aaaa000000000001" {
		t.Fatalf("start body = %v", startBody)
	}
	if got := run("snapshot", "delete", "aaaa000000000001"); !strings.Contains(got, "deleted") {
		t.Fatalf("delete output:\n%s", got)
	}
	if strings.Contains(strings.Join(calls, " "), "fake-workspace-key") {
		t.Fatal("the key leaked into a path")
	}
}
