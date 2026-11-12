// SPDX-License-Identifier: Apache-2.0

package main

// `rysh sandbox` (rr-order-154 C10): drive rysh Sandboxes from a shell.
//
//	rysh sandbox create [--name n] [--size default] [--ssh-key <file.pub>] [--json]
//	rysh sandbox list|ls [--json]
//	rysh sandbox get|stop|resume|delete <id> [--json]
//	rysh sandbox exec <id> [--cwd d] [--timeout s] [--stdin] -- <argv...>
//	rysh sandbox cp <local> <id>:<path> | <id>:<path> <local>
//	rysh sandbox prompt <id> [--pane p] [--wait s] -- <text>
//	rysh sandbox steer <id> [--pane p] -- <text>
//	rysh sandbox interrupt <id> [--pane p]
//	rysh sandbox events <id>
//	rysh sandbox create --ssh …            (a fresh key; the host key is pinned)
//	rysh sandbox ssh <id> [-- <command…>]   (ssh with the pinned config only)
//	rysh sandbox port <id> ls | public <port> on|off | url <port>
//	rysh sandbox screenshot <id> <file.png>
//
// The server is the upstream: block of rysh.config.yaml (as ##hosted). No
// model key is read from the environment here. Exit codes: exec returns the
// command's own exit code; everything else 0 ok, 1 error. --pane defaults to
// the sandbox's first pane.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/hosted"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
)

const sandboxUsage = `usage: rysh sandbox create [--name n] [--size default] [--ssh-key <file.pub>] [--json]
       rysh sandbox list [--json]
       rysh sandbox get|stop|resume|delete <id> [--json]
       rysh sandbox exec <id> [--cwd d] [--timeout s] [--stdin] -- <argv...>
       rysh sandbox cp <local> <id>:<path> | <id>:<path> <local>
       rysh sandbox prompt <id> [--pane p] [--wait s] -- <text>
       rysh sandbox steer <id> [--pane p] -- <text>
       rysh sandbox interrupt <id> [--pane p]
       rysh sandbox events <id>
       rysh sandbox create --ssh …   |   rysh sandbox ssh <id> [-- <command…>]
       rysh sandbox port <id> ls | public <port> on|off | url <port>
       rysh sandbox screenshot <id> <file.png>
       rysh sandbox fork <id> [--count n]   |   rysh sandbox create --from-snapshot <snap>
       rysh sandbox snapshot create <id> <name> | ls | rm <snap>
       rysh sandbox desktop <id> [--control]
       rysh sandbox env ls | get <name> [--version n] | put <name> <file.json> | rm <name>
       create also takes: --environment <name> --env K=V --secret NAME[:path] --allow host:port[,port]
         (a --secret value is read from the environment variable NAME, never from the command line)`

type sandboxArgs struct {
	Sub, ID, Name, Size, SSHKey, Cwd, Pane, Text, Src, Dst string
	Timeout, Wait                                          int
	JSON, Stdin                                            bool
	Argv                                                   []string
	SSH                                                    bool   // create --ssh
	PortOp                                                 string // port: ls | public | url
	Port                                                   int
	Public                                                 bool
	Count                                                  int      // fork --count (default 1)
	SnapOp                                                 string   // snapshot: create | ls | rm
	Snapshot                                               string   // snapshot id (rm; create --from-snapshot)
	Control                                                bool     // desktop --control
	Environment                                            string   // create --environment
	EnvVars, SecretNames, Allow                            []string // create --env K=V, --secret NAME[:path], --allow host:port[,port]
	EnvOp                                                  string   // env: ls | get | put | rm
	Version                                                int      // env get --version
}

// extractRepeatedFlag removes every "--name v" / "--name=v" and returns the
// values in order; a flag with no value is an error.
func extractRepeatedFlag(args []string, name string) ([]string, []string, error) {
	var rest, vals []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == name:
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("%s needs a value", name)
			}
			vals = append(vals, args[i+1])
			i++
		case strings.HasPrefix(args[i], name+"="):
			vals = append(vals, strings.TrimPrefix(args[i], name+"="))
		default:
			rest = append(rest, args[i])
		}
	}
	return rest, vals, nil
}

