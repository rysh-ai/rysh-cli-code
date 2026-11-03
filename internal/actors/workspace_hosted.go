// SPDX-License-Identifier: Apache-2.0

package actors

// ##hosted — start a run on rysh.ai's hosted runner and attach to it, in one
// step (design 074, "Hosted runner"). The user types nothing but the command:
// the server address, workspace and key come from the `upstream:` block of
// rysh.config.yaml, the throwaway ssh key and the pinned host key are made and
// kept for them, and the attach is the existing `##remote add`.
//
//	##hosted [up] [--name N] [--llm anthropic|gemini|openai[/model]] [--prompt TEXT] [--no-attach]
//	##hosted list | status
//	##hosted attach [run-id]
//	##hosted stop [run-id]
//
// Secrets: the workspace key and the model key are used from memory only. They
// never appear in output, in a file under the run directory, or in an argv. The
// model key is sent only when --llm is given, and only in the request body.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/hosted"
)

const hostedUsage = "##hosted [up] [--name N] [--llm anthropic|gemini|openai[/model]] [--prompt TEXT] [--no-attach]"

// hostedKeyNames maps a model provider to the ##secret name that holds its key.
var hostedKeyNames = map[string]string{
	"anthropic": "ANTHROPIC_API_KEY",
	"gemini":    "GEMINI_API_KEY",
	"openai":    "OPENAI_API_KEY",
}

// hostedAPI is what the command needs from the server client.
type hostedAPI interface {
	Start(ctx context.Context, o hosted.StartOptions, publicKey string) (hosted.Run, error)
	List(ctx context.Context) ([]hosted.Run, error)
	Get(ctx context.Context, id string) (hosted.Run, error)
	Stop(ctx context.Context, id string) (hosted.Run, error)
}

type hostedUpOpts struct {
	Name     string
	Provider string
	Model    string
	Prompt   string
	NoAttach bool
}

type hostedUpResult struct {
	Run           hosted.Run
	Alias         string
	Paths         hosted.Paths
	RemoteAddArgs []string
}

func parseHostedUp(args []string) (hostedUpOpts, error) {
	var o hostedUpOpts
	for len(args) > 0 {
		switch args[0] {
		case "--no-attach":
			o.NoAttach = true
			args = args[1:]
			continue
		case "--name", "--llm", "--prompt":
		default:
			return o, fmt.Errorf("unknown flag %s; usage: %s", args[0], hostedUsage)
		}
		if len(args) < 2 {
			return o, fmt.Errorf("%s needs a value; usage: %s", args[0], hostedUsage)
		}
		v := args[1]
		switch args[0] {
		case "--name":
			if len(v) > 32 || strings.ContainsAny(v, "\r\n\x00") {
				return o, errors.New("--name: at most 32 characters, one line")
			}
			o.Name = v
		case "--prompt":
			o.Prompt = v
		case "--llm":
			prov, model, _ := strings.Cut(v, "/")
			if _, ok := hostedKeyNames[prov]; !ok {
				return o, fmt.Errorf("--llm: %q is not available on hosted runs; use anthropic, gemini or openai (see ##llm)", prov)
			}
			o.Provider, o.Model = prov, model
		}
		args = args[2:]
	}
	return o, nil
}

// hostedConn reads the server connection from the workspace's upstream block.
func hostedConn(cfg config.Config) (hosted.Conn, error) {
	c := hosted.Conn{URL: cfg.Upstream.URL, APIKey: cfg.Upstream.APIKey, WorkspaceID: cfg.Upstream.Workspace}
	if err := c.Validate(); err != nil {
		return c, fmt.Errorf("##hosted needs the upstream connection: %w", err)
	}
	return c, nil
}

