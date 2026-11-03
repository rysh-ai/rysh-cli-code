// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// runExitPhrase is the one wording of `rysh run`'s exit codes, built from the
// constants so the help text and docs/commands.md cannot drift from the code
// (web found "2 = approval requested, 3 = timeout" there, which the codes had
// outgrown).
func runExitPhrase() string {
	codes := []struct {
		code int
		name string
	}{
		{runExitDone, "done"},
		{runExitError, "error"},
		{runExitPartial, "partial"},
		{runExitGateBlocked, "gate-blocked"},
		{runExitBudget, "budget"},
		{runExitTimeout, "timeout"},
	}
	var parts []string
	for _, c := range codes {
		parts = append(parts, fmt.Sprintf("%d %s", c.code, c.name))
	}
	return strings.Join(parts, ", ")
}

func TestRunExitCodesInHelpAndCommandsDoc(t *testing.T) {
	want := runExitPhrase()
	if usage := captureStdout(t, printUsage); !strings.Contains(usage, want) {
		t.Errorf("rysh help does not list run's exit codes as %q", want)
	}
	doc, err := os.ReadFile("../../docs/commands.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), want) {
		t.Errorf("docs/commands.md does not list run's exit codes as %q", want)
	}
}
