// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"path/filepath"
	"testing"
)

func TestFSBash_Check(t *testing.T) {
	root := realTemp(t)
	r := loadFSPolicy(t, `
fs:
  read_only: ["**"]
  read_write: ["src/**", "out"]
  deny: [".env", "secrets"]
`).ResolveFS(root)

	refused := []string{
		"cat .env",
		"cat ./secrets/db.txt",
		`grep -r token "secrets"`,
		"ls src && cat .env",
		"echo x > README.md",
		"echo x >> docs/notes.md",
		"echo x 2>docs/err.log",
		"echo x >docs/attached",
		"rm docs/guide.md",
		"rm -rf docs",
		"mv src/a.go docs/a.go",
		"cp src/a.go docs/",
		"touch docs/new",
		"sed -i s/a/b/ README.md",
		"dd if=/dev/zero of=docs/blob bs=1 count=1",
		"cd docs && rm guide.md",
		"sudo rm docs/x",
		"FOO=1 tee docs/x",
		"cat src/../.env",
		"cat " + filepath.Join(root, ".env"),
	}
	for _, cmd := range refused {
		if err := r.CheckBash(cmd, root); err == nil {
			t.Errorf("want refusal: %s", cmd)
		}
	}
	allowed := []string{
		"ls -la",
		"cat README.md docs/guide.md",
		"go test ./...",
		"echo x > src/new.go",
		"cp docs/guide.md out/guide.md",
		"rm -rf out/build",
		"git commit -m 'touch docs'",
		"cat $FILE",
	}
	for _, cmd := range allowed {
		if err := r.CheckBash(cmd, root); err != nil {
			t.Errorf("want allowed: %s: %v", cmd, err)
		}
	}
}

// A bare word is read as a path relative to cwd, so it is checked against deny
// even without a slash (a false positive such as "echo secrets" is the price of
// not missing "ls secrets").
func TestFSBash_BareWordNamesPath(t *testing.T) {
	root := realTemp(t)
	r := loadFSPolicy(t, "fs:\n  deny: [secrets]\n").ResolveFS(root)
	if err := r.CheckBash("ls secrets", root); err == nil {
		t.Error("ls secrets must be refused")
	}
}

func TestFSBash_NoRulesIsToday(t *testing.T) {
	var r *FSRules
	for _, cmd := range []string{"cat .env", "rm -rf /tmp/x", "echo x > y"} {
		if err := r.CheckBash(cmd, "/"); err != nil {
			t.Errorf("nil rules must allow %q: %v", cmd, err)
		}
	}
}
