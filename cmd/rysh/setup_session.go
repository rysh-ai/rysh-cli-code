// SPDX-License-Identifier: Apache-2.0

package main

// setup_session.go — which session `rysh setup` / `rysh assistant` activate
// the assistant in, and the check that it really is this directory's.
//
// Every daemon embeds NATS on port 24242 unless configured otherwise, and a
// daemon that finds the port taken joins that server as a guest (design 028
// §9 limit 7). Subjects are prefixed by the session NAME only. So two
// directories that both run a session called "default" share one set of
// subjects, and a `##humanoid spawn` sent to "default" is answered by
// whichever daemon replies first — in the founder's live test (2026-10-27)
// another project's, which then looked for the skill file under its own
// working directory. A rysh pane makes it worse: it exports RYSH_SESSION as
// its own session's name, so setup run inside one targeted that session.
//
// Two rules fix it:
//   - the name: a fresh directory gets a session of its own, derived from its
//     path and saved as rysh.session_name (resolveAssistantSession);
//   - the bus: before activating, setup asks the bus how many daemons answer
//     for the name and refuses unless it is exactly this directory's own
//     (checkSessionOwnership).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"gopkg.in/yaml.v3"

	"github.com/rysh-ai/rysh-cli-code/internal/bustoken"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// sessionProbeWindow is how long the bus probe collects answers. Workspace
// actors answer a layout-only snapshot in milliseconds.
const sessionProbeWindow = 500 * time.Millisecond

// assistantBusProbe counts the daemons answering for a session name on the
// bus this project's daemon uses. Tests replace it.
var assistantBusProbe = func(cfg config.Config, name string) (int, error) {
	return sessionBusResponders(cfg, name, sessionProbeWindow)
}

// derivedSessionName is the session a directory gets when nothing names one:
// its basename as a NATS-safe slug plus 6 hex of its resolved absolute path,
// so two projects that share a basename never share a session.
func derivedSessionName(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	var b strings.Builder
	for _, r := range strings.ToLower(filepath.Base(dir)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 32 {
		slug = strings.Trim(slug[:32], "-")
	}
	if slug == "" {
		slug = "project"
	}
	sum := sha256.Sum256([]byte(dir))
	return slug + "-" + hex.EncodeToString(sum[:3])
}

// validSessionName refuses a name that is not one NATS subject token.
func validSessionName(name string) error {
	if name == "" || strings.ContainsAny(name, ". \t*>/\\") {
		return fmt.Errorf("session name %q must be one word without '.', spaces, '*', '>' or slashes", name)
	}
	return nil
}

// fileSessionNames reads the session name a config file sets under rysh:, and
// a top-level session_name the loader ignores (where it does not belong).
func fileSessionNames(path string) (underRysh, topLevel string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	var doc struct {
		Rysh struct {
			SessionName string `yaml:"session_name"`
		} `yaml:"rysh"`
		SessionName string `yaml:"session_name"`
	}
	if yaml.Unmarshal(raw, &doc) != nil {
		return "", ""
	}
	return strings.TrimSpace(doc.Rysh.SessionName), strings.TrimSpace(doc.SessionName)
}

// liveHere reports whether session name has a live daemon recorded in this
// project's store whose project root is dir.
func liveHere(store *session.Store, name, dir string) bool {
	if store == nil || !sessionRecordLive(store, name) {
		return false
	}
	rec, err := store.Get(name)
	return err == nil && samePath(rec.Path, dir)
}

func samePath(a, b string) bool {
	clean := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		return filepath.Clean(p)
	}
	return a != "" && b != "" && clean(a) == clean(b)
}

// resolveAssistantSession picks the session the assistant runs in and returns
// cfg with SessionName set to it. In order:
//
//  1. --session <name>;
//  2. RYSH_SESSION, when it is the user's (not exported by a rysh pane);
//  3. rysh.session_name in this directory's config;
//  4. the pane's RYSH_SESSION, only when that session is live in THIS dir;
//  5. "default", only when it is live in this dir (the user's open session);
//  6. otherwise a name derived from the directory, saved as rysh.session_name
//     so later `rysh` runs here open the same session.
//
// A pane's RYSH_SESSION names the session the pane belongs to — usually
// another project's — so it is never adopted for a directory it is not in.
func resolveAssistantSession(cfg config.Config, configPath string, args []string, dir string) (config.Config, error) {
	cfgFile := config.ConfigPath(configPath)
	fromFile, topLevel := fileSessionNames(cfgFile)
	if topLevel != "" && fromFile == "" {
		fmt.Printf("WARN %s has a top-level session_name, which rysh ignores; it belongs under rysh:\n  rysh:\n    session_name: %s\n", cfgFile, topLevel)
	}
	store, _ := session.NewStore(cfg)
	envName := strings.TrimSpace(os.Getenv("RYSH_SESSION"))
	inPane := strings.TrimSpace(os.Getenv("RYSH_PANE")) != ""

	name, how := "", ""
	switch {
	case flagVal(args, "--session") != "":
		name, how = strings.TrimSpace(flagVal(args, "--session")), "--session"
	case envName != "" && !inPane:
		name, how = envName, "RYSH_SESSION"
	case fromFile != "":
		name, how = fromFile, cfgFile
	case envName != "" && liveHere(store, envName, dir):
		name, how = envName, "the rysh pane this runs in"
	case liveHere(store, "default", dir):
		name, how = "default", "the session already running here"
	}
	if name != "" {
		if err := validSessionName(name); err != nil {
			return cfg, err
		}
		if envName != "" && inPane && name != envName {
			fmt.Printf("note: not using RYSH_SESSION=%q — it is the session of the rysh pane this runs in, not of %s\n", envName, dir)
		}
		fmt.Printf("session: %s (from %s)\n", name, how)
		cfg.SessionName = name
		return cfg, nil
	}

	name = derivedSessionName(dir)
	if envName != "" && inPane {
		fmt.Printf("note: not using RYSH_SESSION=%q — it is the session of the rysh pane this runs in, not of %s\n", envName, dir)
	}
	if err := config.SetSessionName(cfgFile, name); err != nil {
		return cfg, fmt.Errorf("save the session name %q to %s: %w", name, cfgFile, err)
	}
	fmt.Printf("session: %s (this directory's own; saved as rysh.session_name in %s)\n", name, cfgFile)
	cfg.SessionName = name
	// D-55 B: a new project also gets a bus port of its own.
	return assignProjectBusPort(cfg, cfgFile, dir, store), nil
}