// parseAllow reads "host:443" or "host:443,22" into an egress rule.
func parseAllow(s string) (hosted.SandboxEgressRule, error) {
	i := strings.LastIndex(s, ":")
	if i <= 0 || i == len(s)-1 {
		return hosted.SandboxEgressRule{}, fmt.Errorf("--allow %q: want host:port[,port]", s)
	}
	r := hosted.SandboxEgressRule{Host: s[:i]}
	for _, p := range strings.Split(s[i+1:], ",") {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return hosted.SandboxEgressRule{}, fmt.Errorf("--allow %q: port %q must be 1..65535", s, p)
		}
		r.Ports = append(r.Ports, n)
	}
	return r, nil
}

var sandboxSizeNames = map[string]bool{"small": true, "default": true, "large": true, "xlarge": true}

func parseSandboxArgs(args []string) (sandboxArgs, error) {
	usage := errors.New(progname.Rewrite(sandboxUsage))
	var a sandboxArgs
	if len(args) < 2 {
		return a, usage
	}
	a.Sub = args[1]
	if a.Sub == "ls" {
		a.Sub = "list"
	}
	rest := args[2:]
	var tail []string
	hasTail := false
	if i := indexOf(rest, "--"); i >= 0 {
		tail, hasTail = rest[i+1:], true
		rest = rest[:i]
	}
	var timeout, wait string
	rest, a.Name = extractStringFlag(rest, "--name")
	rest, a.Size = extractStringFlag(rest, "--size")
	rest, a.SSHKey = extractStringFlag(rest, "--ssh-key")
	rest, a.Cwd = extractStringFlag(rest, "--cwd")
	rest, a.Pane = extractStringFlag(rest, "--pane")
	var version string
	rest, version = extractStringFlag(rest, "--version")
	if version != "" {
		n, err := strconv.Atoi(version)
		if err != nil || n < 1 {
			return a, errors.New("--version must be a positive integer")
		}
		a.Version = n
	}
	rest, timeout = extractStringFlag(rest, "--timeout")
	rest, wait = extractStringFlag(rest, "--wait")
	rest, a.JSON = extractBoolFlag(rest, "--json")
	rest, a.Stdin = extractBoolFlag(rest, "--stdin")
	rest, a.SSH = extractBoolFlag(rest, "--ssh")
	rest, a.Control = extractBoolFlag(rest, "--control")
	var count string
	rest, count = extractStringFlag(rest, "--count")
	rest, a.Snapshot = extractStringFlag(rest, "--from-snapshot")
	rest, a.Environment = extractStringFlag(rest, "--environment")
	for _, f := range []struct {
		name string
		into *[]string
	}{{"--env", &a.EnvVars}, {"--secret", &a.SecretNames}, {"--allow", &a.Allow}} {
		var err error
		if rest, *f.into, err = extractRepeatedFlag(rest, f.name); err != nil {
			return a, err
		}
	}
	for _, kv := range a.EnvVars {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" {
			return a, fmt.Errorf("--env %q: want NAME=value", kv)
		}
	}
	for _, s := range a.SecretNames {
		if n, _, _ := strings.Cut(s, ":"); n == "" {
			return a, errors.New("--secret needs NAME or NAME:path")
		}
	}
	for _, s := range a.Allow {
		if _, err := parseAllow(s); err != nil {
			return a, err
		}
	}
	if (a.Environment != "" || len(a.EnvVars)+len(a.SecretNames)+len(a.Allow) > 0) && a.Sub != "create" {
		return a, usage
	}
	if a.Version != 0 && a.Sub != "env" {
		return a, usage
	}
	if count != "" {
		n, err := strconv.Atoi(count)
		if err != nil || n < 1 || n > 10 {
			return a, errors.New("--count must be 1..10")
		}
		a.Count = n
	}
	if a.Snapshot != "" && a.Sub != "create" {
		return a, usage
	}
	for _, f := range []struct {
		s   string
		dst *int
	}{{timeout, &a.Timeout}, {wait, &a.Wait}} {
		if f.s != "" {
			n, err := strconv.Atoi(f.s)
			if err != nil || n < 0 {
				return a, fmt.Errorf("not a number of seconds: %q", f.s)
			}
			*f.dst = n
		}
	}
	if a.Size != "" && !sandboxSizeNames[a.Size] {
		return a, errors.New("--size must be small, default, large or xlarge")
	}
	for _, r := range rest {
		if strings.HasPrefix(r, "-") {
			return a, usage
		}
	}
	needID := func() error {
		if len(rest) != 1 {
			return usage
		}
		a.ID = rest[0]
		return nil
	}
	switch a.Sub {
	case "create", "list":
		if len(rest) != 0 || hasTail || (a.SSH && a.SSHKey != "") || (a.SSH && a.Sub != "create") {
			return a, usage
		}
	case "ssh":
		if err := needID(); err != nil {
			return a, err
		}
		a.Argv = tail
	case "desktop":
		if err := needID(); err != nil || hasTail {
			return a, usage
		}
	case "fork":
		if err := needID(); err != nil || hasTail {
			return a, usage
		}
		if a.Count == 0 {
			a.Count = 1
		}
	case "env":
		if len(rest) == 0 || hasTail {
			return a, usage
		}
		a.EnvOp = rest[0]
		switch {
		case a.EnvOp == "ls" && len(rest) == 1 && a.Version == 0:
		case a.EnvOp == "get" && len(rest) == 2:
			a.Name = rest[1]
		case a.EnvOp == "put" && len(rest) == 3 && a.Version == 0:
			a.Name, a.Src = rest[1], rest[2]
		case a.EnvOp == "rm" && len(rest) == 2 && a.Version == 0:
			a.Name = rest[1]
		default:
			return a, usage
		}
	case "snapshot":
		if len(rest) == 0 || hasTail {
			return a, usage
		}
		a.SnapOp = rest[0]
		switch {
		case a.SnapOp == "ls" && len(rest) == 1:
		case a.SnapOp == "create" && len(rest) == 3:
			a.ID, a.Name = rest[1], rest[2]
		case a.SnapOp == "rm" && len(rest) == 2:
			a.Snapshot = rest[1]
		default:
			return a, usage
		}
	case "screenshot":
		if len(rest) != 2 || hasTail {
			return a, usage
		}
		a.ID, a.Dst = rest[0], rest[1]
	case "port":
		if len(rest) < 2 || hasTail {
			return a, usage
		}
		a.ID, a.PortOp = rest[0], rest[1]
		port := func(s string) error {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 || n > 65535 || n == 22 {
				return errors.New("port must be 1..65535 and not 22")
			}
			a.Port = n
			return nil
		}
		switch {
		case a.PortOp == "ls" && len(rest) == 2:
		case a.PortOp == "url" && len(rest) == 3:
			if err := port(rest[2]); err != nil {
				return a, err
			}
		case a.PortOp == "public" && len(rest) == 4 && (rest[3] == "on" || rest[3] == "off"):
			if err := port(rest[2]); err != nil {
				return a, err
			}
			a.Public = rest[3] == "on"
		default:
			return a, usage
		}
	case "get", "stop", "resume", "delete", "events", "interrupt":
		if err := needID(); err != nil || hasTail {
			return a, usage
		}
	case "exec":
		if err := needID(); err != nil || len(tail) == 0 {
			return a, usage
		}
		a.Argv = tail
	case "prompt", "steer":
		if err := needID(); err != nil {
			return a, err
		}
		a.Text = strings.Join(tail, " ")
		if strings.TrimSpace(a.Text) == "" {
			return a, usage
		}
	case "cp":
		if len(rest) != 2 || hasTail {
			return a, usage
		}
		a.Src, a.Dst = rest[0], rest[1]
		if _, _, _, _, err := cpDirection(a.Src, a.Dst); err != nil {
			return a, err
		}
	default:
		return a, usage
	}
	return a, nil
}

