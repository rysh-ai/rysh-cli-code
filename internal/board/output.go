// SPDX-License-Identifier: Apache-2.0

package board

// `output`: write the report to a file, print it to PDF if asked, open it.
//
// PDF is printed from the same HTML, never rendered a second way, so the two
// cannot disagree. The converter is whatever is INSTALLED, detected
// explicitly: headless Chrome/Chromium first, then wkhtmltopdf. With neither,
// the HTML is still written and the error names its path — a PDF that
// silently did not happen is the failure this surface must not have.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// OutputDirEnv overrides where output files go.
const OutputDirEnv = "RYSH_BOARD_OUTPUT_DIR"

// DefaultOutputDir is $RYSH_BOARD_OUTPUT_DIR, else $TMPDIR/rysh-board-output.
func DefaultOutputDir() string {
	if d := strings.TrimSpace(os.Getenv(OutputDirEnv)); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "rysh-board-output")
}

// OutputFileName is board-<board>[-<worker>][-last<N>]-<YYYYMMDD-HHMMSS>.<ext>.
// The worker is reduced to a filename-safe form; it is agent-chosen text.
func OutputFileName(board, worker string, n int, now time.Time, ext string) string {
	name := "board-" + fileSafe(board)
	if worker != "" {
		name += "-" + fileSafe(worker)
	}
	if n > 0 {
		name += fmt.Sprintf("-last%d", n)
	}
	return name + "-" + now.Format("20060102-150405") + "." + ext
}

func fileSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// Seams for tests: which programs exist, and running one.
var (
	lookPath   = exec.LookPath
	statExec   = func(p string) bool { i, err := os.Stat(p); return err == nil && !i.IsDir() && i.Mode()&0o111 != 0 }
	runCommand = func(name string, args ...string) error {
		return exec.Command(name, args...).Run()
	}
	startCommand = func(name string, args ...string) error {
		return exec.Command(name, args...).Start()
	}
	goos = runtime.GOOS
)

// chromeCandidates are the headless-Chrome binaries tried, in order.
var chromeCandidates = []string{
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	"/Applications/Chromium.app/Contents/MacOS/Chromium",
	"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome",
}

// ErrNoPDFConverter means neither Chrome/Chromium nor wkhtmltopdf is installed.
var ErrNoPDFConverter = errors.New("no PDF converter found (install Google Chrome/Chromium or wkhtmltopdf)")

// PDFConverter reports which converter would be used ("" when none).
func PDFConverter() (kind, path string) {
	for _, c := range chromeCandidates {
		if filepath.IsAbs(c) {
			if statExec(c) {
				return "chrome", c
			}
			continue
		}
		if p, err := lookPath(c); err == nil {
			return "chrome", p
		}
	}
	if p, err := lookPath("wkhtmltopdf"); err == nil {
		return "wkhtmltopdf", p
	}
	return "", ""
}

// ConvertPDF prints htmlPath to pdfPath. On failure the error says why and
// always names the HTML file, which exists either way.
func ConvertPDF(htmlPath, pdfPath string) error {
	kind, bin := PDFConverter()
	var errs []string
	if kind == "chrome" {
		abs, _ := filepath.Abs(htmlPath)
		if err := runCommand(bin, "--headless=new", "--disable-gpu", "--no-pdf-header-footer",
			"--print-to-pdf="+pdfPath, "file://"+abs); err != nil {
			errs = append(errs, fmt.Sprintf("chrome: %v", err))
		}
		if nonEmpty(pdfPath) {
			return nil
		}
		// Chrome present but failed: wkhtmltopdf is still worth a try.
		if p, err := lookPath("wkhtmltopdf"); err == nil {
			kind, bin = "wkhtmltopdf", p
		}
	}
	if kind == "wkhtmltopdf" {
		if err := runCommand(bin, "-q", "--enable-local-file-access", htmlPath, pdfPath); err != nil {
			errs = append(errs, fmt.Sprintf("wkhtmltopdf: %v", err))
		}
		if nonEmpty(pdfPath) {
			return nil
		}
	}
	if kind == "" {
		return fmt.Errorf("[board] PDF conversion failed: %v; HTML is at %s", ErrNoPDFConverter, htmlPath)
	}
	why := "the converter produced no file"
	if len(errs) > 0 {
		why = strings.Join(errs, "; ")
	}
	return fmt.Errorf("[board] PDF conversion failed (%s); HTML is at %s", why, htmlPath)
}

func nonEmpty(p string) bool {
	i, err := os.Stat(p)
	return err == nil && i.Size() > 0
}

// OpenFile opens path with the platform opener, without waiting for it.
func OpenFile(path string) error {
	switch goos {
	case "darwin":
		return startCommand("open", path)
	case "windows":
		return startCommand("cmd", "/c", "start", "", path)
	default:
		return startCommand("xdg-open", path)
	}
}

// OutputRequest is one `output` run.
type OutputRequest struct {
	Page   HTMLPage
	Format string // html | pdf
	Dir    string // "" = DefaultOutputDir()
	Open   bool
}

// WriteOutput renders, writes, converts and opens. It returns the file that
// was produced. An empty selection is refused before anything is written.
func WriteOutput(req OutputRequest) (string, error) {
	if len(req.Page.Picked) == 0 {
		return "", ErrNoMessages{Worker: req.Page.Worker}
	}
	if req.Format != FormatHTML && req.Format != FormatPDF {
		return "", ErrNeedsFormat
	}
	dir := req.Dir
	if dir == "" {
		dir = DefaultOutputDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("[board] output dir: %w", err)
	}
	page, err := RenderHTML(req.Page)
	if err != nil {
		return "", fmt.Errorf("[board] render: %w", err)
	}
	now := req.Page.Now
	if now.IsZero() {
		now = time.Now()
	}
	htmlPath := filepath.Join(dir, OutputFileName(req.Page.Board, req.Page.Worker, req.Page.N, now, FormatHTML))
	if err := os.WriteFile(htmlPath, page, 0o644); err != nil {
		return "", fmt.Errorf("[board] write %s: %w", htmlPath, err)
	}
	out := htmlPath
	if req.Format == FormatPDF {
		pdfPath := strings.TrimSuffix(htmlPath, ".html") + ".pdf"
		if err := ConvertPDF(htmlPath, pdfPath); err != nil {
			return htmlPath, err
		}
		out = pdfPath
	}
	if req.Open {
		// A failed opener is not a failed output: the file is written and the
		// confirmation names it.
		_ = OpenFile(out)
	}
	return out, nil
}

// OutputSummary is the confirmation every output surface prints.
func OutputSummary(picked, matched int, worker, path string) string {
	return fmt.Sprintf("[board] wrote %d of %d messages%s → %s", picked, matched, fromWorker(worker), path)
}
