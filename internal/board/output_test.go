// SPDX-License-Identifier: Apache-2.0

package board

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// PDF conversion depends on what is installed, so every exec is stubbed.

type fakeExec struct {
	installed map[string]bool // names or absolute paths
	ran       [][]string
	opened    []string
	produce   bool // the converter writes the pdf
}

func stubExec(t *testing.T, f *fakeExec) {
	t.Helper()
	pl, se, rc, sc, g := lookPath, statExec, runCommand, startCommand, goos
	lookPath = func(name string) (string, error) {
		if f.installed[name] {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	statExec = func(p string) bool { return f.installed[p] }
	runCommand = func(name string, args ...string) error {
		f.ran = append(f.ran, append([]string{name}, args...))
		if f.produce {
			for _, a := range args {
				if out, ok := strings.CutPrefix(a, "--print-to-pdf="); ok {
					return os.WriteFile(out, []byte("%PDF"), 0o644)
				}
			}
			if strings.HasSuffix(name, "wkhtmltopdf") {
				return os.WriteFile(args[len(args)-1], []byte("%PDF"), 0o644)
			}
		}
		return errors.New("exit 1")
	}
	startCommand = func(name string, args ...string) error {
		f.opened = append(f.opened, strings.Join(append([]string{name}, args...), " "))
		return nil
	}
	goos = "darwin"
	t.Cleanup(func() { lookPath, statExec, runCommand, startCommand, goos = pl, se, rc, sc, g })
}

func outputReq(t *testing.T, format string) OutputRequest {
	items := Flatten(reply(fleet()))
	picked, matched, total := Select(items, "issue-314", 1)
	return OutputRequest{
		Page: HTMLPage{Board: "etl-fix", Worker: "issue-314", N: 1, Picked: picked, Matched: matched, Total: total,
			Now: time.Date(2026, 9, 30, 13, 4, 5, 0, time.Local)},
		Format: format, Dir: t.TempDir(), Open: true,
	}
}

func TestOutputFileName(t *testing.T) {
	now := time.Date(2026, 9, 30, 13, 4, 5, 0, time.Local)
	if got := OutputFileName("etl-fix", "issue-314", 3, now, "pdf"); got != "board-etl-fix-issue-314-last3-20260930-130405.pdf" {
		t.Fatalf("name = %q", got)
	}
	if got := OutputFileName("etl-fix", "", 0, now, "html"); got != "board-etl-fix-20260930-130405.html" {
		t.Fatalf("name = %q", got)
	}
	if got := OutputFileName("b", "../../etc", 0, now, "html"); strings.Contains(got, "/") {
		t.Fatalf("a worker name escaped the directory: %q", got)
	}
	t.Setenv(OutputDirEnv, "/somewhere")
	if DefaultOutputDir() != "/somewhere" {
		t.Fatal("RYSH_BOARD_OUTPUT_DIR ignored")
	}
}

func TestWriteOutputHTMLOpens(t *testing.T) {
	f := &fakeExec{}
	stubExec(t, f)
	req := outputReq(t, FormatHTML)
	path, err := WriteOutput(req)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "board-etl-fix-issue-314-last1-20260930-130405.html" {
		t.Fatalf("path = %s", path)
	}
	page, _ := os.ReadFile(path)
	if !strings.Contains(string(page), "last 1 of 2") {
		t.Fatalf("page meta wrong")
	}
	if len(f.opened) != 1 || f.opened[0] != "open "+path {
		t.Fatalf("opened %q", f.opened)
	}
	req.Open = false
	f.opened = nil
	if _, err := WriteOutput(req); err != nil || len(f.opened) != 0 {
		t.Fatalf("--no-open still opened: %q", f.opened)
	}
}

func TestWriteOutputPDFPrefersChrome(t *testing.T) {
	f := &fakeExec{installed: map[string]bool{"google-chrome": true, "wkhtmltopdf": true}, produce: true}
	stubExec(t, f)
	path, err := WriteOutput(outputReq(t, FormatPDF))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, ".pdf") || len(f.ran) != 1 || f.ran[0][0] != "/usr/bin/google-chrome" ||
		!strings.Contains(strings.Join(f.ran[0], " "), "--headless=new --disable-gpu --no-pdf-header-footer --print-to-pdf=") {
		t.Fatalf("ran %q → %s", f.ran, path)
	}
	if len(f.opened) != 1 || !strings.HasSuffix(f.opened[0], ".pdf") {
		t.Fatalf("opened %q", f.opened)
	}
}

func TestWriteOutputPDFFallsBackToWkhtmltopdf(t *testing.T) {
	f := &fakeExec{installed: map[string]bool{"wkhtmltopdf": true}, produce: true}
	stubExec(t, f)
	path, err := WriteOutput(outputReq(t, FormatPDF))
	if err != nil || !strings.HasSuffix(path, ".pdf") || f.ran[0][0] != "/usr/bin/wkhtmltopdf" {
		t.Fatalf("ran %q → %s, %v", f.ran, path, err)
	}
}

// TestWriteOutputWithoutAConverterNamesTheHTML: never fail silently.
func TestWriteOutputWithoutAConverterNamesTheHTML(t *testing.T) {
	f := &fakeExec{}
	stubExec(t, f)
	path, err := WriteOutput(outputReq(t, FormatPDF))
	if err == nil {
		t.Fatal("a PDF with no converter reported success")
	}
	if !strings.HasSuffix(path, ".html") || !strings.Contains(err.Error(), path) ||
		!strings.Contains(err.Error(), "PDF conversion failed") {
		t.Fatalf("err = %v (html %s)", err, path)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatal("the HTML the error names does not exist")
	}
	if len(f.opened) != 0 {
		t.Fatal("opened something although the PDF failed")
	}

	// A converter that runs but produces nothing is a failure too.
	f2 := &fakeExec{installed: map[string]bool{"chromium": true}}
	stubExec(t, f2)
	if _, err := WriteOutput(outputReq(t, FormatPDF)); err == nil || !strings.Contains(err.Error(), "HTML is at") {
		t.Fatalf("a converter that produced nothing = %v", err)
	}
}

func TestWriteOutputRefusesAnEmptySelection(t *testing.T) {
	stubExec(t, &fakeExec{})
	req := outputReq(t, FormatHTML)
	req.Page.Picked = nil
	if _, err := WriteOutput(req); err == nil {
		t.Fatal("an empty page was written")
	}
	entries, _ := os.ReadDir(req.Dir)
	if len(entries) != 0 {
		t.Fatalf("files written for an empty selection: %v", entries)
	}
	if got := OutputSummary(1, 2, "issue-314", "/tmp/x.pdf"); got != "[board] wrote 1 of 2 messages from issue-314 → /tmp/x.pdf" {
		t.Fatalf("summary = %q", got)
	}
}