// cpDirection reads `cp` operands: exactly one is <id>:<path>.
func cpDirection(src, dst string) (up bool, id, remote, local string, err error) {
	split := func(s string) (string, string, bool) {
		i := strings.Index(s, ":")
		if i <= 0 || !strings.HasPrefix(s[i+1:], "/") {
			return "", "", false
		}
		return s[:i], s[i+1:], true
	}
	sid, sp, sok := split(src)
	did, dp, dok := split(dst)
	switch {
	case sok && !dok:
		return false, sid, sp, dst, nil
	case dok && !sok:
		return true, did, dp, src, nil
	}
	return false, "", "", "", errors.New("cp: exactly one side must be <sandbox id>:<absolute path>")
}

func runSandboxCmd(cfg config.Config, args []string) (int, error) {
	a, err := parseSandboxArgs(args)
	if err != nil {
		return 1, err
	}
	if a.Sub == "ssh" { // local only: the pinned config, no server call
		return sandboxDo(context.Background(), nil, a, os.Stdin, os.Stdout, os.Stderr)
	}
	conn := hosted.Conn{URL: cfg.Upstream.URL, APIKey: cfg.Upstream.APIKey, WorkspaceID: cfg.Upstream.Workspace}
	if err := conn.Validate(); err != nil {
		return 1, fmt.Errorf("sandbox: %w", err)
	}
	c := &hosted.Client{Conn: conn}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return sandboxDo(ctx, c, a, os.Stdin, os.Stdout, os.Stderr)
}

