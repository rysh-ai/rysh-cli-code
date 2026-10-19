// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rysh-ai/rysh-cli-shared/forge/ir"
)

// Diff is the operation-level delta between two versions of an API spec — the
// basis of spec-driven maintenance ("what changed when the spec changed").
type Diff struct {
	Added   []string // operation ids present only in the new spec
	Removed []string // operation ids present only in the old spec
	Changed []string // operation ids whose method/path/params/mutating changed

	// Stream deltas are reported SEPARATELY from the operation deltas above,
	// never merged into them. Two reasons, both load-bearing:
	//
	//   - The consequences differ. A removed stream breaks a background
	//     streaming session; a removed operation breaks a tool call. A caller
	//     deciding whether to adopt a new spec needs to tell them apart.
	//   - The id spaces overlap. ir.API.Streams and ir.API.Operations are
	//     separate lists of ir.Operation, so one id can name both a unary
	//     operation and a stream. Merging would collide and report one while
	//     hiding the other.
	StreamsAdded   []string // stream ids present only in the new spec
	StreamsRemoved []string // stream ids present only in the old spec
	StreamsChanged []string // stream ids whose signature changed
}

// Empty reports whether the two specs are equivalent across every surface the
// diff covers — operations AND streams.
func (d Diff) Empty() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Changed) == 0 &&
		len(d.StreamsAdded) == 0 && len(d.StreamsRemoved) == 0 && len(d.StreamsChanged) == 0
}

// DiffAPIs computes the delta from oldAPI to newAPI, over both the unary
// operations and the streaming methods (gRPC server-streaming, GraphQL
// subscriptions). The two surfaces are diffed independently — see Diff.
func DiffAPIs(oldAPI, newAPI *ir.API) Diff {
	var d Diff
	d.Added, d.Removed, d.Changed = diffOpSets(opsOf(oldAPI), opsOf(newAPI))
	d.StreamsAdded, d.StreamsRemoved, d.StreamsChanged = diffOpSets(streamsOf(oldAPI), streamsOf(newAPI))
	return d
}

// diffOpSets compares two id→operation indexes and returns sorted added,
// removed and changed id lists. Operations and streams share this because they
// are the same ir.Operation shape; keeping one implementation is what stops a
// signature rule from applying to one surface and not the other.
func diffOpSets(oldOps, newOps map[string]*ir.Operation) (added, removed, changed []string) {
	for id := range newOps {
		if _, ok := oldOps[id]; !ok {
			added = append(added, id)
		}
	}
	for id, o := range oldOps {
		n, ok := newOps[id]
		if !ok {
			removed = append(removed, id)
			continue
		}
		if opSignature(o) != opSignature(n) {
			changed = append(changed, id)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return added, removed, changed
}

func opsOf(api *ir.API) map[string]*ir.Operation {
	if api == nil {
		return map[string]*ir.Operation{}
	}
	return indexOps(api.Operations)
}

func streamsOf(api *ir.API) map[string]*ir.Operation {
	if api == nil {
		return map[string]*ir.Operation{}
	}
	return indexOps(api.Streams)
}

func indexOps(ops []ir.Operation) map[string]*ir.Operation {
	m := make(map[string]*ir.Operation, len(ops))
	for i := range ops {
		op := &ops[i]
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
