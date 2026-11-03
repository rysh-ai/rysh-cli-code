// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"bytes"
	"strings"
	"testing"
)

// gap01 slice 2 P2: `rysh sandbox-bridge` (host end) and `rysh sandbox-forward`
// (in-sandbox end). Both refuse anything but their one fixed shape.

func TestBridgeMainRefusesBadInput(t *testing.T) {
	for name, args := range map[string][]string{
		"no args":      {},
		"non-loopback": {"--dir", "/tmp/x", "--upstream", "10.0.0.1:443", "--parent", "1"},
		"bus port":     {"--dir", "/tmp/x", "--upstream", "127.0.0.1:4222", "--parent", "1"},
		"relative dir": {"--dir", "rel", "--upstream", "127.0.0.1:8080", "--parent", "1"},
		"no parent":    {"--dir", "/tmp/x", "--upstream", "127.0.0.1:8080"},
	} {
		var stderr bytes.Buffer
		if rc := BridgeMain(args, &stderr); rc == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestForwardMainRunsTheAgentAndPassesItsExitCode(t *testing.T) {
	sock := shortTempDir(t) + "/none.sock"
	var stderr bytes.Buffer
	rc := ForwardMain([]string{"--listen", "127.0.0.1:0", "--socket", sock, "--", "/bin/sh", "-c", "exit 3"}, &stderr)
	if rc != 3 {
		t.Fatalf("rc = %d, want the agent's 3 (stderr %s)", rc, stderr.String())
	}
}

func TestForwardMainRefusesANonLoopbackListen(t *testing.T) {
	var stderr bytes.Buffer
	rc := ForwardMain([]string{"--listen", "0.0.0.0:8080", "--socket", "/x.sock", "--", "/bin/true"}, &stderr)
	if rc == 0 || !strings.Contains(stderr.String(), "127.0.0.1") {
		t.Fatalf("rc=%d stderr=%s", rc, stderr.String())
	}
}
