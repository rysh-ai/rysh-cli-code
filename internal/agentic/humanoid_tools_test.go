// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// A humanoid's input comes from outside the session (WhatsApp, Slack, email
// contacts), so it must not get pane_inspect: a contact could otherwise have
// it read any pane's screen into a reply (security-lead ledger 113, C1). Panes
// and agents keep the tool (C2).
func TestHumanoidRegistryLacksPaneInspect(t *testing.T) {
	os.Setenv("RYSH_AGENTIC_ENABLED", "true")
	defer os.Unsetenv("RYSH_AGENTIC_ENABLED")

	srv, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	defer srv.Shutdown()
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	cfg := config.Config{ProviderName: "claude"}
	s := NewSetupAlwaysOn(cfg, nil)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())

	humanoid := s.CreateHumanoidLLMWithChannelTools("h", cfg, pub, nc, "", "", HumanoidChannelTools{})
	if _, ok := humanoid.TurnTools().Get("pane_inspect"); ok {
		t.Error("the humanoid registry offers pane_inspect")
	}

	pane := s.CreateLLMPromptExecutionActor(ScopeIDs{PaneID: "pane-1"}, cfg, pub, nc, nil, nil)
	if _, ok := pane.TurnTools().Get("pane_inspect"); !ok {
		t.Error("the pane registry lost pane_inspect")
	}
	agent := s.CreateAgentLLMPromptExecutionActor("a", cfg, pub, nc, "", "")
	if _, ok := agent.TurnTools().Get("pane_inspect"); !ok {
		t.Error("the agent registry lost pane_inspect")
	}
}
