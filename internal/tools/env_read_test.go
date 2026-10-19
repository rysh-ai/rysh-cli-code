// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// All values here are synthetic and set with t.Setenv. The {"all": true} and
// {"prefix": …} paths also see the real process environment, so these tests
// only ever look at — and on failure only ever print — the one line that
// belongs to the synthetic variable under test, never the whole output.

func synthValue(i int) string { return fmt.Sprintf("sk-test-%012d", 900000000000+i) }

func runEnvRead(t *testing.T, params map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	out, err := NewEnvReadTool().Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return out.Content
}

// lineFor returns only the output line for name ("" if absent).
func lineFor(out, name string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, name+"=") {
			return l
		}
	}
	return ""
}

// listingPrefix is the prefix filter that selects name: the filter is a
// case-sensitive HasPrefix, so a lower-case name needs a lower-case prefix.
func listingPrefix(name string) string {
	if strings.HasPrefix(name, "fake_") {
		return "fake_"
	}
	return "FAKE_"
}

// checkAllPaths runs the three read paths for one synthetic variable and
// asserts masked (wantMasked) or returned in clear (!wantMasked).
func checkAllPaths(t *testing.T, name, val string, wantMasked bool) {
	t.Helper()
	paths := []struct {
		label  string
		params map[string]any
	}{
		{"name", map[string]any{"name": name}},
		{"prefix", map[string]any{"prefix": listingPrefix(name)}},
		{"all", map[string]any{"all": true}},
	}
	for _, p := range paths {
		line := lineFor(runEnvRead(t, p.params), name)
		if line == "" {
			t.Errorf("%s path: no line for %s", p.label, name)
			continue
		}
		leaked := strings.Contains(line, val)
		redacted := strings.Contains(line, "<redacted>")
		switch {
		case wantMasked && (leaked || !redacted):
			t.Errorf("%s path: %s must be masked; got line %q", p.label, name, line)
		case !wantMasked && (!leaked || redacted):
			t.Errorf("%s path: %s must NOT be masked (false positive); got line %q", p.label, name, line)
		}
	}
}

// Must-mask: API keys, access keys, AUTH as a whole name segment, and any
// name ending in _KEY. These came back in clear before T6.
func TestEnvReadMasksAPIKeysAndAuth(t *testing.T) {
	names := []string{
		"FAKE_ANTHROPIC_API_KEY",
		"FAKE_OPENAI_API_KEY",
		"FAKE_SERVICE_APIKEY",
		"FAKE_AWS_ACCESS_KEY_ID",
		"FAKE_AUTH",
		"FAKE_AUTH_HEADER",
		"FAKE_SIGNING_KEY",
		"fake_lower_api_key",
	}
	for i, name := range names {
		t.Run(name, func(t *testing.T) {
			val := synthValue(i)
			t.Setenv(name, val)
			checkAllPaths(t, name, val, true)
		})
	}
}

// False positives: names that merely contain KEY or AUTH as part of another
// word, or KEY not as a suffix, must still be returned in clear.
func TestEnvReadDoesNotMaskLookalikes(t *testing.T) {
	names := []string{
		"FAKE_KEYBOARD",
		"FAKE_KEY_REPEAT",
		"FAKE_MONKEY",
		"FAKE_KEYS_DIR",
		"FAKE_GIT_AUTHOR_NAME",
		"FAKE_AUTHOR",
		"FAKE_TURKEY_MODE",
		"FAKE_HOTKEY",
		"FAKE_HOME_DIR",
	}
	for i, name := range names {
		t.Run(name, func(t *testing.T) {
			val := synthValue(100 + i)
			t.Setenv(name, val)
			checkAllPaths(t, name, val, false)
		})
	}
}

// Regression: the pre-existing substring rules keep masking.
func TestEnvReadExistingRulesStillMask(t *testing.T) {
	names := []string{
		"FAKE_DB_PASSWORD",
		"FAKE_GH_TOKEN",
		"FAKE_CLIENT_SECRET",
	}
	for i, name := range names {
		t.Run(name, func(t *testing.T) {
			val := synthValue(200 + i)
			t.Setenv(name, val)
			checkAllPaths(t, name, val, true)
		})
	}
}
