// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/hosted"
)

// `rysh sandbox` (rr-order-154 C10): argument parsing.
func TestParseSandboxArgs(t *testing.T) {
	for _, c := range []struct {
		args []string
		want sandboxArgs
	}{
		{[]string{"sandbox", "create", "--name", "ci", "--size", "default", "--json"}, sandboxArgs{Sub: "create", Name: "ci", Size: "default", JSON: true}},
		{[]string{"sandbox", "ls"}, sandboxArgs{Sub: "list"}},
		{[]string{"sandbox", "stop", "abc12345"}, sandboxArgs{Sub: "stop", ID: "abc12345"}},
		{[]string{"sandbox", "exec", "abc12345", "--cwd", "/tmp", "--timeout", "20", "--", "go", "test", "-run", "X"}, sandboxArgs{Sub: "exec", ID: "abc12345", Cwd: "/tmp", Timeout: 20, Argv: []string{"go", "test", "-run", "X"}}},
		{[]string{"sandbox", "cp", "a.txt", "abc12345:/tmp/a.txt"}, sandboxArgs{Sub: "cp", Src: "a.txt", Dst: "abc12345:/tmp/a.txt"}},
		{[]string{"sandbox", "prompt", "abc12345", "--pane", "p", "--wait", "40", "--", "fix", "it"}, sandboxArgs{Sub: "prompt", ID: "abc12345", Pane: "p", Wait: 40, Text: "fix it"}},
		{[]string{"sandbox", "steer", "abc12345", "--", "also", "lint"}, sandboxArgs{Sub: "steer", ID: "abc12345", Text: "also lint"}},
		{[]string{"sandbox", "interrupt", "abc12345", "--pane", "p"}, sandboxArgs{Sub: "interrupt", ID: "abc12345", Pane: "p"}},
		{[]string{"sandbox", "events", "abc12345"}, sandboxArgs{Sub: "events", ID: "abc12345"}},
	} {
		got, err := parseSandboxArgs(c.args)
		if err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		if strings.Join(got.Argv, " ") != strings.Join(c.want.Argv, " ") {
			t.Errorf("%v: argv %q", c.args, got.Argv)
		}
		got.Argv, c.want.Argv = nil, nil
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v:\n got %+v\nwant %+v", c.args, got, c.want)
		}
	}
	for _, bad := range [][]string{
		{"sandbox"}, {"sandbox", "nope"}, {"sandbox", "stop"}, {"sandbox", "exec", "abc12345"},
		{"sandbox", "cp", "a"}, {"sandbox", "cp", "a", "b"}, {"sandbox", "prompt", "abc12345"},
		{"sandbox", "create", "--size", "huge"}, {"sandbox", "exec", "abc12345", "--timeout", "x", "--", "ls"},
	} {
		if _, err := parseSandboxArgs(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestSandboxCpDirection(t *testing.T) {
	up, id, remote, local, err := cpDirection("a.txt", "abc12345:/tmp/a.txt")
	if err != nil || !up || id != "abc12345" || remote != "/tmp/a.txt" || local != "a.txt" {
		t.Fatalf("up: %v %q %q %q %v", up, id, remote, local, err)
	}
	up, id, remote, local, err = cpDirection("abc12345:/home/rysh/x", "./x")
	if err != nil || up || id != "abc12345" || remote != "/home/rysh/x" || local != "./x" {
		t.Fatalf("down: %v %q %q %q %v", up, id, remote, local, err)
	}
	for _, c := range [][2]string{{"a", "b"}, {"x:/a", "y:/b"}} {
		if _, _, _, _, err := cpDirection(c[0], c[1]); err == nil {
			t.Errorf("%v accepted", c)
		}
	}
}

func TestSandboxDoExecCodeAndCp(t *testing.T) {
	const key = "test-only-not-a-credential"
	files := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(401)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/exec"):
			_, _ = io.WriteString(w, `{"exit_code":42,"stdout":"so","stderr":"se"}`)
		case strings.HasSuffix(r.URL.Path, "/files") && r.Method == "PUT":
			files[r.URL.Query().Get("path")], _ = io.ReadAll(r.Body)
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/files"):
			_, _ = w.Write(files[r.URL.Query().Get("path")])
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &hosted.Client{Conn: hosted.Conn{URL: srv.URL, APIKey: key, WorkspaceID: "ws1"}}
	var out, errb bytes.Buffer
	code, err := sandboxDo(context.Background(), c, sandboxArgs{Sub: "exec", ID: "abc12345", Argv: []string{"false"}}, strings.NewReader(""), &out, &errb)
	if err != nil || code != 42 || out.String() != "so" || errb.String() != "se" {
		t.Fatalf("exec: code=%d err=%v out=%q errb=%q", code, err, out.String(), errb.String())
	}
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "in.bin"), filepath.Join(dir, "out.bin")
	data := []byte{0, 9, 255}
	_ = os.WriteFile(src, data, 0o600)
	if code, err := sandboxDo(context.Background(), c, sandboxArgs{Sub: "cp", Src: src, Dst: "abc12345:/tmp/x"}, nil, &out, &errb); code != 0 || err != nil {
		t.Fatalf("cp up: %d %v", code, err)
	}
	if code, err := sandboxDo(context.Background(), c, sandboxArgs{Sub: "cp", Src: "abc12345:/tmp/x", Dst: dst}, nil, &out, &errb); code != 0 || err != nil {
		t.Fatalf("cp down: %d %v", code, err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, data) {
		t.Fatalf("round trip = %v", got)
	}
	if strings.Contains(out.String()+errb.String(), key) {
		t.Fatal("the key reached the output")
	}
}

func TestParseSandboxSSHPortScreenshot(t *testing.T) {
	for _, c := range []struct {
		args []string
		want sandboxArgs
	}{
		{[]string{"sandbox", "create", "--ssh"}, sandboxArgs{Sub: "create", SSH: true}},
		{[]string{"sandbox", "ssh", "abc12345"}, sandboxArgs{Sub: "ssh", ID: "abc12345"}},
		{[]string{"sandbox", "ssh", "abc12345", "--", "uname", "-a"}, sandboxArgs{Sub: "ssh", ID: "abc12345", Argv: []string{"uname", "-a"}}},
		{[]string{"sandbox", "port", "abc12345", "ls"}, sandboxArgs{Sub: "port", ID: "abc12345", PortOp: "ls"}},
		{[]string{"sandbox", "port", "abc12345", "public", "3000", "on"}, sandboxArgs{Sub: "port", ID: "abc12345", PortOp: "public", Port: 3000, Public: true}},
		{[]string{"sandbox", "port", "abc12345", "public", "3000", "off"}, sandboxArgs{Sub: "port", ID: "abc12345", PortOp: "public", Port: 3000}},
		{[]string{"sandbox", "port", "abc12345", "url", "8080"}, sandboxArgs{Sub: "port", ID: "abc12345", PortOp: "url", Port: 8080}},
		{[]string{"sandbox", "screenshot", "abc12345", "shot.png"}, sandboxArgs{Sub: "screenshot", ID: "abc12345", Dst: "shot.png"}},
	} {
		got, err := parseSandboxArgs(c.args)
		if err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v:\n got %+v\nwant %+v", c.args, got, c.want)
		}
	}
	for _, bad := range [][]string{
		{"sandbox", "port", "abc12345"}, {"sandbox", "port", "abc12345", "public", "3000"}, {"sandbox", "port", "abc12345", "public", "x", "on"},
		{"sandbox", "port", "abc12345", "url", "99999"}, {"sandbox", "port", "abc12345", "nope"}, {"sandbox", "screenshot", "abc12345"},
		{"sandbox", "ssh"},
	} {
		if _, err := parseSandboxArgs(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestSandboxSSHRunsSSHWithThePinnedConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RYSH_SANDBOX_DIR", dir)
	st := hosted.Store{Dir: dir}
	kp, _ := hosted.NewKeyPair()
	run := hosted.Run{ID: "423b54f6fd36fa94", WorkspaceID: "ws1", Name: "n", State: "running", Host: "sbx1.runner.test", Port: 41022,
		HostKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UkOGRLrDDDRvVLHKPtC21iXSUBVl"}
	p, err := st.SaveAs(run, kp, "user")
	if err != nil {
		t.Fatal(err)
	}
	var argv []string
	sandboxRunSSH = func(a []string) (int, error) { argv = a; return 0, nil }
	defer func() { sandboxRunSSH = execSSH }()
	code, err := sandboxDo(context.Background(), nil, sandboxArgs{Sub: "ssh", ID: run.ID, Argv: []string{"uname", "-a"}}, nil, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil || code != 0 {
		t.Fatalf("ssh: %d %v", code, err)
	}
	want := "ssh -F " + p.SSHConfig + " " + hosted.Alias(run.ID) + " -- uname -a"
	if strings.Join(argv, " ") != want {
		t.Fatalf("argv = %q\nwant %q", strings.Join(argv, " "), want)
	}
	if _, err := sandboxDo(context.Background(), nil, sandboxArgs{Sub: "ssh", ID: "ffffffffffffffff"}, nil, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("ssh to a sandbox without a saved key worked")
	}
}

func TestParseSandboxForkSnapshot(t *testing.T) {
	for _, c := range []struct {
		args []string
		want sandboxArgs
	}{
		{[]string{"sandbox", "fork", "abc12345"}, sandboxArgs{Sub: "fork", ID: "abc12345", Count: 1}},
		{[]string{"sandbox", "fork", "abc12345", "--count", "3"}, sandboxArgs{Sub: "fork", ID: "abc12345", Count: 3}},
		{[]string{"sandbox", "snapshot", "create", "abc12345", "base"}, sandboxArgs{Sub: "snapshot", SnapOp: "create", ID: "abc12345", Name: "base"}},
		{[]string{"sandbox", "snapshot", "ls"}, sandboxArgs{Sub: "snapshot", SnapOp: "ls"}},
		{[]string{"sandbox", "snapshot", "rm", "snap1"}, sandboxArgs{Sub: "snapshot", SnapOp: "rm", Snapshot: "snap1"}},
		{[]string{"sandbox", "create", "--from-snapshot", "snap1"}, sandboxArgs{Sub: "create", Snapshot: "snap1"}},
	} {
		got, err := parseSandboxArgs(c.args)
		if err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v:\n got %+v\nwant %+v", c.args, got, c.want)
		}
	}
	for _, bad := range [][]string{
		{"sandbox", "fork"}, {"sandbox", "fork", "abc12345", "--count", "0"}, {"sandbox", "fork", "abc12345", "--count", "11"},
		{"sandbox", "snapshot"}, {"sandbox", "snapshot", "create", "abc12345"}, {"sandbox", "snapshot", "rm"}, {"sandbox", "snapshot", "nope"},
	} {
		if _, err := parseSandboxArgs(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestParseSandboxDesktop(t *testing.T) {
	a, err := parseSandboxArgs([]string{"sandbox", "desktop", "abc12345"})
	if err != nil || a.Sub != "desktop" || a.ID != "abc12345" || a.Control {
		t.Fatalf("watch: %+v %v", a, err)
	}
	a, err = parseSandboxArgs([]string{"sandbox", "desktop", "abc12345", "--control"})
	if err != nil || !a.Control {
		t.Fatalf("control: %+v %v", a, err)
	}
	if _, err := parseSandboxArgs([]string{"sandbox", "desktop"}); err == nil {
		t.Fatal("no id accepted")
	}
}

func TestParseSandboxCreateEnvironment(t *testing.T) {
	a, err := parseSandboxArgs([]string{"sandbox", "create", "--environment", "node", "--env", "A=1", "--env", "B=x=y",
		"--secret", "NPM_TOKEN", "--secret", "DEPLOY_KEY:.ssh/deploy", "--allow", "registry.npmjs.org:443", "--allow", "api.github.com:443,22"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Environment != "node" || !reflect.DeepEqual(a.EnvVars, []string{"A=1", "B=x=y"}) ||
		!reflect.DeepEqual(a.SecretNames, []string{"NPM_TOKEN", "DEPLOY_KEY:.ssh/deploy"}) ||
		!reflect.DeepEqual(a.Allow, []string{"registry.npmjs.org:443", "api.github.com:443,22"}) {
		t.Fatalf("parsed %+v", a)
	}
	for _, bad := range [][]string{
		{"sandbox", "create", "--env", "NOEQUALS"},
		{"sandbox", "create", "--env", "=v"},
		{"sandbox", "create", "--allow", "host-only"},
		{"sandbox", "create", "--allow", "h:0"},
		{"sandbox", "create", "--allow", "h:70000"},
		{"sandbox", "create", "--secret", ""},
		{"sandbox", "list", "--env", "A=1"},
	} {
		if _, err := parseSandboxArgs(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestSandboxCreateSendsEnvironmentSecretsAndEgress(t *testing.T) {
	const key = "test-only-not-a-credential"
	const secretVal = "test-only-secret-value"
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"id":"423b54f6fd36fa94","state":"running","size":"default"}`)
	}))
	defer srv.Close()
	c := &hosted.Client{Conn: hosted.Conn{URL: srv.URL, APIKey: key, WorkspaceID: "ws1"}}
	t.Setenv("NPM_TOKEN", secretVal)
	a, err := parseSandboxArgs([]string{"sandbox", "create", "--environment", "node", "--env", "A=1",
		"--secret", "NPM_TOKEN", "--secret", "NPM_TOKEN:.npmrc", "--allow", "registry.npmjs.org:443"})
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code, err := sandboxDo(context.Background(), c, a, nil, &out, &errb); code != 0 || err != nil {
		t.Fatalf("create = %d %v", code, err)
	}
	for _, want := range []string{`"environment":"node"`, `"env":{"A":"1"}`,
		`{"name":"NPM_TOKEN","value":"` + secretVal + `"}`, `{"name":"NPM_TOKEN","value":"` + secretVal + `","path":".npmrc"}`,
		`"egress":{"mode":"allowlist","rules":[{"host":"registry.npmjs.org","ports":[443]}]}`} {
		if !strings.Contains(body, want) {
			t.Errorf("body has no %s:\n%s", want, body)
		}
	}
	if strings.Contains(out.String()+errb.String(), secretVal) {
		t.Fatal("a secret value was printed")
	}
	// A named secret whose variable is unset: refused before any request.
	body = ""
	a, _ = parseSandboxArgs([]string{"sandbox", "create", "--secret", "UNSET_TEST_ONLY_VAR"})
	if code, err := sandboxDo(context.Background(), c, a, nil, &out, &errb); code == 0 || err == nil || !strings.Contains(err.Error(), "UNSET_TEST_ONLY_VAR") || body != "" {
		t.Fatalf("unset secret = %d %v (sent: %v)", code, err, body != "")
	}
}

func TestParseSandboxEnv(t *testing.T) {
	for _, c := range []struct {
		args []string
		want sandboxArgs
	}{
		{[]string{"sandbox", "env", "ls"}, sandboxArgs{Sub: "env", EnvOp: "ls"}},
		{[]string{"sandbox", "env", "get", "node"}, sandboxArgs{Sub: "env", EnvOp: "get", Name: "node"}},
		{[]string{"sandbox", "env", "get", "node", "--version", "2"}, sandboxArgs{Sub: "env", EnvOp: "get", Name: "node", Version: 2}},
		{[]string{"sandbox", "env", "put", "node", "env.json"}, sandboxArgs{Sub: "env", EnvOp: "put", Name: "node", Src: "env.json"}},
		{[]string{"sandbox", "env", "rm", "node"}, sandboxArgs{Sub: "env", EnvOp: "rm", Name: "node"}},
	} {
		got, err := parseSandboxArgs(c.args)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v:\n got %+v %v\nwant %+v", c.args, got, err, c.want)
		}
	}
	for _, bad := range [][]string{{"sandbox", "env"}, {"sandbox", "env", "get"}, {"sandbox", "env", "put", "node"},
		{"sandbox", "env", "rm"}, {"sandbox", "env", "nope"}, {"sandbox", "env", "get", "node", "--version", "0"}, {"sandbox", "get", "abc12345", "--version", "2"}} {
		if _, err := parseSandboxArgs(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
