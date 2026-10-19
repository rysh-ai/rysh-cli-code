// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	pc "github.com/rysh-ai/rysh-cli-shared/peercoord"
	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/peer"
	coord "github.com/rysh-ai/rysh-cli-code/internal/peercoord"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

func runCoordinationCmd(cfg config.Config, args []string, sess, as string) error {
	a, e := coord.Parse(args)
	if e != nil {
		return e
	}
	if a.Verb == "help" {
		fmt.Print(coord.Usage)
		return nil
	}
	if sess == "" {
		sess = cfg.SessionName
	}
	store, e := session.NewStore(cfg)
	if e != nil {
		return e
	}
	call := func(query []string) (string, error) {
		resp, e := cli.PeerCommand(store, sess, as, query)
		if e != nil {
			return "", e
		}
		if !resp.OK {
			return "", fmt.Errorf("%s", resp.Error)
		}
		return resp.Output, nil
	}
	watch := a.Verb == "board" && a.Sub == "watch"
	if !watch {
		out, e := call(args)
		if e != nil {
			return e
		}
		fmt.Print(out)
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		query := []string{"state", "--json"}
		if b := a.Flags["board"]; b != "" {
			query = append(query, "--board", b)
		}
		out, e := call(query)
		if e != nil {
			fmt.Println("Peer board:", e)
		} else {
			var view struct {
				Events []pc.Event     `json:"events"`
				Board  string         `json:"board"`
				State  pc.State       `json:"state"`
				Local  map[string]any `json:"local"`
			}
			if e = json.Unmarshal([]byte(out), &view); e != nil {
				return e
			}
			fmt.Print("\033[2J\033[H")
			fmt.Printf("PEER BOARD · %s · messages only\n", peer.Display(view.Board))
			leader := view.State.Agents[view.State.Leadership.AgentID]
			if leader != nil {
				fmt.Printf("Leader: %s · term %d · lease %s\n", peer.Display(leader.Label), view.State.Leadership.Term, view.State.Leadership.LeaseUntil.Format("15:04:05"))
			}
			fmt.Printf("Listener: %v · cursor %v · outgoing %v\n\n", view.Local["listening"], view.Local["cursor"], view.Local["outbox"])
			if detail, _ := view.Local["last_error"].(string); detail != "" {
				fmt.Printf("Listener error: %s\n", peer.Display(detail))
			}
			if admissions, ok := view.Local["admission_errors"].(map[string]any); ok {
				for task, detail := range admissions {
					fmt.Printf("Admission %s: %s\n", peer.Display(task), peer.Display(fmt.Sprint(detail)))
				}
			}
			agents := []*pc.Agent{}
			for _, ag := range view.State.Agents {
				agents = append(agents, ag)
			}
			sort.Slice(agents, func(i, j int) bool { return agents[i].Label < agents[j].Label })
			for _, ag := range agents {
				presence := peerBoardPresence(view.State.Peers[ag.PeerID], view.State.ServerTime)
				fmt.Printf("%-28s %-9s %-7s ready=%-5t %s [%s]\n", peer.Display(ag.Label), peerBoardRole(ag, view.State.Leadership), ag.Runner, ag.Ready, presence, ag.ID[:8])
			}
			fmt.Println("\nTASKS")
			tasks := []*pc.Task{}
			for _, t := range view.State.Tasks {
				tasks = append(tasks, t)
			}
			sort.Slice(tasks, func(i, j int) bool { return tasks[i].Number < tasks[j].Number })
			for _, t := range tasks {
				label := t.AgentID
				if ag := view.State.Agents[t.AgentID]; ag != nil {
					label = ag.Label
				}
				fmt.Printf("%s  %-22s %-24s %s\n", t.Label(), t.State, peer.Display(label), peer.Display(t.Objective))
				if at := t.Current(); at != nil && at.Detail != "" {
					fmt.Printf("  %s\n", peer.Display(at.Detail))
				}
			}
			fmt.Println("\nRECENT MESSAGES")
			messagesQuery := []string{"tail", "--json", "--limit", "8"}
			if board := a.Flags["board"]; board != "" {
				messagesQuery = append(messagesQuery, "--board", board)
			}
			messagesJSON, messagesErr := call(messagesQuery)
			var messages peer.Page
			if messagesErr == nil {
				messagesErr = json.Unmarshal([]byte(messagesJSON), &messages)
			}
			if messagesErr != nil {
				fmt.Printf("Messages unavailable: %s\n", peer.Display(messagesErr.Error()))
			} else {
				for _, message := range messages.Messages {
					fmt.Println(peer.FormatMessage(message))
				}
			}
			fmt.Println("\nCtrl-C closes this view. peer task show T-N --json shows details.\npeer tail displays message threads.")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

func peerBoardRole(agent *pc.Agent, leadership pc.Leadership) string {
	if agent.Role == "leader" && agent.ID != leadership.AgentID {
		return "candidate"
	}
	return agent.Role
}

func peerBoardPresence(p *pc.Peer, serverTime time.Time) string {
	if p == nil {
		return "offline"
	}
	if p.State == "revoked" {
		return "left (registration revoked)"
	}
	if p.State != "offline" || p.LastSeen.IsZero() || serverTime.IsZero() {
		return peer.Display(p.State)
	}
	age := serverTime.Sub(p.LastSeen)
	if age < 0 {
		age = 0
	}
	suffix := ""
	if age > 15*time.Minute {
		suffix = " (>15m; history retained)"
	}
	return "offline; last seen " + age.Truncate(time.Second).String() + " ago" + suffix
}
