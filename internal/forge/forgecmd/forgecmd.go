// SPDX-License-Identifier: Apache-2.0

// Package forgecmd implements the `forge` subcommand family (add / generate /
// list / diff / targets) independently of any particular front-end. It writes
// all human-readable output to an io.Writer so the same logic backs both the
// `rysh forge` shell CLI and the in-session `##forge` command.
package forgecmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/forge"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/gen"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/ingest"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/runtime"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/toolpack"

	// Blank imports register the generator targets via their init() functions.
	// Importing forgecmd therefore makes every target available to gen.Get /
	// gen.Names, whether the importer is the CLI or the daemon.
	_ "github.com/rysh-ai/rysh-cli-code/internal/forge/gen/docs"
	_ "github.com/rysh-ai/rysh-cli-code/internal/forge/gen/gosdk"
	_ "github.com/rysh-ai/rysh-cli-code/internal/forge/gen/javasdk"
	_ "github.com/rysh-ai/rysh-cli-code/internal/forge/gen/mcpserver"
	_ "github.com/rysh-ai/rysh-cli-code/internal/forge/gen/pysdk"
	_ "github.com/rysh-ai/rysh-cli-code/internal/forge/gen/tssdk"
)

// Run executes a forge subcommand in workDir, writing output to w:
//
//	add <name> <spec-file> [flags]   ingest a spec + generate artifacts
//	add <name> --graphql-url URL     introspect a running GraphQL endpoint
//	add <name> --grpc-target H:P     discover a running gRPC server (reflection)
//	generate <name> [flags]          re-generate from the stored spec
//	list                             list configured integrations
//	diff <name> <new-spec-file>      operation-level changes vs the stored spec
//	targets                          list available generator targets
//
// Spec-file paths are resolved relative to workDir when not absolute, so the
// daemon (`##forge`) and the CLI (`rysh forge`) behave identically.
func Run(workDir string, args []string, w io.Writer) error {
	if len(args) == 0 {
		Usage(w)
		return nil
	}
	switch args[0] {
	case "add":
		return forgeAdd(workDir, args[1:], w)
	case "generate", "gen":
		return forgeGenerate(workDir, args[1:], w)
	case "list", "ls":
		return forgeList(workDir, w)
	case "diff":
		return forgeDiff(workDir, args[1:], w)
	case "targets":
		fmt.Fprintln(w, "Available generator targets:")
		for _, n := range gen.Names() {
			fmt.Fprintf(w, "  %s\n", n)
		}
		return nil
	default:
		Usage(w)
		return fmt.Errorf("unknown forge subcommand %q", args[0])
	}
}

// Usage writes the forge command help to w.
func Usage(w io.Writer) {
	fmt.Fprint(w, `forge — turn an API spec into governed agent tools, SDKs, docs, and an MCP server

Usage:
  forge add <name> <spec-file> [flags]    ingest an OpenAPI/GraphQL/gRPC spec, store it, and generate artifacts
  forge add <name> --graphql-url URL      introspect a RUNNING GraphQL endpoint instead of reading a file
  forge add <name> --grpc-target H:P      discover a RUNNING gRPC server via server reflection
  forge generate <name> [flags]           re-generate artifacts from the stored spec
  forge list                              list configured integrations
  forge diff <name> <new-spec-file>       show operation-level changes vs the stored spec
  forge targets                           list available generator targets

Live-introspection flags (add only):
  --graphql-url URL          POST the standard introspection query to URL and ingest the result
  --grpc-target HOST:PORT    pull FileDescriptorProtos over gRPC server reflection (v1, then v1alpha)
  --grpc-tls                 dial the gRPC target with TLS (default: plaintext)
  --header "K: V"            extra header (GraphQL) / call metadata (gRPC); repeatable
  --timeout DUR              live introspection timeout (default 30s)

Add/generate flags:
  --source openapi|graphql|grpc  spec format (default: inferred, else openapi)
  --targets t1,t2            comma-separated targets (default: rysh-toolpack,docs)
  --out DIR                  output directory (default: .rysh/forge/<name>/gen)
  --base-url URL             override the spec's server URL (for live execution)
  --ws-url URL               websocket URL for GraphQL subscriptions (default: base URL, http→ws)
  --cred-env VAR             env var holding the API key/token (live execution)
  --mode auto|static|dynamic exposure mode (default: auto — dynamic above ~50 ops)
  --tags a,b                 expose only operations with these tags
  --read-only                expose only GET/HEAD operations
  --max-tools N              cap statically-registered tools (default 200)
  --module PATH              Go module path for the go-sdk target

After 'forge add', enable live tools inside rysh with:  ##integration enable <name>
`)
}

