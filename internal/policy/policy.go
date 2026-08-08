// Package policy implements policy-as-code (design 013 §1): governance rules as
// a versioned file in the repo (.rysh/policy.yaml), loaded at session start,
// optionally constrained by an org-level policy file (config `policy.org_file`
// or RYSH_ORG_POLICY).
//
// The format is open (it also underpins CI mode, design 009). Org-level central
// *distribution* is the private/enterprise half and is out of scope here — this
// package only consumes an org file already on disk.
//
// Org merge (Merge) is strictest-wins: a project file may narrow the org
// policy but can never loosen it. Per rule class:
//
//   - approval.always_gate / bash.deny: org rules are evaluated before any
//     project rule, so anything the org gates or denies stays gated/denied no
//     matter what the project file says;
//   - approval.auto_approve / bash.allow (permissive): the lists intersect —
//     when both files are present a call is auto-approved only if BOTH allow
//     it; with only an org file, its permissive rules stand alone;
//   - budget.ceilings: the lower ceiling wins per pane id;
//   - proxy.required: required by either file ⇒ required;
//   - snat.required: the union of both files' required secrets.
//
// Fail-closed: a policy file that exists but fails to parse is NOT silently
// ignored — Load returns a Policy whose Err is set and whose rule sets are
// empty, so nothing is auto-approved on a broken policy. Callers surface the
// error loudly (##policy) rather than running as if governed. The org file is
// held to a stricter standard: one that is configured but MISSING also fails
// closed (LoadOrg) — a governed session silently losing its org policy is a
// tamper signal, not "no rules".
package policy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileName is the repo-level policy file under .rysh/.
const FileName = "policy.yaml"

// Policy is the resolved rule set.
type Policy struct {
	// Bash allowlist adjustments layered over the built-in classifier.
	BashAllow []string
	BashDeny  []string
	// Approval rules: glob/pattern lists.
	AutoApprove []string
	AlwaysGate  []string
	// BudgetCeilings maps a pane/agent id to a hard token ceiling.
	BudgetCeilings map[string]int64
	// ProxyRequired forces the governance proxy on and blocks ##proxy off.
	ProxyRequired bool
	// ProxyStrict escalates the ungoverned-CLI WARNING (design 022 §4.4) to a
	// block: a known agent CLI that runs past the grace window with no governed
	// request is stopped.
	//
	// Separate from ProxyRequired on purpose (022 §8.2). "The proxy must be
	// running" is a fact about rysh and is always safe to enforce. "This CLI is
	// escaping the proxy" is NEGATIVE evidence — an idle CLI is indistinguishable
	// from an escaped one — so acting on it can stop innocent work, and that is
	// a decision an operator has to make deliberately.
	ProxyStrict bool
	// SNATRequired names registered secrets that must be present.
	SNATRequired []string

	// Org overlay (design 013 §1), populated by Merge when an org policy is
	// active. Restrictive org rules are evaluated before any project rule;
	// permissive org lists intersect with the project's — see
	// EvaluateToolApproval.
	OrgBashAllow   []string
	OrgBashDeny    []string
	OrgAutoApprove []string
	OrgAlwaysGate  []string

	// Provenance / status.
	Path      string // project policy file ("" if none)
	Loaded    bool   // a policy file existed and parsed
	Err       error  // set when a policy could not be loaded (fail-closed)
	OrgPath   string // org policy file ("" when no org policy is configured)
	OrgActive bool   // an org policy parsed and its constraints are merged in

	// projectLoaded distinguishes, inside a merged policy, "no project file"
	// (org permissive rules stand alone) from "project file present" (permissive
	// rules intersect — a project that declares no allowlist auto-approves
	// nothing, it does not inherit the org's).
	projectLoaded bool
}

// yamlPolicy is the on-disk shape.
type yamlPolicy struct {
	Bash struct {
		Allow []string `yaml:"allow"`
		Deny  []string `yaml:"deny"`
	} `yaml:"bash"`
	Approval struct {
		AutoApprove []string `yaml:"auto_approve"`
		AlwaysGate  []string `yaml:"always_gate"`
	} `yaml:"approval"`
	Budget struct {
		Ceilings map[string]int64 `yaml:"ceilings"`
	} `yaml:"budget"`
	Proxy struct {
		Required bool `yaml:"required"`
		Strict   bool `yaml:"strict"`
	} `yaml:"proxy"`
	SNAT struct {
		Required []string `yaml:"required"`
	} `yaml:"snat"`
}

