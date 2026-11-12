// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/hosted"
)

const hostedTestHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UkOGRLrDDDRvVLHKPtC21iXSUBVl"

type fakeHostedAPI struct {
	started  []hosted.StartOptions
	pubs     []string
	stopped  []string
	startErr error
	runs     []hosted.Run
}

func (f *fakeHostedAPI) Start(_ context.Context, o hosted.StartOptions, pub string) (hosted.Run, error) {
	if f.startErr != nil {
		return hosted.Run{}, f.startErr
	}
	f.started = append(f.started, o)
	f.pubs = append(f.pubs, pub)
	return hosted.Run{ID: "423b54f6fd36fa94", State: "running", Host: "dev.rysh.ai", Port: 32770, HostKey: hostedTestHostKey, Name: o.Name}, nil
}
func (f *fakeHostedAPI) List(context.Context) ([]hosted.Run, error) { return f.runs, nil }
func (f *fakeHostedAPI) Get(_ context.Context, id string) (hosted.Run, error) {
	return hosted.Run{ID: id, State: "running"}, nil
}
func (f *fakeHostedAPI) Stop(_ context.Context, id string) (hosted.Run, error) {
	f.stopped = append(f.stopped, id)
	return hosted.Run{ID: id, State: "stopped"}, nil
}

