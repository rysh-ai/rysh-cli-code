// SPDX-License-Identifier: Apache-2.0

package peercoord

import (
	"encoding/json"
	"strings"
	"testing"

	pc "github.com/rysh-ai/rysh-cli-shared/peercoord"
	"github.com/rysh-ai/rysh-cli-code/internal/peer"
)

func TestSummarySeparatesAccountsRunnersAndHumanDecisions(t *testing.T) {
	b := &Board{Connection: peer.Connection{Board: "math"}, Token: "do-not-export", State: pc.NewStore().State,
		Endpoints: map[string]*Endpoint{}, Executions: map[string]*Execution{}}
	b.State.Peers["p1"] = &pc.Peer{UserID: "owner"}
	b.State.Peers["p2"] = &pc.Peer{UserID: "owner"}
	b.State.Agents["a"] = &pc.Agent{ID: "a", Runner: "claude", Ready: true}
	b.State.Agents["b"] = &pc.Agent{ID: "b", Runner: "codex"}
	b.Endpoints["b"] = &Endpoint{Agent: *b.State.Agents["b"], Mode: "manual", Enrolled: true}
	b.State.Tasks["t1"] = &pc.Task{ID: "t1", Number: 1, AgentID: "b", State: "offered"}
	b.State.Tasks["t2"] = &pc.Task{ID: "t2", Number: 2, State: "result_submitted", Attempts: []pc.Attempt{{Result: json.RawMessage(`{"summary":"private-result"}`)}}}
	b.State.Tasks["t3"] = &pc.Task{ID: "t3", Number: 3, AgentID: "b", State: "queued"}
	s := summarizeBoard(b)
	if s.RegisteredAccounts != 1 || s.RegisteredAgents != 2 || s.ReadyAgents != 1 {
		t.Fatalf("wrong counts: %+v", s)
	}
	if s.Runners["claude"] != 1 || s.Runners["codex"] != 1 {
		t.Fatal(s.Runners)
	}
	if len(s.Attention) != 2 || s.Attention[0].Kind != "leader_review" || s.Attention[1].Kind != "local_admission" {
		t.Fatal(s.Attention)
	}
	data, err := json.Marshal(s)
	if err != nil || strings.Contains(string(data), "do-not-export") || strings.Contains(string(data), "private-result") {
		t.Fatal("summary exposed internal content")
	}
}
