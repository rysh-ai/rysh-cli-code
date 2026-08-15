// SPDX-License-Identifier: Apache-2.0

package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Never configured: the zero value, and not an error.
	got, err := LoadWebSettings(dir, "s1")
	if err != nil {
		t.Fatalf("load (absent): %v", err)
	}
	if got.Configured() {
		t.Fatalf("an unconfigured session reports settings: %+v", got)
	}

	want := WebSettings{
		AutoStart: true, Host: "0.0.0.0", Port: 23002,
		Ngrok: true, NgrokDomain: "rysh-web-23002.ngrok.app",
	}
	if err := SaveWebSettings(dir, "s1", want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err = LoadWebSettings(dir, "s1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.AutoStart != true || got.Host != "0.0.0.0" || got.Port != 23002 ||
		!got.Ngrok || got.NgrokDomain != "rysh-web-23002.ngrok.app" {
		t.Fatalf("round-trip lost values: %+v", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt not stamped")
	}
	if !got.Configured() {
		t.Error("Configured() false for a saved setup")
	}
}

// The whole point of the move: one session's settings must not be another's.
func TestWebSettingsArePerSession(t *testing.T) {
	dir := t.TempDir()
	if err := SaveWebSettings(dir, "macmini-rysh", WebSettings{AutoStart: true, Port: 23002}); err != nil {
		t.Fatal(err)
	}
	other, err := LoadWebSettings(dir, "macmini-rysh-elect")
	if err != nil {
		t.Fatal(err)
	}
	if other.Configured() {
		t.Fatalf("a sibling session inherited settings: %+v", other)
	}
}

// The settings file must not be mistaken for a session record — Store.List
// reads every *.json under sessions/, so a file placed there would show up in
// `##session list` as a session nobody created.
func TestWebSettingsAreNotInTheRegistryDir(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveWebSettings(dir, "s1", WebSettings{AutoStart: true, Port: 23002}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("web settings landed in the registry directory: %v", entries)
	}
	if !strings.HasSuffix(filepath.Dir(WebSettingsPath(dir, "s1")), "web") {
		t.Fatalf("unexpected settings location: %s", WebSettingsPath(dir, "s1"))
	}
}

// A file that cannot be parsed must not read as "never configured": that would
// silently change how a session is exposed.
func TestWebSettingsCorruptFileErrors(t *testing.T) {
	dir := t.TempDir()
	if err := SaveWebSettings(dir, "s1", WebSettings{AutoStart: true, Port: 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(WebSettingsPath(dir, "s1"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWebSettings(dir, "s1"); err == nil {
		t.Fatal("a corrupt settings file loaded cleanly")
	}
}

func TestWebSettingsPublishPort(t *testing.T) {
	if got := (WebSettings{Port: 23232}).PublishPort(); got != 23232 {
		t.Errorf("PublishPort = %d, want the server's own", got)
	}
	// A shared door is the address other people were given, so it is the one
	// worth publishing.
	if got := (WebSettings{Port: 56036, SharedPort: 23001}).PublishPort(); got != 23001 {
		t.Errorf("PublishPort = %d, want the shared door", got)
	}
}

func TestClearWebSettings(t *testing.T) {
	dir := t.TempDir()
	if err := SaveWebSettings(dir, "s1", WebSettings{AutoStart: true, Port: 23002}); err != nil {
		t.Fatal(err)
	}
	removed, err := ClearWebSettings(dir, "s1")
	if err != nil || !removed {
		t.Fatalf("clear = (%v,%v)", removed, err)
	}
	removed, err = ClearWebSettings(dir, "s1")
	if err != nil || removed {
		t.Fatalf("second clear = (%v,%v), want (false,nil)", removed, err)
	}
}
