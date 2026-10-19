// SPDX-License-Identifier: Apache-2.0

package peercoord

import (
	"sort"
	"time"

	pc "github.com/rysh-ai/rysh-cli-shared/peercoord"
)

type Attention struct {
	Kind    string `json:"kind"`
	TaskID  string `json:"task_id,omitempty"`
	Task    string `json:"task,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// Summary deliberately excludes conversation transcripts, local credentials,
// task result bodies and provider configuration. Accounts are not human counts.
type Summary struct {
	Board              string         `json:"board"`
	AsOf               time.Time      `json:"as_of"`
	AgentLimit         int            `json:"agent_limit"`
	RegisteredAgents   int            `json:"registered_agents"`
	ReadyAgents        int            `json:"ready_agents"`
	RegisteredAccounts int            `json:"registered_accounts"`
	Runners            map[string]int `json:"runners"`
	Tasks              map[string]int `json:"tasks"`
	Leader             string         `json:"leader"`
	Term               int64          `json:"term"`
	Listening          bool           `json:"local_listening"`
	Attention          []Attention    `json:"attention"`
}

func summarizeBoard(b *Board) Summary {
	s := Summary{Board: b.Connection.Board, AsOf: b.State.ServerTime, AgentLimit: pc.MaxAgents,
		RegisteredAgents: len(b.State.Agents), Runners: map[string]int{}, Tasks: map[string]int{},
		Term: b.State.Leadership.Term, Listening: b.Listening, Attention: []Attention{}}
	owners := map[string]bool{}
	for _, p := range b.State.Peers {
		if p != nil {
			owners[p.UserID] = true
		}
	}
	s.RegisteredAccounts = len(owners)
	for _, a := range b.State.Agents {
		if a == nil {
			continue
		}
		s.Runners[a.Runner]++
		if a.Ready {
			s.ReadyAgents++
		}
		if a.ID == b.State.Leadership.AgentID {
			s.Leader = a.Label
		}
	}
	for _, t := range b.State.Tasks {
		if t == nil {
			continue
		}
		s.Tasks[t.State]++
		kind := ""
		switch t.State {
		case "result_submitted":
			kind = "leader_review"
		case "blocked", "waiting_local_input":
			kind = "question_or_blocker"
		case "needs_reconciliation":
			kind = "confirm_execution_state"
		case "failed":
			kind = "failed_task"
		case "offered":
			for _, ep := range b.Endpoints {
				if ep.Agent.ID == t.AgentID && ep.Enrolled && ep.Mode == "manual" {
					kind = "local_admission"
				}
			}
		}
		if kind != "" {
			detail := ""
			if at := t.Current(); at != nil {
				detail = at.Detail
			}
			s.Attention = append(s.Attention, Attention{Kind: kind, TaskID: t.ID, Task: t.Label(), AgentID: t.AgentID, Detail: detail})
		}
	}
	for _, ep := range b.Endpoints {
		if ep.SetupError != "" {
			s.Attention = append(s.Attention, Attention{Kind: "runner_setup", AgentID: ep.Agent.ID, Detail: ep.SetupError})
		}
	}
	for _, x := range b.Executions {
		if x.Paused && x.Phase != "stopped" && x.Phase != "failed" {
			s.Attention = append(s.Attention, Attention{Kind: "local_pause", AgentID: x.AgentID, TaskID: x.TaskID})
		}
	}
	if b.LastError != "" {
		s.Attention = append(s.Attention, Attention{Kind: "listener_error", Detail: b.LastError})
	}
	sort.Slice(s.Attention, func(i, j int) bool {
		a, z := s.Attention[i], s.Attention[j]
		return a.Kind+"/"+a.TaskID+"/"+a.AgentID < z.Kind+"/"+z.TaskID+"/"+z.AgentID
	})
	return s
}
