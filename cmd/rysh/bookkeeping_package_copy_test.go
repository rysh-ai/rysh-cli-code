// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// U-27 Q6 (founder 2026-11-05, AI analysis): Books stays software the owner
// operates. So no package name or text says "accountant" (c), and the
// bookkeeping skill keeps limits L3 (own data and public deadlines only; never
// statutory accounts, a tax computation, a VAT return or a confirmation
// statement) and L6 (no tax advice on the owner's own position: general rules
// and GOV.UK pointers only); E-136: it says it can be wrong.
func TestNoPackageSaysAccountant(t *testing.T) {
	err := filepath.WalkDir("../../packages", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(strings.ToLower(d.Name()), "accountant") {
			t.Errorf("%s: a package path says accountant", path)
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(strings.ToLower(string(b)), "accountant") {
			t.Errorf("%s says accountant", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBookkeepingSkillKeepsItsLimits(t *testing.T) {
	pkg, err := os.ReadFile("../../packages/bookkeeping/rysh.pkg.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pkg), `name: "@rysh/bookkeeping"`) {
		t.Errorf("package name is not @rysh/bookkeeping:\n%s", pkg)
	}
	b, err := os.ReadFile("../../packages/bookkeeping/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	skill := strings.Join(strings.Fields(string(b)), " ") // wrapping-insensitive
	for _, want := range []string{
		"name: bookkeeping",
		// L3
		"statutory accounts", "tax computation", "VAT return", "confirmation statement",
		// L6
		"GOV.UK", "tell the owner what they should claim",
		// E-136
		"I can be wrong",
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("SKILL.md lacks %q", want)
		}
	}
}
