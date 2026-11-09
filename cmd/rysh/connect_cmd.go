// SPDX-License-Identifier: Apache-2.0

package main

// rysh connect — bind this machine to an AI-native company (design 037,
// "Company Connect").
//
//	rysh connect <server> <code> | --file <path> [--json] [--device <label>] [--vehicle desktop|terminal] [--dir <path>] [--no-open]
//
// The dashboard issues a one-time connect code for a company. This command
// claims it: the server answers once with a READ-ONLY workspace key scoped to
// the company's dedicated workspace, and this command writes a company
// workspace directory — ~/Rysh/<slug> by default — whose rysh.config.yaml
// carries the `upstream:` connection and a `company:` block, then opens the
// session there. The desktop app runs exactly this with --json and adopts the
// directory and session it prints, so both vehicles share one daemon.
//
// Nothing is typed by hand: no UUID, no key, no config editing.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
)

const (
	connectDirEnv         = "RYSH_COMPANIES_DIR"
	connectDefaultDirName = "Rysh"
	connectVehicle        = "terminal" // the default; the desktop app passes --vehicle desktop
)

var connectSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// connectClaim is the server's one-time reply to a claimed code.
type connectClaim struct {
	ServerURL   string `json:"server_url"`
	APIKey      string `json:"api_key"`
	WorkspaceID string `json:"workspace_id"`
	Company     struct {
		ID          string `json:"id"`
		WorkspaceID string `json:"workspace_id"`
		Name        string `json:"name"`
		Slug        string `json:"slug"`
		FQDN        string `json:"fqdn"`
		Status      string `json:"status"`
		TemplateID  string `json:"template_id"`
	} `json:"company"`
	Subjects struct {
		Activity string `json:"activity"`
		Progress string `json:"progress"`
	} `json:"subjects"`
}

// connectResult is what --json prints. The key is deliberately absent: it is
// in the secrets file named here, and stdout is often a log. SecretsFile is
// absolute so the desktop app checks that exact file (CD2).
type connectResult struct {
	Directory   string `json:"directory"`
	ConfigFile  string `json:"config_file"`
	SecretsFile string `json:"secrets_file"`
	Session     string `json:"session"`
	ServerURL   string `json:"server_url"`
	WorkspaceID string `json:"workspace_id"`
	Company     struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Slug   string `json:"slug"`
		FQDN   string `json:"fqdn"`
		Status string `json:"status"`
	} `json:"company"`
}

func runConnect(cfg config.Config, logger *slog.Logger, args []string) error {
	args, asJSON := extractBoolFlag(args, "--json")
	args, noOpen := extractBoolFlag(args, "--no-open")
	args, device := extractStringFlag(args, "--device")
	args, dirFlag := extractStringFlag(args, "--dir")
	before := len(args)
	args, vehicle := extractStringFlag(args, "--vehicle")
	switch {
	case len(args) == before && slices.Contains(args, "--vehicle"):
		return errors.New("--vehicle needs a value: desktop or terminal")
	case len(args) == before:
		vehicle = connectVehicle
	case vehicle != "desktop" && vehicle != "terminal":
		return fmt.Errorf("--vehicle must be desktop or terminal, not %q", vehicle)
	}
	before = len(args)
	args, connectFilePath := extractStringFlag(args, "--file")
	var server, code string
	switch {
	case len(args) == before && slices.Contains(args, "--file"):
		return errors.New("--file needs the path of a .ryshconnect file")
	case len(args) < before && len(args) > 0:
		return errors.New("--file replaces <server> <code>; pass one or the other")
	case len(args) < before:
		var err error
		if server, code, err = readConnectFile(connectFilePath, time.Now()); err != nil {
			return err
		}
	case len(args) < 2:
		return errors.New(progname.Rewrite("usage: rysh connect <server> <code> | --file <path> [--json] [--device <label>] [--vehicle desktop|terminal] [--dir <path>] [--no-open]\n" +
			"  the code (or the connect file) comes from the company page on the dashboard (\"Open in Rysh\")"))
	default:
		server, code = strings.TrimSpace(args[0]), strings.TrimSpace(args[1])
	}
	if device == "" {
		device = defaultConnectDevice()
	}

	claim, err := claimCompany(server, code, vehicle, device)
	if err != nil {
		return err
	}
	if connectFilePath != "" {
		// The code in it is spent; a failed claim above keeps the file.
		if err := os.Remove(connectFilePath); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not remove %s (the code in it is used up): %v\n", connectFilePath, errors.Unwrap(err))
		}
	}
	dir, err := companyWorkspaceDir(dirFlag, claim.Company.Slug)
	if err != nil {
		return err
	}
	cfgPath, secretsPath, err := writeCompanyWorkspace(dir, claim)
	if err != nil {
		return err
	}

	result := connectResult{
		Directory: dir, ConfigFile: cfgPath, SecretsFile: secretsPath, Session: claim.Company.Slug,
		ServerURL: claim.ServerURL, WorkspaceID: claim.WorkspaceID,
	}
	result.Company.ID = claim.Company.ID
	result.Company.Name = claim.Company.Name
	result.Company.Slug = claim.Company.Slug
	result.Company.FQDN = claim.Company.FQDN
	result.Company.Status = claim.Company.Status

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	fmt.Printf("connected to %s (%s)\n", claim.Company.Name, claim.Company.FQDN)
	fmt.Printf("  workspace  %s\n", dir)
	fmt.Printf("  session    %s\n", claim.Company.Slug)
	fmt.Printf("  server     %s\n", claim.ServerURL)
	fmt.Printf("  access     read-only key on the company's workspace; revoke it from the dashboard\n")
	if noOpen {
		fmt.Printf("\nopen it any time with:\n  cd %s && %s\n", dir, progname.Rewrite("rysh"))
		return nil
	}
	fmt.Printf("\nopening…\n")
	if err := os.Chdir(dir); err != nil {
		return fmt.Errorf("could not enter %s: %w", dir, err)
	}
	// Reload from the directory just written: rysh state is project-local, so
	// the session, its .rysh/ and the company block all resolve from cwd.
	sessionCfg := config.LoadFrom("")
	sessionCfg.SessionSource = cfg.SessionSource
	// The company session IS the slug — that is what lets the desktop app
	// adopt it by (directory, session). Force it: a RYSH_SESSION inherited
	// from a rysh pane this command was typed in would otherwise win.
	sessionCfg.SessionName = claim.Company.Slug
	return run(sessionCfg, logger, nil, "", false, false, false)
}

