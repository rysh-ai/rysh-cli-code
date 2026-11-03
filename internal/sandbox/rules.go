// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

// rules.go — the hand-off from the policy fs rules (design 068) to the sandbox.
//
// The daemon owns the policy: it loads policy.yaml and the org file, and
// publishes the compiled rules (policy.SetActiveFS). The sandbox is started by
// `rysh exec-clean`, a different process that cannot see them. So every time
// the daemon publishes rules it also writes them, converted, to
// <rysh>/sandbox/fs-rules.json, and exec-clean reads that file. A missing or
// unreadable file, a policy that failed to load, or a rule the sandbox cannot
// enforce exactly all refuse the start: the sandbox never runs with a posture
// looser than the policy.
//
// Rules only RESTRICT what the sandbox already exposes (the workspace, the
// shared .git, the profile's read_only extras). A read_only rule never mounts a
// path the sandbox would not show anyway, and read_write never opens a path
// outside the workspace.

// RulesFileName is the published rules, under <rysh>/sandbox/.
const RulesFileName = "fs-rules.json"

const rulesVersion = 1

// RulesFile is the on-disk hand-off. Paths are absolute, clean and literal; a
// rule the conversion could not make literal is kept in Unenforceable, which
// refuses the start (and names the rule) rather than dropping it.
type RulesFile struct {
	Version    int    `json:"version"`
	FailClosed bool   `json:"fail_closed,omitempty"`
	Reason     string `json:"reason,omitempty"`
	// ReadOnly and Deny restrict; ReadWrite are carve-outs inside a read_only
	// path, already checked against the full policy (org layer included).
	ReadOnly      []string `json:"read_only"`
	ReadWrite     []string `json:"read_write"`
	Deny          []string `json:"deny"`
	Unenforceable []string `json:"unenforceable,omitempty"`
}

// RulesPath is where the daemon publishes the rules for ryshDir.
func RulesPath(ryshDir string) string {
	return filepath.Join(ryshDir, "sandbox", RulesFileName)
}

// RulesFromFS converts the daemon's compiled fs rules. nil (no fs section)
// gives an empty rule set.
func RulesFromFS(r *policy.FSRules) RulesFile {
	out := RulesFile{Version: rulesVersion, ReadOnly: []string{}, ReadWrite: []string{}, Deny: []string{}}
	if r == nil {
		return out
	}
	m := r.Mounts()
	if m.FailClosed {
		out.FailClosed = true
		out.Reason = "the policy failed to load"
		return out
	}
	lit := func(class string, globs []string, dst *[]string) {
		for _, g := range globs {
			p, ok := literalRule(g)
			if !ok {
				out.Unenforceable = append(out.Unenforceable, "fs."+class+" "+g)
				continue
			}
			*dst = appendPath(*dst, p)
		}
	}
	lit("deny", m.Deny, &out.Deny)
	lit("read_only", m.ReadOnly, &out.ReadOnly)
	for _, g := range m.ReadWrite {
		p, ok := literalRule(g)
		// A wildcard carve-out is dropped, not refused: without it the path
		// stays read-only, which is stricter than the policy, never looser.
		// A carve-out another layer still forbids (an org read_only the
		// project tries to open) is dropped for the same reason.
		if !ok || r.Check(p, policy.FSWrite) != nil {
			continue
		}
		out.ReadWrite = appendPath(out.ReadWrite, p)
	}
	return out
}

// literalRule makes an expanded glob a plain path: "dir/**" covers exactly
// what "dir" covers (a glob matching a directory covers everything beneath
// it). Anything still holding a wildcard is not literal.
func literalRule(g string) (string, bool) {
	g = strings.TrimSpace(g)
	for strings.HasSuffix(g, "/**") {
		g = strings.TrimSuffix(g, "/**")
	}
	if g == "" || !filepath.IsAbs(g) || strings.ContainsAny(g, "*?[") {
		return "", false
	}
	return filepath.Clean(g), true
}

func appendPath(list []string, p string) []string {
	for _, have := range list {
		if have == p {
			return list
		}
	}
	return append(list, p)
}

