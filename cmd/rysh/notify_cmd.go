// SPDX-License-Identifier: Apache-2.0

package main

// gap02 (G6): the daemon-side "needs you" notifier (internal/needsyou) and
// `rysh notify test`.
//
// The notifier is what makes "start it on another machine, close the laptop"
// safe: a session with no UI attached tells you, through a webhook or a
// command you configured, when an agent in it is waiting on you. It runs in
// the daemon whenever `notify: needs_you:` names a sink, and does nothing
// otherwise.
//
//	rysh notify test   — send one synthetic alert through the configured sinks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/needsyou"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

const notifyUsage = "usage: rysh notify test"

// buildNeedsYouSinks turns the config into sinks. The webhook's ${NAME}
// references are resolved through .rysh/secrets and the environment; an error
// names a missing reference, never the URL. session is the connection the
// assistant sink asks its humanoid through; it is needed only when
// notify.needs_you.assistant is set.
func buildNeedsYouSinks(cfg config.Config, ryshDir string, session needsyou.Requester) ([]needsyou.Sink, error) {
	n := cfg.NotifyNeedsYou
	var sinks []needsyou.Sink
	if w := strings.TrimSpace(n.Webhook); w != "" {
		expanded, missing := config.ExpandEnvRefs(w, func(name string) (string, bool) {
			return config.LookupSecretRef(ryshDir, name)
		})
		if len(missing) > 0 {
			return nil, fmt.Errorf("notify.needs_you.webhook: unresolved reference(s) %s (set them with ##secret or in the environment)",
				strings.Join(missing, ", "))
		}
		u, err := url.Parse(expanded)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, errors.New("notify.needs_you.webhook: not an http(s) URL")
		}
		sinks = append(sinks, needsyou.NewWebhookSink(expanded))
	}
	if len(n.Command) > 0 {
		sinks = append(sinks, needsyou.NewCommandSink(n.Command))
	}
	if n.Assistant != "" {
		if session == nil {
			return nil, errors.New("notify.needs_you.assistant: no connection to the session")
		}
		sinks = append(sinks, needsyou.NewAssistantSink(session, n.Assistant))
	}
	return sinks, nil
}

// notifyHost names this machine in an alert, so a phone can tell hosts apart.
func notifyHost() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// startNeedsYouNotifier starts the notifier for a daemon, when configured. The
// returned stop is nil when nothing was started. A configuration problem is
// logged and never stops the daemon.
func startNeedsYouNotifier(cfg config.Config, sessionName string, natsPort int, logger *slog.Logger) (stop func()) {
	n := cfg.NotifyNeedsYou
	for _, p := range n.Problems {
		logger.Warn(progname.Rewrite("rysh: needs-you notifier config"), "problem", p)
	}
	if !n.Enabled() {
		return nil
	}
	c, err := cli.NewPersistentClient(natsPort, sessionName)
	if err != nil {
		logger.Warn(progname.Rewrite("rysh: needs-you notifier disabled"), "err", err)
		return nil
	}
	sinks, err := buildNeedsYouSinks(cfg, cfg.RyshDir, c)
	if err != nil {
		c.Close()
		logger.Warn(progname.Rewrite("rysh: needs-you notifier disabled"), "err", err)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := needsyou.Run(ctx, subscribeExactClient{c}, needsyou.Options{
			Session: sessionName, Host: notifyHost(), After: n.After, Cooldown: n.Cooldown,
			DoneAfter: n.DoneAfter, Sinks: sinks, Logger: logger,
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Warn(progname.Rewrite("rysh: needs-you notifier stopped"), "err", err)
		}
	}()
	names := make([]string, 0, len(sinks))
	for _, s := range sinks {
		names = append(names, s.Name())
	}
	logger.Info(progname.Rewrite("rysh: needs-you notifier on"), "sinks", strings.Join(names, ","),
		"after", n.After.String(), "cooldown", n.Cooldown.String(), "done_after", n.DoneAfter.String())
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		c.Close()
	}
}

// notifyAssistantRequester connects `rysh notify test` to the running session,
// whose humanoid the assistant sink asks. A variable so tests can stand in.
var notifyAssistantRequester = func(cfg config.Config) (needsyou.Requester, func(), error) {
	store, err := session.NewStore(cfg)
	if err != nil {
		return nil, nil, err
	}
	c, err := cli.Connect(store, cfg.SessionName)
	if err != nil {
		return nil, nil, err
	}
	return c, c.Close, nil
}

// runNotifyCmd implements `rysh notify test`: one synthetic alert through each
// configured sink, so the path to your phone is proven before you leave.
func runNotifyCmd(cfg config.Config, args []string, out io.Writer) error {
	if len(args) != 2 || args[1] != "test" {
		return errors.New(progname.Rewrite(notifyUsage))
	}
	for _, p := range cfg.NotifyNeedsYou.Problems {
		fmt.Fprintln(out, "warning:", p)
	}
	if !cfg.NotifyNeedsYou.Enabled() {
		return errors.New("notify: no sink configured (set notify.needs_you.webhook, .command or .assistant in rysh.config.yaml)")
	}
	sess := cfg.SessionName
	if sess == "" {
		sess = "default"
	}
	var session needsyou.Requester
	if cfg.NotifyNeedsYou.Assistant != "" {
		c, closeSession, err := notifyAssistantRequester(cfg)
		if err != nil {
			return fmt.Errorf("notify.needs_you.assistant: the session is not reachable (start it first): %w", err)
		}
		defer closeSession()
		session = c
	}
	sinks, err := buildNeedsYouSinks(cfg, cfg.RyshDir, session)
	if err != nil {
		return err
	}
	a := needsyou.Alert{Session: sess, Host: notifyHost(), PaneID: "test", PaneName: "test", Reason: "test",
		Since: time.Now().UnixMilli()}
	failed := 0
	for _, s := range sinks {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := s.Send(ctx, a)
		cancel()
		if err != nil {
			failed++
			fmt.Fprintf(out, "%s: FAILED: %v\n", s.Name(), err)
			continue
		}
		fmt.Fprintf(out, "%s: sent\n", s.Name())
	}
	if failed > 0 {
		return fmt.Errorf("notify: %d of %d sink(s) failed", failed, len(sinks))
	}
	return nil
}