func printSandbox(w io.Writer, sb hosted.Sandbox, asJSON bool) {
	if asJSON {
		b, _ := json.Marshal(sb)
		fmt.Fprintln(w, string(b))
		return
	}
	fmt.Fprintf(w, "%s  %-10s %-8s %s\n", sb.ID, sb.State, sb.Size, sb.Name)
}

// sandboxDo runs one parsed command against c.
func sandboxDo(ctx context.Context, c *hosted.Client, a sandboxArgs, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	pane := func() (string, error) {
		if a.Pane != "" {
			return a.Pane, nil
		}
		ps, err := c.SandboxPanes(ctx, a.ID)
		if err != nil {
			return "", err
		}
		if len(ps) == 0 {
			return "", errors.New("the sandbox has no pane yet")
		}
		return ps[0].ID, nil
	}
	switch a.Sub {
	case "create":
		o := hosted.SandboxCreateOptions{Name: a.Name, Size: a.Size, FromSnapshot: a.Snapshot, Environment: a.Environment}
		if err := sandboxCreateEnv(&o, a, os.LookupEnv); err != nil {
			return 1, err
		}
		if a.SSHKey != "" {
			b, err := os.ReadFile(a.SSHKey)
			if err != nil {
				return 1, err
			}
			if strings.Contains(string(b), "PRIVATE KEY") {
				return 1, errors.New("--ssh-key must be a PUBLIC key file (.pub)")
			}
			o.SSHPublicKey = strings.TrimSpace(string(b))
		}
		var kp hosted.KeyPair
		if a.SSH {
			k, err := hosted.NewKeyPair()
			if err != nil {
				return 1, err
			}
			kp, o.SSHPublicKey = k, k.Public
		}
		sb, err := c.SandboxCreate(ctx, o)
		if err != nil {
			return 1, err
		}
		if a.SSH {
			if sb.SSH == nil {
				return 1, errors.New("the server returned no ssh endpoint for the sandbox")
			}
			run := hosted.Run{ID: sb.ID, WorkspaceID: c.WorkspaceID, Name: sb.Name, State: sb.State, Host: sb.SSH.Host, Port: sb.SSH.Port, HostKey: sb.SSH.HostKey}
			if _, err := sandboxStore().SaveAs(run, kp, sb.SSH.User); err != nil {
				return 1, fmt.Errorf("sandbox %s was created but its ssh key could not be saved: %w", sb.ID, err)
			}
		}
		printSandbox(stdout, sb, a.JSON)
	case "list":
		l, err := c.SandboxList(ctx)
		if err != nil {
			return 1, err
		}
		for _, sb := range l {
			printSandbox(stdout, sb, a.JSON)
		}
	case "get", "stop", "resume", "delete":
		f := map[string]func(context.Context, string) (hosted.Sandbox, error){
			"get": c.SandboxGet, "stop": c.SandboxStop, "resume": c.SandboxResume, "delete": c.SandboxDelete,
		}[a.Sub]
		sb, err := f(ctx, a.ID)
		if err != nil {
			return 1, err
		}
		printSandbox(stdout, sb, a.JSON)
	case "exec":
		r := hosted.SandboxExecRequest{Argv: a.Argv, Cwd: a.Cwd, TimeoutSeconds: a.Timeout}
		if a.Stdin {
			b, err := io.ReadAll(io.LimitReader(stdin, 1<<20+1))
			if err != nil || len(b) > 1<<20 {
				return 1, errors.New("exec: stdin is unreadable or over 1 MiB")
			}
			r.Stdin = string(b)
		}
		res, err := c.SandboxExec(ctx, a.ID, r)
		if err != nil {
			return 1, err
		}
		fmt.Fprint(stdout, res.Stdout)
		fmt.Fprint(stderr, res.Stderr)
		if res.Truncated {
			fmt.Fprintln(stderr, "rysh sandbox exec: output truncated at 1 MiB")
		}
		return res.ExitCode, nil
	case "cp":
		up, id, remote, local, _ := cpDirection(a.Src, a.Dst)
		if up {
			b, err := os.ReadFile(local)
			if err != nil {
				return 1, err
			}
			return boolCode(c.SandboxPutFile(ctx, id, remote, b))
		}
		b, err := c.SandboxGetFile(ctx, id, remote)
		if err != nil {
			return 1, err
		}
		return boolCode(os.WriteFile(local, b, 0o600))
	case "prompt":
		p, err := pane()
		if err != nil {
			return 1, err
		}
		res, err := c.SandboxPrompt(ctx, a.ID, p, a.Text, a.Wait)
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(stdout, res.Output)
		if res.Status != "done" {
			fmt.Fprintf(stderr, "rysh sandbox prompt: %s %s\n", res.Status, res.Detail)
			return 1, nil
		}
	case "steer":
		p, err := pane()
		if err != nil {
			return 1, err
		}
		return boolCode(c.SandboxSteer(ctx, a.ID, p, a.Text))
	case "interrupt":
		p, err := pane()
		if err != nil {
			return 1, err
		}
		return boolCode(c.SandboxInterrupt(ctx, a.ID, p))
	case "ssh":
		_, p, err := sandboxStore().Load(a.ID)
		if err != nil {
			return 1, fmt.Errorf("no saved ssh key for sandbox %s (create it with --ssh): %w", a.ID, err)
		}
		argv := []string{"ssh", "-F", p.SSHConfig, hosted.Alias(a.ID)}
		if len(a.Argv) > 0 {
			argv = append(append(argv, "--"), a.Argv...)
		}
		return sandboxRunSSH(argv)
	case "port":
		switch a.PortOp {
		case "ls":
			ps, err := c.SandboxPorts(ctx, a.ID)
			if err != nil {
				return 1, err
			}
			for _, p := range ps {
				state := "private"
				if p.Public {
					state = "public"
				}
				fmt.Fprintf(stdout, "%d\t%s\n", p.Port, state)
			}
		case "public":
			if _, err := c.SandboxSetPortPublic(ctx, a.ID, a.Port, a.Public); err != nil {
				return 1, err
			}
		case "url":
			u, err := c.SandboxPortURL(ctx, a.ID, a.Port)
			if err != nil {
				return 1, err
			}
			fmt.Fprintln(stdout, u.URL)
			fmt.Fprintf(stderr, "rysh sandbox port: the url works until %s\n", u.ExpiresAt.Local().Format("15:04"))
		}
	case "desktop":
		mode := "watch"
		if a.Control {
			mode = "control"
		}
		u, err := c.SandboxDesktopURL(ctx, a.ID, mode)
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(stdout, u)
		fmt.Fprintln(stderr, "rysh sandbox desktop: open it in a browser; the link works once and expires")
	case "fork":
		forks, err := c.SandboxFork(ctx, a.ID, a.Count, "")
		if err != nil {
			return 1, err
		}
		for _, sb := range forks {
			printSandbox(stdout, sb, a.JSON)
		}
	case "env":
		switch a.EnvOp {
		case "ls":
			l, err := c.SandboxEnvironments(ctx)
			if err != nil {
				return 1, err
			}
			for _, e := range l {
				fmt.Fprintf(stdout, "%s  v%d  %d var(s), %d secret name(s)\n", e.Name, e.Version, len(e.Vars), len(e.Secrets))
			}
		case "get":
			e, err := c.SandboxEnvironment(ctx, a.Name, a.Version)
			if err != nil {
				return 1, err
			}
			b, _ := json.MarshalIndent(e, "", "  ")
			fmt.Fprintln(stdout, string(b))
		case "put":
			body, err := os.ReadFile(a.Src)
			if err != nil {
				return 1, err
			}
			e, err := c.SandboxEnvironmentPut(ctx, a.Name, body)
			if err != nil {
				return 1, err
			}
			fmt.Fprintf(stdout, "%s  v%d saved\n", e.Name, e.Version)
		case "rm":
			return boolCode(c.SandboxEnvironmentDelete(ctx, a.Name))
		}
	case "snapshot":
		switch a.SnapOp {
		case "create":
			sn, err := c.SandboxSnapshot(ctx, a.ID, a.Name)
			if err != nil {
				return 1, err
			}
			fmt.Fprintf(stdout, "%s  %s  (from %s)\n", sn.ID, sn.Name, sn.SourceSandboxID)
		case "ls":
			l, err := c.SandboxSnapshots(ctx)
			if err != nil {
				return 1, err
			}
			for _, sn := range l {
				fmt.Fprintf(stdout, "%s  %s  (from %s)\n", sn.ID, sn.Name, sn.SourceSandboxID)
			}
		case "rm":
			return boolCode(c.SandboxDeleteSnapshot(ctx, a.Snapshot))
		}
	case "screenshot":
		png, err := c.SandboxScreenshot(ctx, a.ID)
		if err != nil {
			return 1, err
		}
		return boolCode(os.WriteFile(a.Dst, png, 0o600))
	case "events":
		err := c.SandboxEvents(ctx, a.ID, func(e hosted.Event) error {
			fmt.Fprintf(stdout, "%s %s\n", e.Name, e.Data)
			return nil
		})
		if err != nil && ctx.Err() == nil {
			return 1, err
		}
	}
	return 0, nil
}

