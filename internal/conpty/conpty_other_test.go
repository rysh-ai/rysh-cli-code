// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package conpty

import (
	"errors"
	"testing"
)

// Off Windows the package is the same API, refusing honestly, so the actor
// layer can be written once against it.
func TestStartUnsupported(t *testing.T) {
	c, err := Start([]string{"cmd.exe"}, "", nil, Size{Rows: 24, Cols: 80})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Start err = %v, want ErrUnsupported", err)
	}
	if c != nil {
		t.Fatal("Start returned a console off Windows")
	}
	var nilConsole *Console
	if err := nilConsole.Resize(Size{Rows: 1, Cols: 1}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Resize err = %v, want ErrUnsupported", err)
	}
	if err := nilConsole.Close(); err != nil {
		t.Fatalf("Close on the stub = %v, want nil", err)
	}
}
