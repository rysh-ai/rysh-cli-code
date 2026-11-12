// SPDX-License-Identifier: Apache-2.0

package actors

// ##hosted fork — start N new hosted runs from the current state of a running
// one (boat gap #4; server: rysh-server POST .../hosted-runs/:runID/fork).
//
//	##hosted fork [run-id] [-n N] [--llm anthropic|gemini|openai[/model]] [--no-attach]
//
// The server copies the run's home (the session, the work tree) into each fork
// and gives every fork its own host key and environment; the laptop sends one
// fresh throwaway public key for all of them and, with --llm, the model key
// from ##secret (a fork never inherits the parent's key). Only the user who
// started a run may fork it.
//
// The forks show as one group on the fleet board: a fleet named
// fork-<parent id>, registered up when the forks start and set down when the
// last one stops.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/hosted"
)

const hostedForkUsage = "##hosted fork [run-id] [-n N] [--llm anthropic|gemini|openai[/model]] [--no-attach]"

// hostedForkMax mirrors the server's ceiling; the server's own cap (default
// 3) may be lower and is the one that decides.
const hostedForkMax = 10

type hostedForkOpts struct {
	Run      string
	N        int
	Provider string
	Model    string
	NoAttach bool
}

type hostedForkResult struct {
	Parent string
	Forks  []hostedUpResult
	Failed []hosted.Run
	Group  string
}

func parseHostedFork(args []string) (hostedForkOpts, error) {
	o := hostedForkOpts{N: 1}
	for len(args) > 0 {
		a := args[0]
		switch a {
		case "--no-attach":
			o.NoAttach = true
			args = args[1:]
			continue
		case "-n", "--llm":
			if len(args) < 2 {
				return o, fmt.Errorf("%s needs a value; usage: %s", a, hostedForkUsage)
			}
			v := args[1]
			if a == "-n" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 1 || n > hostedForkMax {
					return o, fmt.Errorf("-n: a number from 1 to %d", hostedForkMax)
				}
				o.N = n
			} else {
				prov, model, _ := strings.Cut(v, "/")
				if _, ok := hostedKeyNames[prov]; !ok {
					return o, fmt.Errorf("--llm: %q is not available on hosted runs; use anthropic, gemini or openai (see ##llm)", prov)
				}
				o.Provider, o.Model = prov, model
			}
			args = args[2:]
			continue
		}
		if strings.HasPrefix(a, "-") {
			return o, fmt.Errorf("unknown flag %s; usage: %s", a, hostedForkUsage)
		}
		if o.Run != "" {
			return o, fmt.Errorf("one run id only; usage: %s", hostedForkUsage)
		}
		o.Run = a
		args = args[1:]
	}
	return o, nil
}

// hostedFork asks for the forks and keeps what the laptop needs to attach to
// each running one. A fork that cannot be saved (no usable host key, a disk
// error) is stopped: nobody could reach it.
func hostedFork(ctx context.Context, api hostedAPI, st hosted.Store, parentID string, o hostedForkOpts, keyFor func(string) string) (hostedForkResult, error) {
	var so hosted.StartOptions
	if o.Provider != "" {
		name := hostedKeyNames[o.Provider]
		var key string
		if keyFor != nil {
			key = keyFor(o.Provider)
		}
		if key == "" {
			return hostedForkResult{}, fmt.Errorf("no %s key to send: set it with ##secret new %s <key> (nothing was forked)", o.Provider, name)
		}
		so.LLMProvider, so.LLMModel, so.LLMKey = o.Provider, o.Model, key
	}
	kp, err := hosted.NewKeyPair()
	if err != nil {
		return hostedForkResult{}, err
	}
	runs, err := api.Fork(ctx, parentID, o.N, so, kp.Public)
	if err != nil {
		return hostedForkResult{}, err
	}
	res := hostedForkResult{Parent: parentID, Group: hostedForkGroup(parentID)}
	for _, r := range runs {
		if r.State != "running" {
			res.Failed = append(res.Failed, r)
			continue
		}
		paths, err := st.Save(r, kp)
		if err != nil {
			if r.ID != "" {
				_, _ = api.Stop(ctx, r.ID)
			}
			r.Reason = err.Error() + " (stopped)"
			res.Failed = append(res.Failed, r)
			continue
		}
		alias := hosted.Alias(r.ID)
		res.Forks = append(res.Forks, hostedUpResult{
			Run: r, Alias: alias, Paths: paths,
			RemoteAddArgs: []string{alias, "--ssh-config", paths.SSHConfig, "--session", hosted.SessionName, "--dir", hosted.WorkDir, "--bin", "/usr/local/bin/rysh_local"},
		})
	}
	if len(res.Forks) == 0 {
		return res, errors.New("no fork started (see ##hosted list)")
	}
	return res, nil
}

