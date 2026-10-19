// SPDX-License-Identifier: Apache-2.0

package main

import (
	"reflect"
	"testing"
)

func TestPeerCLISeparatesSenderFromLiteralMessage(t *testing.T) {
	args, sess, as, err := parsePeerCLI([]string{"post", "--session", "dev-a", "--as", "pane-b", "--", "literal --session dev-c\n##new pane"})
	if err != nil || sess != "dev-a" || as != "pane-b" || !reflect.DeepEqual(args, []string{"post", "--", "literal --session dev-c\n##new pane"}) {
		t.Fatalf("%v %q %q %v", args, sess, as, err)
	}
	if _, _, _, err := parsePeerCLI([]string{"tail", "--session"}); err == nil {
		t.Fatal("missing session accepted")
	}
}