type forgeFlags struct {
	// Live introspection (design 015 B4): forge a RUNNING endpoint instead of
	// a spec file. The fetched schema bytes are stored exactly like a
	// user-supplied file, so generate/diff/enable behave identically.
	graphqlURL string
	grpcTarget string
	grpcTLS    bool
	headers    headerFlags
	timeout    time.Duration

	source   string
	targets  string
	out      string
	baseURL  string
	wsURL    string
	credEnv  string
	mode     string
	tags     string
	readOnly bool
	maxTools int
	module   string

	// Token auth (forged-API auth plan, Phase A / ##forge add). Secrets are
	// ENV-VAR NAMES only.
	auth            string // none|static|jwt_login|oauth2_password|oauth2_client_credentials
	tokenURL        string
	refreshURL      string
	authContentType string // form|json
	userEnv         string
	passEnv         string
	clientIDEnv     string
	clientSecretEnv string
	accessTokenEnv  string
	refreshTokenEnv string
	scope           string
	accessField     string
	refreshField    string
	expiresInField  string
	authHeader      string
	authScheme      string
	expiryMargin    string
}

func bindForgeFlags(fs *flag.FlagSet) *forgeFlags {
	f := &forgeFlags{}
	fs.StringVar(&f.graphqlURL, "graphql-url", "", "introspect a running GraphQL endpoint at this URL")
	fs.StringVar(&f.grpcTarget, "grpc-target", "", "discover a running gRPC server (host:port) via server reflection")
	fs.BoolVar(&f.grpcTLS, "grpc-tls", false, "dial the gRPC target with TLS (default plaintext)")
	fs.Var(&f.headers, "header", `extra "Key: Value" header/metadata for live introspection (repeatable)`)
	fs.DurationVar(&f.timeout, "timeout", 30*time.Second, "live introspection timeout")
	fs.StringVar(&f.source, "source", "", "spec format: openapi|graphql|grpc")
	fs.StringVar(&f.targets, "targets", "rysh-toolpack,docs", "comma-separated generator targets")
	fs.StringVar(&f.out, "out", "", "output directory")
	fs.StringVar(&f.baseURL, "base-url", "", "override server base URL")
	fs.StringVar(&f.wsURL, "ws-url", "", "override the websocket URL for GraphQL subscriptions (default: base URL with http→ws / https→wss)")
	fs.StringVar(&f.credEnv, "cred-env", "", "env var holding the credential")
	fs.StringVar(&f.mode, "mode", "auto", "exposure mode: auto|static|dynamic")
	fs.StringVar(&f.tags, "tags", "", "comma-separated tag filter")
	fs.BoolVar(&f.readOnly, "read-only", false, "expose only GET/HEAD operations")
	fs.IntVar(&f.maxTools, "max-tools", 0, "cap statically-registered tools")
	fs.StringVar(&f.module, "module", "", "Go module path for the go-sdk target")
	// Token-auth flags.
	fs.StringVar(&f.auth, "auth", "", "token auth: static|jwt_login|oauth2_password|oauth2_client_credentials")
	fs.StringVar(&f.tokenURL, "token-url", "", "login/token endpoint URL")
	fs.StringVar(&f.refreshURL, "refresh-url", "", "refresh endpoint URL (defaults to token-url)")
	fs.StringVar(&f.authContentType, "auth-content-type", "", "token request body: form|json")
	fs.StringVar(&f.userEnv, "user-env", "", "env var holding the username")
	fs.StringVar(&f.passEnv, "pass-env", "", "env var holding the password")
	fs.StringVar(&f.clientIDEnv, "client-id-env", "", "env var holding the OAuth2 client id")
	fs.StringVar(&f.clientSecretEnv, "client-secret-env", "", "env var holding the OAuth2 client secret")
	fs.StringVar(&f.accessTokenEnv, "access-token-env", "", "env var holding a static access token")
	fs.StringVar(&f.refreshTokenEnv, "refresh-token-env", "", "env var holding a static refresh token")
	fs.StringVar(&f.scope, "scope", "", "OAuth2 scope")
	fs.StringVar(&f.accessField, "access-token-field", "", "response field path for the access token")
	fs.StringVar(&f.refreshField, "refresh-token-field", "", "response field path for the refresh token")
	fs.StringVar(&f.expiresInField, "expires-in-field", "", "response field path for expires_in")
	fs.StringVar(&f.authHeader, "auth-header", "", "header to inject the token into (default Authorization)")
	fs.StringVar(&f.authScheme, "auth-scheme", "", "scheme prefix for the token (default Bearer)")
	fs.StringVar(&f.expiryMargin, "expiry-margin", "", "refresh this long before expiry (e.g. 30s)")
	return f
}