// hostedUp starts a run and keeps what the laptop needs to attach to it.
// keyFor resolves a provider's model key from the secret store; it is called
// only when a provider was chosen.
func hostedUp(ctx context.Context, api hostedAPI, st hosted.Store, o hostedUpOpts, keyFor func(family string) string) (hostedUpResult, error) {
	so := hosted.StartOptions{Name: o.Name, Prompt: o.Prompt}
	if o.Provider != "" {
		name := hostedKeyNames[o.Provider]
		if name == "" {
			return hostedUpResult{}, fmt.Errorf("unsupported provider %q", o.Provider)
		}
		var key string
		if keyFor != nil {
			key = keyFor(o.Provider)
		}
		if key == "" {
			return hostedUpResult{}, fmt.Errorf("no %s key to send: set it with ##secret new %s <key> (nothing was started)", o.Provider, name)
		}
		so.LLMProvider, so.LLMModel, so.LLMKey = o.Provider, o.Model, key
	}
	kp, err := hosted.NewKeyPair()
	if err != nil {
		return hostedUpResult{}, err
	}
	run, err := api.Start(ctx, so, kp.Public)
	if err != nil {
		return hostedUpResult{}, err
	}
	paths, err := st.Save(run, kp)
	if err != nil {
		// Do not leave a container nobody can reach.
		if run.ID != "" {
			_, _ = api.Stop(ctx, run.ID)
		}
		return hostedUpResult{}, fmt.Errorf("%w (the run was stopped)", err)
	}
	alias := hosted.Alias(run.ID)
	return hostedUpResult{
		Run: run, Alias: alias, Paths: paths,
		RemoteAddArgs: []string{alias, "--ssh-config", paths.SSHConfig, "--session", hosted.SessionName, "--dir", hosted.WorkDir, "--bin", "/usr/local/bin/rysh_local"},
	}, nil
}

// hostedStop ends a run on the server, then deletes the local key. A failed
// stop keeps the key: the run may still be alive.
func hostedStop(ctx context.Context, api hostedAPI, st hosted.Store, id string) error {
	if _, err := api.Stop(ctx, id); err != nil {
		return err
	}
	return st.Remove(id)
}

// pickHostedRun chooses the run a bare `attach` or `stop` means.
func pickHostedRun(runs []hosted.Run, arg string) (string, error) {
	if arg != "" {
		return arg, nil
	}
	var live []string
	for _, r := range runs {
		if r.State == "running" || r.State == "starting" {
			live = append(live, r.ID)
		}
	}
	switch len(live) {
	case 0:
		return "", errors.New("no running hosted run (start one with ##hosted)")
	case 1:
		return live[0], nil
	}
	sort.Strings(live)
	return "", fmt.Errorf("which run? %s (give its id)", strings.Join(live, ", "))
}