// Empty returns an empty policy (no file present).
func Empty() *Policy {
	return &Policy{BudgetCeilings: map[string]int64{}}
}

// Load reads .rysh/policy.yaml. A missing file yields an empty policy (not an
// error). A present-but-unparseable file yields an empty policy with Err set
// (fail-closed): the caller must surface Err and must not treat the session as
// governed.
func Load(ryshDir string) *Policy {
	path := filepath.Join(ryshDir, FileName)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Empty()
	}
	if err != nil {
		p := Empty()
		p.Path, p.Err = path, err
		return p
	}
	p, perr := parse(data, path)
	if perr != nil {
		p = Empty()
		p.Path, p.Err = path, fmt.Errorf("parse %s: %w", FileName, perr)
	}
	return p
}

// LoadOrg reads the org-level policy file named by config `policy.org_file` /
// RYSH_ORG_POLICY. An empty path means no org policy is configured: nil is
// returned and Merge leaves the project policy untouched (pre-org behavior).
//
// Stricter than Load: a configured org file that is MISSING is an error, not
// an empty policy — an absent org policy is a tamper signal (the file the org
// mandates was deleted or diverted), so it fails closed exactly like an
// unparseable one.
func LoadOrg(path string) *Policy {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		p := Empty()
		p.Path = path
		if os.IsNotExist(err) {
			p.Err = fmt.Errorf("org policy %s is configured (policy.org_file / RYSH_ORG_POLICY) but missing — refusing to run without it", path)
		} else {
			p.Err = fmt.Errorf("org policy %s: %w", path, err)
		}
		return p
	}
	p, perr := parse(data, path)
	if perr != nil {
		p = Empty()
		p.Path, p.Err = path, fmt.Errorf("parse org policy %s: %w", path, perr)
	}
	return p
}

// parse decodes a policy document anchored at path.
func parse(data []byte, path string) (*Policy, error) {
	var yp yamlPolicy
	if err := yaml.Unmarshal(data, &yp); err != nil {
		return nil, err
	}
	p := &Policy{
		BashAllow:      yp.Bash.Allow,
		BashDeny:       yp.Bash.Deny,
		AutoApprove:    yp.Approval.AutoApprove,
		AlwaysGate:     yp.Approval.AlwaysGate,
		BudgetCeilings: yp.Budget.Ceilings,
		ProxyRequired:  yp.Proxy.Required,
		ProxyStrict:    yp.Proxy.Strict,
		SNATRequired:   yp.SNAT.Required,
		Path:           path,
		Loaded:         true,
	}
	if p.BudgetCeilings == nil {
		p.BudgetCeilings = map[string]int64{}
	}
	return p, nil
}

// Merge layers an org policy over the project policy, strictest-wins (the
// package comment states the per-class rules). org == nil means no org policy
// is configured: project is returned unchanged, so the not-configured path
// behaves exactly as before the org feature existed.
//
// A load error on either side poisons the merge: the result carries Err (and
// the failing file's Path) with no rules, so the fail-closed gate engages. A
// broken project file must not drop the org constraints any more than a broken
// org file may run ungoverned.
func Merge(org, project *Policy) *Policy {
	if org == nil {
		return project
	}
	if project == nil {
		project = Empty()
	}
	if org.Err != nil || project.Err != nil {
		m := Empty()
		m.Err = errors.Join(org.Err, project.Err)
		m.OrgPath = org.Path
		if org.Err != nil {
			m.Path = org.Path
		} else {
			m.Path = project.Path
		}
		return m
	}
	m := &Policy{
		BashAllow:      project.BashAllow,
		BashDeny:       project.BashDeny,
		AutoApprove:    project.AutoApprove,
		AlwaysGate:     project.AlwaysGate,
		OrgBashAllow:   org.BashAllow,
		OrgBashDeny:    org.BashDeny,
		OrgAutoApprove: org.AutoApprove,
		OrgAlwaysGate:  org.AlwaysGate,
		BudgetCeilings: map[string]int64{},
		ProxyRequired:  org.ProxyRequired || project.ProxyRequired,
		// Strictest wins, like every other merged rule: a project file cannot
		// switch off an org's strict mode by omitting it.
		ProxyStrict:   org.ProxyStrict || project.ProxyStrict,
		SNATRequired:  unionStrings(org.SNATRequired, project.SNATRequired),
		Path:          project.Path,
		Loaded:        true,
		OrgPath:       org.Path,
		OrgActive:     true,
		projectLoaded: project.Loaded,
	}
	// Budget ceilings: union of pane ids; where both bind, the LOWER wins.
	for id, c := range org.BudgetCeilings {
		m.BudgetCeilings[id] = c
	}
	for id, c := range project.BudgetCeilings {
		if cur, ok := m.BudgetCeilings[id]; !ok || c < cur {
			m.BudgetCeilings[id] = c
		}
	}
	return m
}

