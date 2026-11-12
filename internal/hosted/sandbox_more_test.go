// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// rr-order-154: rysh sandbox ssh / port / screenshot client side.

func TestStoreSaveAsWritesTheLoginUser(t *testing.T) {
	st := Store{Dir: t.TempDir()}
	kp, err := NewKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	r := testRun()
	p, err := st.SaveAs(r, kp, "user")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := os.ReadFile(p.SSHConfig)
	if !strings.Contains(string(cfg), "    User user\n") || !strings.Contains(string(cfg), "StrictHostKeyChecking yes") {
		t.Fatalf("ssh_config:\n%s", cfg)
	}
	if _, err := st.SaveAs(r, kp, "-oProxyCommand=x"); err == nil {
		t.Fatal("a user that is an ssh option was accepted")
	}
	p2, _ := (Store{Dir: t.TempDir()}).Save(r, kp)
	cfg2, _ := os.ReadFile(p2.SSHConfig)
	if !strings.Contains(string(cfg2), "    User "+SSHUser+"\n") {
		t.Fatalf("Save no longer uses %s:\n%s", SSHUser, cfg2)
	}
}

func TestSandboxPortsAndScreenshotCalls(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+strings.TrimPrefix(r.URL.Path, "/api/v1/workspaces/ws1/sandboxes/423b54f6fd36fa94")+" "+string(b))
		switch {
		case strings.HasSuffix(r.URL.Path, "/ports"):
			_, _ = io.WriteString(w, `{"ports":[{"port":3000,"public":true}]}`)
		case strings.HasSuffix(r.URL.Path, "/url"):
			_, _ = io.WriteString(w, `{"url":"https://x-3000.sbx.test/?rysh_token=t","expires_at":"2026-11-09T01:00:00Z"}`)
		case strings.HasSuffix(r.URL.Path, "/screenshot"):
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("\x89PNG..."))
		default:
			_, _ = io.WriteString(w, `{"port":3000,"public":false}`)
		}
	}))
	defer srv.Close()
	c := &Client{Conn: Conn{URL: srv.URL, APIKey: drvKey, WorkspaceID: "ws1"}}
	ctx := context.Background()
	id := "423b54f6fd36fa94"
	ps, err := c.SandboxPorts(ctx, id)
	if err != nil || len(ps) != 1 || ps[0].Port != 3000 || !ps[0].Public {
		t.Fatalf("ports = %+v %v", ps, err)
	}
	if p, err := c.SandboxSetPortPublic(ctx, id, 3000, false); err != nil || p.Public {
		t.Fatalf("set = %+v %v", p, err)
	}
	u, err := c.SandboxPortURL(ctx, id, 3000)
	if err != nil || u.URL != "https://x-3000.sbx.test/?rysh_token=t" {
		t.Fatalf("url = %+v %v", u, err)
	}
	png, err := c.SandboxScreenshot(ctx, id)
	if err != nil || !strings.HasPrefix(string(png), "\x89PNG") {
		t.Fatalf("screenshot = %q %v", png, err)
	}
	all := strings.Join(calls, "\n")
	for _, want := range []string{"GET /ports ", `PUT /ports/3000 {"public":false}`, "POST /ports/3000/url ", "GET /desktop/screenshot "} {
		if !strings.Contains(all, want) {
			t.Errorf("no call %q in\n%s", want, all)
		}
	}
	if _, err := c.SandboxSetPortPublic(ctx, id, 0, true); err == nil {
		t.Fatal("port 0 sent")
	}
}

