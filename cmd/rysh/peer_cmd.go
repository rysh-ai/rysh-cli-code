// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/peer"
	coord "github.com/rysh-ai/rysh-cli-code/internal/peercoord"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// Separate transport selectors before --; text following -- stays byte-for-byte
// inside its argv strings, even when it contains words such as --session.
func parsePeerCLI(args []string) ([]string, string, string, error) {
	var rest []string
	sess, as := os.Getenv("RYSH_SESSION"), os.Getenv("RYSH_PANE")
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		if args[i] != "--session" && args[i] != "--as" {
			rest = append(rest, args[i])
			continue
		}
		flag := args[i]
		if seen[flag] || i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
			return nil, "", "", fmt.Errorf("invalid %s", flag)
		}
		seen[flag] = true
		i++
		if flag == "--session" {
			sess = args[i]
		} else {
			as = args[i]
		}
	}
	return rest, sess, as, nil
}

func runPeerCmd(cfg config.Config, args []string) error {
	args, sess, as, err := parsePeerCLI(args[1:])
	if err != nil {
		return err
	}
	if path := os.Getenv("RYSH_PEER_CONTEXT"); path != "" {
		data, e := os.ReadFile(path)
		if e != nil {
			return fmt.Errorf("task peer context unavailable")
		}
		var binding coord.CommandContext
		if e = json.Unmarshal(data, &binding); e != nil {
			return fmt.Errorf("invalid task peer context")
		}
		if as != binding.Pane || (sess != "" && sess != binding.Session) {
			return fmt.Errorf("task peer commands cannot override their session or sender pane")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		response, e := coord.Call(ctx, binding.Socket, coord.Control{Op: "command", Execution: binding.Execution, Token: binding.Token, Pane: binding.Pane, Args: args})
		if e != nil {
			return e
		}
		fmt.Print(response.Output)
		return nil
	}
	if coord.IsCommand(args) {
		return runCoordinationCmd(cfg, args, sess, as)
	}
	p, err := peer.Parse(args)
	if err != nil {
		return err
	}
	if sess == "" {
		sess = cfg.SessionName
	}
	store, err := session.NewStore(cfg)
	if err != nil {
		return err
	}
	call := func(args []string) (*msg.MsgCLIResponse, error) {
		response, err := cli.PeerCommand(store, sess, as, args)
		if err != nil {
			return nil, err
		}
		if !response.OK {
			return nil, fmt.Errorf("%s", response.Error)
		}
		return response, nil
	}
	if p.Verb != "watch" {
		response, err := call(args)
		if err != nil {
			return err
		}
		fmt.Print(response.Output)
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	after := p.After
	backoff := 2 * time.Second
	for {
		query := []string{"tail", "--json", "--limit", strconv.Itoa(p.Limit)}
		if p.Board != "" {
			query = append(query, "--board", p.Board)
		}
		if after != nil {
			query = append(query, "--after", strconv.FormatInt(*after, 10))
		}
		response, err := call(query)
		delay := 2 * time.Second
		if err != nil {
			// Revocation, disconnect and rejected requests stop the follower.
			if strings.Contains(err.Error(), "HTTP 4") || strings.Contains(err.Error(), "disconnected") {
				return err
			}
			fmt.Fprintln(os.Stderr, "peer watch:", err, "— retrying")
			delay = backoff
			if backoff < 30*time.Second {
				backoff *= 2
			}
		} else {
			backoff = 2 * time.Second
			var page peer.Page
			if err := json.Unmarshal([]byte(response.Output), &page); err != nil {
				return err
			}
			for _, message := range page.Messages {
				if p.JSON {
					data, _ := json.Marshal(message)
					fmt.Println(string(data))
				} else {
					fmt.Print(peer.FormatMessage(message) + "\n")
				}
			}
			after = &page.Next
			if page.HasMore {
				delay = 0
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
