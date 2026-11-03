// SPDX-License-Identifier: Apache-2.0

package main

// `rysh heymates serve` — the in-container bridge of rysh-family H3 (design 093
// §12.3). Unlike the other `rysh heymates` commands, which are clients of
// rysh-server's /api/family, serve runs INSIDE the family's hosted container
// and talks to the local session daemon: rysh-server holds one long-lived
// `docker exec -i <container> rysh_local family serve --session work` per
// family and speaks newline-delimited JSON with it over stdin/stdout
// (internal/familyserve; protocol in out/rysh-family-h3-protocol.md).
//
//	rysh heymates serve [--session <name>]
//
// stdout carries protocol lines only; every log goes to stderr. Exit 0 when
// stdin closes; exit 1 when the session cannot be reached or its bus goes away
// (the server reconnects with a new exec either way).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/familyserve"
)

const familyServeUsage = "usage: rysh heymates serve [--session <name>]   the hosted-family bridge: JSON lines on stdin/stdout"

func runFamilyServe(cfg config.Config, args []string) error {
	var sess string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--session":
			if i+1 >= len(args) {
				return errors.New("--session needs a value\n" + familyServeUsage)
			}
			sess = args[i+1]
			i++
		case strings.HasPrefix(a, "--session="):
			sess = strings.TrimPrefix(a, "--session=")
		case a == "-h" || a == "--help" || a == "help":
			fmt.Fprintln(os.Stderr, familyServeUsage)
			return nil
		default:
			return fmt.Errorf("unknown argument %q\n%s", a, familyServeUsage)
		}
	}

	// Protocol lines only on stdout: keep the real stdout for the protocol and
	// point os.Stdout at stderr, so a stray print anywhere in the process can
	// never corrupt the stream.
	protoOut := os.Stdout
	os.Stdout = os.Stderr
	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("cmd", "family serve")
	slog.SetDefault(log)

	store, name, err := resolveBoardSession(cfg, sess)
	if err != nil {
		return err
	}
	c, err := cli.Connect(store, name)
	if err != nil {
		return err
	}
	defer c.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	lost := make(chan struct{})
	c.Conn().SetClosedHandler(func(*nats.Conn) {
		select {
		case <-lost:
		default:
			close(lost)
		}
		cancel()
	})

	err = familyserve.Run(ctx, familyserve.Config{
		NC:      c.Conn(),
		Codecs:  c.Codecs(),
		Session: name,
		Version: version,
		In:      os.Stdin,
		Out:     protoOut,
		Log:     log,
	})
	select {
	case <-lost:
		return errors.New("family serve: the session bus closed")
	default:
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