// headerFlags collects repeatable --header "Key: Value" flags for live
// introspection (HTTP headers for GraphQL, call metadata for gRPC).
type headerFlags []string

func (h *headerFlags) String() string { return strings.Join(*h, ", ") }

func (h *headerFlags) Set(v string) error {
	*h = append(*h, v)
	return nil
}

// header parses the collected values, erroring on entries without a colon so
// a typo fails loudly instead of being silently dropped.
func (h headerFlags) header() (http.Header, error) {
	out := http.Header{}
	for _, raw := range h {
		k, v, ok := strings.Cut(raw, ":")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf(`invalid --header %q (want "Key: Value")`, raw)
		}
		out.Add(k, strings.TrimSpace(v))
	}
	return out, nil
}

// authConfig builds the runtime.AuthConfig from the parsed flags, or nil when no
// token-auth flow was requested (--auth empty/none).
func (f *forgeFlags) authConfig() *runtime.AuthConfig {
	if f.auth == "" || f.auth == "none" {
		return nil
	}
	return &runtime.AuthConfig{
		Type:              f.auth,
		TokenURL:          f.tokenURL,
		RefreshURL:        f.refreshURL,
		ContentType:       f.authContentType,
		UsernameEnv:       f.userEnv,
		PasswordEnv:       f.passEnv,
		ClientIDEnv:       f.clientIDEnv,
		ClientSecretEnv:   f.clientSecretEnv,
		AccessTokenEnv:    f.accessTokenEnv,
		RefreshTokenEnv:   f.refreshTokenEnv,
		Scope:             f.scope,
		AccessTokenField:  f.accessField,
		RefreshTokenField: f.refreshField,
		ExpiresInField:    f.expiresInField,
		Header:            f.authHeader,
		Scheme:            f.authScheme,
		ExpiryMargin:      f.expiryMargin,
	}
}

