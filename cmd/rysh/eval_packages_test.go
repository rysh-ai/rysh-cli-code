package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/eval"
)

// TestPackageEvalFixturesLoad is the packages/ variant of
// TestCheckedInFixturesLoad: every seed package (design 005 G4) ships an eval
// suite under packages/<name>/evals/<case>/, and each case must load, carry an
// until: goal, assert something — and fail a null run. That last check is the
// convention that keeps the suites honest: a fixture a do-nothing agent passes
// grades nothing.
func TestPackageEvalFixturesLoad(t *testing.T) {
	pkgRoot := "../../packages"
	entries, err := os.ReadDir(pkgRoot)
	if err != nil {
		t.Fatal(err)
	}
	pkgs := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pkgs++
		evalsDir := filepath.Join(pkgRoot, e.Name(), "evals")
		cases, err := discoverCases(evalsDir)
		if err != nil || len(cases) == 0 {
			t.Errorf("%s: every seed package ships an eval suite (evals/<case>/task.md + expect.yaml): %v", e.Name(), err)
			continue
		}
		for _, dir := range cases {
			c, err := eval.LoadCase(dir)
			if err != nil {
				t.Errorf("LoadCase(%s): %v", dir, err)
				continue
			}
			if c.Prompt == "" {
				t.Errorf("%s: empty prompt", dir)
			}
			if c.Until == "" {
				t.Errorf("%s: task.md frontmatter must carry an until: goal", dir)
			}
			if eval.Passed(eval.Evaluate(c.Expect, eval.Result{})) {
				t.Errorf("%s: a null run (no output, no files, no commands) passes — the case grades nothing", dir)
			}
		}
	}
	if pkgs < 10 {
		t.Errorf("expected the 10 G4 seed packages under %s, found %d", pkgRoot, pkgs)
	}
}
