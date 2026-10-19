// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"fmt"
	"strings"

	"path/filepath"

	"github.com/asynkron/protoactor-go/actor"
	sharedprovider "github.com/rysh-ai/rysh-cli-shared/provider"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/llms"
	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
	"github.com/rysh-ai/rysh-cli-code/internal/meetingexec"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

func (w *WorkspaceActor) handleMeeting(ctx actor.Context, c meeting.Command, env *msg.RequestEnvelope) {
	if c.Action == "register" || c.Action == "unregister" {
		// Pane meetings are bound and dropped by ##meeting and pane close.
		_ = env.Reply(&msg.MsgMeetingReply{Reply: meeting.Reply{Error: c.Action + " is internal to the daemon; use ##meeting join / leave"}})
		return
	}
	w.meetingSpeechVars(&c, "")
	c.Defaults()
	if w.meeting == nil {
		w.meeting = meeting.NewManager()
	}
	manager := w.meeting
	var keys meeting.Credentials
	var execute meeting.Execute
	starts := c.Action == "join" || c.Action == "host"
	if starts || c.Action == "doctor" {
		keys = w.meetingCredentials(c)
	}
	if starts && w.agSetup != nil {
		var err error
		execute, err = meetingexec.New(ctx.ActorSystem(), w.agSetup.Provider, c.ReadDir)
		if err != nil {
			_ = env.Reply(&msg.MsgMeetingReply{Reply: meeting.Reply{Error: err.Error()}})
			return
		}
	}
	var agents []meeting.Agent
	names := c.Agents
	if starts {
		c.Model = w.sessionModelLabel()
		c.AuditPath = filepath.Join(w.cfg.SessionDir, w.cfg.SessionName, "meeting-audit.jsonl")
	}
	if c.Action == "attach" {
		names = []string{c.Agent}
	}
	if starts || c.Action == "attach" {
		for _, name := range names {
			a, err := w.meetingAgent(name)
			if err != nil {
				_ = env.Reply(&msg.MsgMeetingReply{Reply: meeting.Reply{Error: err.Error()}})
				return
			}
			agents = append(agents, a)
		}
	}
	// Provider requests and listener shutdown must never block workspace mailbox.
	go func() {
		reply := manager.Do(c, keys, execute, agents...)
		_ = env.Reply(&msg.MsgMeetingReply{Reply: reply})
	}()
}

// meetingCredentials resolves the secrets a meeting start (join, host) or
// doctor needs from the session's secret scopes; doctor reports only whether
// each one is set.
func (w *WorkspaceActor) meetingCredentials(c meeting.Command) meeting.Credentials {
	var keys meeting.Credentials
	resolve := func(name string) string { v, _, _, _ := w.secrets.GetLayered(w.secretScopeChain(""), name); return v }
	keys.Recall = resolve("RECALL_API_KEY")
	keys.Verification = resolve("RECALL_VERIFICATION_SECRET")
	keys.Speech = resolve(meeting.SpeechKeyName(c.TTS))
	keys.LiveKit.STTKey = resolve(meeting.STTKeyName(c.STT))
	if c.ZoomOBF {
		keys.Zoom = w.zoomOBFCredentials()
	}
	if c.Action == "doctor" {
		// Presence only: doctor reports names, never values (E-86).
		keys.LiveKit = meeting.LiveKitCredentials{URL: resolve(meeting.LiveKitURLSecret), APIKey: resolve(meeting.LiveKitKeySecret), APISecret: resolve(meeting.LiveKitSecretSecret), STTKey: keys.LiveKit.STTKey}
		keys.RyshAPIKey = resolve(meeting.RyshAPIKeySecret)
		keys.UpstreamAPIKey = w.cfg.Upstream.APIKey != ""
	}
	if c.Action == "host" {
		keys.LiveKit = meeting.LiveKitCredentials{URL: resolve(meeting.LiveKitURLSecret), APIKey: resolve(meeting.LiveKitKeySecret), APISecret: resolve(meeting.LiveKitSecretSecret), JoinPage: resolve(meeting.LiveKitJoinURLSecret), STTKey: keys.LiveKit.STTKey}
		if c.Server != "" {
			// A key with write, else the session's upstream key (Company
			// Connect keys are read-only; the server then answers 403).
			keys.RyshAPIKey = resolve(meeting.RyshAPIKeySecret)
			if keys.RyshAPIKey == "" {
				keys.RyshAPIKey = w.cfg.Upstream.APIKey
			}
		}
	}
	return keys
}