func (f *forgeFlags) policy(name string) toolpack.Policy {
	var tags []string
	for _, t := range strings.Split(f.tags, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	return toolpack.Policy{
		Mode:     toolpack.ExposureMode(f.mode),
		Tags:     tags,
		ReadOnly: f.readOnly,
		MaxTools: f.maxTools,
		Prefix:   toolpack.SanitizeName(name) + "_",
	}
}

// resolveSpecPath makes a spec-file path absolute relative to workDir so reads
// work regardless of the calling process's CWD (the daemon's CWD may differ).
func resolveSpecPath(workDir, specFile string) string {
	if filepath.IsAbs(specFile) {
		return specFile
	}
	return filepath.Join(workDir, specFile)
}

func forgeAdd(workDir string, args []string, w io.Writer) error {
	// Positional <name> [spec-file] come first, then flags (the stdlib flag
	// package stops parsing at the first positional, so we split manually).
	// The spec file is optional: live introspection (--graphql-url /
	// --grpc-target) fetches the spec over the wire instead.
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: forge add <name> <spec-file> [flags], or forge add <name> --graphql-url URL | --grpc-target HOST:PORT")
	}
	name := args[0]
	rest := args[1:]
	specFile := ""
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		specFile, rest = rest[0], rest[1:]
	}
	fs := flag.NewFlagSet("forge add", flag.ContinueOnError)
	fs.SetOutput(w)
	f := bindForgeFlags(fs)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	switch {
	case specFile == "" && f.graphqlURL == "" && f.grpcTarget == "":
		return fmt.Errorf("usage: forge add <name> <spec-file> [flags], or forge add <name> --graphql-url URL | --grpc-target HOST:PORT")
	case specFile != "" && (f.graphqlURL != "" || f.grpcTarget != ""):
		return fmt.Errorf("give either a spec file or a live endpoint (--graphql-url/--grpc-target), not both")
	case f.graphqlURL != "" && f.grpcTarget != "":
		return fmt.Errorf("--graphql-url and --grpc-target are mutually exclusive")
	}

	// Accept a base URL given as a POSITIONAL arg (e.g.
	// `forge add weather spec.json http://localhost:8080`) in addition to
	// --base-url. The stdlib flag parser leaves such a positional in fs.Args();
	// silently dropping it (the prior behavior) produced integrations with an
	// empty base URL whose tools fail with "unsupported protocol scheme".
	baseURL := f.baseURL
	if baseURL == "" {
		for _, a := range fs.Args() {
			if strings.Contains(a, "://") {
				baseURL = a
				break
			}
		}
	}

	// Acquire the spec bytes: from a file, or live over the wire. Live specs
	// are stored verbatim, so everything downstream (generate, diff, enable)
	// is byte-identical to the file-based flow. --source is ignored for live
	// endpoints — the protocol determines it.
	var (
		spec   []byte
		source string
		ext    string
		err    error
	)
	switch {
	case f.graphqlURL != "":
		hdrs, herr := f.headers.header()
		if herr != nil {
			return herr
		}
		spec, err = ingest.GraphQLIntrospect(context.Background(), f.graphqlURL, hdrs, f.timeout)
		if err != nil {
			return err
		}
		source, ext = forge.SourceGraphQL, "json"
		// The GraphQL IR models every operation as POST /graphql, so default
		// the base URL to the endpoint minus that suffix: base+path then hits
		// the endpoint the schema was pulled from.
		if baseURL == "" {
			baseURL = strings.TrimSuffix(strings.TrimSuffix(f.graphqlURL, "/"), "/graphql")
		}
		fmt.Fprintf(w, "✓ introspected GraphQL endpoint %s\n", f.graphqlURL)
	case f.grpcTarget != "":
		hdrs, herr := f.headers.header()
		if herr != nil {
			return herr
		}
		md := map[string]string{}
		for k, vs := range hdrs {
			md[k] = strings.Join(vs, ",")
		}
		ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
		spec, err = ingest.GRPCReflect(ctx, f.grpcTarget, ingest.GRPCReflectOptions{TLS: f.grpcTLS, Metadata: md})
		cancel()
		if err != nil {
			return err
		}
		source, ext = forge.SourceGRPC, "pb"
		fmt.Fprintf(w, "✓ discovered services on %s via gRPC server reflection\n", f.grpcTarget)
	default:
		spec, err = os.ReadFile(resolveSpecPath(workDir, specFile))
		if err != nil {
			return fmt.Errorf("read spec %q: %w", specFile, err)
		}
		source = f.source
		if source == "" {
			source = inferSource(specFile, spec)
		}
		ext = strings.TrimPrefix(strings.ToLower(filepath.Ext(specFile)), ".")
	}

	// How to re-run this add — used in hint messages below.
	specArg := specFile
	if f.graphqlURL != "" {
		specArg = "--graphql-url " + f.graphqlURL
	} else if f.grpcTarget != "" {
		specArg = "--grpc-target " + f.grpcTarget
	}

	api, err := ingestSpec(source, spec)
	if err != nil {
		return err
	}

	// Warn loudly when neither the base URL nor the spec provides an ABSOLUTE
	// server URL: the generated tools would issue scheme-less requests, every call
	// fails, and the agent quietly falls back to its own knowledge — so it looks
	// like it "works" when it does not.
	effective := baseURL
	if effective == "" {
		effective = api.BaseURL()
	}
	if !strings.Contains(effective, "://") {
		fmt.Fprintf(w, "⚠ no absolute base URL (spec server = %q) — tools will fail with \"unsupported protocol scheme\".\n  Re-run with an absolute URL: ##forge add %s %s --base-url http://host:port\n",
			api.BaseURL(), name, specArg)
	}

	// Honest limits (design 015): a descriptor set describes the RPC surface,
	// not a REST one. Unary methods are modeled as unary POSTs to the gRPC wire
	// path, which the forge runtime speaks over HTTP/JSON — so live calls work
	// only against a server that accepts JSON on that path (grpc-gateway
	// transcoding or an Envoy/grpc-web bridge). Server-streaming sessions
	// additionally require grpc-gateway's newline-delimited JSON framing;
	// Connect's enveloped streaming framing is not supported. Generation, docs
	// and SDK output are unaffected. Say so at add time rather than letting the
	// user discover it when every call 404s.
	if source == forge.SourceGRPC {
		fmt.Fprintf(w, "ℹ gRPC source: unary methods are exposed as POST %s\n"+
			"  Live calls require a JSON-over-HTTP endpoint (grpc-gateway);\n"+
			"  a plain gRPC/HTTP2 server will reject them. Server-streaming\n"+
			"  sessions consume grpc-gateway NDJSON frames (Connect framing is not supported).\n",
			"/<package>.<Service>/<Method>")
	}
	// Report what the ingester routed to streaming sessions (gRPC
	// server-streaming, GraphQL subscriptions) and what it deliberately did not
	// expose (client/bidi streaming) — specific names and kinds, so every
	// claim matches what actually happened.
	for _, line := range streamsSummary(api) {
		fmt.Fprintf(w, "ℹ %s\n", line)
	}
	for _, line := range skippedSummary(api) {
		fmt.Fprintf(w, "ℹ %s\n", line)
	}

	rel, err := forge.StoreSpec(workDir, name, spec, ext)
	if err != nil {
		return fmt.Errorf("store spec: %w", err)
	}

	def := forge.Integration{
		Name:          name,
		Source:        source,
		SpecFile:      rel,
		BaseURL:       baseURL,
		WSURL:         f.wsURL,
		CredentialEnv: f.credEnv,
		Auth:          f.authConfig(),
		Policy:        f.policy(name),
		Enabled:       false,
	}
	defs, _ := forge.LoadStore(workDir)
	found := false
	for i := range defs {
		if defs[i].Name == name {
			def.Enabled = defs[i].Enabled
			defs[i] = def
			found = true
		}
	}
	if !found {
		defs = append(defs, def)
	}
	if err := forge.SaveStore(workDir, defs); err != nil {
		return fmt.Errorf("save store: %w", err)
	}

	fmt.Fprintf(w, "✓ ingested %s: %d operations, %d auth scheme(s) [%s]\n", api.Name, len(api.Operations), len(api.Auth), source)
	if err := generateTargets(workDir, name, api, f, w); err != nil {
		return err
	}
	fmt.Fprintf(w, "✓ integration %q saved. Enable live tools with: ##integration enable %s\n", name, name)
	return nil
}

