package mcp

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseAddArgs parses the `##mcp add` argument list (everything after "add")
// into a ServerDef.
//
// Grammar:
//
//	<name> http  <url>       [flags...]
//	<name> stdio <cmd> [args... up to the first --flag] [flags...]
//
// Flags: --approve, --prefix <p>, --max-tools <n>, --header <k:v> (http,
// repeatable), --env <k=v> (stdio, repeatable). For stdio, command arguments
// beginning with "--" cannot be expressed (they read as flags); single-dash
// args like "-y" are fine.
func ParseAddArgs(args []string) (ServerDef, error) {
	var def ServerDef
	usage := fmt.Errorf("usage: ##mcp add <name> http <url> | ##mcp add <name> stdio <cmd> [args]")
	if len(args) < 2 {
		return def, usage
	}
	def.Name = args[0]
	if strings.HasPrefix(def.Name, "-") || def.Name == "" {
		return def, fmt.Errorf("invalid server name %q", def.Name)
	}

	i := 1
	switch args[i] {
	case "http", "https":
		def.Transport = TransportHTTP
		i++
		if i >= len(args) {
			return def, fmt.Errorf("http transport requires a url")
		}
		def.URL = args[i]
		i++
	case "stdio":
		def.Transport = TransportStdio
		i++
		if i >= len(args) {
			return def, fmt.Errorf("stdio transport requires a command")
		}
		def.Command = args[i]
		i++
		for i < len(args) && !strings.HasPrefix(args[i], "--") {
			def.Args = append(def.Args, args[i])
			i++
		}
	default:
		return def, fmt.Errorf("transport must be 'http' or 'stdio', got %q", args[i])
	}

	for i < len(args) {
		flag := args[i]
		switch flag {
		case "--approve":
			def.RequiresApproval = true
			i++
		case "--prefix":
			i++
			if i >= len(args) {
				return def, fmt.Errorf("--prefix requires a value")
			}
			p := args[i]
			def.Prefix = &p
			i++
		case "--max-tools":
			i++
			if i >= len(args) {
				return def, fmt.Errorf("--max-tools requires a number")
			}
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 0 {
				return def, fmt.Errorf("--max-tools must be a non-negative integer, got %q", args[i])
			}
			def.MaxTools = n
			i++
		case "--header":
			i++
			if i >= len(args) {
				return def, fmt.Errorf("--header requires K:V")
			}
			k, v, ok := strings.Cut(args[i], ":")
			if !ok || strings.TrimSpace(k) == "" {
				return def, fmt.Errorf("--header must be K:V, got %q", args[i])
			}
			if def.Headers == nil {
				def.Headers = map[string]string{}
			}
			def.Headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
			i++
		case "--env":
			i++
			if i >= len(args) {
				return def, fmt.Errorf("--env requires K=V")
			}
			if !strings.Contains(args[i], "=") {
				return def, fmt.Errorf("--env must be K=V, got %q", args[i])
			}
			def.Env = append(def.Env, args[i])
			i++
		default:
			return def, fmt.Errorf("unknown flag %q", flag)
		}
	}

	return def, def.Validate()
}