// WriteRules publishes f for ryshDir (0600, written to a temp file and renamed
// so exec-clean never reads half a file). On any failure it removes the old
// file, so a stale posture is never read: exec-clean then refuses to start.
func WriteRules(ryshDir string, f RulesFile) (err error) {
	path := RulesPath(ryshDir)
	defer func() {
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fs-rules-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return renameFile(tmp.Name(), path)
}

// renameFile is os.Rename; tests replace it to make a publish fail.
var renameFile = os.Rename

// LoadRules reads the published rules for ryshDir and refuses anything the
// sandbox cannot enforce exactly.
func LoadRules(ryshDir string) (PathRules, error) {
	path := RulesPath(ryshDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return PathRules{}, fmt.Errorf("sandbox: the policy path rules for %s were never published (%s is missing) — start the agent from a running rysh session", ryshDir, path)
		}
		return PathRules{}, fmt.Errorf("sandbox: read %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f RulesFile
	if err := dec.Decode(&f); err != nil {
		return PathRules{}, fmt.Errorf("sandbox: %s: %w", path, err)
	}
	if f.Version != rulesVersion {
		return PathRules{}, fmt.Errorf("sandbox: %s: version %d, want %d", path, f.Version, rulesVersion)
	}
	if f.FailClosed {
		reason := f.Reason
		if reason == "" {
			reason = "the policy failed to load"
		}
		return PathRules{}, fmt.Errorf("sandbox: %s, so the path rules are unknown — fix policy.yaml and run ##policy reload", reason)
	}
	if len(f.Unenforceable) > 0 {
		return PathRules{}, fmt.Errorf("sandbox: the policy has fs rules a sandbox cannot enforce exactly (wildcards): %s — name the paths literally (a trailing /** is fine)", strings.Join(f.Unenforceable, ", "))
	}
	for _, list := range [][]string{f.ReadOnly, f.ReadWrite, f.Deny} {
		for _, p := range list {
			if p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, "*?[") {
				return PathRules{}, fmt.Errorf("sandbox: %s: %q is not an absolute, literal path", path, p)
			}
		}
	}
	return PathRules{ReadOnly: withResolved(f.ReadOnly), ReadWrite: withResolved(f.ReadWrite), Deny: withResolved(f.Deny)}, nil
}

// withResolved adds, for every path, its form with the longest existing prefix
// symlink-resolved (macOS /var -> /private/var, a workspace reached through a
// link). The sandbox compares rules with a resolved workspace; a rule kept only
// in its unresolved form would silently match nothing.
func withResolved(list []string) []string {
	out := []string{}
	for _, p := range list {
		out = appendPath(out, p)
		out = appendPath(out, resolvePrefix(p))
	}
	return out
}

func resolvePrefix(p string) string {
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// layered is the policy's read_only / read_write paths in the order their
// verdicts stack: shallowest first, so a deeper rule overrides the one it sits
// in (with literal paths, deeper means more literal characters, which is
// design 068's precedence). At the same path read_only wins.
type layeredRule struct {
	Path     string
	ReadOnly bool
}

func layered(r PathRules) []layeredRule {
	var out []layeredRule
	for _, p := range r.ReadWrite {
		out = append(out, layeredRule{Path: p})
	}
	for _, p := range r.ReadOnly {
		out = append(out, layeredRule{Path: p, ReadOnly: true})
	}
	sort.SliceStable(out, func(i, j int) bool {
		di, dj := depth(out[i].Path), depth(out[j].Path)
		if di != dj {
			return di < dj
		}
		return !out[i].ReadOnly && out[j].ReadOnly
	})
	return out
}

// verdictReadOnly reports whether the policy makes p read-only: the deepest
// read_only/read_write rule covering p decides.
func verdictReadOnly(p string, r PathRules) bool {
	ro := false
	for _, l := range layered(r) {
		if within(p, l.Path) {
			ro = l.ReadOnly
		}
	}
	return ro
}

// deniedByRules reports whether p is (inside) a deny path.
func deniedByRules(p string, r PathRules) bool {
	for _, d := range r.Deny {
		if within(p, d) {
			return true
		}
	}
	return false
}