// meetingAgent turns a loaded `##agent` agent into a meeting participant
// (design 040 §MTG-12). Founder decision 2026-09-28: it answers with its own
// full toolset — the same executor construction as the session's agent, with
// its prompt, registry, MCP/integration scope and policy — and no per-action
// approval. The controls are the speaker allowlist, stop/pause/leave and the
// audit log (meeting/audit.go). Each turn gets a fresh executor with its own
// bus id, so it never interleaves with the live agent's conversation.
func (w *WorkspaceActor) meetingAgent(name string) (meeting.Agent, error) {
	if err := meeting.ValidAgentName(name); err != nil {
		return meeting.Agent{}, fmt.Errorf("agent %q: %v", name, err)
	}
	if w.agentRegistryPID == nil || w.agSetup == nil {
		return meeting.Agent{}, errors.New("agent registry not available (agentic mode disabled?)")
	}
	live := func() error {
		a, ok := w.loadedAgent(name)
		switch {
		case !ok:
			return fmt.Errorf("agent %q is not loaded; spawn it with ##agent spawn %s", name, name)
		case !a.Active:
			return fmt.Errorf("agent %q is deactivated", name)
		}
		return nil
	}
	if err := live(); err != nil {
		return meeting.Agent{}, err
	}
	info, _ := w.loadedAgent(name)
	// Voice and model come from the skill file; the system prompt is the one
	// the loaded agent runs with, so it answers exactly as it does in the
	// session (founder decision 2026-09-28).
	var voice, model string
	if def, err := meetingDefinition(name, info); err == nil {
		voice, model = def.Voice, def.Model
	}
	prov, label, note := w.meetingModel(model)
	if err := meetingexec.LiveProviderError(prov); err != nil {
		return meeting.Agent{}, err
	}
	setup := *w.agSetup
	setup.Provider = prov
	cfg, pub, nc, prompt := w.cfg, w.pub, w.nc, info.SystemPrompt
	execute, err := meetingexec.NewToolAgent(w.actorSystem, meetingexec.ToolAgent{
		Name: name,
		Live: live,
		Build: func(id string) *agentic.LLMPromptExecutionActor {
			a := setup.CreateAgentLLMPromptExecutionActor(id, cfg, pub, nc, prompt, "")
			a.SetAgentName(name) // usage is attributed to the agent, not the turn id
			return a
		},
		NC:     nc,
		Codecs: pub.Codecs(),
	})
	if err != nil {
		return meeting.Agent{}, fmt.Errorf("agent %q: %v", name, err)
	}
	return meeting.Agent{Name: name, Voice: voice, Model: label, Note: note, Execute: execute}, nil
}

// meetingDefinition reads the agent's voice and model (and role) from its
// skill file WITHOUT ${VAR} expansion — nothing here needs secret values.
// The file is the one recorded at spawn (any path);
// agents restored from before that record fall back to a lookup by name.
// An inline agent (no file) has only the text typed at spawn, which was
// never expanded.
func meetingDefinition(name string, info msg.AgentInfo) (*agentDefinition, error) {
	noExpand := func(s string) string { return s }
	if info.SkillPath != "" {
		def, err := parseSkillFile(info.SkillPath, noExpand)
		if err != nil || def.Name != name {
			// Never fall back to the loaded (expanded) prompt for a file agent.
			return nil, fmt.Errorf("agent %q: its skill file %s is gone or renamed; re-spawn it with ##agent spawn", name, info.SkillPath)
		}
		return def, nil
	}
	if def, err := parseSkillFile(name, noExpand); err == nil && def.Name == name {
		return def, nil
	}
	// A frontmatter name that differs from its directory name.
	if defs, err := parseSkillDir("", noExpand); err == nil {
		for _, def := range defs {
			if def.Name == name {
				return def, nil
			}
		}
	}
	return &agentDefinition{Name: name, SystemPrompt: info.SystemPrompt}, nil
}

// meetingModel resolves an agent's `model:` for its meeting turns. It is
// applied only when the session's ##llm registry declares it (a
// "provider/name" ref, a registry name or an API model id), enabled and
// runnable, with a key in reach; anything else answers on the session model,
// with a note saying why. The label names the model that answers.
func (w *WorkspaceActor) meetingModel(model string) (provider.AgenticProvider, string, string) {
	session := w.sessionModelLabel()
	if strings.TrimSpace(model) == "" {
		return w.agSetup.Provider, session, ""
	}
	fallback := func(why string) (provider.AgenticProvider, string, string) {
		return w.agSetup.Provider, session, "model " + model + " " + why + "; answering on the session model " + session + "."
	}
	spec := findModel(llms.NewStore(w.cfg.RyshDir), model)
	switch {
	case spec == nil:
		return fallback("is not configured in this session (##llm list)")
	case spec.Disabled:
		return fallback("is disabled")
	case !spec.Executable():
		return fallback("has no executor")
	}
	family, configured := providerFamily(spec.Provider), providerFamily(w.cfg.ProviderName)
	if key, missing := w.missingProviderKey(family); missing {
		return fallback("needs " + key + " in this session's secrets")
	}
	cfg := w.cfg
	cfg.ProviderName = family
	cfg.DefaultModel = spec.Model
	if family != configured {
		// The configured provider's key and endpoint never go to another family.
		cfg.APIURL = ""
		cfg.APIKey = w.providerAPIKey(family)
	}
	var prov provider.AgenticProvider = provider.NewAgenticProvider(cfg)
	if mo, ok := prov.(sharedprovider.ModelEffortOverridable); ok {
		prov = mo.WithModelEffort(spec.Model, spec.Effort)
	}
	return prov, spec.Provider + "/" + spec.Name, ""
}

