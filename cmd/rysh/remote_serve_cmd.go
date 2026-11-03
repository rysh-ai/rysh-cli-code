// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/remote"
	"github.com/rysh-ai/rysh-cli-code/internal/remote/remoteserve"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// runRemoteServeCmd implements `rysh remote-serve [--session <name>]`: the
// remote end of feature #14 (design 070). A local rysh runs it through
// `ssh <alias> rysh_local remote-serve …` and speaks framed JSON on its
// stdin/stdout; it serves one running session of this host and exits when the
// local side hangs up. It never starts a session: a missing one is an error the
// local side shows.
func runRemoteServeCmd(cfg config.Config, args []string) error {
	// stdout is the frame stream. Keep the real one for frames and point
	// os.Stdout at stderr, so a stray print anywhere in the process can never
	// corrupt the stream.
	frames := os.Stdout
	os.Stdout = os.Stderr

	rest, sess := extractStringFlag(args[1:], "--session")
	if len(rest) > 0 {
		return fmt.Errorf("usage: rysh remote-serve [--session <name>]")
	}
	if sess == "" {
		sess = cfg.SessionName
	}
	if err := remote.ValidateSessionName(sess); err != nil {
		return err
	}
	store, err := session.NewStore(cfg)
	if err != nil {
		return err
	}
	c, err := cli.Connect(store, sess)
	if err != nil {
		return fmt.Errorf("remote-serve: session %q: %w", sess, err)
	}
	defer c.Close()

	b, err := remoteserve.New(subscribeExactClient{c})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	err = remote.Serve(ctx, os.Stdin, frames, b, remote.ServeOptions{Version: version, Session: sess})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// subscribeExactClient adapts cli.Client to remoteserve.SessionClient.
type subscribeExactClient struct{ *cli.Client }

func (s subscribeExactClient) SubscribeExact(subject string, fn func(interface{})) (func(), error) {
	// Exact subjects only: a wildcard would hear other panes' traffic.
	if strings.ContainsAny(subject, "*>") {
		return nil, fmt.Errorf("remote-serve: refusing wildcard subscription %q", subject)
	}
	sub, err := s.Client.Subscribe(subject, fn)
	if err != nil {
		return nil, err
	}
	return func() { _ = sub.Unsubscribe() }, nil
}
