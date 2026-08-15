// SPDX-License-Identifier: Apache-2.0

// Command rysh-script is a shebang shim: it runs `rysh script` with whatever
// arguments it was given.
//
// It exists for one reason. The natural shebang for a .rysh file is
//
//	#!/usr/bin/env -S rysh script
//
// but `env -S` needs coreutils >= 8.30 (2018). A kernel hands a shebang line to
// the interpreter as at most ONE argument, so without -S the "script" word
// would arrive glued to the path and nothing would run. Shipping a binary whose
// name IS the interpreter sidesteps the whole problem:
//
//	#!/usr/bin/env rysh-script
//
// which works everywhere, including the BSDs and older distributions.
//
// It deliberately does no work of its own — it finds the rysh binary and execs
// it — so there is no second implementation of the language to drift.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	bin, err := findRysh()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rysh-script: %v\n", err)
		os.Exit(1)
	}

	args := append([]string{"script"}, os.Args[1:]...)
	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			// The script's exit code is the whole point; pass it through.
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "rysh-script: %v\n", err)
		os.Exit(1)
	}
}

// ryshBinaryNames are the names the main binary ships under: "rysh" for the
// open-source build, "ry" for the closed one.
//
// They are listed literally rather than derived with progname.Rewrite, which
// resolves the name from argv[0] — this binary's own. Asking it produces
// "rysh-script", so the shim looked for itself, found itself, and exec'd itself
// forever. Hence also the self-check in findRysh: a shim that can re-exec
// itself is a fork bomb one rename away.
var ryshBinaryNames = []string{"rysh", "ry"}

// findRysh locates the main rysh binary, preferring one installed beside this
// shim so a local build is not silently driven by a different rysh on PATH.
func findRysh() (string, error) {
	self, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}

	sameAsSelf := func(path string) bool {
		if self == "" {
			return false
		}
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		return path == self
	}

	if self != "" {
		for _, name := range ryshBinaryNames {
			candidate := filepath.Join(filepath.Dir(self), name)
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() && !sameAsSelf(candidate) {
				return candidate, nil
			}
		}
	}
	for _, name := range ryshBinaryNames {
		if path, err := exec.LookPath(name); err == nil && !sameAsSelf(path) {
			return path, nil
		}
	}
	return "", fmt.Errorf("cannot find the rysh binary (looked for %v beside this shim and on PATH)",
		ryshBinaryNames)
}

func asExitError(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}