// claimCompany exchanges the code for the key. Every code failure comes back
// as the server's single message, so this only relays it.
func claimCompany(server, code, vehicle, device string) (*connectClaim, error) {
	server = strings.TrimRight(server, "/")
	if !strings.HasPrefix(server, "http://") && !strings.HasPrefix(server, "https://") {
		return nil, fmt.Errorf("server must be a URL such as https://letchat.ai, got %q", server)
	}
	if err := checkClaimServer(server, server); err != nil {
		return nil, err
	}
	if code == "" {
		return nil, errors.New("the connect code is empty")
	}
	body, _ := json.Marshal(map[string]string{"token": code, "vehicle": vehicle, "device": device})
	req, err := http.NewRequest(http.MethodPost, server+"/api/company-connect/claim", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := connectHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach %s — check the URL and your connection", server)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &apiErr)
		if apiErr.Error != "" {
			return nil, fmt.Errorf("%s: %s", server, apiErr.Error)
		}
		return nil, fmt.Errorf("%s returned HTTP %d — is that a rysh server?", server, resp.StatusCode)
	}
	var claim connectClaim
	if err := json.Unmarshal(raw, &claim); err != nil {
		return nil, fmt.Errorf("%s did not return a valid claim — is that a rysh server?", server)
	}
	if claim.APIKey == "" || claim.WorkspaceID == "" || claim.Company.ID == "" || claim.Company.Slug == "" {
		return nil, fmt.Errorf("%s returned an incomplete claim", server)
	}
	if claim.ServerURL == "" {
		claim.ServerURL = server
	}
	if err := checkClaimServer(server, claim.ServerURL); err != nil {
		return nil, err
	}
	return &claim, nil
}

// connectHTTPClient is the client the claim is sent with (a test seam: tests
// swap in an httptest TLS client).
var connectHTTPClient = func() *http.Client { return &http.Client{Timeout: 15 * time.Second} }

