// SPDX-License-Identifier: Apache-2.0

package main

// `rysh cron apply` — the non-interactive half of `##cron edit <name>`.
//
// `##cron edit` opens $EDITOR on a temp file holding a job's Input, then
// chains this command onto the editor's exit: `$EDITOR file && rysh cron
// apply <name> file`. It exists as its own subcommand rather than routing the
// file's content through `rysh exec -- "##cron edit-apply ..."` because that
// path token-parses its command line (workspace_cron.go's handleCronCommand),
// and a file's content — quotes, newlines, a stray "##" line — is not safe or
// lossless to smuggle through that parser. Reading the file here and sending
// it as a typed field (MsgCLICronApply) sidesteps parsing entirely.
//
// Exit codes: 0 applied, 1 refused or unreachable.

import (
	"errors"
	"os"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

const cronApplyUsage = "usage: rysh cron apply <job-name> <file> [--session <name>]"

// runCronCmd implements `rysh cron apply <job-name> <file>`.
func runCronCmd(cfg config.Config, args []string) error {
	// args[0] == "cron".
	rest := args[1:]
	if len(rest) == 0 || rest[0] != "apply" {
		return errors.New(progname.Rewrite(cronApplyUsage))
	}
	rest = rest[1:]

	rest, sess := extractStringFlag(rest, "--session")
	if len(rest) < 2 {
		return errors.New(progname.Rewrite(cronApplyUsage))
	}
	jobName, file := rest[0], rest[1]

	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	input := strings.TrimRight(string(data), "\r\n")
	if input == "" {
		return errors.New(progname.Rewrite(
			"rysh cron apply: the edited file is empty — nothing to apply"))
	}

	if sess == "" {
		sess = os.Getenv("RYSH_SESSION")
	}
	if sess == "" {
		sess = cfg.SessionName
	}
	store, err := session.NewStore(cfg)
	if err != nil {
		return err
	}

	resp, err := cli.CronApply(store, sess, jobName, input)
	if err != nil {
		// Transport failure: the edit never reached the daemon. Distinct from
		// an edit the daemon refused (e.g. failed re-validation), though both
		// exit 1.
		return err
	}
	if !resp.OK {
		if resp.Error != "" {
			return errors.New(resp.Error)
		}
		return errors.New("cron apply failed")
	}
	return nil
}