// checkSessionOwnership refuses to activate the assistant in a session that
// is not unambiguously this directory's: a name another directory's daemon
// answers on the shared bus, a live record for another root, or two daemons
// answering at once. ours reports whether the live daemon is this dir's.
func checkSessionOwnership(cfg config.Config, store *session.Store, name, dir string) (ours bool, err error) {
	own := derivedSessionName(dir)
	if name == own {
		own = "<a name of your own>"
	}
	fix := progname.Rewrite("rysh "+setupCommand+" --session ") + own
	if store != nil && sessionRecordLive(store, name) {
		if rec, gerr := store.Get(name); gerr == nil && !samePath(rec.Path, dir) {
			return false, fmt.Errorf("session %q is live for another directory (%s), not %s — setup will not activate the assistant there; give this directory its own session: %s", name, rec.Path, dir, fix)
		}
		ours = true
	}
	n, perr := assistantBusProbe(cfg, name)
	if perr != nil {
		return ours, fmt.Errorf("could not check who serves session %q on the bus: %w", name, perr)
	}
	switch {
	case !ours && n > 0:
		return false, fmt.Errorf("session %q is already served on this machine's rysh bus by another directory's daemon (%d answered) — setup will not activate the assistant there; give this directory its own session: %s", name, n, fix)
	case ours && n > 1:
		return true, fmt.Errorf("session %q is answered by %d daemons on this machine's rysh bus — this directory's and another's with the same name, so commands would reach either; give this directory its own session: %s", name, n, fix)
	}
	return ours, nil
}

// sessionBusResponders counts the daemons that answer a layout-only snapshot
// request for session name on the NATS server this project's daemon would
// use, within window. No server listening, or a private (kernel-assigned)
// port, means no one else can be on it: 0.
func sessionBusResponders(cfg config.Config, name string, window time.Duration) (int, error) {
	url := ""
	switch {
	case cfg.NATS.Mode == "external":
		url = cfg.NATS.URL
	case cfg.NATS.Port > 0:
		url = fmt.Sprintf("nats://127.0.0.1:%d", cfg.NATS.Port)
	default:
		return 0, nil
	}
	tokOpts := []nats.Option{nats.Timeout(time.Second), nats.MaxReconnects(0)} // + the bus token below
	if cfg.NATS.Mode != "external" {
		tokOpt, err := bustoken.Option(cfg.NATS.Port)
		if err != nil {
			return 0, bustoken.Missing(cfg.NATS.Port, err)
		}
		tokOpts = append(tokOpts, tokOpt)
	}
	nc, err := nats.Connect(url, tokOpts...)
	if bustoken.IsAuthError(err) {
		// A server is there but refuses this process's token: never read
		// that as a free port (gap01 slice 3).
		return 0, bustoken.Missing(cfg.NATS.Port, err)
	}
	if err != nil {
		return 0, nil // nothing listening: no daemon of any session is on it
	}
	defer nc.Close()

	codecs := msg.DefaultCodecRegistry()
	req := &msg.MsgGetWorkspaceSnapshot{LayoutOnly: true, NoHistories: true}
	payload, err := json.Marshal(req)
	if err != nil {
		return 0, err
	}
	inbox := nats.NewInbox()
	data, err := json.Marshal(msg.NATSEnvelope{TypeTag: codecs.TagOf(req), ReplyTo: inbox, Payload: payload})
	if err != nil {
		return 0, err
	}
	sub, err := nc.SubscribeSync(inbox)
	if err != nil {
		return 0, err
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := nc.Publish(name+".ws.snapshot", data); err != nil {
		return 0, err
	}
	n := 0
	deadline := time.Now().Add(window)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return n, nil
		}
		if _, err := sub.NextMsg(left); err != nil {
			return n, nil
		}
		n++
	}
}