// checkClaimServer refuses a connection that would carry the connect code or
// the key over plain http to anything but this machine, or move a link opened
// over https somewhere weaker (security-lead ledger 190). The server_url the
// claim names is written into the workspace config and carries the key from
// then on, so for an https link it must be https on the same host and port;
// http, for the link or the claim, is allowed only to a loopback address.
// checkClaimServer(link, link) checks the link alone, before anything is sent.
func checkClaimServer(link, claimed string) error {
	l, err := url.Parse(strings.TrimRight(strings.TrimSpace(link), "/"))
	if err != nil || l.Host == "" {
		return fmt.Errorf("server must be a URL such as https://letchat.ai, got %q", link)
	}
	if l.Scheme == "http" && !isLoopbackHost(l.Hostname()) {
		return fmt.Errorf("refusing %s: plain http is allowed only to a loopback server; use https", link)
	}
	c, err := url.Parse(strings.TrimRight(strings.TrimSpace(claimed), "/"))
	if err != nil || c.Host == "" {
		return fmt.Errorf("%s returned a server_url that is not a URL; refusing the claim", link)
	}
	switch c.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(c.Hostname()) {
			return fmt.Errorf("%s answered with server_url http://%s; refusing the claim: plain http is allowed only to a loopback server, use https", link, c.Host)
		}
	default:
		return fmt.Errorf("%s answered with a server_url that is not https; refusing the claim", link)
	}
	if l.Scheme == "https" {
		if c.Scheme != "https" {
			return fmt.Errorf("%s answered with server_url %s://%s; refusing the claim: a link opened over https stays on https", link, c.Scheme, c.Host)
		}
		if !strings.EqualFold(c.Host, l.Host) {
			return fmt.Errorf("%s answered with server_url on another host (%s); refusing the claim: the key stays with the server you opened", link, c.Host)
		}
	}
	return nil
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// companyWorkspaceDir is <base>/<slug>: --dir wins, then $RYSH_COMPANIES_DIR,
// then ~/Rysh. Directories are user-visible on purpose — a company workspace
// is a folder the owner can find, back up, and open from the desktop app's
// "Open workspace" dialog like any other.
func companyWorkspaceDir(explicit, slug string) (string, error) {
	slug = strings.TrimSpace(slug)
	if !connectSlugRe.MatchString(slug) {
		return "", fmt.Errorf("refusing to create a directory for company slug %q", slug)
	}
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	base := strings.TrimSpace(os.Getenv(connectDirEnv))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("could not find your home directory: %w", err)
		}
		base = filepath.Join(home, connectDefaultDirName)
	}
	return filepath.Join(base, slug), nil
}

// writeCompanyWorkspace creates the directory (if needed), stores the key in
// its .rysh/secrets tier and writes the upstream connection (the key as the
// ${RYSH_UPSTREAM_API_KEY} reference), the company block and the session name
// into its rysh.config.yaml. A plain key left by an older connect is replaced.
// A directory that already belongs to ANOTHER company is refused: the connect
// key would otherwise be written into someone else's workspace and both
// companies would fight over one session name. Returns the config path and
// the secrets file path.
func writeCompanyWorkspace(dir string, claim *connectClaim) (string, string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("could not create %s: %w", dir, err)
	}
	cfgPath := filepath.Join(dir, "rysh.config.yaml")
	if _, err := os.Stat(cfgPath); err == nil {
		existing := config.LoadFrom(cfgPath)
		if existing.Company.ID != "" && existing.Company.ID != claim.Company.ID {
			return "", "", fmt.Errorf("%s already belongs to company %q (%s); pass --dir <path> to connect this one elsewhere",
				dir, existing.Company.Name, existing.Company.Slug)
		}
	} else {
		header := "# rysh company workspace — written by `rysh connect` (design 037).\n" +
			"# upstream.api_key names a READ-ONLY key for this company's workspace, stored in .rysh/secrets;\n" +
			"# revoke it from the dashboard.\n" +
			"rysh:\n  session_name: " + claim.Company.Slug + "\n"
		if err := os.WriteFile(cfgPath, []byte(header), 0o600); err != nil {
			return "", "", fmt.Errorf("could not write %s: %w", cfgPath, err)
		}
	}
	secretsPath, err := config.WriteCompanyBinding(cfgPath, config.RyshDirForConfigFile(cfgPath), &config.CompanyBinding{
		URL: claim.ServerURL, APIKey: claim.APIKey, WorkspaceID: claim.WorkspaceID,
		Company: config.CompanyConfig{
			ID: claim.Company.ID, Name: claim.Company.Name, Slug: claim.Company.Slug,
			FQDN: claim.Company.FQDN, Server: claim.ServerURL,
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("write company binding: %w", err)
	}
	if err := config.SetSessionName(cfgPath, claim.Company.Slug); err != nil {
		return "", "", fmt.Errorf("write session name: %w", err)
	}
	return cfgPath, secretsPath, nil
}

func defaultConnectDevice() string {
	host, _ := os.Hostname()
	name := ""
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	switch {
	case name != "" && host != "":
		return name + "@" + host + " (terminal)"
	case host != "":
		return host + " (terminal)"
	}
	return "terminal"
}
