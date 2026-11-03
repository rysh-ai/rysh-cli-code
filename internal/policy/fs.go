// SPDX-License-Identifier: Apache-2.0

package policy

// Filesystem path rules (design 068): the `fs:` section of policy.yaml.
//
//	fs:
//	  read_only:  ["**"]
//	  read_write: ["src/**", "out"]
//	  deny:       [".env", "secrets", "$HOME/.ssh"]
//
// Globs are relative to the workspace root (the parent of .rysh/); a leading
// $HOME or ~ expands; absolute globs stay absolute. `*` and `?` match within one
// path segment, `**` matches any number of segments. A glob that matches a
// directory covers everything beneath it.
//
// Rules only restrict. `deny` beats everything; between read_only and
// read_write the more specific rule (more literal characters) wins, and a tie
// goes to read_only. A path no rule matches keeps today's behaviour, so
// read_write can only carve an exception out of a broader read_only — it never
// grants anything rysh refuses without an fs section. Org and project rules are
// separate layers and the strictest verdict of the two wins, so a project file
// cannot carve out of an org rule.
//
// The single verdict function is (*FSRules).Check. rysh's own file tools, file
// browse and the bash pre-check call it through CheckFS; the sandbox (#13)
// builds its mounts from (*FSRules).Mounts.

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

// FSSpec is the on-disk `fs:` section, as written (globs unexpanded).
type FSSpec struct {
	ReadOnly  []string `yaml:"read_only"`
	ReadWrite []string `yaml:"read_write"`
	Deny      []string `yaml:"deny"`
}

func (s FSSpec) empty() bool {
	return len(s.ReadOnly) == 0 && len(s.ReadWrite) == 0 && len(s.Deny) == 0
}

// FSAccess is the kind of access a caller asks for.
type FSAccess int

const (
	FSRead FSAccess = iota
	FSWrite
)

func (a FSAccess) String() string {
	if a == FSWrite {
		return "write"
	}
	return "read"
}

type fsKind int

// Ordered by strictness: a higher kind wins when verdicts are combined.
const (
	fsNone fsKind = iota
	fsReadWrite
	fsReadOnly
	fsDeny
)

// FSDeniedError is the refusal returned by Check. Its message is the one a
// tool shows the agent and the log records.
type FSDeniedError struct {
	Rule   string // e.g. "fs.deny[0]" or "org.fs.read_only[1]"
	Glob   string // the glob as written in the policy file
	Path   string
	Access FSAccess
}

func (e *FSDeniedError) Error() string {
	if e.Rule == "" {
		return fmt.Sprintf("policy: fs rules are unknown (%s) — refusing %s of %s; fix the file and run ##policy reload", e.Glob, e.Access, e.Path)
	}
	return fmt.Sprintf("policy: %s (%q) refuses %s of %s", e.Rule, e.Glob, e.Access, e.Path)
}

