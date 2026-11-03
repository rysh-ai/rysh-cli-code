// SPDX-License-Identifier: Apache-2.0

package conpty

import (
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestNewSize(t *testing.T) {
	for _, tc := range []struct {
		rows, cols int
		ok         bool
	}{
		{24, 80, true},
		{1, 1, true},
		{32767, 32767, true},
		{0, 80, false},
		{24, 0, false},
		{-1, 80, false},
		{32768, 80, false},
		{24, 32768, false},
	} {
		s, err := NewSize(tc.rows, tc.cols)
		if tc.ok != (err == nil) {
			t.Fatalf("NewSize(%d, %d): err = %v, want ok=%v", tc.rows, tc.cols, err, tc.ok)
		}
		if tc.ok && (int(s.Rows) != tc.rows || int(s.Cols) != tc.cols) {
			t.Fatalf("NewSize(%d, %d) = %+v", tc.rows, tc.cols, s)
		}
	}
}

// fakeLook answers lookPath for the names in found, as a PATH search would.
func fakeLook(found map[string]string) func(string) (string, error) {
	return func(name string) (string, error) {
		if p, ok := found[name]; ok {
			return p, nil
		}
		return "", errors.New("not found")
	}
}

func TestResolveShell(t *testing.T) {
	const (
		pwsh = `C:\Program Files\PowerShell\7\pwsh.exe`
		ps   = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
		cmd  = `C:\Windows\System32\cmd.exe`
	)
	all := map[string]string{"pwsh.exe": pwsh, "powershell.exe": ps, "cmd.exe": cmd}
	noEnv := func(string) string { return "" }

	for _, tc := range []struct {
		name        string
		configured  string
		interactive bool
		getenv      func(string) string
		found       map[string]string
		want        Shell
		wantErr     bool
	}{
		{name: "default prefers pwsh", found: all,
			want: Shell{Path: pwsh, Args: []string{"-NoLogo"}, Kind: KindPowerShell}},
		{name: "falls back to Windows PowerShell",
			found: map[string]string{"powershell.exe": ps, "cmd.exe": cmd},
			want:  Shell{Path: ps, Args: []string{"-NoLogo"}, Kind: KindPowerShell}},
		{name: "falls back to ComSpec",
			getenv: func(k string) string {
				if k == "ComSpec" {
					return cmd
				}
				return ""
			},
			want: Shell{Path: cmd, Kind: KindCmd}},
		{name: "falls back to cmd.exe on PATH", found: map[string]string{"cmd.exe": cmd},
			want: Shell{Path: cmd, Kind: KindCmd}},
		{name: "nothing found", wantErr: true},
		{name: "POSIX default is ignored", configured: "/bin/bash", interactive: true, found: all,
			want: Shell{Path: pwsh, Args: []string{"-NoLogo"}, Kind: KindPowerShell}},
		{name: "configured shell wins", configured: ps, interactive: true, found: all,
			want: Shell{Path: ps, Args: []string{"-NoLogo"}, Kind: KindPowerShell}},
		{name: "PowerShell never gets -i", configured: `PWSH.EXE`, interactive: true, found: all,
			want: Shell{Path: pwsh, Args: []string{"-NoLogo"}, Kind: KindPowerShell}},
		{name: "configured cmd", configured: "cmd", interactive: true, found: all,
			want: Shell{Path: cmd, Kind: KindCmd}},
		{name: "bash keeps -i when interactive", configured: `C:\Program Files\Git\bin\bash.exe`, interactive: true,
			want: Shell{Path: `C:\Program Files\Git\bin\bash.exe`, Args: []string{"-i"}, Kind: KindPOSIX}},
		{name: "bash without -i in legacy mode", configured: `C:\Program Files\Git\bin\bash.exe`,
			want: Shell{Path: `C:\Program Files\Git\bin\bash.exe`, Kind: KindPOSIX}},
		{name: "configured name not on PATH", configured: "nu.exe", found: all, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := tc.getenv
			if getenv == nil {
				getenv = noEnv
			}
			got, err := ResolveShell(tc.configured, tc.interactive, getenv, fakeLook(tc.found))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveShell = %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveShell: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ResolveShell = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestMergeEnv(t *testing.T) {
	base := []string{
		"=C:=C:\\work",
		"Path=C:\\a",
		"TERM=dumb",
		"rysh_web_control=1",
		"garbage",
	}
	got := MergeEnv(base, []string{"RYSH_WEB_CONTROL"}, "PATH=C:\\b", "TERM=xterm-256color", "RYSH_PANE=p1")
	want := []string{
		"=C:=C:\\work",
		"PATH=C:\\b",
		"TERM=xterm-256color",
		"RYSH_PANE=p1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MergeEnv =\n%q\nwant\n%q", got, want)
	}
}

func TestEnvBlock(t *testing.T) {
	block, err := EnvBlock([]string{"b=2", "A=1", "=C:=C:\\x"})
	if err != nil {
		t.Fatalf("EnvBlock: %v", err)
	}
	want := utf16.Encode([]rune("=C:=C:\\x\x00A=1\x00b=2\x00\x00"))
	if !reflect.DeepEqual(block, want) {
		t.Fatalf("EnvBlock = %q, want %q", string(utf16.Decode(block)), string(utf16.Decode(want)))
	}

	empty, err := EnvBlock(nil)
	if err != nil || !reflect.DeepEqual(empty, []uint16{0, 0}) {
		t.Fatalf("EnvBlock(nil) = %v, %v; want [0 0]", empty, err)
	}

	if _, err := EnvBlock([]string{"A=1\x00B=2"}); err == nil {
		t.Fatal("EnvBlock accepted an entry containing NUL")
	}
}

func decodeEncodedCommand(t *testing.T, args []string) string {
	t.Helper()
	for i, a := range args {
		if a == "-EncodedCommand" && i+1 < len(args) {
			raw, err := base64.StdEncoding.DecodeString(args[i+1])
			if err != nil || len(raw)%2 != 0 {
				t.Fatalf("-EncodedCommand is not base64 UTF-16LE: %v", err)
			}
			u := make([]uint16, len(raw)/2)
			for j := range u {
				u[j] = uint16(raw[2*j]) | uint16(raw[2*j+1])<<8
			}
			return string(utf16.Decode(u))
		}
	}
	t.Fatalf("no -EncodedCommand in %q", args)
	return ""
}

func TestShellHookPowerShell(t *testing.T) {
	sh := Shell{Path: `C:\pwsh\pwsh.exe`, Args: []string{"-NoLogo"}, Kind: KindPowerShell}

	got, env := ShellHook(sh, true)
	if len(env) != 0 {
		t.Errorf("PowerShell hook env = %q, want none", env)
	}
	if got.Path != sh.Path || got.Kind != KindPowerShell {
		t.Errorf("hook changed the program: %+v", got)
	}
	if strings.Join(got.Args[:2], " ") != "-NoLogo -NoExit" {
		t.Errorf("args = %q, want -NoLogo -NoExit first (the shell must stay open)", got.Args)
	}
	if len(sh.Args) != 1 {
		t.Errorf("ShellHook mutated the input's Args: %q", sh.Args)
	}
	script := decodeEncodedCommand(t, got.Args)
	for _, want := range []string{
		"function global:prompt",
		"]7;file://",
		"[char]27",
		"FileSystem",
		"$global:__ryshPrompt",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("interactive hook script lacks %q:\n%s", want, script)
		}
	}

	legacy, _ := ShellHook(sh, false)
	if s := decodeEncodedCommand(t, legacy.Args); !strings.Contains(s, "function global:prompt { '$ ' }") {
		t.Errorf("legacy hook must force the minimal \"$ \" prompt rysh strips:\n%s", s)
	}
}

func TestShellHookCmd(t *testing.T) {
	sh := Shell{Path: `C:\Windows\System32\cmd.exe`, Kind: KindCmd}
	got, env := ShellHook(sh, true)
	if len(got.Args) != 0 {
		t.Errorf("cmd args = %q, want none", got.Args)
	}
	if len(env) != 1 || env[0] != `PROMPT=$E]7;file://localhost/$P$E\$P$G` {
		t.Errorf("cmd interactive env = %q", env)
	}
	_, env = ShellHook(sh, false)
	if len(env) != 1 || env[0] != `PROMPT=$$$S` {
		t.Errorf("cmd legacy env = %q, want PROMPT=$$$S (\"$ \")", env)
	}
}

func TestShellHookPOSIXAndOther(t *testing.T) {
	bash := Shell{Path: `C:\Git\bin\bash.exe`, Args: []string{"-i"}, Kind: KindPOSIX}
	got, env := ShellHook(bash, true)
	if strings.Join(got.Args, " ") != "-i" {
		t.Errorf("bash args = %q", got.Args)
	}
	if len(env) != 1 || !strings.HasPrefix(env[0], "PROMPT_COMMAND=") || !strings.Contains(env[0], "]7;file://") {
		t.Errorf("bash interactive env = %q, want the OSC 7 PROMPT_COMMAND", env)
	}
	if _, env := ShellHook(bash, false); len(env) != 1 || env[0] != "PS1=$ " {
		t.Errorf("bash legacy env = %q, want PS1=$ ", env)
	}
	other := Shell{Path: `C:\nu\nu.exe`, Kind: KindOther}
	if got, env := ShellHook(other, true); len(got.Args) != 0 || len(env) != 0 {
		t.Errorf("unknown shell hook = %+v %q, want untouched", got, env)
	}
}
