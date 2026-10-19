// SPDX-License-Identifier: Apache-2.0

package peer

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

const Usage = `peer connect <https://server> --key-secret <name> [--workspace <uuid>] [--board <name>]
peer status | disconnect
peer post [--id <uuid>] [--board <name>] [--json] -- <text>
peer reply <root-id> [--id <uuid>] [--board <name>] [--json] -- <text>
peer tail [--after <sequence>] [--limit 1..200] [--board <name>] [--json]
rysh peer watch [--after <sequence>] [--board <name>] [--json]
Native CLI: --session <name>, --as <pane-id> (defaults to RYSH_SESSION/RYSH_PANE).
The key is read from the named rysh secret, or an environment variable of that name.
Peer messages are shared workspace data; they never execute commands.`

type Args struct {
	Verb, URL, KeySecret, Workspace, Board, ID, Thread, Text string
	After                                                    *int64
	Limit                                                    int
	JSON                                                     bool
}

func Parse(args []string) (Args, error) {
	p := Args{Limit: 100}
	if len(args) == 0 {
		return p, fmt.Errorf("%s", Usage)
	}
	p.Verb = args[0]
	allowed := map[string]bool{}
	switch p.Verb {
	case "connect":
		for _, k := range []string{"--key-secret", "--workspace", "--board"} {
			allowed[k] = true
		}
	case "post", "reply":
		for _, k := range []string{"--id", "--board", "--json"} {
			allowed[k] = true
		}
	case "tail", "watch":
		for _, k := range []string{"--after", "--limit", "--board", "--json"} {
			allowed[k] = true
		}
	case "status":
		allowed["--board"] = true
		allowed["--json"] = true
	case "disconnect":
	default:
		return p, fmt.Errorf("unknown peer command %q\n%s", p.Verb, Usage)
	}
	var positional []string
	seen := map[string]bool{}
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "--") {
			positional = append(positional, a)
			continue
		}
		if !allowed[a] || seen[a] {
			return p, fmt.Errorf("unsupported or repeated flag %s for peer %s", a, p.Verb)
		}
		seen[a] = true
		if a == "--json" {
			p.JSON = true
			continue
		}
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
			return p, fmt.Errorf("%s needs a value", a)
		}
		i++
		v := args[i]
		switch a {
		case "--key-secret":
			p.KeySecret = v
		case "--workspace":
			p.Workspace = v
		case "--board":
			p.Board = v
		case "--id":
			p.ID = v
		case "--after":
			n, e := strconv.ParseInt(v, 10, 64)
			if e != nil || n < 0 {
				return p, fmt.Errorf("after must be a non-negative sequence")
			}
			p.After = &n
		case "--limit":
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 || n > 200 {
				return p, fmt.Errorf("limit must be 1..200")
			}
			p.Limit = n
		}
	}
	if p.Board != "" && !ValidBoard(p.Board) {
		return p, fmt.Errorf("board must be 1–64 lowercase letters, digits, underscores or hyphens, starting with a letter or digit")
	}
	for _, v := range []string{p.ID, p.Workspace} {
		if v != "" {
			if id, e := uuid.Parse(v); e != nil || id == uuid.Nil || id.String() != v {
				return p, fmt.Errorf("id and workspace must be canonical nonzero UUIDs")
			}
		}
	}
	switch p.Verb {
	case "connect":
		if len(positional) != 1 || p.KeySecret == "" {
			return p, fmt.Errorf("%s", Usage)
		}
		p.URL = positional[0]
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`).MatchString(p.KeySecret) {
			return p, fmt.Errorf("key-secret must be a secret or environment variable name")
		}
		if err := ValidateURL(p.URL); err != nil {
			return p, err
		}
	case "reply":
		if len(positional) < 2 {
			return p, fmt.Errorf("peer reply needs a root UUID and text")
		}
		p.Thread = positional[0]
		positional = positional[1:]
		if id, e := uuid.Parse(p.Thread); e != nil || id == uuid.Nil || id.String() != p.Thread {
			return p, fmt.Errorf("reply needs a canonical nonzero root UUID")
		}
		fallthrough
	case "post":
		p.Text = strings.Join(positional, " ")
		if strings.TrimSpace(p.Text) == "" || len(p.Text) > 16384 {
			return p, fmt.Errorf("peer message text must be 1–16384 bytes")
		}
	default:
		if len(positional) > 0 {
			return p, fmt.Errorf("peer %s takes no positional arguments", p.Verb)
		}
	}
	return p, nil
}