func TestSandboxForkAndSnapshotCalls(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+strings.TrimPrefix(r.URL.Path, "/api/v1/workspaces/ws1")+" "+string(b))
		switch {
		case strings.HasSuffix(r.URL.Path, "/fork"):
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{"sandboxes":[{"id":"aaaaaaaaaaaaaaaa","state":"running"},{"id":"bbbbbbbbbbbbbbbb","state":"running"}]}`)
		case strings.HasSuffix(r.URL.Path, "/snapshots") && r.Method == "POST":
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{"id":"snap1","name":"base","source_sandbox_id":"423b54f6fd36fa94"}`)
		case r.URL.Path == "/api/v1/workspaces/ws1/snapshots":
			_, _ = io.WriteString(w, `{"snapshots":[{"id":"snap1","name":"base"}]}`)
		case r.Method == "DELETE":
			w.WriteHeader(204)
		default:
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{"id":"cccccccccccccccc","state":"running"}`)
		}
	}))
	defer srv.Close()
	c := &Client{Conn: Conn{URL: srv.URL, APIKey: drvKey, WorkspaceID: "ws1"}}
	ctx := context.Background()
	id := "423b54f6fd36fa94"
	forks, err := c.SandboxFork(ctx, id, 2, "")
	if err != nil || len(forks) != 2 {
		t.Fatalf("fork = %+v %v", forks, err)
	}
	snap, err := c.SandboxSnapshot(ctx, id, "base")
	if err != nil || snap.ID != "snap1" {
		t.Fatalf("snapshot = %+v %v", snap, err)
	}
	if l, err := c.SandboxSnapshots(ctx); err != nil || len(l) != 1 {
		t.Fatalf("snapshots = %+v %v", l, err)
	}
	if err := c.SandboxDeleteSnapshot(ctx, "snap1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SandboxCreate(ctx, SandboxCreateOptions{FromSnapshot: "snap1"}); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(calls, "\n")
	for _, want := range []string{
		"POST /sandboxes/" + id + `/fork {"count":2}`,
		"POST /sandboxes/" + id + `/snapshots {"name":"base"}`,
		"GET /snapshots ", "DELETE /snapshots/snap1 ",
		`POST /sandboxes {"from_snapshot":"snap1"}`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("no call %q in\n%s", want, all)
		}
	}
	if _, err := c.SandboxFork(ctx, id, 11, ""); err == nil {
		t.Fatal("count 11 sent")
	}
	if err := c.SandboxDeleteSnapshot(ctx, "../x"); err == nil {
		t.Fatal("bad snapshot id sent")
	}
}

func TestSandboxDesktopEmbedCall(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = r.Method + " " + r.URL.Path + " " + string(b)
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"id":"e1","mode":"watch","path":"/embed/desktop/e1","fragment":"t=fake-embed-token","expires_at":"2026-11-09T02:00:00Z"}`)
	}))
	defer srv.Close()
	c := &Client{Conn: Conn{URL: srv.URL, APIKey: drvKey, WorkspaceID: "ws1"}}
	u, err := c.SandboxDesktopURL(context.Background(), "423b54f6fd36fa94", "watch")
	if err != nil || u != srv.URL+"/embed/desktop/e1#t=fake-embed-token" {
		t.Fatalf("url = %q %v", u, err)
	}
	if got != `POST /api/v1/workspaces/ws1/sandboxes/423b54f6fd36fa94/desktop/embeds {"mode":"watch"}` {
		t.Fatalf("call = %q", got)
	}
	if _, err := c.SandboxDesktopURL(context.Background(), "423b54f6fd36fa94", "root"); err == nil {
		t.Fatal("a bad mode was sent")
	}
}

func TestSandboxEnvironmentCalls(t *testing.T) {
	var calls []string
	exists := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+strings.TrimPrefix(r.URL.RequestURI(), "/api/v1/workspaces/ws1")+" "+string(b))
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/workspaces/ws1/environments":
			_, _ = io.WriteString(w, `{"environments":[{"name":"node","version":2}]}`)
		case r.Method == "GET":
			_, _ = io.WriteString(w, `{"name":"node","version":1,"vars":{"A":"1"}}`)
		case r.Method == "PUT" && !exists:
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"error":"no such environment"}`)
		case r.Method == "PUT":
			_, _ = io.WriteString(w, `{"name":"node","version":3}`)
		case r.Method == "POST":
			exists = true
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{"name":"node","version":1}`)
		case r.Method == "DELETE":
			w.WriteHeader(204)
		}
	}))
	defer srv.Close()
	c := &Client{Conn: Conn{URL: srv.URL, APIKey: drvKey, WorkspaceID: "ws1"}}
	ctx := context.Background()
	if l, err := c.SandboxEnvironments(ctx); err != nil || len(l) != 1 || l[0].Version != 2 {
		t.Fatalf("list = %+v %v", l, err)
	}
	if e, err := c.SandboxEnvironment(ctx, "node", 1); err != nil || e.Vars["A"] != "1" {
		t.Fatalf("get = %+v %v", e, err)
	}
	body := []byte(`{"vars":{"A":"2"}}`)
	if e, err := c.SandboxEnvironmentPut(ctx, "node", body); err != nil || e.Version != 1 {
		t.Fatalf("put (new) = %+v %v", e, err)
	}
	if e, err := c.SandboxEnvironmentPut(ctx, "node", body); err != nil || e.Version != 3 {
		t.Fatalf("put (update) = %+v %v", e, err)
	}
	if err := c.SandboxEnvironmentDelete(ctx, "node"); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(calls, "\n")
	for _, want := range []string{"GET /environments ", "GET /environments/node?version=1 ",
		`PUT /environments/node {"vars":{"A":"2"}}`, `POST /environments {"name":"node","vars":{"A":"2"}}`, "DELETE /environments/node "} {
		if !strings.Contains(all, want) {
			t.Errorf("no call %q in\n%s", want, all)
		}
	}
	for _, bad := range []string{"../x", "Node", ""} {
		if _, err := c.SandboxEnvironment(ctx, bad, 0); err == nil {
			t.Errorf("name %q sent", bad)
		}
	}
	if _, err := c.SandboxEnvironmentPut(ctx, "node", []byte(`[1]`)); err == nil {
		t.Error("a non-object body was sent")
	}
}
