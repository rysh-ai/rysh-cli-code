package forge

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
)

// Diff is the operation-level delta between two versions of an API spec — the
// basis of spec-driven maintenance ("what changed when the spec changed").
type Diff struct {
	Added   []string // operation ids present only in the new spec
	Removed []string // operation ids present only in the old spec
	Changed []string // operation ids whose method/path/params/mutating changed
}

// Empty reports whether the two specs are operation-equivalent.
func (d Diff) Empty() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Changed) == 0
}

// DiffAPIs computes the operation-level delta from oldAPI to newAPI.
func DiffAPIs(oldAPI, newAPI *ir.API) Diff {
	oldOps := indexOps(oldAPI)
	newOps := indexOps(newAPI)
	var d Diff
	for id := range newOps {
		if _, ok := oldOps[id]; !ok {
			d.Added = append(d.Added, id)
		}
	}
	for id, o := range oldOps {
		n, ok := newOps[id]
		if !ok {
			d.Removed = append(d.Removed, id)
			continue
		}
		if opSignature(o) != opSignature(n) {
			d.Changed = append(d.Changed, id)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Removed)
	sort.Strings(d.Changed)
	return d
}

func indexOps(api *ir.API) map[string]*ir.Operation {
	m := map[string]*ir.Operation{}
	if api == nil {
		return m
	}
	for i := range api.Operations {
		op := &api.Operations[i]
		m[op.ID] = op
	}
	return m
}

// opSignature is a stable fingerprint of an operation's externally-visible shape.
func opSignature(op *ir.Operation) string {
	params := make([]string, 0, len(op.Params))
	for _, p := range op.Params {
		req := ""
		if p.Required {
			req = "!"
		}
		params = append(params, p.In+":"+p.Name+req)
	}
	if op.RequestBody != nil {
		params = append(params, "body")
	}
	sort.Strings(params)
	return fmt.Sprintf("%s %s [%s] mut=%v", op.Method, op.Path, strings.Join(params, ","), op.Mutating)
}