func forgeGenerate(workDir string, args []string, w io.Writer) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: forge generate <name> [flags]")
	}
	name := args[0]
	fs := flag.NewFlagSet("forge generate", flag.ContinueOnError)
	fs.SetOutput(w)
	f := bindForgeFlags(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	defs, err := forge.LoadStore(workDir)
	if err != nil {
		return err
	}
	var def *forge.Integration
	for i := range defs {
		if defs[i].Name == name {
			def = &defs[i]
		}
	}
	if def == nil {
		return fmt.Errorf("no integration named %q (add it first)", name)
	}
	api, err := forge.LoadSpecAPI(workDir, *def)
	if err != nil {
		return err
	}
	return generateTargets(workDir, name, api, f, w)
}

func forgeList(workDir string, w io.Writer) error {
	defs, err := forge.LoadStore(workDir)
	if err != nil {
		return err
	}
	if len(defs) == 0 {
		fmt.Fprintln(w, "No integrations configured. Add one with: forge add <name> <spec-file>")
		return nil
	}
	fmt.Fprintf(w, "Integrations (%d):\n", len(defs))
	for _, d := range defs {
		state := "disabled"
		if d.Enabled {
			state = "enabled"
		}
		fmt.Fprintf(w, "  %-20s %-8s %-8s %s\n", d.Name, d.Source, state, d.SpecFile)
	}
	return nil
}

