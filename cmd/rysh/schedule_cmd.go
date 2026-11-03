// SPDX-License-Identifier: Apache-2.0

package main

// `rysh schedule` and `rysh trigger` (gap06) — top-level names for the
// in-daemon scheduler (##cron) and the inbound webhook triggers (##trigger).
//
// Both are pure aliases of `rysh exec -- '##cron …'` / `'##trigger …'`: the
// daemon's command table stays the single implementation. The alias adds one
// thing — `rysh schedule add <name> "0 9 * * *" …` arrives with the shell's
// quotes stripped, so the schedule argument is re-quoted to stay one token for
// the ##cron parser (extractSchedule). Exit codes are rysh exec's.

import (
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// aliasExecArgs rewrites `<alias> [--session S] [--json] <args…>` into the
// argument list runExecCmd takes for `##<word> <args…>`.
func aliasExecArgs(word string, args []string) []string {
	rest := args[1:]
	rest, sess := extractStringFlag(rest, "--session")
	rest, asJSON := extractBoolFlag(rest, "--json")

	if word == "cron" && len(rest) >= 3 && rest[0] == "add" && strings.ContainsAny(rest[2], " \t") {
		rest = append([]string{}, rest...)
		q := `"`
		if strings.Contains(rest[2], `"`) {
			q = `'`
		}
		rest[2] = q + rest[2] + q
	}
	body := strings.Join(rest, " ")
	if body == "" {
		body = "list"
	}

	out := []string{"exec"}
	if sess != "" {
		out = append(out, "--session", sess)
	}
	if asJSON {
		out = append(out, "--json")
	}
	return append(out, "--", "##"+word+" "+body)
}

// runScheduleCmd implements `rysh schedule …` (= ##cron).
func runScheduleCmd(cfg config.Config, args []string) error {
	return runExecCmd(cfg, aliasExecArgs("cron", args))
}

// runTriggerCmd implements `rysh trigger …` (= ##trigger).
func runTriggerCmd(cfg config.Config, args []string) error {
	return runExecCmd(cfg, aliasExecArgs("trigger", args))
}
