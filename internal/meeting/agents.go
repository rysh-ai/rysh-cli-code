// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
)

// meetingAgent is an attached Agent plus the normalized words that address it.
type meetingAgent struct {
	Agent
	words []string
}

// agentName is the shape of a meeting-addressable agent name: one to three
// letter-only words joined by '-' or '_' ("scribe", "code-reviewer"). STT
// writes words, not digits or punctuation, so anything else could never be
// addressed by voice.
var agentName = regexp.MustCompile(`^[A-Za-z]{2,20}([-_][A-Za-z]{2,20}){0,2}$`)

// ValidAgentName reports whether name can be attached to a meeting.
func ValidAgentName(name string) error {
	if !agentName.MatchString(name) {
		return errors.New("agent name must be 1–3 letter-only words joined by - or _ to be addressable by voice")
	}
	words := agentWords(name)
	joined := strings.Join(words, "")
	if wakeName.MatchString(joined) || wakeMerged.MatchString(joined) {
		return errors.New("agent name sounds like \"rysh\"; rename the agent")
	}
	for _, w := range words {
		if wakeGreetings[w] {
			return errors.New("agent name must not contain a greeting word")
		}
	}
	return nil
}

func agentWords(name string) []string {
	return strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return r == '-' || r == '_' })
}

// Voices a skill file may select with `voice:`; the Deepgram route takes an
// Aura 2 model name instead, ElevenLabs a voice id (speech.go).
var openAIVoices = map[string]bool{"alloy": true, "ash": true, "ballad": true, "coral": true, "echo": true, "fable": true, "nova": true, "onyx": true, "sage": true, "shimmer": true, "verse": true, "marin": true, "cedar": true}
var deepgramVoice = regexp.MustCompile(`^aura-2-[a-z]{2,20}-en$`)

// Distinct defaults so several agents in one meeting sound different; rysh
// itself keeps the provider default (coral / thalia).
var defaultVoices = map[string][]string{
	"openai":   {"ash", "sage", "verse", "ballad"},
	"deepgram": {"aura-2-apollo-en", "aura-2-andromeda-en", "aura-2-helena-en", "aura-2-arcas-en"},
	// George, Sarah, Brian, Alice (premade; rysh itself speaks as Rachel).
	"elevenlabs": {"JBFqnCBsd6RMkjVDRZzb", "EXAVITQu4vr4xnSDxMaL", "nPczCjzI2devNBz1zQrb", "Xb7hH8MSUJpSbSDYk0k2"},
}

// bindAgent validates a against the meeting's TTS provider and the agents
// already attached, choosing an unused default voice when none is set.
func bindAgent(a Agent, tts string, attached []meetingAgent) (meetingAgent, error) {
	if err := ValidAgentName(a.Name); err != nil {
		return meetingAgent{}, err
	}
	if a.Execute == nil {
		return meetingAgent{}, errors.New("agent executor unavailable")
	}
	if len(attached) >= maxAgents {
		return meetingAgent{}, errors.New("a meeting answers as at most 4 agents; detach one first")
	}
	words := agentWords(a.Name)
	used := map[string]bool{}
	for _, b := range attached {
		if strings.Join(b.words, " ") == strings.Join(words, " ") {
			return meetingAgent{}, errors.New("agent already attached to this meeting")
		}
		used[b.Voice] = true
	}
	switch {
	case a.Voice == "":
		for _, v := range defaultVoices[tts] {
			if !used[v] {
				a.Voice = v
				break
			}
		}
	case !validVoice(tts, a.Voice):
		return meetingAgent{}, errors.New("unsupported voice for the meeting's --tts provider")
	}
	return meetingAgent{Agent: a, words: words}, nil
}

// matchAgent returns the longest attached agent addressed at norm[i:] and the
// index after its name, or "", -1. A multi-word name also matches written
// together ("codereviewer").
func matchAgent(norm []string, i int, agents []meetingAgent) (string, int) {
	name, end := "", -1
	for _, a := range agents {
		n := len(a.words)
		if i+n <= len(norm) && strings.Join(norm[i:i+n], " ") == strings.Join(a.words, " ") && i+n > end {
			name, end = a.Name, i+n
		} else if n > 1 && i < len(norm) && norm[i] == strings.Join(a.words, "") && i+1 > end {
			name, end = a.Name, i+1
		}
	}
	return name, end
}

// wakeTarget finds the first address in a finalized segment: "hey rysh" (and
// its STT spellings) or "hey <agent>". It returns the addressed agent ("" =
// rysh) and the index of the first request word, or -1 when nobody was
// addressed. prefixed means this speaker's previous segment ended in a
// greeting, so a bare name at the start completes the address.
func wakeTarget(fields []string, agents []meetingAgent, prefixed bool) (string, int) {
	norm := make([]string, len(fields))
	for i, f := range fields {
		norm[i] = normWord(f)
	}
	for i := range norm {
		if wakeMerged.MatchString(norm[i]) {
			return "", i + 1
		}
		if wakeGreetings[norm[i]] {
			if a, end := matchAgent(norm, i+1, agents); a != "" {
				return a, end
			}
			if i+1 < len(norm) && wakeName.MatchString(norm[i+1]) {
				return "", i + 2
			}
		}
	}
	// Name-first address, marked by punctuation: "Scribe, ..." / "Rish: ...".
	addressed := func(end int) bool {
		return end < len(fields) && strings.ContainsAny(fields[end-1][len(fields[end-1])-1:], ",:!")
	}
	if a, end := matchAgent(norm, 0, agents); a != "" && (prefixed || addressed(end)) {
		return a, end
	}
	if len(norm) >= 1 && wakeName.MatchString(norm[0]) && (prefixed || addressed(1)) {
		return "", 1
	}
	return "", -1
}