func TestParseHostedUp(t *testing.T) {
	o, err := parseHostedUp([]string{"--name", "fix-tests", "--llm", "anthropic/claude-x", "--prompt", "run the tests"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Name != "fix-tests" || o.Provider != "anthropic" || o.Model != "claude-x" || o.Prompt != "run the tests" || o.NoAttach {
		t.Errorf("opts = %+v", o)
	}
	o, err = parseHostedUp([]string{"--llm", "gemini", "--no-attach"})
	if err != nil || o.Provider != "gemini" || o.Model != "" || !o.NoAttach {
		t.Errorf("opts = %+v err %v", o, err)
	}
	if o, err = parseHostedUp(nil); err != nil || o.Provider != "" {
		t.Errorf("no args must mean a shell-only run: %+v %v", o, err)
	}
	for _, bad := range [][]string{{"--llm", "grok"}, {"--llm", "nonsense/x"}, {"--bogus"}, {"--name"}, {"--name", "bad name\nx"}} {
		if _, err := parseHostedUp(bad); err == nil {
			t.Errorf("parseHostedUp(%v) accepted", bad)
		}
	}
}

func TestHostedConnFromUpstream(t *testing.T) {
	var cfg config.Config
	if _, err := hostedConn(cfg); err == nil || !strings.Contains(err.Error(), "upstream") {
		t.Errorf("empty config: err = %v; it must say how to set upstream", err)
	}
	cfg.Upstream.URL = "https://rysh.ai"
	cfg.Upstream.APIKey = "k"
	cfg.Upstream.Workspace = "ws-1"
	c, err := hostedConn(cfg)
	if err != nil || c.URL != "https://rysh.ai" || c.APIKey != "k" || c.WorkspaceID != "ws-1" {
		t.Errorf("conn = %+v err %v", c, err)
	}
}

func TestHostedUpStartsSavesAndNamesTheAttachArgs(t *testing.T) {
	api := &fakeHostedAPI{}
	st := hosted.Store{Dir: t.TempDir()}
	res, err := hostedUp(context.Background(), api, st, hostedUpOpts{Name: "n", Provider: "anthropic", Model: "m"}, func(family string) string {
		if family != "anthropic" {
			t.Errorf("resolved the key for %q", family)
		}
		return "sk-from-secret-store"
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(api.started) != 1 || api.started[0].LLMKey != "sk-from-secret-store" || api.started[0].LLMProvider != "anthropic" {
		t.Fatalf("started = %+v", api.started)
	}
	if !strings.HasPrefix(api.pubs[0], "ssh-ed25519 ") {
		t.Errorf("the public half was not sent: %q", api.pubs[0])
	}
	if res.Run.ID != "423b54f6fd36fa94" || res.Alias != "hosted-423b54f6fd36fa94" {
		t.Errorf("res = %+v", res)
	}
	want := []string{"hosted-423b54f6fd36fa94", "--ssh-config", res.Paths.SSHConfig, "--session", "work", "--dir", "work", "--bin", "/usr/local/bin/rysh_local"}
	if strings.Join(res.RemoteAddArgs, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("remote add args = %v\nwant %v", res.RemoteAddArgs, want)
	}
	if _, err := os.Stat(res.Paths.Key); err != nil {
		t.Errorf("private key not kept: %v", err)
	}
}

func TestHostedUpWithoutKeyStartsNothing(t *testing.T) {
	api := &fakeHostedAPI{}
	_, err := hostedUp(context.Background(), api, hosted.Store{Dir: t.TempDir()}, hostedUpOpts{Provider: "openai"}, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("err = %v; it must name the secret to set", err)
	}
	if len(api.started) != 0 {
		t.Error("a run was started although the model key is missing")
	}
}

func TestHostedUpShellOnlyNeedsNoModelKey(t *testing.T) {
	api := &fakeHostedAPI{}
	_, err := hostedUp(context.Background(), api, hosted.Store{Dir: t.TempDir()}, hostedUpOpts{}, func(string) string {
		t.Error("no key should be looked up for a shell-only run")
		return ""
	})
	if err != nil || len(api.started) != 1 || api.started[0].LLMKey != "" || api.started[0].LLMProvider != "" {
		t.Fatalf("started=%+v err=%v", api.started, err)
	}
}

func TestHostedUpServerRefusalLeavesNoFiles(t *testing.T) {
	dir := t.TempDir()
	api := &fakeHostedAPI{startErr: errors.New("this api key is read-only; the hosted runner needs a key with write permission")}
	_, err := hostedUp(context.Background(), api, hosted.Store{Dir: dir}, hostedUpOpts{}, nil)
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("err = %v", err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Errorf("a refused start left %d entries behind", len(ents))
	}
}

func TestHostedStopRemovesLocalKeyAfterTheServerStopped(t *testing.T) {
	api := &fakeHostedAPI{}
	st := hosted.Store{Dir: t.TempDir()}
	res, err := hostedUp(context.Background(), api, st, hostedUpOpts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := hostedStop(context.Background(), api, st, res.Run.ID); err != nil {
		t.Fatal(err)
	}
	if len(api.stopped) != 1 || api.stopped[0] != res.Run.ID {
		t.Errorf("stopped = %v", api.stopped)
	}
	if _, err := os.Stat(res.Paths.Key); !os.IsNotExist(err) {
		t.Errorf("the private key survived stop: %v", err)
	}
}

func TestHostedStopKeepsKeyWhenServerFails(t *testing.T) {
	api := &fakeHostedAPI{}
	st := hosted.Store{Dir: t.TempDir()}
	res, _ := hostedUp(context.Background(), api, st, hostedUpOpts{}, nil)
	failing := &failingStop{fakeHostedAPI: api}
	if err := hostedStop(context.Background(), failing, st, res.Run.ID); err == nil {
		t.Fatal("a failed stop must be reported")
	}
	if _, err := os.Stat(res.Paths.Key); err != nil {
		t.Errorf("the key was deleted although the run may still be alive: %v", err)
	}
}

type failingStop struct{ *fakeHostedAPI }

func (f *failingStop) Stop(context.Context, string) (hosted.Run, error) {
	return hosted.Run{}, errors.New("could not complete the request (HTTP 502)")
}

func TestLastHostedRunPicksNewestRunning(t *testing.T) {
	runs := []hosted.Run{{ID: "aaaaaaaa", State: "stopped"}, {ID: "bbbbbbbb", State: "running"}}
	if id, err := pickHostedRun(runs, ""); err != nil || id != "bbbbbbbb" {
		t.Errorf("pick = %q %v", id, err)
	}
	if id, err := pickHostedRun(runs, "aaaaaaaa"); err != nil || id != "aaaaaaaa" {
		t.Errorf("explicit id = %q %v", id, err)
	}
	if _, err := pickHostedRun(nil, ""); err == nil {
		t.Error("no runs must be an error")
	}
	two := []hosted.Run{{ID: "aaaaaaaa", State: "running"}, {ID: "bbbbbbbb", State: "running"}}
	if _, err := pickHostedRun(two, ""); err == nil || !strings.Contains(err.Error(), "which") {
		t.Errorf("two running runs must ask which one: %v", err)
	}
}
