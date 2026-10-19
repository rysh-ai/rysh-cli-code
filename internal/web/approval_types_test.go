// SPDX-License-Identifier: Apache-2.0

package web

import (
	"fmt"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// F-92 residue: the server remembers each forwarded request's type so a
// question's answer is echoed as an answer. Requests answered elsewhere (the
// TUI, a timeout) never come back to be forgotten, so in a long session the map
// fills — and it used to be CLEARED WHOLESALE at 1024, wiping a question asked a
// second earlier along with a thousand stale gates. Its answer was then echoed
// [approved] / [rejected]. Eviction must drop the oldest entries, not the live
// one.
func TestApprovalTypesEvictOldestNotTheLiveQuestion(t *testing.T) {
	s := &Server{}
	for i := 0; i < 1000; i++ { // gates answered in the TUI: never taken here
		s.rememberApprovalType(fmt.Sprintf("stale-%d", i), msg.ApprovalTypeDestructive)
	}
	s.rememberApprovalType("q", msg.ApprovalTypeQuestion)
	for i := 0; i < 100; i++ { // the session keeps going while the question waits
		s.rememberApprovalType(fmt.Sprintf("later-%d", i), msg.ApprovalTypeDiff)
	}

	if got := s.takeApprovalType("q"); got != msg.ApprovalTypeQuestion {
		t.Fatalf("live question's type = %q, want %q (lost to the map's reset)", got, msg.ApprovalTypeQuestion)
	}
	if got := s.takeApprovalType("stale-0"); got != "" {
		t.Errorf("oldest stale entry survived (%q): the map is not bounded", got)
	}
	if got := s.takeApprovalType("later-99"); got != msg.ApprovalTypeDiff {
		t.Errorf("newest entry lost: %q", got)
	}
	if n := len(s.approvalTypes); n > maxRememberedApprovalTypes {
		t.Errorf("map holds %d entries, cap %d", n, maxRememberedApprovalTypes)
	}
}

// Every request answered at the door (the common case) keeps the map tiny, so
// eviction never runs — the queue of taken IDs must still not grow without bound.
func TestApprovalTypesOrderStaysBounded(t *testing.T) {
	s := &Server{}
	for i := 0; i < 10*maxRememberedApprovalTypes; i++ {
		id := fmt.Sprintf("r-%d", i)
		s.rememberApprovalType(id, msg.ApprovalTypeQuestion)
		s.takeApprovalType(id)
	}
	if n := len(s.approvalTypes); n > maxRememberedApprovalTypes {
		t.Errorf("map holds %d entries, cap %d", n, maxRememberedApprovalTypes)
	}
	if n := len(s.approvalOrder); n > 2*maxRememberedApprovalTypes {
		t.Errorf("order queue holds %d ids, want <= %d", n, 2*maxRememberedApprovalTypes)
	}
}
