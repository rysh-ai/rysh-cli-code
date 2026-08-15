// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"
)

func TestExtractShellCommands(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{name: "simple command", input: "ls", want: []string{"ls"}},
		{name: "command with flags", input: "ls -la", want: []string{"ls"}},
		{name: "two commands with &&", input: "ls && cat foo", want: []string{"ls", "cat"}},
		{name: "two commands with semicolon", input: "ls; rm -rf /", want: []string{"ls", "rm"}},
		{name: "pipe", input: "cat file | grep pattern", want: []string{"cat", "grep"}},
		{name: "sudo prefix", input: "sudo rm -rf /", want: []string{"rm"}},
		{name: "env prefix with assignment", input: "env VAR=1 go test", want: []string{"go"}},
		{name: "env assignment prefix", input: "KEY=val command", want: []string{"command"}},
		{name: "subshell dollar-paren", input: "$(malicious)", wantErr: true},
		{name: "backtick subshell", input: "`malicious`", wantErr: true},
		{name: "full path command", input: "/usr/bin/ls", want: []string{"ls"}},
		{name: "mixed operators", input: "ls && cat || grep", want: []string{"ls", "cat", "grep"}},
		{name: "empty input", input: "", want: nil},
		{name: "whitespace only", input: "   ", want: nil},
		{name: "nohup prefix", input: "nohup python server.py", want: []string{"python"}},
		{name: "time prefix", input: "time make build", want: []string{"make"}},
		{name: "multiple env assignments", input: "FOO=1 BAR=2 go test ./...", want: []string{"go"}},
		{name: "semicolon chain", input: "cd /tmp; ls; pwd", want: []string{"cd", "ls", "pwd"}},
		{name: "or operator", input: "test -f foo || echo missing", want: []string{"test", "echo"}},
		{name: "env with flags", input: "env -i PATH=/bin ls", want: []string{"ls"}},
		{name: "sudo with flag", input: "sudo -u root whoami", want: []string{"whoami"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractShellCommands(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if len(got) != len(tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
				return
			}
			for i, g := range got {
				if g != tt.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, g, tt.want[i])
				}
			}
		})
	}
}

func TestToStringSet(t *testing.T) {
	set := toStringSet([]string{"ls", "cat", "grep"})
	if !set["ls"] || !set["cat"] || !set["grep"] {
		t.Errorf("expected all items in set, got %v", set)
	}
	if set["rm"] {
		t.Errorf("expected rm not in set")
	}
}
