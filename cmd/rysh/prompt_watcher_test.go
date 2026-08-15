// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestStartPromptWatcher_DebouncedReloadOnMdChange proves a burst of writes to
// a .md file coalesces into exactly one reload trigger.
func TestStartPromptWatcher_DebouncedReloadOnMdChange(t *testing.T) {
	dir := t.TempDir()
	orig := promptReloadDebounce
	promptReloadDebounce = 40 * time.Millisecond
	defer func() { promptReloadDebounce = orig }()

	fires := make(chan struct{}, 16)
	stop, err := startPromptWatcher(dir, func() { fires <- struct{}{} })
	if err != nil {
		t.Fatalf("startPromptWatcher: %v", err)
	}
	if stop == nil {
		t.Fatal("expected a non-nil stop")
	}
	defer stop()

	// Let the watcher goroutine register before we write.
	time.Sleep(30 * time.Millisecond)

	p := filepath.Join(dir, "system_default.md")
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	select {
	case <-fires:
	case <-time.After(2 * time.Second):
		t.Fatal("expected a reload trigger after .md write")
	}

	// The burst must coalesce — no second fire.
	select {
	case <-fires:
		t.Fatal("burst should coalesce into a single reload")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestStartPromptWatcher_IgnoresNonMd proves non-prompt files don't trigger a
// reload.
func TestStartPromptWatcher_IgnoresNonMd(t *testing.T) {
	dir := t.TempDir()
	orig := promptReloadDebounce
	promptReloadDebounce = 40 * time.Millisecond
	defer func() { promptReloadDebounce = orig }()

	var fired int32
	stop, err := startPromptWatcher(dir, func() { atomic.AddInt32(&fired, 1) })
	if err != nil {
		t.Fatalf("startPromptWatcher: %v", err)
	}
	defer stop()
	time.Sleep(30 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := atomic.LoadInt32(&fired); n != 0 {
		t.Fatalf("non-.md write should not trigger reload, got %d", n)
	}
}

// TestStartPromptWatcher_NoopWhenDirEmpty proves the watcher is a no-op when no
// override directory is configured.
func TestStartPromptWatcher_NoopWhenDirEmpty(t *testing.T) {
	stop, err := startPromptWatcher("", func() {})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stop != nil {
		t.Fatal("expected nil stop for empty dir")
	}
}
