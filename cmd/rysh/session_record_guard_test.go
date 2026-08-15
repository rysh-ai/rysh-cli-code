// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// TestSessionRecordGuardHeals verifies the daemon's session-record guard: a
// record clobbered (marked stopped) or deleted while the daemon lives is
// rewritten to reflect the live daemon within one guard interval.
func TestSessionRecordGuardHeals(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewStore(config.Config{SessionDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	self := session.Record{
		Name: "s", Path: "/proj", State: "detached",
		PID: os.Getpid(), NATSPort: 4222, Source: session.SourceApp,
	}
	if _, err := store.Upsert(self); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	stop := startSessionRecordGuard(store, self, 20*time.Millisecond, logger)
	defer stop()

	waitHealed := func(what string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			rec, err := store.Get("s")
			if err == nil && rec.PID == os.Getpid() && rec.State == "detached" && rec.NATSPort == 4222 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%s: record was not healed within the deadline", what)
	}

	// Clobber the record the way a stale build or manual edit would.
	if _, err := store.Upsert(session.Record{Name: "s", Path: "/proj", State: "stopped"}); err != nil {
		t.Fatal(err)
	}
	waitHealed("clobbered to stopped")

	// Delete the record file outright — the guard recreates it. (Not
	// store.Delete: that would SIGTERM the record's PID — this test process.)
	if err := os.Remove(filepath.Join(dir, "s.json")); err != nil {
		t.Fatal(err)
	}
	waitHealed("record deleted")
}