// parseFS decodes the `fs:` node strictly: an unknown key, a wrong type or a
// glob validateFSGlob rejects fails the whole policy file (fail-closed). The
// other sections keep their lenient decoding; only fs is held to this.
func parseFS(node *yaml.Node) (FSSpec, error) {
	var spec FSSpec
	if node == nil || node.Kind == 0 {
		return spec, nil
	}
	if node.Kind != yaml.MappingNode {
		return spec, errors.New("fs: must be a mapping with read_only, read_write and deny")
	}
	raw, err := yaml.Marshal(node)
	if err != nil {
		return spec, fmt.Errorf("fs: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return FSSpec{}, fmt.Errorf("fs: %w", err)
	}
	for class, globs := range map[string][]string{"read_only": spec.ReadOnly, "read_write": spec.ReadWrite, "deny": spec.Deny} {
		for i, g := range globs {
			if err := validateFSGlob(g); err != nil {
				return FSSpec{}, fmt.Errorf("fs.%s[%d] %q: %w", class, i, g, err)
			}
		}
	}
	return spec, nil
}

func validateFSGlob(g string) error {
	g = strings.TrimSpace(g)
	switch {
	case g == "":
		return errors.New("empty glob")
	case strings.ContainsAny(g, "\x00\\[]{}"):
		return errors.New(`unsupported character (only *, ** and ? are wildcards; no \ [ ] { })`)
	case strings.HasPrefix(g, "~") && g != "~" && !strings.HasPrefix(g, "~/"):
		return errors.New("~user is not supported; use ~/ or $HOME/")
	}
	rest := g
	for _, pfx := range []string{"${HOME}", "$HOME"} {
		if rest == pfx || strings.HasPrefix(rest, pfx+"/") {
			rest = strings.TrimPrefix(rest, pfx)
			break
		}
	}
	if strings.Contains(rest, "$") {
		return errors.New("only a leading $HOME is expanded")
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == ".." {
			return errors.New(`".." is not allowed in a glob`)
		}
	}
	return nil
}

// fsPattern is one compiled glob, anchored to an absolute path.
type fsPattern struct {
	id      string
	glob    string // as written
	abs     string // absolute, expanded
	segs    []string
	literal int
	kind    fsKind
}

type fsLayer struct {
	patterns []fsPattern
}

// FSRules is the resolved, absolute form of a policy's fs rules. A nil
// *FSRules means no fs rules: every access keeps today's behaviour.
type FSRules struct {
	Root   string
	layers []fsLayer
	// failReason is set by FailClosedFS: the policy did not load, so the rules
	// are unknown and every access is refused.
	failReason string
	mounts     FSMounts
}

// FSMounts is the hand-off to the sandbox (#13): absolute, $HOME-expanded
// globs per class, org and project merged. Globs may still contain wildcards;
// a literal entry is a plain path to mount. FailClosed means the policy failed
// to load and the sandbox must not start with an unknown posture.
type FSMounts struct {
	ReadOnly   []string
	ReadWrite  []string
	Deny       []string
	FailClosed bool
}

// ResolveFS compiles the policy's fs rules against the workspace root. It
// returns nil when neither the project nor the org file has an fs section (or
// the policy failed to load — callers publish FailClosedFS for that case).
func (p *Policy) ResolveFS(root string) *FSRules {
	if p == nil || p.Err != nil {
		return nil
	}
	type src struct {
		prefix string
		spec   FSSpec
	}
	var srcs []src
	if p.OrgActive && !p.OrgFS.empty() {
		srcs = append(srcs, src{"org.", p.OrgFS})
	}
	if !p.FS.empty() {
		srcs = append(srcs, src{"", p.FS})
	}
	if len(srcs) == 0 {
		return nil
	}
	roots := pathVariants(root)
	homes := []string{""}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		homes = pathVariants(h)
	}
	r := &FSRules{Root: roots[0]}
	for _, s := range srcs {
		var l fsLayer
		add := func(class string, kind fsKind, globs []string, mount *[]string) {
			for i, g := range globs {
				id := fmt.Sprintf("%sfs.%s[%d]", s.prefix, class, i)
				for vi, abs := range expandGlob(strings.TrimSpace(g), roots, homes) {
					if vi == 0 {
						*mount = appendUnique(*mount, abs)
					}
					l.patterns = append(l.patterns, fsPattern{
						id: id, glob: g, abs: abs, segs: splitSegs(abs),
						literal: literalCount(abs), kind: kind,
					})
				}
			}
		}
		add("deny", fsDeny, s.spec.Deny, &r.mounts.Deny)
		add("read_only", fsReadOnly, s.spec.ReadOnly, &r.mounts.ReadOnly)
		add("read_write", fsReadWrite, s.spec.ReadWrite, &r.mounts.ReadWrite)
		r.layers = append(r.layers, l)
	}
	return r
}

