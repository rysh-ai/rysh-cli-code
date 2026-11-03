// SPDX-License-Identifier: Apache-2.0

package bus

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/bustoken"
)

// `rysh attach` dials the session bus in external mode (cmd/rysh runAttachUI:
// Mode "external", URL nats://127.0.0.1:<session port>). Since gap01 slice 3
// (6fe1bc48, merged ba7cc477 on 2026-10-29) every embedded session server
// requires its per-server token, and the external branch never presented it:
// re-attaching to a detached session failed with "nats: Authorization
// Violation". Found by the F-100 idle-attach probe (E9) against a real test
// daemon.

func TestExternalAttachPresentsTheOwnersToken(t *testing.T) {
	tokenHome(t)
	owner, err := New(Config{Mode: "embedded", Port: 0, SessionName: "s", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	defer owner.Close()
	url := "nats://127.0.0.1:" + strconv.Itoa(owner.ClientPort())

	att, err := New(Config{Mode: "external", URL: url, SessionName: "s", Attach: true})
	if err != nil {
		t.Fatalf("attach to a token-protected session bus: %v", err)
	}
	defer att.Close()
	if _, err := owner.WorkspaceKV().Put("k", []byte("v")); err != nil {
		t.Fatalf("owner put: %v", err)
	}
	if e, err := att.WorkspaceKV().Get("k"); err != nil || string(e.Value()) != "v" {
		t.Fatalf("attach sees k = %v, %v; want the owner's value", e, err)
	}
}

// With the token file gone, the refusal says what is missing instead of a bare
// "Authorization Violation".
func TestExternalAttachWithoutTheTokenSaysSo(t *testing.T) {
	tokenHome(t)
	owner, err := New(Config{Mode: "embedded", Port: 0, SessionName: "s", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	defer owner.Close()
	p, err := bustoken.Path(owner.ClientPort())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	url := "nats://127.0.0.1:" + strconv.Itoa(owner.ClientPort())
	att, err := New(Config{Mode: "external", URL: url, SessionName: "s", Attach: true})
	if err == nil {
		att.Close()
		t.Fatal("attach without the token file connected")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("refusal %q does not name the bus token", err)
	}
}
