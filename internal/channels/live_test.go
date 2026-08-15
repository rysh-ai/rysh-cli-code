// SPDX-License-Identifier: Apache-2.0

//go:build livechannels

// Package channels live round-trip suite (LV1, design 010). These tests talk to
// REAL platforms and are compiled only under the `livechannels` build tag, so
// they never run in the default `go test ./...` (which stays fixture-only). Each
// test reads its credentials from RYSH_LIVE_<CHANNEL>_* env vars and SKIPS when
// they are absent — a missing credential is a skip, never a failure. Run one with:
//
//	RYSH_LIVE_EMAIL_IMAP_HOST=… RYSH_LIVE_EMAIL_SMTP_HOST=… \
//	RYSH_LIVE_EMAIL_USERNAME=… RYSH_LIVE_EMAIL_PASSWORD=… \
//	go test -tags livechannels -run TestLiveEmail -v ./internal/channels/
//
// Credentials come from the environment only and are never logged; assertions
// print subjects/addresses, never secrets.

package channels

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// requireEnv returns the values for keys, or skips the test when any is unset.
// The skip message names exactly what to set, so a partial credential set is
// self-explanatory rather than a mysterious failure.
func requireEnv(t *testing.T, keys ...string) map[string]string {
	t.Helper()
	vals := make(map[string]string, len(keys))
	var missing []string
	for _, k := range keys {
		v := os.Getenv(k)
		if v == "" {
			missing = append(missing, k)
		}
		vals[k] = v
	}
	if len(missing) > 0 {
		t.Skipf("live test skipped — set %s", strings.Join(missing, ", "))
	}
	return vals
}

// envInt reads an int env var, falling back to def when unset or unparseable.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// awaitInbound waits for an InboundMessage matching pred, ignoring any unrelated
// traffic, and fails after timeout. A per-test nonce in pred keeps the match to
// the message this test produced.
func awaitInbound(t *testing.T, ch <-chan InboundMessage, timeout time.Duration, pred func(InboundMessage) bool) InboundMessage {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case m := <-ch:
			if pred(m) {
				return m
			}
			t.Logf("ignoring unrelated inbound from %s", m.SenderID)
		case <-deadline:
			t.Fatalf("no matching inbound within %s", timeout)
			return InboundMessage{}
		}
	}
}
