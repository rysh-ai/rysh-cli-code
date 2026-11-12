// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// `rysh run --hosted` (boat gap #5): the same prompt and exit codes, run on a
// rysh.ai hosted runner instead of a throwaway local daemon.

func TestParseRunArgsHosted(t *testing.T) {
	o, err := parseRunArgs([]string{"--hosted", "--provider", "anthropic", "--model", "sonnet-5", "--size", "default", "--timeout", "3m", "--keep", "--json", "fix", "it"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.Hosted || o.Provider != "anthropic" || o.Model != "sonnet-5" || o.Size != "default" || o.Timeout != 3*time.Minute || !o.Keep || !o.JSON || o.Prompt != "fix it" {
		t.Fatalf("opts = %+v", o)
	}
	// A "--" ends the flags: everything after it is the prompt.
	o, err = parseRunArgs([]string{"--hosted", "--provider", "openai", "--", "--json", "is", "text"})
	if err != nil || o.Prompt != "--json is text" || o.JSON {
		t.Fatalf("after --: opts=%+v err=%v", o, err)
	}
}

func TestParseRunArgsHostedRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no provider":   {[]string{"--hosted", "x"}, "--hosted needs --provider"},
		"ollama":        {[]string{"--hosted", "--provider", "ollama", "x"}, "anthropic, gemini or openai"},
		"size":          {[]string{"--hosted", "--provider", "anthropic", "--size", "xl", "x"}, `one size today: "default"`},
		"size without":  {[]string{"--size", "default", "x"}, "--size needs --hosted"},
		"model without": {[]string{"--model", "m", "x"}, "--model needs --hosted"},
		"worktree":      {[]string{"--hosted", "--provider", "anthropic", "--worktree", "x"}, "--worktree is not available with --hosted"},
		"record":        {[]string{"--hosted", "--provider", "anthropic", "--record", "d", "x"}, "--record is not available with --hosted"},
		"replay":        {[]string{"--hosted", "--provider", "anthropic", "--replay", "d", "x"}, "--replay is not available with --hosted"},
		"share":         {[]string{"--hosted", "--provider", "anthropic", "--share", "d", "x"}, "--share is not available with --hosted"},
		"budget":        {[]string{"--hosted", "--provider", "anthropic", "--budget", "$2", "x"}, "--budget is not available with --hosted"},
		"result-out":    {[]string{"--hosted", "--provider", "anthropic", "--result-out", "f", "x"}, "--result-out is not available with --hosted"},
		"json-audit":    {[]string{"--hosted", "--provider", "anthropic", "--json-audit", "f", "x"}, "--json-audit is not available with --hosted"},
	} {
		_, err := parseRunArgs(c.args)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

func TestHostedRunKeyComesFromTheEnvironmentByName(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	_, err := hostedRunLLMKey("anthropic")
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("missing key: err = %v", err)
	}
	t.Setenv("OPENAI_API_KEY", "test-only-llm-not-a-key")
	k, err := hostedRunLLMKey("openai")
	if err != nil || k != "test-only-llm-not-a-key" {
		t.Fatalf("key = %q err = %v", k, err)
	}
	if err != nil && strings.Contains(err.Error(), "test-only") {
		t.Fatal("the error carries the key")
	}
}

// cli-core finding (R3 / ledger-167 class): `rysh run --hosted` must never send
// an environment model key to a server that a config file chose. The key goes
// only to a first-party upstream or to the one RYSH_UPSTREAM_URL names.
func TestHostedRunEnvKeyNeverGoesToAFileChosenServer(t *testing.T) {
	const fakeKey = "fake-env-llm-key-r3"
	t.Setenv("RYSH_SESSION", "")
	t.Setenv("RYSH_DIR", "")
	t.Setenv("RYSH_UPSTREAM_URL", "")
	t.Setenv("RYSH_UPSTREAM_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", fakeKey)
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, fmt.Sprint(r.Header)+string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	p := filepath.Join(t.TempDir(), "evil.rysh.config.yaml")
	body := "upstream:\n  enabled: true\n  url: " + srv.URL + "\n  api_key: rysh_file_key\n  workspace: 11111111-1111-1111-1111-111111111111\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := runOptions{Hosted: true, Provider: "anthropic", Prompt: "x", Timeout: 2 * time.Second}

	code, err := runHostedRun(config.LoadFrom(p), opts)
	if code == 0 || err == nil || !strings.Contains(err.Error(), "RYSH_UPSTREAM_URL") || strings.Contains(err.Error(), fakeKey) {
		t.Fatalf("file-chosen server: code=%d err=%v", code, err)
	}
	mu.Lock()
	for _, s := range seen {
		if strings.Contains(s, fakeKey) {
			t.Fatal("the environment's model key went to a server the config file chose")
		}
	}
	n := len(seen)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("%d request(s) reached the file-chosen server before the refusal", n)
	}

	// Named by the environment: the key may go (the server answers 404, so
	// the run fails, but the request was made).
	t.Setenv("RYSH_UPSTREAM_URL", srv.URL)
	if code, _ := runHostedRun(config.LoadFrom(p), opts); code == 0 {
		t.Fatal("a 404 server gave exit 0")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("with RYSH_UPSTREAM_URL set no request was made: this proves nothing")
	}
}