// agentStopRequested is stopRequested for attached agents' names: "scribe
// stop", "hey code reviewer, cancel". Any agent's name stops the current turn,
// whoever it belongs to: there is one speaker, and stopping is always safe.
func agentStopRequested(fields []string, agents []meetingAgent) bool {
	if len(agents) == 0 || len(fields) == 0 || len(fields) > 8 {
		return false
	}
	norm := make([]string, len(fields))
	for i, f := range fields {
		norm[i] = normWord(f)
	}
	for i := range norm {
		name, end := matchAgent(norm, i, agents)
		if name == "" {
			continue
		}
		for j := max(0, i-2); j <= min(len(norm)-1, end+1); j++ {
			if (j < i || j >= end) && stopWords[norm[j]] {
				return true
			}
		}
	}
	return false
}

// attach adds an agent to a live meeting.
func (s *Service) attach(a Agent) Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal {
		return Reply{Error: "meeting has ended"}
	}
	bound, err := bindAgent(a, s.cfg.TTS, s.agents)
	if err != nil {
		return Reply{Error: err.Error()}
	}
	s.agents = append(s.agents, bound)
	r := s.statusLocked(false)
	r.Notice = "Agent " + a.Name + " attached; say \"hey " + strings.Join(bound.words, " ") + "\"."
	if a.Model != "" {
		r.Notice += " Model: " + a.Model + "."
	}
	if a.Note != "" {
		r.Notice += " " + a.Note
	}
	return r
}

// detach removes an agent; a turn addressed to it is discarded, including one
// already executing or speaking.
func (s *Service) detach(name string) Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.agents {
		if a.Name != name {
			continue
		}
		s.agents = append(s.agents[:i:i], s.agents[i+1:]...)
		if s.primary == name {
			s.primary = ""
		}
		if s.pending != nil && s.pending.agent == name {
			s.clearLocked()
			if !s.terminal && !s.paused {
				s.state = "listening"
			}
		}
		return s.statusLocked(false)
	}
	return Reply{Error: "agent not attached to this meeting"}
}

// hasAgent reports whether name is attached to this bot.
func (s *Service) hasAgent(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.agentLocked(name)
	return ok
}

// setPrimary applies D9's "hey rysh" rule: the only bot of a meeting answers
// it as before; one of several answers only its own agent's name.
func (s *Service) setPrimary(sole bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noRysh = !sole
}

// addresseeLocked maps a wake target to the agent that answers it: "hey rysh"
// goes to the primary agent when the meeting was joined with --agent.
func (s *Service) addresseeLocked(agent string) string {
	if agent == "" {
		return s.primary
	}
	return agent
}

func (s *Service) agentLocked(name string) (meetingAgent, bool) {
	for _, a := range s.agents {
		if a.Name == name {
			return a, true
		}
	}
	return meetingAgent{}, false
}

// executorLocked returns the executor for a turn's agent; false means the
// agent was detached after it was addressed.
func (s *Service) executorLocked(agent string) (Execute, bool) {
	if agent == "" {
		return s.execute, true
	}
	a, ok := s.agentLocked(agent)
	return a.Execute, ok
}

// synthLocked returns speech in the turn's agent's voice.
// defaultVoiceLocked reports whether the agent speaks in the session's
// default voice (s.speech) rather than a per-agent one.
func (s *Service) defaultVoiceLocked(agent string) bool {
	_, ok := s.agentLocked(agent)
	return agent == "" || !ok || s.voice == nil
}

// streamLocked is the streamed counterpart of synthLocked, or nil when the
// turn's voice has no streamed synthesis (the whole-file path then speaks).
func (s *Service) streamLocked(agent string) SynthesizeStream {
	if s.defaultVoiceLocked(agent) {
		return s.speechStream
	}
	a, _ := s.agentLocked(agent)
	voiceStream := s.voiceStream
	if voiceStream == nil {
		return nil
	}
	return func(ctx context.Context, text string) (io.ReadCloser, error) { return voiceStream(ctx, a.Voice, text) }
}

func (s *Service) synthLocked(agent string) Synthesize {
	a, ok := s.agentLocked(agent)
	if agent == "" || !ok || s.voice == nil {
		return s.speech
	}
	voice := s.voice
	return func(ctx context.Context, text string) ([]byte, error) { return voice(ctx, a.Voice, text) }
}

func (s *Service) agentNamesLocked() []string {
	names := make([]string, 0, len(s.agents))
	for _, a := range s.agents {
		names = append(names, a.Name)
	}
	return names
}

// agentCount is the number of agents attached to this bot.
func (s *Service) agentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.agents)
}

// voices lists the voices this bot's agents speak in.
func (s *Service) voices() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.voicesLocked()
}

func (s *Service) voicesLocked() []string {
	out := make([]string, 0, len(s.agents))
	for _, a := range s.agents {
		out = append(out, a.Voice)
	}
	return out
}
