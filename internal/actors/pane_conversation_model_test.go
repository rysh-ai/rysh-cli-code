// SPDX-License-Identifier: Apache-2.0

package actors

// Model attribution has to survive the two conversions between the pane's live
// buffers and its stored JSON. Both are field-by-field copies, so a new field
// is silently dropped unless something checks — and a dropped tag looks exactly
// like "this answer predates attribution", which is the one state that must
// mean "nobody claims it".

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestConversationAttributionSurvivesSnapshotRoundtrip(t *testing.T) {
	live := []*msg.ConversationMessage{
		{
			TurnID: "t1", TurnType: msg.TurnQuestion, ConversationType: msg.ConvAI,
			MessageSource: msg.SourceHuman, Content: "what model are you?",
		},
		{
			TurnID: "t1", TurnType: msg.TurnAnswer, ConversationType: msg.ConvAI,
			MessageSource: msg.SourceAI, Content: "I'm ChatGPT...",
			ProviderName: "openai", Model: "gpt-5.6-luna",
		},
	}

	snaps := convertConvMsgs(live)
	if snaps[1].ProviderName != "openai" || snaps[1].Model != "gpt-5.6-luna" {
		t.Fatalf("attribution lost on the way OUT: %+v", snaps[1])
	}
	if got := snaps[1].Attribution(); got != "openai (gpt-5.6-luna)" {
		t.Errorf("Attribution() = %q", got)
	}
	// A question carries no attribution — only answers have a producer.
	if snaps[0].Attribution() != "" {
		t.Errorf("a question was attributed to %q", snaps[0].Attribution())
	}

	back := snapshotToConvMsg(snaps[1])
	if back.ProviderName != "openai" || back.Model != "gpt-5.6-luna" {
		t.Fatalf("attribution lost on the way BACK: %+v", back)
	}

	// The pane's stored form is JSON, so the tag has to appear there too.
	raw, err := json.Marshal(snaps[1])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"provider_name":"openai"`, `"model":"gpt-5.6-luna"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("stored JSON missing %s:\n%s", want, raw)
		}
	}

	// An unattributed answer must stay unattributed rather than gaining a
	// default owner: that is what stops a pre-attribution transcript from
	// being claimed by whichever model happens to be active.
	var legacy domain.ConversationMessageSnapshot
	if err := json.Unmarshal([]byte(`{"turn_id":"old","content":"x"}`), &legacy); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if legacy.Attribution() != "" {
		t.Errorf("legacy message attributed to %q", legacy.Attribution())
	}
}
