// SPDX-License-Identifier: Apache-2.0

package actors

import "testing"

func TestDaemonConfigArg(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"rysh", "daemon", "dev", "--config", "/w/rysh.config.yaml"}, "/w/rysh.config.yaml"},
		{[]string{"rysh", "daemon", "dev", "--config=/w/c.yaml"}, "/w/c.yaml"},
		{[]string{"rysh", "daemon", "dev"}, ""},
		{[]string{"rysh", "daemon", "dev", "--config"}, ""},
	}
	for _, c := range cases {
		if got := daemonConfigArg(c.argv); got != c.want {
			t.Errorf("daemonConfigArg(%v) = %q, want %q", c.argv, got, c.want)
		}
	}
}
