// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"reflect"
	"testing"
)

func TestParseAgentTools(t *testing.T) {
	if got := ParseAgentTools(" a, b,,a ,c "); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("ParseAgentTools = %v", got)
	}
	if got := ParseAgentTools(""); len(got) != 0 {
		t.Fatalf("empty = %v", got)
	}
	meta := map[string]string{PaneMetaAgentTools: "family_ask, family_reply"}
	if !AgentToolsAllow(meta, "family_reply") || AgentToolsAllow(meta, "bash") || AgentToolsAllow(nil, "family_reply") {
		t.Fatal("AgentToolsAllow wrong")
	}
	if !IsPaneAgentProfileKey(PaneMetaAgentTools) || !IsPaneAgentProfileKey(PaneMetaAgentPromptFile) || IsPaneAgentProfileKey("board.id") {
		t.Fatal("IsPaneAgentProfileKey wrong")
	}
}
