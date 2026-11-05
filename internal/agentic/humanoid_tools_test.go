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

// The tools that reach into the session (pane_send, session_history,
// agents_list, rysh_command) are not on a humanoid's registry unless the
// caller grants SessionTools; pane_inspect never is. rysh_command comes only
// with RyshCommand (the adapter-less owner assistant, as before).
func TestHumanoidSessionToolsOnlyWhenGranted(t *testing.T) {
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
	session := []string{"pane_send", "session_history", "agents_list", "rysh_command", "pane_inspect"}

	team := s.CreateHumanoidLLMWithChannelTools("team", cfg, pub, nc, "", "", HumanoidChannelTools{})
	for _, name := range session {
		if _, ok := team.TurnTools().Get(name); ok {
			t.Errorf("a humanoid without SessionTools offers %s", name)
		}
	}
	owner := s.CreateHumanoidLLMWithChannelTools("owner", cfg, pub, nc, "", "", HumanoidChannelTools{SessionTools: true})
	for _, name := range []string{"pane_send", "session_history", "agents_list"} {
		if _, ok := owner.TurnTools().Get(name); !ok {
			t.Errorf("a humanoid with SessionTools lacks %s", name)
		}
	}
	for _, name := range []string{"pane_inspect", "rysh_command"} {
		if _, ok := owner.TurnTools().Get(name); ok {
			t.Errorf("a humanoid with SessionTools (no RyshCommand) offers %s", name)
		}
	}
	adapterless := s.CreateHumanoidLLMWithChannelTools("tg", cfg, pub, nc, "", "", HumanoidChannelTools{SessionTools: true, RyshCommand: true})
	if _, ok := adapterless.TurnTools().Get("rysh_command"); !ok {
		t.Error("the adapter-less owner assistant lost rysh_command")
	}
	if _, ok := adapterless.TurnTools().Get("pane_inspect"); ok {
		t.Error("the adapter-less owner assistant offers pane_inspect")
	}
}
