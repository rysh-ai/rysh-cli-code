// SPDX-License-Identifier: Apache-2.0

package main

// `rysh heymates` — rysh-family from the terminal (design 093 §6).
//
// rysh-family shows a fleet of AI agents as a family of avatars (partner,
// parent, colleague, work, research, gigs). In the MVP every avatar is a
// server-side LLM pane inside rysh-server, reached through /api/family
// (rysh-family docs/API.md). This command is a client of that API: it lists
// and shows families, chats with one avatar (streaming the reply from the
// family socket), and watches a family's event stream.
//
// Server and credentials, in order:
//
//	server: --server URL > $RYSH_SERVER_URL > the server of the last
//	        `rysh heymates login` > [upstream] url in rysh.config.yaml
//	token:  --token-env NAME (the token is read from that environment
//	        variable; never refreshed) > the session `rysh heymates login`
//	        saved for that server in <rysh dir>/family/auth.json (0600),
//	        refreshed on 401 via /api/auth/refresh and saved back.
//
// The [upstream] api_key is NOT sent: it is a workspace key, and the family
// routes accept only a user's JWT. A token is never accepted as a literal
// flag value — argv lands in shell history and process listings.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/familyclient"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
)

const familyUsage = "usage: rysh heymates ls [--json]                       families, avatars, live status\n" +
	"       rysh heymates show <family> [--json]             avatars with status/pct/note, open tasks\n" +
	"       rysh heymates chat <family> <avatar> [--timeout 120s] [-- <message>]\n" +
	"                        with a message: send it, stream the reply, exit on reply_done;\n" +
	"                        without one: a line-mode chat (Ctrl-D exits)\n" +
	"       rysh heymates watch <family> [--json]            one line per event until Ctrl-C\n" +
	"       rysh heymates login [--server <url>] [--email <e>]   sign in; the password is read without echo\n" +
	"       rysh heymates logout [--server <url>]\n" +
	"       rysh heymates serve [--session <name>]          inside a hosted family container: the\n" +
	"                        server's bridge to the session (JSON lines on stdin/stdout)\n" +
	"       rysh heymates apply [--session <name>]          inside a hosted family container: make the\n" +
	"                        session match the manifest on stdin; prints one JSON result\n" +
	"       every command takes --server <url> and --token-env <NAME> (the token is read\n" +
	"       from that environment variable; a token is never accepted on the command line).\n" +
	"       <family> and <avatar> are an id, an id prefix, or a case-insensitive name."

const defaultFamilyTimeout = 120 * time.Second

// familyCLI holds everything `rysh heymates` touches, so tests can drive it
// with a fake server, buffers and a temporary rysh dir.
type familyCLI struct {
	cfg          config.Config
	stdin        io.Reader
	stdout       io.Writer
	stderr       io.Writer
	getenv       func(string) string
	readPassword func() (string, error)
	stdinIsTTY   bool
	// follow tunes the socket (tests shorten the backoff).
	follow familyclient.FollowOptions
}

func runFamilyCmd(cfg config.Config, args []string) error {
	tty := term.IsTerminal(int(os.Stdin.Fd()))
	f := &familyCLI{
		cfg:        cfg,
		stdin:      os.Stdin,
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		getenv:     os.Getenv,
		stdinIsTTY: tty,
		readPassword: func() (string, error) {
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			return string(b), err
		},
	}
	return f.run(args)
}

// familyArgs is one parsed invocation.
type familyArgs struct {
	pos      []string
	message  string // everything after "--"
	hasMsg   bool
	server   string
	tokenEnv string
	email    string
	timeout  time.Duration
	json     bool
}

func parseFamilyArgs(args []string) (familyArgs, error) {
	a := familyArgs{timeout: defaultFamilyTimeout}
	value := func(i int, name string) (string, error) {
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
			return "", fmt.Errorf("%s needs a value", name)
		}
		return args[i+1], nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			a.hasMsg = true
			a.message = strings.Join(args[i+1:], " ")
			break
		}
		name, inline, hasInline := strings.Cut(arg, "=")
		if !strings.HasPrefix(name, "--") {
			hasInline = false
			name = arg
		}
		get := func() (string, error) {
			if hasInline {
				return inline, nil
			}
			v, err := value(i, name)
			if err == nil {
				i++
			}
			return v, err
		}
		var err error
		switch name {
		case "--token", "--access-token", "--refresh-token", "--password":
			return a, fmt.Errorf("%s is not accepted: a secret on the command line lands in shell history and process listings; put the token in an environment variable and pass --token-env <NAME>", name)
		case "--server":
			a.server, err = get()
		case "--token-env":
			a.tokenEnv, err = get()
		case "--email":
			a.email, err = get()
		case "--timeout":
			var v string
			if v, err = get(); err == nil {
				a.timeout, err = parseFamilyTimeout(v)
			}
		case "--json":
			a.json = true
		case "-h", "--help":
			return a, errors.New(progname.Rewrite(familyUsage))
		default:
			if strings.HasPrefix(arg, "--") {
				return a, fmt.Errorf("unknown flag %s\n%s", arg, progname.Rewrite(familyUsage))
			}
			a.pos = append(a.pos, arg)
		}
		if err != nil {
			return a, err
		}
	}
	return a, nil
}