// findModel looks a model up in the ##llm registry by ref, name or API id.
func findModel(store *llms.Store, model string) *llms.ModelSpec {
	if p, n, err := llms.ParseRef(model); err == nil {
		spec, err := store.Get(p, n)
		if err != nil {
			return nil
		}
		return spec
	}
	_, byProvider, err := store.List()
	if err != nil {
		return nil
	}
	var byID *llms.ModelSpec
	for _, specs := range byProvider {
		for i := range specs {
			if specs[i].Name == model {
				return &specs[i]
			}
			if specs[i].Model == model && byID == nil {
				byID = &specs[i]
			}
		}
	}
	return byID
}

// agentMeeting implements `##agent meeting <name> [off]`: attach a spawned
// agent to the session's live meeting, or detach it. Joining stays with
// `rysh meeting join`, which needs consent and a public URL.
func (w *WorkspaceActor) agentMeeting(out *strings.Builder, paneID string, args []string) {
	const usage = "##agent meeting <name> [off]"
	if len(args) < 1 || len(args) > 2 || (len(args) == 2 && args[1] != "off") {
		ryshWriter(out).UsageLineIn("agents", usage)
		w.failRyshUsage("usage: %s", usage)
		return
	}
	name := args[0]
	if w.meeting == nil {
		fmt.Fprintf(out, "\n[agents] no active meeting; join one with rysh meeting join <url> --agent %s …\n", name)
		w.failRysh("no active meeting")
		return
	}
	c := meeting.Command{Action: "detach", Agent: name}
	var agents []meeting.Agent
	if len(args) == 1 {
		a, err := w.meetingAgent(name)
		if err != nil {
			fmt.Fprintf(out, "\n[agents] %v\n", err)
			w.failRysh("%v", err)
			return
		}
		c = meeting.Command{Action: "attach", Agent: name}
		agents = []meeting.Agent{a}
	}
	manager, pub := w.meeting, w.pub
	fmt.Fprintf(out, "\n[agents] %s agent %q …\n", map[string]string{"attach": "attaching", "detach": "detaching"}[c.Action], name)
	// Manager.Do may wait behind a join's provider call; never block the mailbox.
	go func() {
		r := manager.Do(c, meeting.Credentials{}, nil, agents...)
		text := fmt.Sprintf("\n[agents] meeting agents: %s\n", strings.Join(r.Agents, ", "))
		if r.Error != "" {
			text = "\n[agents] meeting: " + r.Error + "\n"
		} else if r.Notice != "" {
			text += "[agents] " + r.Notice + "\n"
		}
		_ = pub.SendPaneRyshOutput(paneID, text)
	}()
}

// zoomOBFCredentials resolves the --zoom-obf secrets. Zoom rotates the refresh
// token on every refresh, so the new one is written back to the scope and
// tier the old one came from; env/config values cannot be rewritten, and
// the bridge then reports that the Zoom user must re-authorise.
func (w *WorkspaceActor) zoomOBFCredentials() meeting.ZoomCredentials {
	chain := w.secretScopeChain("")
	get := func(name string) string { v, _, _, _ := w.secrets.GetLayered(chain, name); return v }
	z := meeting.ZoomCredentials{ClientID: get(meeting.ZoomClientIDSecret), ClientSecret: get(meeting.ZoomClientSecretSecret)}
	refresh, src, scope, _ := w.secrets.GetLayered(chain, meeting.ZoomRefreshTokenSecret)
	z.RefreshToken = refresh
	secrets := w.secrets
	z.SaveRefresh = func(v string) error {
		switch src {
		case secretSourcePersist:
			return secrets.Set(scope, meeting.ZoomRefreshTokenSecret, v, true)
		case secretSourceSession:
			return secrets.Set(scope, meeting.ZoomRefreshTokenSecret, v, false)
		}
		return errors.New("refresh token not in a writable secret tier")
	}
	return z
}
