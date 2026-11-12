// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/hosted"
)

const forkParentID = "423b54f6fd36fa94"

// fakeForkAPI is fakeHostedAPI plus Fork.
type fakeForkAPI struct {
	fakeHostedAPI
	forkID   string
	forkN    int
	forkOpts hosted.StartOptions
	forkPubs []string
	forkRuns []hosted.Run
	forkErr  error
	listRuns []hosted.Run
}

func (f *fakeForkAPI) Fork(_ context.Context, id string, n int, o hosted.StartOptions, pub string) ([]hosted.Run, error) {
	f.forkID, f.forkN, f.forkOpts = id, n, o
	f.forkPubs = append(f.forkPubs, pub)
	return f.forkRuns, f.forkErr
}
func (f *fakeForkAPI) List(context.Context) ([]hosted.Run, error) { return f.listRuns, nil }

func forkRun(id, state string) hosted.Run {
	return hosted.Run{ID: id, State: state, Host: "dev.rysh.ai", Port: 32771, HostKey: hostedTestHostKey, Name: "work-f", ForkOf: forkParentID}
}

func TestParseHostedFork(t *testing.T) {
	o, err := parseHostedFork([]string{forkParentID, "-n", "3", "--llm", "anthropic/sonnet-5", "--no-attach"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Run != forkParentID || o.N != 3 || o.Provider != "anthropic" || o.Model != "sonnet-5" || !o.NoAttach {
		t.Fatalf("opts = %+v", o)
	}
	o, err = parseHostedFork(nil)
	if err != nil || o.Run != "" || o.N != 1 {
		t.Fatalf("defaults = %+v %v", o, err)
	}
	for _, bad := range [][]string{{"-n", "0"}, {"-n", "11"}, {"-n", "x"}, {"-n"}, {"--llm", "grok"}, {"--bogus"}, {forkParentID, "extra"}} {
		if _, err := parseHostedFork(bad); err == nil {
			t.Errorf("parseHostedFork(%v) accepted", bad)
		}
	}
}

func TestHostedForkSavesEveryRunningForkWithOneKey(t *testing.T) {
	api := &fakeForkAPI{forkRuns: []hosted.Run{
		forkRun("aaaa000000000001", "running"), forkRun("aaaa000000000002", "failed"), forkRun("aaaa000000000003", "running"),
	}}
	st := hosted.Store{Dir: t.TempDir()}
	res, err := hostedFork(context.Background(), api, st, forkParentID, hostedForkOpts{N: 3, Provider: "anthropic"}, func(string) string { return "sk-from-secret-store" })
	if err != nil {
		t.Fatal(err)
	}
	if api.forkID != forkParentID || api.forkN != 3 || api.forkOpts.LLMKey != "sk-from-secret-store" || api.forkOpts.LLMProvider != "anthropic" {
		t.Fatalf("fork call = %s %d %+v", api.forkID, api.forkN, api.forkOpts)
	}
	if len(api.forkPubs) != 1 || !strings.HasPrefix(api.forkPubs[0], "ssh-ed25519 ") {
		t.Fatalf("pubs = %v", api.forkPubs)
	}
	if len(res.Forks) != 2 || len(res.Failed) != 1 || res.Failed[0].ID != "aaaa000000000002" {
		t.Fatalf("res = %+v", res)
	}
	for _, f := range res.Forks {
		if _, err := os.Stat(f.Paths.Key); err != nil {
			t.Fatalf("fork %s key not kept: %v", f.Run.ID, err)
		}
		if f.Alias != hosted.Alias(f.Run.ID) || f.RemoteAddArgs[0] != f.Alias {
			t.Fatalf("fork attach = %+v", f)
		}
	}
	if res.Group != "fork-"+forkParentID {
		t.Fatalf("group = %q", res.Group)
	}
}

func TestHostedForkUnsavableForkIsStopped(t *testing.T) {
	bad := forkRun("aaaa000000000001", "running")
	bad.HostKey = "" // nothing to pin: never attach, stop it
	api := &fakeForkAPI{forkRuns: []hosted.Run{bad, forkRun("aaaa000000000002", "running")}}
	res, err := hostedFork(context.Background(), api, hosted.Store{Dir: t.TempDir()}, forkParentID, hostedForkOpts{N: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(api.stopped) != 1 || api.stopped[0] != "aaaa000000000001" || len(res.Forks) != 1 {
		t.Fatalf("stopped = %v, forks = %d", api.stopped, len(res.Forks))
	}
	// No usable fork at all is an error.
	api2 := &fakeForkAPI{forkRuns: []hosted.Run{forkRun("aaaa000000000009", "failed")}}
	if _, err := hostedFork(context.Background(), api2, hosted.Store{Dir: t.TempDir()}, forkParentID, hostedForkOpts{N: 1}, nil); err == nil {
		t.Fatal("no fork started but no error")
	}
}

func TestHostedForkWithoutModelKeyForksNothing(t *testing.T) {
	api := &fakeForkAPI{}
	_, err := hostedFork(context.Background(), api, hosted.Store{Dir: t.TempDir()}, forkParentID, hostedForkOpts{N: 1, Provider: "openai"}, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "##secret") || api.forkN != 0 {
		t.Fatalf("err = %v, forkN = %d", err, api.forkN)
	}
	api.forkErr = errors.New("server says no")
	if _, err := hostedFork(context.Background(), api, hosted.Store{Dir: t.TempDir()}, forkParentID, hostedForkOpts{N: 1}, nil); err == nil {
		t.Fatal("a server refusal returned nil")
	}
}

// The forks show as one group on the fleet board: a fleet named after the
// parent, registered and marked up; down again when no fork is left.
func TestHostedForkGroupIsAFleet(t *testing.T) {
	name := hostedForkGroup(forkParentID)
	if err := fleet.ValidateName(name); err != nil {
		t.Fatalf("group %q is not a legal fleet name: %v", name, err)
	}
	ups := hostedForkGroupUpdates(forkParentID, true)
	if len(ups) != 2 || ups[0].Op != fleet.OpRegister || ups[0].Fleet.Name != name || ups[0].Fleet.Source != "hosted-fork:"+forkParentID ||
		ups[1].Op != fleet.OpState || ups[1].State != fleet.StateUp {
		t.Fatalf("updates = %+v", ups)
	}
	down := hostedForkGroupUpdates(forkParentID, false)
	if len(down) != 1 || down[0].Op != fleet.OpState || down[0].Name != name || down[0].State != fleet.StateDown {
		t.Fatalf("down = %+v", down)
	}
}

func TestHostedForksLeftOf(t *testing.T) {
	runs := []hosted.Run{
		forkRun("aaaa000000000001", "running"), forkRun("aaaa000000000002", "stopped"),
		{ID: "bbbb000000000001", State: "running", ForkOf: "other"},
	}
	if n := liveForksOf(runs, forkParentID, "aaaa000000000001"); n != 0 {
		t.Fatalf("after stopping the last live fork: %d left", n)
	}
	if n := liveForksOf(runs, forkParentID, ""); n != 1 {
		t.Fatalf("live forks = %d", n)
	}
}

// fakeHostedAPI has no fork half of its own; the plain tests never fork.
func (f *fakeHostedAPI) Fork(context.Context, string, int, hosted.StartOptions, string) ([]hosted.Run, error) {
	return nil, errors.New("fakeHostedAPI does not fork")
}

// A Boat sandbox id ("bx_" + up to 32) still gives a legal fleet name.
func TestHostedForkGroupFitsABoatID(t *testing.T) {
	for _, id := range []string{"bx_0123456789abcdefghijklmnopqrstuv", "bx_01234567"} {
		name := hostedForkGroup(id)
		if err := fleet.ValidateName(name); err != nil {
			t.Errorf("group for %q = %q: %v", id, name, err)
		}
		if !strings.HasPrefix(name, "fork-bx_") {
			t.Errorf("group for %q = %q", id, name)
		}
	}
}
