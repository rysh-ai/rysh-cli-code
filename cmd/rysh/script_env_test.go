package main

import "testing"

// paneEnv is the identity a pane's shell exports. Every case here starts from
// it, because the question in each is which parts of it still describe the
// target once the script's own targeting is applied.
func paneEnv() []string {
	return []string{
		"RYSH_SESSION=my-session", "RYSH_TAB=tab-1", "RYSH_LANE=lane-2",
		"RYSH_STACK=stack-3", "RYSH_PANE=pane-9",
	}
}

func TestScriptEnv(t *testing.T) {
	cases := []struct {
		name                string
		sess, tabID, paneID string
		want                map[string]string
		why                 string
	}{
		{
			name: "a script in a pane, no flags, keeps the pane's own address",
			sess: "my-session",
			want: map[string]string{
				"RYSH_SESSION": "my-session", "RYSH_TAB": "tab-1", "RYSH_LANE": "lane-2",
				"RYSH_STACK": "stack-3", "RYSH_PANE": "pane-9",
			},
			why: "writing empty strings over the pane's address would silently re-point " +
				"every ## command at whichever pane is active — a failure that looks like success",
		},
		{
			name: "explicit targeting wins, and takes the rest of the path with it",
			sess: "my-session", tabID: "tab-7", paneID: "pane-4",
			want: map[string]string{
				"RYSH_SESSION": "my-session", "RYSH_TAB": "tab-7", "RYSH_PANE": "pane-4",
				"RYSH_LANE": "", "RYSH_STACK": "",
			},
			why: "pane-4 need not live in lane-2/stack-3, and a half-inherited path " +
				"resolves to a pane nobody asked for",
		},
		{
			name: "another session drops every inherited coordinate",
			sess: "script-1699",
			want: map[string]string{
				"RYSH_SESSION": "script-1699", "RYSH_TAB": "", "RYSH_PANE": "",
				"RYSH_LANE": "", "RYSH_STACK": "",
			},
			why: "--ephemeral boots a fresh session where our pane does not exist; " +
				"carrying the id in is how a script ends up addressing a pane in the wrong session",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := scriptEnv(paneEnv(), "/usr/bin/rysh", tc.sess, tc.tabID, tc.paneID, "/tmp/x.rysh")
			for name, want := range tc.want {
				if got := envValue(env, name); got != want {
					t.Errorf("%s = %q, want %q — %s", name, got, want, tc.why)
				}
			}
			if got := envValue(env, "RYSH_BIN"); got != "/usr/bin/rysh" {
				t.Errorf("RYSH_BIN = %q, want the running binary", got)
			}
		})
	}
}