// FailClosedFS is published when the policy failed to load: rules are unknown,
// so every checked access is refused (agents are already blocked by the
// fail-closed gate; this also covers file browse).
func FailClosedFS(reason string) *FSRules {
	if reason == "" {
		reason = "policy failed to load"
	}
	return &FSRules{failReason: reason, mounts: FSMounts{FailClosed: true}}
}

// Mounts returns the hand-off for the sandbox (#13).
func (r *FSRules) Mounts() FSMounts {
	if r == nil {
		return FSMounts{}
	}
	m := r.mounts
	m.ReadOnly = append([]string(nil), m.ReadOnly...)
	m.ReadWrite = append([]string(nil), m.ReadWrite...)
	m.Deny = append([]string(nil), m.Deny...)
	return m
}

// Check is the single fs verdict. p should be absolute (a relative path is
// made absolute against the process cwd). The cleaned path and its
// symlink-resolved form are both checked; the stricter verdict wins.
func (r *FSRules) Check(p string, access FSAccess) error {
	if r == nil {
		return nil
	}
	if r.failReason != "" {
		return &FSDeniedError{Glob: r.failReason, Path: p, Access: access}
	}
	worst, var0 := fsNone, fsPattern{}
	for _, c := range fsCandidates(p) {
		segs := splitSegs(c)
		for _, l := range r.layers {
			k, pat := l.verdict(segs)
			if k > worst {
				worst, var0 = k, pat
			}
		}
	}
	if worst == fsDeny || (worst == fsReadOnly && access == FSWrite) {
		return &FSDeniedError{Rule: var0.id, Glob: var0.glob, Path: p, Access: access}
	}
	return nil
}

// verdict returns this layer's kind for a path: any deny wins; otherwise the
// most literal read_only/read_write match, a tie going to read_only.
func (l fsLayer) verdict(segs []string) (fsKind, fsPattern) {
	best, bestPat := fsNone, fsPattern{}
	for _, pat := range l.patterns {
		if !coversSegs(pat.segs, segs) {
			continue
		}
		if pat.kind == fsDeny {
			return fsDeny, pat
		}
		if best == fsNone || pat.literal > bestPat.literal ||
			(pat.literal == bestPat.literal && pat.kind > best) {
			best, bestPat = pat.kind, pat
		}
	}
	return best, bestPat
}

// coversSegs reports whether the pattern matches the path or one of its
// ancestors (a directory rule covers what is beneath it).
func coversSegs(pat, p []string) bool {
	for k := len(p); k >= 0; k-- {
		if matchSegs(pat, p[:k]) {
			return true
		}
	}
	return false
}

func matchSegs(pat, p []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(p); i++ {
				if matchSegs(pat[1:], p[i:]) {
					return true
				}
			}
			return false
		}
		if len(p) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], p[0]); err != nil || !ok {
			return false
		}
		pat, p = pat[1:], p[1:]
	}
	return len(p) == 0
}

