// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
	"time"

	pc "github.com/rysh-ai/rysh-cli-shared/peercoord"
)

func TestPeerBoardDistinguishesLongDisconnectFromDeparture(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	p := &pc.Peer{State: "offline", LastSeen: now.Add(-15 * time.Minute)}
	if strings.Contains(peerBoardPresence(p, now), ">15m") {
		t.Fatal("15-minute boundary marked exceeded")
	}
	if got := peerBoardPresence(p, now.Add(time.Second)); !strings.Contains(got, "15m1s") || !strings.Contains(got, ">15m; history retained") {
		t.Fatalf("long disconnect: %s", got)
	}
	p.State = "revoked"
	if got := peerBoardPresence(p, now.Add(time.Hour)); !strings.HasPrefix(got, "left") || strings.Contains(got, "offline") {
		t.Fatalf("departure mislabeled: %s", got)
	}
	p.State = "ready"
	p.LastSeen = now
	if got := peerBoardPresence(p, now); got != "ready" {
		t.Fatalf("reconnected peer: %s", got)
	}
}

func TestPeerBoardOnlyAppointedAgentDisplaysAsLeader(t *testing.T) {
	old := &pc.Agent{ID: "old", Role: "leader"}
	next := &pc.Agent{ID: "next", Role: "leader"}
	leadership := pc.Leadership{AgentID: "next", Term: 2}
	if peerBoardRole(old, leadership) != "candidate" || peerBoardRole(next, leadership) != "leader" {
		t.Fatal("handover leaves two visible leaders")
	}
}
