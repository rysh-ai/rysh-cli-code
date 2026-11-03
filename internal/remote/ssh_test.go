// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"slices"
	"strings"
	"testing"
)

func TestSSHArgsShape(t *testing.T) {
	args, err := SSHArgs(SSHOptions{Host: "rysh-agent-fleet-dev", Session: "dev-1"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-T", "StrictHostKeyChecking=yes", "BatchMode=yes"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv missing %q: %v", want, args)
		}
	}
	if slices.Contains(args, "-i") {
		t.Fatalf("rysh must never pass an identity file: %v", args)
	}
	for _, a := range args {
		if strings.Contains(a, "IdentityFile") || strings.Contains(a, "StrictHostKeyChecking=no") ||
			strings.Contains(a, "accept-new") || strings.Contains(a, "UserKnownHostsFile=/dev/null") {
			t.Fatalf("forbidden ssh option %q", a)
		}
	}
	// "--" ends ssh's options so the host can never be read as one.
	i := slices.Index(args, "--")
	if i < 0 || args[i+1] != "rysh-agent-fleet-dev" {
		t.Fatalf("host must follow --: %v", args)
	}
	remote := args[len(args)-1]
	if remote != "rysh_local remote-serve --session 'dev-1'" {
		t.Fatalf("remote command = %q", remote)
	}
}

func TestSSHArgsConfigFileAndBinary(t *testing.T) {
	args, err := SSHArgs(SSHOptions{Host: "h", Session: "s", ConfigFile: "/tmp/x/ssh_config", RemoteBinary: "/opt/rysh/bin/rysh", Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(args, "-F")
	if i < 0 || args[i+1] != "/tmp/x/ssh_config" {
		t.Fatalf("-F missing: %v", args)
	}
	if slices.Contains(args, "BatchMode=yes") {
		t.Fatalf("an interactive (first) connect may prompt for a passphrase: %v", args)
	}
	if !slices.Contains(args, "StrictHostKeyChecking=yes") {
		t.Fatalf("StrictHostKeyChecking=yes is never dropped: %v", args)
	}
	if got := args[len(args)-1]; got != "'/opt/rysh/bin/rysh' remote-serve --session 's'" {
		t.Fatalf("remote command = %q", got)
	}
}

func TestSSHArgsRemoteDirAndConfig(t *testing.T) {
	args, err := SSHArgs(SSHOptions{Host: "h", Session: "s", RemoteDir: "/home/u/ws it's", RemoteConfig: "/home/u/ws/rysh.config.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	want := `cd '/home/u/ws it'\''s' && exec rysh_local --config '/home/u/ws/rysh.config.yaml' remote-serve --session 's'`
	if got := args[len(args)-1]; got != want {
		t.Fatalf("remote command\n got %s\nwant %s", got, want)
	}
	if _, err := SSHArgs(SSHOptions{Host: "h", RemoteDir: "a\nb"}); err == nil {
		t.Fatal("newline in a remote path accepted")
	}
}

func TestValidateHostAlias(t *testing.T) {
	for _, ok := range []string{"rysh-agent-fleet-dev", "user@host", "host.example.com", "m5", "h_1"} {
		if err := ValidateHostAlias(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-oProxyCommand=sh", "a b", "a;b", "a$(x)", "a\nb", "-F", "host'", strings.Repeat("a", 300)} {
		if err := ValidateHostAlias(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestValidateSessionName(t *testing.T) {
	for _, ok := range []string{"", "dev", "prod-pera", "a_b.c"} {
		if err := ValidateSessionName(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"a b", "a'b", "$(x)", "-x", "a/b"} {
		if err := ValidateSessionName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestClassifySSHError(t *testing.T) {
	cases := map[string]string{
		"Host key verification failed.": "host key",
		"No ED25519 host key is known for [127.0.0.1]:2222 and you have requested strict checking.": "host key",
		"@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @":                               "host key",
		"user@h: Permission denied (publickey).":                                                    "authentication",
		"ssh: Could not resolve hostname nope: nodename nor servname":                               "connect",
		"bash: rysh_local: command not found":                                                       "not installed",
	}
	for stderr, want := range cases {
		if got := ClassifySSHError(stderr); !strings.Contains(got, want) {
			t.Errorf("ClassifySSHError(%q) = %q, want it to mention %q", stderr, got, want)
		}
	}
}
