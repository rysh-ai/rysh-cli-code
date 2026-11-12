// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rysh Sandboxes client (rr-order-154 C10): the calls `rysh sandbox` makes.

func sbxServer(t *testing.T) (*httptest.Server, *[]string, map[string][]byte) {
	var calls []string
	files := map[string][]byte{}
	base := "/api/v1/workspaces/ws1/sandboxes"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+drvKey {
			w.WriteHeader(401)
			return
		}
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+strings.TrimPrefix(r.URL.Path, base)+" "+r.URL.RawQuery+" "+string(bytes.TrimSpace(b)))
		sb := `{"id":"423b54f6fd36fa94","name":"n","state":"running","size":"default"}`
		switch {
		case r.Method == "POST" && r.URL.Path == base:
			w.WriteHeader(201)
			_, _ = io.WriteString(w, sb)
		case r.Method == "GET" && r.URL.Path == base:
			_, _ = io.WriteString(w, `{"sandboxes":[`+sb+`]}`)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			_, _ = io.WriteString(w, `{"exit_code":7,"stdout":"out","stderr":"err"}`)
		case strings.HasSuffix(r.URL.Path, "/files") && r.Method == "PUT":
			files[r.URL.Query().Get("path")] = b
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/files"):
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(files[r.URL.Query().Get("path")])
		case strings.HasSuffix(r.URL.Path, "/panes"):
			_, _ = io.WriteString(w, `{"panes":[{"id":"`+drvPane+`","title":"t","status":"idle","needs_you":false}]}`)
		case strings.HasSuffix(r.URL.Path, "/prompt"):
			_, _ = io.WriteString(w, `{"status":"done","exit_code":0,"output":"ok"}`)
		case strings.HasSuffix(r.URL.Path, "/steer"), strings.HasSuffix(r.URL.Path, "/interrupt"):
			w.WriteHeader(202)
			_, _ = io.WriteString(w, `{"status":"queued","pane":"`+drvPane+`"}`)
		default:
			_, _ = io.WriteString(w, sb)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, files
}

func TestSandboxClientCalls(t *testing.T) {
	srv, calls, files := sbxServer(t)
	c := drvClient(srv)
	ctx := context.Background()
	id := "423b54f6fd36fa94"
	sb, err := c.SandboxCreate(ctx, SandboxCreateOptions{Name: "n", Size: "default"})
	if err != nil || sb.ID != id || sb.State != "running" {
		t.Fatalf("create = %+v %v", sb, err)
	}
	if l, err := c.SandboxList(ctx); err != nil || len(l) != 1 {
		t.Fatalf("list = %+v %v", l, err)
	}
	for _, f := range []func(context.Context, string) (Sandbox, error){c.SandboxGet, c.SandboxStop, c.SandboxResume, c.SandboxDelete} {
		if _, err := f(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	res, err := c.SandboxExec(ctx, id, SandboxExecRequest{Argv: []string{"ls", "-la"}, Stdin: "in", Cwd: "/tmp", TimeoutSeconds: 5})
	if err != nil || res.ExitCode != 7 || res.Stdout != "out" || res.Stderr != "err" {
		t.Fatalf("exec = %+v %v", res, err)
	}
	data := []byte{0, 1, 255}
	if err := c.SandboxPutFile(ctx, id, "/tmp/a b", data); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(files["/tmp/a b"], data) {
		t.Fatalf("server got %q", files["/tmp/a b"])
	}
	got, err := c.SandboxGetFile(ctx, id, "/tmp/a b")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("get = %q %v", got, err)
	}
	if _, err := c.SandboxPrompt(ctx, id, drvPane, "fix", 10); err != nil {
		t.Fatal(err)
	}
	if err := c.SandboxSteer(ctx, id, drvPane, "also"); err != nil {
		t.Fatal(err)
	}
	if err := c.SandboxInterrupt(ctx, id, drvPane); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(*calls, "\n")
	for _, want := range []string{
		`POST   {"name":"n","size":"default"}`,
		"POST /" + id + "/stop", "POST /" + id + "/resume", "DELETE /" + id,
		`POST /` + id + `/exec  {"argv":["ls","-la"],"cwd":"/tmp","stdin":"in","timeout_seconds":5}`,
		"PUT /" + id + "/files path=%2Ftmp%2Fa+b",
		`POST /` + id + `/agents/` + drvPane + `/prompt  {"text":"fix","wait_seconds":10}`,
		`POST /` + id + `/agents/` + drvPane + `/steer  {"text":"also"}`,
		"POST /" + id + "/agents/" + drvPane + "/interrupt  ",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("no call %q in\n%s", want, all)
		}
	}
	// Bad ids never leave the machine.
	n := len(*calls)
	if _, err := c.SandboxGet(ctx, "../x"); err == nil {
		t.Fatal("bad sandbox id sent")
	}
	if err := c.SandboxSteer(ctx, id, "@x", "y"); err == nil {
		t.Fatal("bad pane id sent")
	}
	if len(*calls) != n {
		t.Fatal("a refused call reached the server")
	}
	var probe map[string]any
	_ = json.Unmarshal([]byte(`{}`), &probe)
}
