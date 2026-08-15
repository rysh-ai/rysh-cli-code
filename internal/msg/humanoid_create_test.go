// SPDX-License-Identifier: Apache-2.0

package msg

// MsgHumanoidCreate gained additive Provider/Profile fields (designs 006 MP2
// and 007 PM1/PM3). Lock the wire contract: they round-trip when set and stay
// absent (omitempty) when not, so pre-MP2 payloads decode unchanged.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMsgHumanoidCreateProviderProfileRoundTrip(t *testing.T) {
	in := MsgHumanoidCreate{
		Name:         "assistant",
		SystemPrompt: "You are my personal assistant.",
		Contacts:     map[string]ChannelConfig{"telegram": {Enabled: true, Allowlist: []string{"123"}}},
		Provider:     "ollama",
		Profile:      "assistant",
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out MsgHumanoidCreate
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Provider != "ollama" || out.Profile != "assistant" {
		t.Errorf("round trip lost provider/profile: %+v", out)
	}

	// Empty fields are omitted — backward-compatible wire shape.
	raw, _ = json.Marshal(MsgHumanoidCreate{Name: "support"})
	if strings.Contains(string(raw), "provider") || strings.Contains(string(raw), "profile") {
		t.Errorf("empty provider/profile serialized: %s", raw)
	}

	// A pre-MP2 payload (no provider/profile keys) decodes to empty fields.
	var legacy MsgHumanoidCreate
	if err := json.Unmarshal([]byte(`{"name":"support","system_prompt":"hi"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Provider != "" || legacy.Profile != "" {
		t.Errorf("legacy payload gained provider/profile: %+v", legacy)
	}
}