// unionStrings appends the items of b not already in a, preserving order.
func unionStrings(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	seen := make(map[string]bool, len(a))
	out := append([]string(nil), a...)
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Summary renders a human-readable overview for ##policy.
func (p *Policy) Summary() string {
	if p.Err != nil {
		return fmt.Sprintf("policy: FAILED to load %s\n  %v\n  (fail-closed: no rules applied — fix the file and ##policy reload)\n", p.Path, p.Err)
	}
	if !p.Loaded {
		return "policy: none (.rysh/policy.yaml not present)\n"
	}
	var out string
	switch {
	case p.OrgActive && p.projectLoaded:
		out = fmt.Sprintf("policy: loaded from %s\n", p.Path)
		out += fmt.Sprintf("  org policy:    ACTIVE — %s (strictest wins: org gates/denies stick, allowlists intersect, lower budget wins)\n", p.OrgPath)
	case p.OrgActive:
		out = fmt.Sprintf("policy: org policy only — %s (no .rysh/policy.yaml)\n", p.OrgPath)
	default:
		out = fmt.Sprintf("policy: loaded from %s\n", p.Path)
	}
	if len(p.BashAllow) > 0 {
		out += fmt.Sprintf("  bash allow:    %v\n", p.BashAllow)
	}
	if len(p.BashDeny) > 0 {
		out += fmt.Sprintf("  bash deny:     %v\n", p.BashDeny)
	}
	if len(p.AutoApprove) > 0 {
		out += fmt.Sprintf("  auto-approve:  %v\n", p.AutoApprove)
	}
	if len(p.AlwaysGate) > 0 {
		out += fmt.Sprintf("  always-gate:   %v\n", p.AlwaysGate)
	}
	if len(p.BudgetCeilings) > 0 {
		out += fmt.Sprintf("  budget ceils:  %v (applied to the usage ledger)\n", p.BudgetCeilings)
	}
	if p.ProxyRequired {
		out += "  proxy:         REQUIRED (##proxy off blocked)\n"
	}
	if p.ProxyStrict {
		out += "  proxy strict:  ON (an ungoverned agent CLI is stopped, not just warned)\n"
	}
	if len(p.SNATRequired) > 0 {
		out += fmt.Sprintf("  snat required: %v\n", p.SNATRequired)
	}
	if p.OrgActive {
		if len(p.OrgBashAllow) > 0 {
			out += fmt.Sprintf("  org bash allow:   %v (intersected with project allow)\n", p.OrgBashAllow)
		}
		if len(p.OrgBashDeny) > 0 {
			out += fmt.Sprintf("  org bash deny:    %v (cannot be overridden)\n", p.OrgBashDeny)
		}
		if len(p.OrgAutoApprove) > 0 {
			out += fmt.Sprintf("  org auto-approve: %v (intersected with project auto-approve)\n", p.OrgAutoApprove)
		}
		if len(p.OrgAlwaysGate) > 0 {
			out += fmt.Sprintf("  org always-gate:  %v (cannot be overridden)\n", p.OrgAlwaysGate)
		}
	}
	return out
}
