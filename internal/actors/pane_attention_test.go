// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

type attentionRig struct {
	t      *testing.T
	p      *PaneActor
	codecs *msg.CodecRegistry
	attn   *nats.Subscription
	dirty  *nats.Subscription
}

func newAttentionRig(t *testing.T) *attentionRig {
	t.Helper()
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	attn, err := nc.SubscribeSync(msg.T("ws", "attention"))
	if err != nil {
		t.Fatal(err)
	}
	dirty, err := nc.SubscribeSync(msg.T("ws", "layoutDirty"))
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	return &attentionRig{t: t, p: &PaneActor{id: "attn-pane", pub: pub}, codecs: codecs, attn: attn, dirty: dirty}
}

func (r *attentionRig) nextAttention(wait time.Duration) interface{} {
	r.t.Helper()
	m, err := r.attn.NextMsg(wait)
	if err != nil {
		return nil
	}
	var env msg.NATSEnvelope
	if err := json.Unmarshal(m.Data, &env); err != nil {
		r.t.Fatalf("envelope: %v", err)
	}
	v, err := r.codecs.Decode(env.TypeTag, env.Payload)
	if err != nil {
		r.t.Fatalf("decode %s: %v", env.TypeTag, err)
	}
	return v
}

func (r *attentionRig) expectDirty(what string) {
	r.t.Helper()
	if _, err := r.dirty.NextMsg(time.Second); err != nil {
		r.t.Fatalf("%s: no layoutDirty", what)
	}
}

func (r *attentionRig) expectNoDirty(what string) {
	r.t.Helper()
	if _, err := r.dirty.NextMsg(150 * time.Millisecond); err == nil {
		r.t.Fatalf("%s: unexpected layoutDirty", what)
	}
}

func liveMeta(p *PaneActor) map[string]string { return p.buildSnapshot(false, false, false).Meta }