func parseFamilyTimeout(v string) (time.Duration, error) {
	if n, err := strconv.Atoi(v); err == nil {
		v = strconv.Itoa(n) + "s"
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("--timeout %q: want a positive duration like 120s or 5m", v)
	}
	return d, nil
}

func (f *familyCLI) run(args []string) error {
	// args[0] == "family".
	rest := args[1:]
	if len(rest) == 0 {
		return errors.New(progname.Rewrite(familyUsage))
	}
	sub := rest[0]
	if sub == "serve" {
		// The in-container bridge (design 093 H3): a different kind of
		// command — it talks to the local session, not to rysh-server — with
		// its own flags. See family_serve_cmd.go.
		return runFamilyServe(f.cfg, rest[1:])
	}
	if sub == "apply" {
		// The in-container builder (design 093 H2), also a local-session
		// command. See family_apply_cmd.go.
		return runFamilyApply(f.cfg, rest[1:])
	}
	a, err := parseFamilyArgs(rest[1:])
	if err != nil {
		return err
	}
	switch sub {
	case "ls", "list":
		return f.cmdList(a)
	case "show":
		return f.cmdShow(a)
	case "chat":
		return f.cmdChat(a)
	case "watch":
		return f.cmdWatch(a)
	case "login":
		return f.cmdLogin(a)
	case "logout":
		return f.cmdLogout(a)
	case "help", "-h", "--help":
		fmt.Fprintln(f.stdout, progname.Rewrite(familyUsage))
		return nil
	}
	return fmt.Errorf("unknown heymates command %q\n%s", sub, progname.Rewrite(familyUsage))
}

func (f *familyCLI) store() *familyclient.TokenStore {
	return familyclient.DefaultTokenStore(f.cfg.RyshDir)
}

// resolveServer picks the server URL (see the file comment for the order).
// Whichever source wins, the URL is checked (familyclient.ValidateServer)
// before anything is sent to it.
func (f *familyCLI) resolveServer(flag string) (string, error) {
	s, err := f.pickServer(flag)
	if err != nil {
		return "", err
	}
	if err := familyclient.ValidateServer(s); err != nil {
		return "", err
	}
	return s, nil
}

func (f *familyCLI) pickServer(flag string) (string, error) {
	if s := familyclient.NormalizeServer(flag); s != "" {
		return s, nil
	}
	if s := familyclient.NormalizeServer(f.getenv("RYSH_SERVER_URL")); s != "" {
		return s, nil
	}
	if s, err := f.store().DefaultServer(); err != nil {
		return "", err
	} else if s != "" {
		return s, nil
	}
	if s := familyclient.NormalizeServer(f.cfg.Upstream.URL); s != "" {
		return s, nil
	}
	return "", errors.New(progname.Rewrite("no rysh server: pass --server <url>, set RYSH_SERVER_URL, or run `rysh heymates login --server <url>`"))
}

// client builds an authenticated client for the resolved server.
func (f *familyCLI) client(a familyArgs) (*familyclient.Client, error) {
	server, err := f.resolveServer(a.server)
	if err != nil {
		return nil, err
	}
	if a.tokenEnv != "" {
		tok := strings.TrimSpace(f.getenv(a.tokenEnv))
		if tok == "" {
			return nil, fmt.Errorf("--token-env %s: that environment variable is not set or is empty", a.tokenEnv)
		}
		// The server may have come from a fallback (env, saved default,
		// [upstream] url): say where this token is going.
		fmt.Fprintf(f.stderr, "using the token in %s for %s\n", a.tokenEnv, familyclient.ServerHost(server))
		return familyclient.New(server, familyclient.NewAuth(tok, "")), nil
	}
	st := f.store()
	sess, err := st.Get(server)
	if err != nil {
		return nil, err
	}
	if sess == nil || sess.AccessToken == "" {
		return nil, fmt.Errorf(progname.Rewrite("not signed in to %s: run `rysh heymates login --server %s`, or pass --token-env <NAME>"), server, server)
	}
	auth := familyclient.NewAuth(sess.AccessToken, sess.RefreshToken)
	auth.OnRefresh = func(t familyclient.Tokens) error { return st.UpdateTokens(server, t) }
	return familyclient.New(server, auth), nil
}

