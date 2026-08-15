// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ApplyJQ trims a JSON document using a deliberately small subset of jq path
// syntax — enough for the common "trim a large response before it enters the
// model's context" case, without embedding a full jq engine:
//
//	.            identity (whole document)
//	.field       object field access
//	.a.b.c       nested field access
//	.arr[0]      array index
//	.arr[]       iterate an array (maps the rest of the path over each element)
//	.data[].id   project one field from each element of an array
//
// It returns the selected value rendered as indented JSON.
func ApplyJQ(data []byte, filter string) (string, error) {
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return "", fmt.Errorf("response is not JSON: %w", err)
	}
	filter = strings.TrimSpace(filter)
	if filter == "" || filter == "." {
		return indent(root)
	}
	if !strings.HasPrefix(filter, ".") {
		return "", fmt.Errorf("filter must start with '.'")
	}

	tokens, err := tokenizePath(filter)
	if err != nil {
		return "", err
	}
	out, err := evalPath(root, tokens)
	if err != nil {
		return "", err
	}
	return indent(out)
}

type pathToken struct {
	field   string // object field (empty if index/iter)
	index   int    // array index when isIndex
	isIndex bool
	isIter  bool // "[]" iterate
}

func tokenizePath(filter string) ([]pathToken, error) {
	// Strip leading "."
	s := filter[1:]
	var tokens []pathToken
	for len(s) > 0 {
		switch {
		case strings.HasPrefix(s, "[]"):
			tokens = append(tokens, pathToken{isIter: true})
			s = s[2:]
		case strings.HasPrefix(s, "["):
			end := strings.IndexByte(s, ']')
			if end < 0 {
				return nil, fmt.Errorf("unclosed '[' in filter")
			}
			idxStr := s[1:end]
			idx, err := strconv.Atoi(idxStr)
			if err != nil {
				return nil, fmt.Errorf("non-integer array index %q", idxStr)
			}
			tokens = append(tokens, pathToken{isIndex: true, index: idx})
			s = s[end+1:]
		case strings.HasPrefix(s, "."):
			s = s[1:] // separator between fields
		default:
			// read a field name up to the next . or [
			end := strings.IndexAny(s, ".[")
			if end < 0 {
				end = len(s)
			}
			name := s[:end]
			if name == "" {
				return nil, fmt.Errorf("empty field in filter")
			}
			tokens = append(tokens, pathToken{field: name})
			s = s[end:]
		}
	}
	return tokens, nil
}

func evalPath(v any, tokens []pathToken) (any, error) {
	if len(tokens) == 0 {
		return v, nil
	}
	tok := tokens[0]
	rest := tokens[1:]

	switch {
	case tok.isIter:
		arr, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("[] applied to non-array")
		}
		out := make([]any, 0, len(arr))
		for _, el := range arr {
			sub, err := evalPath(el, rest)
			if err != nil {
				return nil, err
			}
			out = append(out, sub)
		}
		return out, nil

	case tok.isIndex:
		arr, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("[%d] applied to non-array", tok.index)
		}
		idx := tok.index
		if idx < 0 {
			idx += len(arr)
		}
		if idx < 0 || idx >= len(arr) {
			return nil, fmt.Errorf("index %d out of range (len %d)", tok.index, len(arr))
		}
		return evalPath(arr[idx], rest)

	default: // field
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("field %q applied to non-object", tok.field)
		}
		next, ok := obj[tok.field]
		if !ok {
			return nil, fmt.Errorf("field %q not found", tok.field)
		}
		return evalPath(next, rest)
	}
}

func indent(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