// forgeDiff reports the operation-level delta between an integration's stored
// spec and a new spec file — spec-driven maintenance ("what changed").
func forgeDiff(workDir string, args []string, w io.Writer) error {
	if len(args) < 2 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: forge diff <name> <new-spec-file>")
	}
	name, specFile := args[0], args[1]
	defs, err := forge.LoadStore(workDir)
	if err != nil {
		return err
	}
	var def *forge.Integration
	for i := range defs {
		if defs[i].Name == name {
			def = &defs[i]
		}
	}
	if def == nil {
		return fmt.Errorf("no integration named %q (add it first)", name)
	}
	oldAPI, err := forge.LoadSpecAPI(workDir, *def)
	if err != nil {
		return fmt.Errorf("load stored spec: %w", err)
	}
	newBytes, err := os.ReadFile(resolveSpecPath(workDir, specFile))
	if err != nil {
		return fmt.Errorf("read new spec %q: %w", specFile, err)
	}
	source := def.Source
	if source == "" {
		source = forge.SourceOpenAPI
	}
	newAPI, err := ingestSpec(source, newBytes)
	if err != nil {
		return err
	}
	d := forge.DiffAPIs(oldAPI, newAPI)
	if d.Empty() {
		fmt.Fprintf(w, "No operation-level changes for %q (%d operations).\n", name, len(newAPI.Operations))
		return nil
	}
	fmt.Fprintf(w, "Operation changes for %q:\n", name)
	for _, id := range d.Added {
		fmt.Fprintf(w, "  + %s\n", id)
	}
	for _, id := range d.Removed {
		fmt.Fprintf(w, "  - %s\n", id)
	}
	for _, id := range d.Changed {
		fmt.Fprintf(w, "  ~ %s\n", id)
	}
	fmt.Fprintf(w, "(%d added, %d removed, %d changed)\nRun 'forge add %s %s' to adopt the new spec and regenerate.\n",
		len(d.Added), len(d.Removed), len(d.Changed), name, specFile)
	return nil
}

func generateTargets(workDir, name string, api *ir.API, f *forgeFlags, w io.Writer) error {
	out := f.out
	if out == "" {
		out = filepath.Join(forge.SpecDir(workDir, name), "gen")
	} else if !filepath.IsAbs(out) {
		out = filepath.Join(workDir, out)
	}
	opts := gen.Options{Prefix: toolpack.SanitizeName(name) + "_", PackageName: ir.Slugify(name), ModulePath: f.module}

	var targets []string
	for _, t := range strings.Split(f.targets, ",") {
		if t = strings.TrimSpace(t); t != "" {
			targets = append(targets, t)
		}
	}
	for _, target := range targets {
		g, ok := gen.Get(target)
		if !ok {
			fmt.Fprintf(w, "  ! unknown target %q (see: forge targets)\n", target)
			continue
		}
		fset, err := g.Generate(api, opts)
		if err != nil {
			fmt.Fprintf(w, "  ! %s: %v\n", target, err)
			continue
		}
		dir := filepath.Join(out, target)
		if err := fset.WriteDir(dir); err != nil {
			return fmt.Errorf("write %s: %w", target, err)
		}
		fmt.Fprintf(w, "  ✓ %-14s → %s (%d file(s))\n", target, dir, len(fset.Files))
	}
	return nil
}

