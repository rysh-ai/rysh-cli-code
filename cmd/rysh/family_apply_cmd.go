// SPDX-License-Identifier: Apache-2.0

package main

// `rysh heymates apply` — rysh-family H2 (design 093 §12.3). Like `rysh heymates
// serve` it runs INSIDE the family's hosted container and talks to the local
// session daemon: rysh-server runs
//
//	docker exec -i <container> rysh_local family apply --session work
//
// with one JSON manifest on stdin (internal/familyapply), and the command
// makes the session match it — one pane per avatar, the family fleet, each
// avatar's agent profile with exactly the family tools — touching only what
// differs. The tool allow-list is fixed in the command; no flag changes it.
//
// stdout carries exactly one JSON object (the Result) on success and nothing
// on failure; every log and the error go to stderr, and a failure exits 1.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/familyapply"
)

const familyApplyUsage = "usage: rysh heymates apply [--session <name>] < manifest.json   make the session match a hosted-family manifest; prints one JSON result"

// familyApplyTimeout bounds one whole apply (8 avatars, each a few
// requests). It stays under the 60 s rysh-server gives one docker exec, so
// the command reports its own error instead of being killed mid-write.
const familyApplyTimeout = 50 * time.Second

// runFamilyApply prefixes every failure with "family apply:". rysh-server's
// hosted runner (internal/hostedrunner/docker.go:540) reads that text in the
// exec output as "this image has the command", so an apply that fails for any
// reason is never mistaken for an image without it. Keep the prefix until the
// family alias is dropped and docker.go's matchers change with it (heymates
// rename phase 4).
func runFamilyApply(cfg config.Config, args []string) error {
	if err := applyFamilyManifest(cfg, args); err != nil {
		return fmt.Errorf("family apply: %w", err)
	}
	return nil
}

func applyFamilyManifest(cfg config.Config, args []string) error {
	var sess string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--session":
			if i+1 >= len(args) {
				return errors.New("--session needs a value\n" + familyApplyUsage)
			}
			sess = args[i+1]
			i++
		case strings.HasPrefix(a, "--session="):
			sess = strings.TrimPrefix(a, "--session=")
		case a == "-h" || a == "--help" || a == "help":
			fmt.Fprintln(os.Stderr, familyApplyUsage)
			return nil
		default:
			return fmt.Errorf("unknown argument %q\n%s", a, familyApplyUsage)
		}
	}

	// The result is the only thing on stdout: keep the real stdout for it and
	// point os.Stdout at stderr, so a stray print can never corrupt it.
	out := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = out }()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("cmd", "family apply")

	// The manifest is read before the session is reached: a bad manifest
	// fails without touching the bus.
	m, err := familyapply.Parse(os.Stdin)
	if err != nil {
		return err
	}
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
	ctx, cancelT := context.WithTimeout(ctx, familyApplyTimeout)
	defer cancelT()

	res, err := familyapply.Apply(ctx, familyapply.Config{NC: c.Conn(), Codecs: c.Codecs(), Log: log}, m)
	if err != nil {
		return err
	}
	return familyapply.WriteResult(out, res)
}