func (f *familyCLI) writeJSON(v any) error {
	enc := json.NewEncoder(f.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (f *familyCLI) cmdList(a familyArgs) error {
	if len(a.pos) != 0 {
		return errors.New(progname.Rewrite("usage: rysh heymates ls [--json]"))
	}
	c, err := f.client(a)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fams, err := c.Families(ctx)
	if err != nil {
		return err
	}
	if a.json {
		if fams == nil {
			fams = []familyclient.Family{}
		}
		return f.writeJSON(fams)
	}
	familyclient.FormatFamilies(f.stdout, fams)
	return nil
}

// findFamily resolves a family query against the user's families.
func findFamily(ctx context.Context, c *familyclient.Client, query string) (familyclient.Family, error) {
	fams, err := c.Families(ctx)
	if err != nil {
		return familyclient.Family{}, err
	}
	return familyclient.ResolveFamily(fams, query)
}

func (f *familyCLI) cmdShow(a familyArgs) error {
	if len(a.pos) != 1 {
		return errors.New(progname.Rewrite("usage: rysh heymates show <family> [--json]"))
	}
	c, err := f.client(a)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fam, err := findFamily(ctx, c, a.pos[0])
	if err != nil {
		return err
	}
	tasks, err := c.Tasks(ctx, fam.ID)
	if err != nil {
		return err
	}
	if a.json {
		if tasks == nil {
			tasks = []familyclient.Task{}
		}
		return f.writeJSON(map[string]any{"family": fam, "tasks": tasks})
	}
	familyclient.FormatFamily(f.stdout, fam, tasks)
	return nil
}

func (f *familyCLI) cmdChat(a familyArgs) error {
	pos := a.pos
	if len(pos) < 2 {
		return errors.New(progname.Rewrite("usage: rysh heymates chat <family> <avatar> [--timeout 120s] [-- <message>]"))
	}
	message := a.message
	hasMsg := a.hasMsg
	if len(pos) > 2 { // `chat home mia hello there` without "--"
		message = strings.TrimSpace(strings.Join(pos[2:], " ") + " " + message)
		hasMsg = true
	}
	if hasMsg && strings.TrimSpace(message) == "" {
		return errors.New("empty message")
	}
	c, err := f.client(a)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	fam, err := findFamily(ctx, c, pos[0])
	if err == nil {
		var av familyclient.Avatar
		if av, err = familyclient.ResolveAvatar(fam, pos[1]); err == nil {
			cancel()
			return f.chat(c, fam, av, message, hasMsg, a.timeout)
		}
	}
	cancel()
	return err
}

func (f *familyCLI) chat(c *familyclient.Client, fam familyclient.Family, av familyclient.Avatar, message string, oneShot bool, timeout time.Duration) error {
	opts := f.follow
	opts.OnDisconnect = func(err error, retry time.Duration) {
		fmt.Fprintf(f.stderr, "[family socket lost (%v); reconnecting in %s]\n", err, retry.Round(time.Millisecond))
	}
	cctx, ccancel := context.WithTimeout(context.Background(), minDuration(timeout, 30*time.Second))
	ch, err := c.OpenChat(cctx, fam, opts)
	ccancel()
	if err != nil {
		return err
	}
	defer ch.Close()

	ask := func(text string) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		_, err := ch.Ask(ctx, av.ID, text, f.stdout, f.stderr)
		if errors.Is(err, familyclient.ErrReplyTimeout) {
			return fmt.Errorf("no reply from %s within %s (raise --timeout)", av.Name, timeout)
		}
		return err
	}
	if oneShot {
		return ask(message)
	}

	if f.stdinIsTTY {
		fmt.Fprintf(f.stderr, "chatting with %s (%s) in %s; Ctrl-D to leave\n", av.Name, av.Kind, fam.Name)
	}
	sc := bufio.NewScanner(f.stdin)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for {
		if f.stdinIsTTY {
			fmt.Fprint(f.stderr, "you> ")
		}
		if !sc.Scan() {
			if f.stdinIsTTY {
				fmt.Fprintln(f.stderr)
			}
			return sc.Err()
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if f.stdinIsTTY {
			fmt.Fprintf(f.stdout, "%s> ", av.Name)
		}
		if err := ask(line); err != nil {
			// One failed turn does not end the conversation; a dead socket does.
			fmt.Fprintf(f.stderr, "rysh heymates: %v\n", err)
			if errors.Is(err, familyclient.ErrSocketGone) {
				return err
			}
		}
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (f *familyCLI) cmdWatch(a familyArgs) error {
	if len(a.pos) != 1 {
		return errors.New(progname.Rewrite("usage: rysh heymates watch <family> [--json]"))
	}
	c, err := f.client(a)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return f.watch(ctx, c, a)
}

func (f *familyCLI) watch(ctx context.Context, c *familyclient.Client, a familyArgs) error {
	lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	fam, err := findFamily(lctx, c, a.pos[0])
	cancel()
	if err != nil {
		return err
	}
	fm := familyclient.NewEventFormatter(fam)
	fm.Clock = func(t time.Time) string { return t.Format("15:04:05") }
	opts := f.follow
	opts.OnConnect = func(n int) {
		if n > 1 {
			fmt.Fprintln(f.stderr, "[family socket reconnected]")
		}
	}
	opts.OnDisconnect = func(err error, retry time.Duration) {
		fmt.Fprintf(f.stderr, "[family socket lost (%v); reconnecting in %s]\n", err, retry.Round(time.Millisecond))
	}
	enc := json.NewEncoder(f.stdout)
	err = c.Follow(ctx, fam.ID, opts, func(ev familyclient.Event) {
		if a.json {
			if ev.Type != "delta" {
				_ = enc.Encode(ev)
			}
			return
		}
		if line := fm.Line(ev); line != "" {
			fmt.Fprintln(f.stdout, line)
		}
	})
	if errors.Is(err, context.Canceled) {
		return nil // Ctrl-C is the normal way out
	}
	return err
}

func (f *familyCLI) cmdLogin(a familyArgs) error {
	if len(a.pos) != 0 {
		return errors.New(progname.Rewrite("usage: rysh heymates login [--server <url>] [--email <e>]"))
	}
	if a.tokenEnv != "" {
		return errors.New("--token-env needs no login; it is used as-is on each command")
	}
	server, err := f.resolveServer(a.server)
	if err != nil {
		return err
	}
	in := bufio.NewReader(f.stdin)
	email := strings.TrimSpace(a.email)
	if email == "" {
		fmt.Fprintf(f.stderr, "email for %s: ", server)
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return errors.New("no email given")
		}
		email = strings.TrimSpace(line)
	}
	if email == "" {
		return errors.New("no email given")
	}
	fmt.Fprintf(f.stderr, "password for %s: ", familyclient.ServerHost(server))
	var password string
	if f.stdinIsTTY && f.readPassword != nil {
		password, err = f.readPassword()
		fmt.Fprintln(f.stderr)
		if err != nil {
			return fmt.Errorf("reading the password: %w", err)
		}
	} else {
		// Piped stdin (scripts, tests): one line, never echoed by us.
		line, rerr := in.ReadString('\n')
		if rerr != nil && line == "" {
			return errors.New("no password given")
		}
		password = strings.TrimRight(line, "\r\n")
		fmt.Fprintln(f.stderr)
	}
	if password == "" {
		return errors.New("no password given")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t, err := familyclient.New(server, nil).Login(ctx, email, password)
	if err != nil {
		return err
	}
	if err := f.store().Put(server, familyclient.StoredSession{
		Email: email, AccessToken: t.AccessToken, RefreshToken: t.RefreshToken,
	}); err != nil {
		return fmt.Errorf("signed in, but saving the session failed: %w", err)
	}
	fmt.Fprintf(f.stdout, "signed in to %s as %s (session saved to %s)\n", server, email, f.store().Path)
	return nil
}

func (f *familyCLI) cmdLogout(a familyArgs) error {
	server, err := f.resolveServer(a.server)
	if err != nil {
		return err
	}
	had, err := f.store().Delete(server)
	if err != nil {
		return err
	}
	if had {
		fmt.Fprintf(f.stdout, "signed out of %s\n", server)
	} else {
		fmt.Fprintf(f.stdout, "no saved session for %s\n", server)
	}
	return nil
}