// streamsSummary reports what landed in api.Streams — server-streaming methods
// / subscription fields exposed as background streaming sessions, e.g.
//
//	2 server-streaming methods exposed as streaming sessions (stream_start / stream_session): Pub_Tail, Pub_Watch
//	2 subscription fields exposed as streaming sessions over graphql-ws (graphql_subscribe / stream_session): onEvent, onUserCreated
//
// Empty when the API has no streams, so no claims are printed for plain specs.
func streamsSummary(api *ir.API) []string {
	if api == nil || len(api.Streams) == 0 {
		return nil
	}
	names := make([]string, 0, len(api.Streams))
	for i := range api.Streams {
		names = append(names, api.Streams[i].ID)
	}
	sort.Strings(names)
	n := len(names)
	if api.SourceType == "graphql" {
		return []string{fmt.Sprintf("%d subscription %s exposed as streaming sessions over graphql-ws (graphql_subscribe / stream_session): %s",
			n, plural(n, "field", "fields"), joinCapped(names, 10))}
	}
	return []string{fmt.Sprintf("%d server-streaming %s exposed as streaming sessions (stream_start / stream_session): %s",
		n, plural(n, "method", "methods"), joinCapped(names, 10))}
}

// skippedSummary renders api.Skipped as specific, honest report lines, e.g.
//
//	2 streaming methods skipped (client-streaming: Pub_Upload; bidi: Pub_Chat) — client/bidi streams cannot ride a JSON-over-HTTP bridge
//
// Empty when nothing was skipped, so sources without client/bidi streaming
// print no exclusion claims at all.
func skippedSummary(api *ir.API) []string {
	if api == nil || len(api.Skipped) == 0 {
		return nil
	}
	byKind := map[string][]string{}
	for _, s := range api.Skipped {
		byKind[s.Kind] = append(byKind[s.Kind], s.ID)
	}
	for _, names := range byKind {
		sort.Strings(names)
	}

	var lines []string

	var streamParts []string
	streamTotal := 0
	for _, kind := range []string{ir.SkipClientStreaming, ir.SkipServerStreaming, ir.SkipBidiStreaming} {
		names := byKind[kind]
		if len(names) == 0 {
			continue
		}
		streamTotal += len(names)
		label := kind
		if kind == ir.SkipBidiStreaming {
			label = "bidi"
		}
		streamParts = append(streamParts, label+": "+joinCapped(names, 10))
	}
	if streamTotal > 0 {
		lines = append(lines, fmt.Sprintf("%d streaming %s skipped (%s) — client/bidi streams cannot ride a JSON-over-HTTP bridge",
			streamTotal, plural(streamTotal, "method", "methods"), strings.Join(streamParts, "; ")))
	}

	// Legacy IR only: current ingesters route subscriptions to api.Streams.
	if subs := byKind[ir.SkipSubscription]; len(subs) > 0 {
		lines = append(lines, fmt.Sprintf("%d subscription %s skipped: %s",
			len(subs), plural(len(subs), "field", "fields"), joinCapped(subs, 10)))
	}
	return lines
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// joinCapped joins up to max names, summarizing the remainder so a huge schema
// cannot flood the add output.
func joinCapped(names []string, max int) string {
	if len(names) <= max {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:max], ", ") + fmt.Sprintf(", … (+%d more)", len(names)-max)
}

func ingestSpec(source string, spec []byte) (*ir.API, error) {
	switch source {
	case forge.SourceGraphQL:
		return ingest.GraphQL(spec)
	case forge.SourceGRPC:
		return ingest.GRPC(spec)
	default:
		return ingest.OpenAPI(spec)
	}
}

// descriptorSetExts are the conventional file names for a compiled protobuf
// FileDescriptorSet.
var descriptorSetExts = []string{".pb", ".desc", ".protoset", ".descriptor"}

func inferSource(path string, spec []byte) string {
	lp := strings.ToLower(path)

	// gRPC first: a FileDescriptorSet is binary, so the text heuristics below
	// would misread it. Extension is the strong signal; content sniffing
	// catches an unconventionally named descriptor.
	for _, ext := range descriptorSetExts {
		if strings.HasSuffix(lp, ext) {
			return forge.SourceGRPC
		}
	}
	if ingest.LooksLikeDescriptorSet(spec) {
		return forge.SourceGRPC
	}

	if strings.Contains(lp, "graphql") || strings.HasSuffix(lp, ".gql") {
		return forge.SourceGraphQL
	}
	if strings.Contains(string(spec), "__schema") && strings.Contains(string(spec), "queryType") {
		return forge.SourceGraphQL
	}
	return forge.SourceOpenAPI
}
