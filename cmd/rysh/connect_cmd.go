// SPDX-License-Identifier: Apache-2.0

package main

// rysh connect — bind this machine to an AI-native company (design 037,
// "Company Connect").
//
//	rysh connect <server> <code> [--json] [--device <label>] [--dir <path>] [--no-open]
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
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
)

const (
	connectDirEnv         = "RYSH_COMPANIES_DIR"
	connectDefaultDirName = "Rysh"
	connectVehicle        = "terminal"
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
// in the config file the caller adopts, and stdout is often a log.
type connectResult struct {
	Directory   string `json:"directory"`
	ConfigFile  string `json:"config_file"`
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
	if len(args) < 2 {
		return errors.New(progname.Rewrite("usage: rysh connect <server> <code> [--json] [--device <label>] [--dir <path>] [--no-open]\n" +
			"  the code comes from the company page on the dashboard (\"Open in Rysh\")"))
	}
	server, code := strings.TrimSpace(args[0]), strings.TrimSpace(args[1])
	if device == "" {
		device = defaultConnectDevice()
	}

	claim, err := claimCompany(server, code, connectVehicle, device)
	if err != nil {
		return err
	}
	dir, err := companyWorkspaceDir(dirFlag, claim.Company.Slug)
	if err != nil {
		return err
	}
	cfgPath, err := writeCompanyWorkspace(dir, claim)
	if err != nil {
		return err
	}

	result := connectResult{
		Directory: dir, ConfigFile: cfgPath, Session: claim.Company.Slug,
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
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
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
	return &claim, nil
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

// writeCompanyWorkspace creates the directory (if needed) and writes the
// upstream connection, the company block and the session name into its
// rysh.config.yaml. A directory that already belongs to ANOTHER company is
// refused: the connect key would otherwise be written into someone else's
// workspace and both companies would fight over one session name.
func writeCompanyWorkspace(dir string, claim *connectClaim) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("could not create %s: %w", dir, err)
	}
	cfgPath := filepath.Join(dir, "rysh.config.yaml")
	if _, err := os.Stat(cfgPath); err == nil {
		existing := config.LoadFrom(cfgPath)
		if existing.Company.ID != "" && existing.Company.ID != claim.Company.ID {
			return "", fmt.Errorf("%s already belongs to company %q (%s); pass --dir <path> to connect this one elsewhere",
				dir, existing.Company.Name, existing.Company.Slug)
		}
	} else {
		header := "# rysh company workspace — written by `rysh connect` (design 037).\n" +
			"# upstream.api_key is a READ-ONLY key for this company's workspace; revoke it from the dashboard.\n" +
			"rysh:\n  session_name: " + claim.Company.Slug + "\n"
		if err := os.WriteFile(cfgPath, []byte(header), 0o600); err != nil {
			return "", fmt.Errorf("could not write %s: %w", cfgPath, err)
		}
	}
	if err := config.SetUpstreamConnection(cfgPath, claim.ServerURL, claim.APIKey, claim.WorkspaceID); err != nil {
		return "", fmt.Errorf("write upstream connection: %w", err)
	}
	if err := config.SetCompanyBlock(cfgPath, config.CompanyConfig{
		ID: claim.Company.ID, Name: claim.Company.Name, Slug: claim.Company.Slug,
		FQDN: claim.Company.FQDN, Server: claim.ServerURL,
	}); err != nil {
		return "", fmt.Errorf("write company block: %w", err)
	}
	if err := config.SetSessionName(cfgPath, claim.Company.Slug); err != nil {
		return "", fmt.Errorf("write session name: %w", err)
	}
	// The key is in the file: keep it to the owner.
	_ = os.Chmod(cfgPath, 0o600)
	return cfgPath, nil
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
