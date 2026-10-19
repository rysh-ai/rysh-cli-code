// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/company"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
)

// `rysh company …` (design 037, second cut): the bound company's data at the
// shell, and the same data as an MCP server for agents.
//
//	rysh company status|orders|enquiries|activity|services [--json] [--config <file>]
//	rysh company mcp [--config <file>]
//
// The binding comes from the directory's rysh.config.yaml (what `##config
// load` wrote), from --config, or from RYSH_COMPANY_CONFIG — which is how the
// daemon starts the MCP server for a session.
func runCompanyCLI(cfg config.Config, args []string) error {
	args, cfgFlag := extractStringFlag(args, "--config")
	args, asJSON := extractBoolFlag(args, "--json")
	if cfgFlag == "" {
		cfgFlag = strings.TrimSpace(os.Getenv("RYSH_COMPANY_CONFIG"))
	}
	if cfgFlag != "" {
		cfg = config.LoadFrom(cfgFlag)
	}
	usage := errors.New(progname.Rewrite("usage: rysh company <status|orders|enquiries|activity|services|mcp> [--json] [--config <file>]"))
	if len(args) == 0 {
		return usage
	}
	sub := strings.ToLower(strings.TrimSpace(args[0]))
	if !cfg.Company.Enabled() || strings.TrimSpace(cfg.Upstream.APIKey) == "" {
		return errors.New(progname.Rewrite("rysh company: this directory is not bound to a company\n" +
			"  in a running session:  ##config load <file>   (the file comes from the company page)\n" +
			"  from a shell:          rysh company " + sub + " --config <file>"))
	}
	server := strings.TrimRight(strings.TrimSpace(cfg.Company.Server), "/")
	if server == "" {
		server = strings.TrimRight(strings.TrimSpace(cfg.Upstream.URL), "/")
	}
	client := company.NewClient(server, cfg.Upstream.APIKey)
	companyID := cfg.Company.ID
	tools := newCompanyTools(func(ctx context.Context) (*msg.CompanySnapshot, error) {
		return client.Fetch(ctx, companyID)
	})

	switch sub {
	case "mcp":
		return serveCompanyMCP(os.Stdin, os.Stdout, tools)
	case "status", "orders", "enquiries", "activity", "services":
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		text, isErr := tools.call(ctx, "company_"+sub, nil)
		if isErr {
			return errors.New(progname.Rewrite("rysh company: " + text))
		}
		if asJSON {
			fmt.Println(text)
			return nil
		}
		snap, err := tools.snapshot(ctx)
		if err != nil {
			return err
		}
		printCompanySection(sub, snap)
		return nil
	default:
		return usage
	}
}

// printCompanySection is the human rendering; --json prints the tool's text.
func printCompanySection(section string, snap *msg.CompanySnapshot) {
	switch section {
	case "status":
		fmt.Printf("%s  %s\n", snap.Company.Name, snap.Company.FQDN)
		fmt.Printf("  status     %s\n", snap.Company.Status)
		if snap.Company.SiteURL != "" {
			fmt.Printf("  site       %s\n", snap.Company.SiteURL)
		}
		fmt.Printf("  server     %s\n", snap.Company.ServerURL)
		fmt.Printf("  link       %s\n", describeCompanyLink(snap.Link))
		fmt.Printf("  orders     %d\n", len(snap.Orders))
		fmt.Printf("  enquiries  %d\n", len(snap.Enquiries))
		fmt.Printf("  services   %d\n", len(snap.Services))
	case "orders":
		if len(snap.Orders) == 0 {
			fmt.Println("no orders")
			return
		}
		for _, o := range snap.Orders {
			fmt.Printf("%s  %-10s %-9s %s  %s %s  %s\n", o.CreatedAt.Local().Format("02 Jan 15:04"), o.Status, o.PaymentStatus,
				formatCompanyMinor(o.AmountPence, o.Currency), o.Reference, o.Fulfilment, o.CustomerName)
			for _, it := range o.Items {
				fmt.Printf("    %dx %s\n", it.Quantity, it.Name)
			}
		}
	case "enquiries":
		if len(snap.Enquiries) == 0 {
			fmt.Println("no enquiries")
			return
		}
		for _, e := range snap.Enquiries {
			fmt.Printf("%s  %-12s %-8s %s  %s  %s\n", e.CreatedAt.Local().Format("02 Jan 15:04"), e.Kind, e.Status, e.Reference, e.Name, e.Subject)
		}
	case "activity":
		if len(snap.Activity) == 0 {
			fmt.Println("no activity")
			return
		}
		for _, a := range snap.Activity {
			fmt.Printf("%s  %s/%s %s\n", a.OccurredAt.Local().Format("02 Jan 15:04"), a.Kind, a.Action, a.Summary)
		}
	case "services":
		if snap.Email != nil {
			fmt.Printf("email    %s  %s\n", snap.Email.Address, snap.Email.State)
		}
		if snap.Phone != nil {
			fmt.Printf("phone    %s  %s\n", snap.Phone.Number, snap.Phone.State)
		}
		for _, s := range snap.Services {
			fmt.Printf("service  %-16s %s  %s\n", s.ServiceKey, s.State, s.StatusMessage)
		}
		for _, c := range snap.Channels {
			b, _ := json.Marshal(c)
			fmt.Printf("channel  %s\n", b)
		}
	}
}

func describeCompanyLink(l msg.CompanyLink) string {
	switch {
	case l.AuthFailed:
		return "key refused — reconnect from the dashboard"
	case l.Live:
		return "live"
	case l.Connected:
		return "connected (polling)"
	case l.LastError != "":
		return "unreachable: " + l.LastError
	default:
		return "not fetched yet"
	}
}

func formatCompanyMinor(minor int64, currency string) string {
	cur := strings.ToUpper(strings.TrimSpace(currency))
	if cur == "" {
		cur = "GBP"
	}
	return fmt.Sprintf("%s %d.%02d", cur, minor/100, minor%100)
}
