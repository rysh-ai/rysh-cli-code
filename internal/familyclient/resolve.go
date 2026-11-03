// SPDX-License-Identifier: Apache-2.0

package familyclient

import (
	"fmt"
	"sort"
	"strings"
)

// AmbiguousError is returned when a query matches more than one family or
// avatar. It is never resolved by guessing.
type AmbiguousError struct {
	What       string // "family" or "avatar"
	Query      string
	Candidates []string // "name (id)"
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("%s %q is ambiguous: it matches %s; use more of the id",
		e.What, e.Query, strings.Join(e.Candidates, ", "))
}

// NotFoundError is returned when a query matches nothing.
type NotFoundError struct {
	What  string
	Query string
	Known []string
}

func (e *NotFoundError) Error() string {
	if len(e.Known) == 0 {
		return fmt.Sprintf("no %s matches %q (there are none)", e.What, e.Query)
	}
	return fmt.Sprintf("no %s matches %q; known: %s", e.What, e.Query, strings.Join(e.Known, ", "))
}

// resolve picks one index from items for query, in this order:
//  1. exact id;
//  2. case-insensitive exact name;
//  3. id prefix.
//
// Each step that matches more than one item is an AmbiguousError; a step
// that matches none falls through to the next.
func resolve(what, query string, n int, id, name func(int) string) (int, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return -1, fmt.Errorf("empty %s name", what)
	}
	label := func(i int) string { return fmt.Sprintf("%s (%s)", name(i), id(i)) }
	steps := []func(int) bool{
		func(i int) bool { return id(i) == q },
		func(i int) bool { return strings.EqualFold(name(i), q) },
		func(i int) bool { return strings.HasPrefix(strings.ToLower(id(i)), strings.ToLower(q)) },
	}
	for _, match := range steps {
		var hits []int
		for i := 0; i < n; i++ {
			if match(i) {
				hits = append(hits, i)
			}
		}
		switch len(hits) {
		case 0:
			continue
		case 1:
			return hits[0], nil
		default:
			c := make([]string, len(hits))
			for k, i := range hits {
				c[k] = label(i)
			}
			sort.Strings(c)
			return -1, &AmbiguousError{What: what, Query: q, Candidates: c}
		}
	}
	known := make([]string, n)
	for i := 0; i < n; i++ {
		known[i] = label(i)
	}
	sort.Strings(known)
	return -1, &NotFoundError{What: what, Query: q, Known: known}
}

// ResolveFamily finds one family by id, case-insensitive name or id prefix.
func ResolveFamily(fams []Family, query string) (Family, error) {
	i, err := resolve("family", query, len(fams),
		func(i int) string { return fams[i].ID },
		func(i int) string { return fams[i].Name })
	if err != nil {
		return Family{}, err
	}
	return fams[i], nil
}

// ResolveAvatar finds one avatar of fam by id, case-insensitive name or id prefix.
func ResolveAvatar(fam Family, query string) (Avatar, error) {
	av := fam.Avatars
	i, err := resolve("avatar", query, len(av),
		func(i int) string { return av[i].ID },
		func(i int) string { return av[i].Name })
	if err != nil {
		return Avatar{}, err
	}
	return av[i], nil
}
