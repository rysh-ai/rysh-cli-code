// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/tools"
)

// Design 093 H1, at the cli wiring: a real pane LLM actor from
// Setup.CreateLLMPromptExecutionActor. Without a profile its turn tools are the
// session's full set and the family tools are NOT among them; with a family
// profile its turn offers exactly the allow-list — the native family tools
// included, bash and every file/git tool absent; a clear restores the defaults.
func TestPaneProfile_FamilyAllowListOnRealPaneActor(t *testing.T) {
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
	llm := s.CreateLLMPromptExecutionActor(ScopeIDs{PaneID: "pane-ada"}, cfg, pub, nc, nil, nil)

	def := llm.TurnTools()
	for _, want := range []string{"bash", "file_write", "edit", "git_commit"} {
		if _, ok := def.Get(want); !ok {
			t.Errorf("default pane lost %s", want)
		}
	}
	for _, n := range tools.FamilyToolNames {
		if _, ok := def.Get(n); ok {
			t.Errorf("a pane WITHOUT a profile offers %s", n)
		}
	}
	defaultNames := def.Names()

	llm.SetAgentProfile(&msg.MsgSetPaneAgentProfile{
		SystemPrompt: "You are Ada.",
		AllowTools:   []string{"family_ask", "family_reply", "family_status", "web_search"},
	})
	if got, want := llm.TurnTools().Names(), []string{"family_ask", "family_reply", "family_status", "web_search"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("profiled turn tools = %v, want %v", got, want)
	}
	for _, denied := range []string{"bash", "bash_background", "bash_session", "file_write", "edit", "file_read", "git_commit", "rysh_build_pipeline", "pane_send"} {
		if _, ok := llm.TurnTools().Get(denied); ok {
			t.Errorf("profiled pane can still reach %s", denied)
		}
	}

	// e20 W2: list_tools on the allow-list lists exactly the turn's tools —
	// the pane's own list_tools would list the whole default set.
	llm.SetAgentProfile(&msg.MsgSetPaneAgentProfile{AllowTools: []string{"family_reply", "list_tools"}})
	lt, ok := llm.TurnTools().Get("list_tools")
	if !ok {
		t.Fatal("list_tools missing from the profiled turn")
	}
	out, err := lt.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.Content, "Available tools (2):") || !strings.Contains(out.Content, "- family_reply:") ||
		strings.Contains(out.Content, "- bash:") || strings.Contains(out.Content, "- file_write:") {
		t.Fatalf("profiled list_tools lists more than the turn registry:\n%s", out.Content)
	}
	if pl, _ := def.Get("list_tools"); pl != nil {
		full, _ := pl.Execute(context.Background(), json.RawMessage(`{}`))
		if !strings.Contains(full.Content, "- bash:") {
			t.Errorf("an unprofiled pane's list_tools changed:\n%.200s", full.Content)
		}
	}

	llm.SetAgentProfile(&msg.MsgSetPaneAgentProfile{})
	if got := llm.TurnTools().Names(); !reflect.DeepEqual(got, defaultNames) {
		t.Errorf("clear did not restore the default tools:\n got %v\nwant %v", got, defaultNames)
	}
}

// PaneProfileDir is <daemon workspace root>/.rysh/pane-profiles — the single
// place the workspace writes profile prompts and panes read them (e20 W1).
func TestSetup_PaneProfileDir(t *testing.T) {
	os.Setenv("RYSH_AGENTIC_ENABLED", "true")
	defer os.Unsetenv("RYSH_AGENTIC_ENABLED")
	s := NewSetupAlwaysOn(config.Config{ProviderName: "claude"}, nil)
	wd, _ := os.Getwd()
	wd, _ = filepath.Abs(wd)
	if got, want := s.PaneProfileDir(), filepath.Join(wd, ".rysh", "pane-profiles"); got != want {
		t.Fatalf("PaneProfileDir = %q, want %q", got, want)
	}
	var nilSetup *Setup
	if nilSetup.PaneProfileDir() != "" || (&Setup{}).PaneProfileDir() != "" {
		t.Fatal("a Setup without a root must report no directory")
	}
}