func TestPromptAttentionRaisesAnEventWithNoScreenText(t *testing.T) {
	r := newAttentionRig(t)
	r.p.applyPromptAttention(true)

	ev, ok := r.nextAttention(time.Second).(*msg.MsgAttentionEvent)
	if !ok {
		t.Fatal("no MsgAttentionEvent on ws.attention")
	}
	if ev.PaneID != "attn-pane" || ev.Category != msg.AttentionPrompt {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Summary != "" {
		t.Fatalf("event summary carries text %q; it must never carry screen content", ev.Summary)
	}
	for _, s := range []string{ev.Title, ev.Summary} {
		if strings.Contains(s, "Do you want") || strings.Contains(s, "1. Yes") {
			t.Fatalf("event carries screen text: %q", s)
		}
	}
	r.expectDirty("raise")

	reason, ok := domain.PaneNeedsYou(liveMeta(r.p))
	if !ok || reason != domain.AttentionReasonPrompt {
		t.Fatalf("snapshot meta = %v", liveMeta(r.p))
	}
	if liveMeta(r.p)[domain.PaneMetaAttentionSince] == "" {
		t.Fatal("snapshot meta has no attention.since")
	}

	// Raising again is not a new event.
	r.p.applyPromptAttention(true)
	if v := r.nextAttention(150 * time.Millisecond); v != nil {
		t.Fatalf("a repeated raise sent %T", v)
	}
	r.expectNoDirty("repeated raise")
}

func TestPromptAttentionClearSendsAnAck(t *testing.T) {
	r := newAttentionRig(t)
	r.p.applyPromptAttention(true)
	r.nextAttention(time.Second)
	r.expectDirty("raise")

	r.p.applyPromptAttention(false)
	ack, ok := r.nextAttention(time.Second).(*msg.MsgAttentionAck)
	if !ok || ack.PaneID != "attn-pane" {
		t.Fatalf("clear did not send MsgAttentionAck for the pane (got %#v)", ack)
	}
	r.expectDirty("clear")
	if _, ok := domain.PaneNeedsYou(liveMeta(r.p)); ok {
		t.Fatalf("snapshot meta still needs you: %v", liveMeta(r.p))
	}
}

func TestAttentionIsNeverPersisted(t *testing.T) {
	r := newAttentionRig(t)
	r.p.applyPromptAttention(true)
	if _, ok := domain.PaneNeedsYou(liveMeta(r.p)); !ok {
		t.Fatal("precondition: the live snapshot does not need you")
	}
	for k := range r.p.meta {
		if domain.IsAttentionMetaKey(k) {
			t.Fatalf("attention key %q written to durable meta", k)
		}
	}
	persisted := r.p.buildSnapshot(true, true, true)
	for k := range persisted.Meta {
		if domain.IsAttentionMetaKey(k) {
			t.Fatalf("attention key %q in the persisted snapshot", k)
		}
	}
	// A snapshot written by some older build that did carry the keys must not
	// bring a stale "needs you" back after a restart.
	restored := &PaneActor{id: "attn-pane"}
	restored.RestoreState(domain.PaneSnapshot{ID: "attn-pane", Meta: map[string]string{
		"fleet.role":                   "worker",
		domain.PaneMetaAttention:       domain.AttentionNeedsYou,
		domain.PaneMetaAttentionReason: domain.AttentionReasonPrompt,
	}})
	if _, ok := domain.PaneNeedsYou(liveMeta(restored)); ok {
		t.Fatal("RestoreState brought back a stale needs-you")
	}
	if liveMeta(restored)["fleet.role"] != "worker" {
		t.Fatal("RestoreState dropped an unrelated meta key")
	}
}

func TestNativeApprovalAndQuestionSetTheReason(t *testing.T) {
	r := newAttentionRig(t)
	r.p.noteApprovalRequest(&msg.MsgApprovalRequest{RequestID: "q1", Type: msg.ApprovalTypeQuestion, Description: "secret-looking text"})
	if reason, ok := domain.PaneNeedsYou(liveMeta(r.p)); !ok || reason != domain.AttentionReasonQuestion {
		t.Fatalf("question: meta = %v", liveMeta(r.p))
	}
	r.expectDirty("question")
	for _, v := range liveMeta(r.p) {
		if strings.Contains(v, "secret-looking") {
			t.Fatal("request description copied into meta")
		}
	}

	// A response for a different request does not clear it.
	r.p.noteApprovalResponse(&msg.MsgApprovalResponse{RequestID: "other"})
	if _, ok := domain.PaneNeedsYou(liveMeta(r.p)); !ok {
		t.Fatal("an unrelated response cleared the question")
	}
	r.p.noteApprovalResponse(&msg.MsgApprovalResponse{RequestID: "q1"})
	if _, ok := domain.PaneNeedsYou(liveMeta(r.p)); ok {
		t.Fatal("the matching response did not clear the question")
	}
	r.expectDirty("answered")

	r.p.noteApprovalRequest(&msg.MsgApprovalRequest{RequestID: "a1", Type: msg.ApprovalTypeDestructive})
	if reason, _ := domain.PaneNeedsYou(liveMeta(r.p)); reason != domain.AttentionReasonApproval {
		t.Fatalf("tool approval: reason = %q", reason)
	}
}

func TestNativeApprovalOutranksAPromptAndThePromptComesBack(t *testing.T) {
	r := newAttentionRig(t)
	r.p.applyPromptAttention(true)
	r.p.noteApprovalRequest(&msg.MsgApprovalRequest{RequestID: "a1", Type: msg.ApprovalTypeDestructive})
	if reason, _ := domain.PaneNeedsYou(liveMeta(r.p)); reason != domain.AttentionReasonApproval {
		t.Fatalf("reason = %q, want approval", reason)
	}
	// A screen that stops matching does not clear a native approval.
	r.p.applyPromptAttention(false)
	if reason, _ := domain.PaneNeedsYou(liveMeta(r.p)); reason != domain.AttentionReasonApproval {
		t.Fatalf("prompt clear removed the native approval (reason %q)", reason)
	}
	r.p.applyPromptAttention(true)
	r.p.noteApprovalResponse(&msg.MsgApprovalResponse{RequestID: "a1"})
	if reason, _ := domain.PaneNeedsYou(liveMeta(r.p)); reason != domain.AttentionReasonPrompt {
		t.Fatalf("after the approval, reason = %q, want prompt", reason)
	}
}