func hostedStateDir() (string, error) {
	if d := os.Getenv("RYSH_HOSTED_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".rysh", "hosted"), nil
}

func (w *WorkspaceActor) hostedTools() (hostedAPI, hosted.Store, error) {
	conn, err := hostedConn(w.cfg)
	if err != nil {
		return nil, hosted.Store{}, err
	}
	dir, err := hostedStateDir()
	if err != nil {
		return nil, hosted.Store{}, err
	}
	return &hosted.Client{Conn: conn}, hosted.Store{Dir: dir}, nil
}

// handleHostedCommand implements ##hosted.
func (w *WorkspaceActor) handleHostedCommand(out *strings.Builder, args []string) error {
	o := ryshWriter(out)
	sub := "up"
	if len(args) > 0 && !strings.HasPrefix(args[0], "--") {
		sub, args = args[0], args[1:]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	switch sub {
	case "up", "start":
		opts, err := parseHostedUp(args)
		if err != nil {
			o.Usage(hostedUsage)
			return err
		}
		api, st, err := w.hostedTools()
		if err != nil {
			return err
		}
		res, err := hostedUp(ctx, api, st, opts, w.providerAPIKey)
		if err != nil {
			return err
		}
		o.Header("hosted run started")
		o.Row("id      %s", res.Run.ID)
		o.Row("where   %s:%d (host key pinned in %s)", res.Run.Host, res.Run.Port, res.Paths.KnownHosts)
		o.Row("ends    %s (##hosted stop %s)", res.Run.ExpiresAt.Format(time.RFC3339), res.Run.ID)
		if opts.Provider == "" {
			o.Row("model   none: a shell-only run (add --llm anthropic|gemini|openai to bring your own model key)")
		} else {
			o.Row("model   %s%s, your key from ##secret %s (sent over https, kept only in the container)", opts.Provider, modelSuffix(opts.Model), hostedKeyNames[opts.Provider])
		}
		if opts.NoAttach {
			o.Row("attach  ##hosted attach %s", res.Run.ID)
			return nil
		}
		return w.remoteAdd(out, res.RemoteAddArgs)
	case "attach":
		api, st, err := w.hostedTools()
		if err != nil {
			return err
		}
		arg := ""
		if len(args) > 0 {
			arg = args[0]
		}
		id, err := w.hostedTarget(ctx, api, st, arg)
		if err != nil {
			return err
		}
		run, paths, err := st.Load(id)
		if err != nil {
			return fmt.Errorf("%w (a run started on another machine cannot be attached from here: its key is not here)", err)
		}
		if live, gerr := api.Get(ctx, id); gerr != nil {
			return gerr
		} else if live.State != "running" {
			return fmt.Errorf("run %s is %s", id, live.State)
		}
		alias := hosted.Alias(run.ID)
		if _, dup := w.remoteHosts[alias]; dup {
			return fmt.Errorf("run %s is already attached (see ##remote list)", id)
		}
		return w.remoteAdd(out, []string{alias, "--ssh-config", paths.SSHConfig, "--session", hosted.SessionName, "--dir", hosted.WorkDir, "--bin", "/usr/local/bin/rysh_local"})
	case "stop":
		api, st, err := w.hostedTools()
		if err != nil {
			return err
		}
		arg := ""
		if len(args) > 0 {
			arg = args[0]
		}
		id, err := w.hostedTarget(ctx, api, st, arg)
		if err != nil {
			return err
		}
		if err := hostedStop(ctx, api, st, id); err != nil {
			return err
		}
		if _, ok := w.remoteHosts[hosted.Alias(id)]; ok {
			_ = w.remoteRemove(out, hosted.Alias(id))
		}
		fmt.Fprintf(out, "\n[rysh] hosted run %s stopped; its key was deleted from this machine\n", id)
		return nil
	case "list", "ls":
		api, st, err := w.hostedTools()
		if err != nil {
			return err
		}
		runs, err := api.List(ctx)
		if err != nil {
			return err
		}
		o.Header("hosted runs")
		if len(runs) == 0 {
			o.Row("none — %s", hostedUsage)
			return nil
		}
		saved := map[string]bool{}
		if s, _ := st.List(); s != nil {
			for _, r := range s {
				saved[r.ID] = true
			}
		}
		o.Row("%-18s %-9s %-26s %-9s %s", "ID", "STATE", "WHERE", "ATTACHED", "NAME")
		for _, r := range runs {
			_, att := w.remoteHosts[hosted.Alias(r.ID)]
			where := "-"
			if r.Host != "" {
				where = fmt.Sprintf("%s:%d", r.Host, r.Port)
			}
			note := r.Name
			if r.State == "running" && !saved[r.ID] {
				note += " (key not on this machine)"
			}
			o.Row("%-18s %-9s %-26s %-9v %s", r.ID, r.State, where, att, note)
		}
		return nil
	case "status":
		o.Header("hosted runner")
		conn, err := hostedConn(w.cfg)
		if err != nil {
			o.Row("not ready: %v", err)
			return nil
		}
		o.Row("server     %s", conn.URL)
		o.Row("workspace  %s", conn.WorkspaceID)
		o.Row("api key    set (from rysh.config.yaml upstream.api_key; needs write permission)")
		for _, p := range []string{"anthropic", "gemini", "openai"} {
			have := "missing"
			if w.providerAPIKey(p) != "" {
				have = "set"
			}
			o.Row("%-10s %s ##secret %s", p, have, hostedKeyNames[p])
		}
		return nil
	}
	o.Usage(hostedUsage, "##hosted list", "##hosted attach [run-id]", "##hosted stop [run-id]", "##hosted status")
	return fmt.Errorf("unknown ##hosted subcommand %q", sub)
}

func modelSuffix(m string) string {
	if m == "" {
		return ""
	}
	return "/" + m
}

// hostedTarget resolves the run id for attach and stop.
func (w *WorkspaceActor) hostedTarget(ctx context.Context, api hostedAPI, st hosted.Store, arg string) (string, error) {
	if arg != "" {
		return arg, nil
	}
	runs, err := api.List(ctx)
	if err != nil {
		return "", err
	}
	return pickHostedRun(runs, "")
}