// hostedForkGroup is the fleet a run's forks are grouped under on the fleet
// board: "fork-<parent id>", lowercase and cut to the 32 characters a board id
// may have (a rysh run id is 16 hex; a Boat id is "bx_" + up to 32).
func hostedForkGroup(parentID string) string {
	name := "fork-" + strings.ToLower(parentID)
	if len(name) > 32 {
		name = name[:32]
	}
	return name
}

// hostedForkGroupUpdates are the fleet registry writes for a fork group:
// register + up when forks start, down when the last one stops.
func hostedForkGroupUpdates(parentID string, up bool) []fleet.Update {
	name := hostedForkGroup(parentID)
	if !up {
		return []fleet.Update{{Op: fleet.OpState, Name: name, State: fleet.StateDown}}
	}
	return []fleet.Update{
		{Op: fleet.OpRegister, Fleet: fleet.Fleet{Name: name, Source: "hosted-fork:" + parentID, Created: time.Now().UnixMilli()}},
		{Op: fleet.OpState, Name: name, State: fleet.StateUp},
	}
}

// liveForksOf counts parentID's forks still pending or running, leaving out
// except (the one being stopped).
func liveForksOf(runs []hosted.Run, parentID, except string) int {
	n := 0
	for _, r := range runs {
		if r.ForkOf == parentID && r.ID != except && (r.State == "running" || r.State == "pending") {
			n++
		}
	}
	return n
}

// hostedFleetSend writes one fleet registry update (a variable so tests can
// stand in for the session bus).
var hostedFleetSend = func(w *WorkspaceActor, u fleet.Update) error {
	_, err := fleet.Send(w.nc, u, fleetQueryTimeout)
	return err
}

// setForkGroup registers or downs a fork group, best effort: without a fleet
// registry the forks still work; the note says why the board shows nothing.
func (w *WorkspaceActor) setForkGroup(parentID string, up bool) string {
	for _, u := range hostedForkGroupUpdates(parentID, up) {
		if err := hostedFleetSend(w, u); err != nil {
			return fmt.Sprintf("not on the fleet board (%v)", err)
		}
	}
	if up {
		return fmt.Sprintf("fleet %s (##fleet board)", hostedForkGroup(parentID))
	}
	return ""
}

// hostedForkCommand implements ##hosted fork.
func (w *WorkspaceActor) hostedForkCommand(ctx context.Context, out *strings.Builder, args []string) error {
	o := ryshWriter(out)
	opts, err := parseHostedFork(args)
	if err != nil {
		o.Usage(hostedForkUsage)
		return err
	}
	api, st, err := w.hostedTools()
	if err != nil {
		return err
	}
	parent, err := w.hostedTarget(ctx, api, st, opts.Run)
	if err != nil {
		return err
	}
	res, err := hostedFork(ctx, api, st, parent, opts, w.providerAPIKey)
	if err != nil {
		return err
	}
	o.Header(fmt.Sprintf("%d fork(s) of hosted run %s", len(res.Forks), parent))
	for _, f := range res.Forks {
		o.Row("%-18s %-12s %s:%d (host key pinned)", f.Run.ID, f.Run.Name, f.Run.Host, f.Run.Port)
	}
	for _, f := range res.Failed {
		o.Row("%-18s %-12s %s %s", f.ID, f.Name, f.State, f.Reason)
	}
	o.Row("group   %s", w.setForkGroup(parent, true))
	if opts.Provider == "" {
		o.Row("model   none sent: shell-only forks (add --llm to send your own key; the parent's is never copied)")
	} else {
		o.Row("model   %s%s, your key from ##secret %s", opts.Provider, modelSuffix(opts.Model), hostedKeyNames[opts.Provider])
	}
	if opts.NoAttach {
		o.Row("attach  ##hosted attach <id>")
		return nil
	}
	for _, f := range res.Forks {
		if err := w.remoteAdd(out, f.RemoteAddArgs); err != nil {
			return err
		}
	}
	return nil
}