// sandboxStore keeps each sandbox's ssh key and pinned host key
// (RYSH_SANDBOX_DIR, default ~/.rysh/sandboxes).
func sandboxStore() hosted.Store {
	if d := os.Getenv("RYSH_SANDBOX_DIR"); d != "" {
		return hosted.Store{Dir: d}
	}
	home, _ := os.UserHomeDir()
	return hosted.Store{Dir: filepath.Join(home, ".rysh", "sandboxes")}
}

// sandboxRunSSH runs ssh with the terminal attached (tests replace it).
var sandboxRunSSH = execSSH

func execSSH(argv []string) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

func boolCode(err error) (int, error) {
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// sandboxCreateEnv fills a create's variables, secrets and egress. A secret's
// value comes from the environment variable of the same name (never argv, so
// it stays out of shell history and process lists); unset is an error.
func sandboxCreateEnv(o *hosted.SandboxCreateOptions, a sandboxArgs, lookup func(string) (string, bool)) error {
	for _, kv := range a.EnvVars {
		k, v, _ := strings.Cut(kv, "=")
		if o.Env == nil {
			o.Env = map[string]string{}
		}
		o.Env[k] = v
	}
	for _, s := range a.SecretNames {
		name, path, _ := strings.Cut(s, ":")
		v, ok := lookup(name)
		if !ok {
			return fmt.Errorf("--secret %s: the environment variable %s is not set (rysh reads the value from it)", name, name)
		}
		o.Secrets = append(o.Secrets, hosted.SandboxSecret{Name: name, Value: v, Path: path})
	}
	if len(a.Allow) > 0 {
		o.Egress = &hosted.SandboxEgress{Mode: "allowlist"}
		for _, s := range a.Allow {
			r, _ := parseAllow(s) // checked at parse
			o.Egress.Rules = append(o.Egress.Rules, r)
		}
	}
	return nil
}