func splitSegs(abs string) []string {
	abs = filepath.ToSlash(abs)
	var out []string
	for _, s := range strings.Split(abs, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func literalCount(g string) int {
	n := 0
	for _, c := range g {
		if c != '*' && c != '?' {
			n++
		}
	}
	return n
}

// expandGlob anchors a validated glob: $HOME/~ to each home variant, an
// absolute glob as is, anything else to each root variant.
func expandGlob(g string, roots, homes []string) []string {
	var bases []string
	rest := g
	switch {
	case g == "~" || strings.HasPrefix(g, "~/"):
		bases, rest = homes, strings.TrimPrefix(g, "~")
	case g == "$HOME" || strings.HasPrefix(g, "$HOME/"):
		bases, rest = homes, strings.TrimPrefix(g, "$HOME")
	case g == "${HOME}" || strings.HasPrefix(g, "${HOME}/"):
		bases, rest = homes, strings.TrimPrefix(g, "${HOME}")
	case filepath.IsAbs(g):
		return []string{path.Clean(filepath.ToSlash(g))}
	default:
		bases = roots
	}
	var out []string
	for _, b := range bases {
		if b == "" {
			// Home is unknown: anchor somewhere that matches nothing real
			// rather than to the root, which would widen the rule.
			b = "/nonexistent-home"
		}
		out = appendUnique(out, path.Clean(filepath.ToSlash(b)+"/"+strings.TrimPrefix(rest, "/")))
	}
	return out
}

// pathVariants returns the cleaned absolute path and, when it differs, its
// symlink-resolved form (macOS /var → /private/var, a workspace reached
// through a link).
func pathVariants(p string) []string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	p = filepath.Clean(p)
	out := []string{p}
	return appendUnique(out, resolveExisting(p))
}

// fsCandidates is what Check matches: the cleaned path, the cleaned path with
// its longest existing prefix resolved, and the raw path resolved by the OS
// rules (a symlink before a "..").
func fsCandidates(p string) []string {
	raw := p
	if !filepath.IsAbs(raw) {
		if wd, err := os.Getwd(); err == nil {
			raw = wd + string(filepath.Separator) + raw
		}
	}
	out := pathVariants(raw)
	if r, err := filepath.EvalSymlinks(raw); err == nil {
		out = appendUnique(out, r)
	}
	return out
}

// resolveExisting resolves symlinks in the longest existing prefix of p and
// re-appends the rest, so a file that does not exist yet is judged by where
// it would be created.
func resolveExisting(p string) string {
	cur, rest := p, ""
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			if rest == "" {
				return r
			}
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		if rest == "" {
			rest = filepath.Base(cur)
		} else {
			rest = filepath.Join(filepath.Base(cur), rest)
		}
		cur = parent
	}
}

func appendUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

// The active rules, published process-wide by WorkspaceActor on policy load
// and every ##policy reload — the same reach problem the fail-closed gate
// solves (gate.go): tools, file browse and the bash pre-check cannot see
// WorkspaceActor state.
var activeFS atomic.Pointer[FSRules]

// SetActiveFS publishes the rules (nil = no fs rules).
func SetActiveFS(r *FSRules) { activeFS.Store(r) }

// ActiveFS returns the published rules, or nil. The sandbox (#13) reads its
// mounts from ActiveFS().Mounts().
func ActiveFS() *FSRules { return activeFS.Load() }

// CheckFS checks a path against the active rules and logs a refusal at error
// level, like the other policy refusals. nil means allowed.
func CheckFS(p string, access FSAccess) error {
	err := ActiveFS().Check(p, access)
	if err != nil {
		var d *FSDeniedError
		rule := ""
		if errors.As(err, &d) {
			rule = d.Rule
		}
		slog.Error("policy: fs access refused", "rule", rule, "access", access.String(), "path", p)
	}
	return err
}

func (s FSSpec) summary(prefix, note string) string {
	var out string
	if len(s.ReadOnly) > 0 {
		out += fmt.Sprintf("  %sfs read-only:  %v%s\n", prefix, s.ReadOnly, note)
	}
	if len(s.ReadWrite) > 0 {
		out += fmt.Sprintf("  %sfs read-write: %v%s\n", prefix, s.ReadWrite, note)
	}
	if len(s.Deny) > 0 {
		out += fmt.Sprintf("  %sfs deny:       %v%s\n", prefix, s.Deny, note)
	}
	return out
}

// fsSummary renders the fs lines of ##policy.
func (p *Policy) fsSummary() string {
	out := p.FS.summary("", "")
	if p.OrgActive {
		out += p.OrgFS.summary("org ", " (cannot be overridden)")
	}
	if out != "" {
		out += "                 fs globs are relative to the workspace root (the parent of .rysh/); deny beats all,\n" +
			"                 the more specific of read-only/read-write wins; enforced in rysh's file tools and file\n" +
			"                 browse; the bash check reads the command text only and is not a sandbox\n"
	}
	return out
}
