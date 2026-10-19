// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestRestartAttachArgs(t *testing.T) {
	got := restartAttachArgs("dev", "/w/rysh.config.yaml", "debug")
	want := []string{"attach", "dev", "--upgrade", "--force", "--config", "/w/rysh.config.yaml", "--log-level", "debug"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	got = restartAttachArgs("dev", "", "")
	want = []string{"attach", "dev", "--upgrade", "--force"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// main() finds the request with errors.As, so it must survive wrapping.
func TestRestartRequestUnwraps(t *testing.T) {
	err := fmt.Errorf("attach: %w", &restartRequest{session: "dev"})
	var rr *restartRequest
	if !errors.As(err, &rr) || rr.session != "dev" {
		t.Fatalf("errors.As lost the restart request: %v", err)
	}
}
