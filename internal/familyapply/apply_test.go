// SPDX-License-Identifier: Apache-2.0

package familyapply

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The test world: an embedded NATS session bus with a fake workspace that
// answers what apply asks of the real WorkspaceActor (ws.snapshot; on
// ws.inbox: create pane, delete pane, MsgCLIPaneProfile), a pane shim per pane
// on its exact pane.<id>.inbox (meta, given-name), and the REAL fleet
// registry served over the bus. It counts every write it receives.

type fpane struct {
	id      string
	given   string
	meta    map[string]string
	program string
	status  string
}

type world struct {
	t   *testing.T
	nc  *nats.Conn
	pub *msg.NATSPublisher
	reg *fleet.Registry

	mu      sync.Mutex
	panes   []*fpane
	prompts map[string]string // pane -> profile prompt file content
	next    int
	// counters
	creates, deletes, metaWrites, nameWrites, profileSets, profileClears, fleetWrites int
	refusedDeletes                                                                    int
	// faults
	badPrompt map[string]bool // a profile set on this pane stores the wrong text
	keepPanes bool            // refuse every pane delete
}

func newWorld(t *testing.T, initial ...string) *world {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(ns.Shutdown)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	w := &world{t: t, nc: nc, pub: msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry()), reg: fleet.New(),
		prompts: map[string]string{}, badPrompt: map[string]bool{}}
	if _, err := fleet.ServeQueries(nc, w.reg, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.ServeUpdates(nc, w.reg, func(fleet.Update, fleet.UpdateReply) {
		w.mu.Lock()
		w.fleetWrites++
		w.mu.Unlock()
	}); err != nil {
		t.Fatal(err)
	}
	w.sub(msg.T("ws", "snapshot"), func(_ interface{}, env *msg.NATSEnvelope) {
		_ = w.pub.Send(env.ReplyTo, &msg.MsgWorkspaceSnapshotReply{Snapshot: w.snapshot()})
	})
	w.sub(msg.T("ws", "inbox"), func(d interface{}, env *msg.NATSEnvelope) {
		_ = w.pub.Send(env.ReplyTo, w.handleWS(d))
	})
	for _, id := range initial {
		w.addPane(id, "")
	}
	_ = nc.Flush()
	return w
}

func (w *world) sub(subject string, fn func(interface{}, *msg.NATSEnvelope)) {
	if _, err := w.nc.Subscribe(subject, func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		d, err := w.pub.Codecs().Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		fn(d, &env)
	}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) addPane(id, given string) {
	w.mu.Lock()
	w.panes = append(w.panes, &fpane{id: id, given: given, meta: map[string]string{}})
	w.mu.Unlock()
	w.sub(msg.T("pane", id, "inbox"), func(d interface{}, _ *msg.NATSEnvelope) {
		w.mu.Lock()
		defer w.mu.Unlock()
		p := w.findLocked(id)
		if p == nil {
			return
		}
		switch m := d.(type) {
		case *msg.MsgPaneSetMeta:
			w.metaWrites++
			if m.Value == "" {
				delete(p.meta, m.Key)
			} else {
				p.meta[m.Key] = m.Value
			}
		case *msg.MsgPaneSetGivenName:
			w.nameWrites++
			p.given = m.Name
		}
	})
}

func (w *world) findLocked(id string) *fpane {
	for _, p := range w.panes {
		if p.id == id {
			return p
		}
	}
	return nil
}

func (w *world) snapshot() domain.WorkspaceSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	var groups []domain.PaneGroupSnapshot
	for _, p := range w.panes {
		meta := map[string]string{}
		for k, v := range p.meta {
			meta[k] = v
		}
		groups = append(groups, domain.PaneGroupSnapshot{ID: "g-" + p.id,
			Panes: []domain.PaneSnapshot{{ID: p.id, Title: "p", GivenName: p.given, Meta: meta, Program: p.program, Status: p.status}}})
	}
	return domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{{ID: "t1", Lanes: []domain.LaneSnapshot{{ID: "l1", PaneGroups: groups}}}}}
}

func (w *world) handleWS(d interface{}) interface{} {
	switch m := d.(type) {
	case *msg.MsgCLICreatePaneGroup:
		w.mu.Lock()
		w.creates++
		w.next++
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", w.next)
		w.mu.Unlock()
		w.addPane(id, "")
		_ = w.nc.Flush()
		return &msg.MsgCLIResponse{OK: true}
	case *msg.MsgCLIDeletePane:
		// Every fake group holds one pane: the daemon refuses to empty a group.
		w.mu.Lock()
		w.refusedDeletes++
		w.mu.Unlock()
		return &msg.MsgCLIResponse{OK: false, Error: "pane " + m.PaneID + " is the only pane in its pane group"}
	case *msg.MsgCLIDeletePaneGroup:
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.keepPanes {
			return &msg.MsgCLIResponse{OK: false, Error: "kept"}
		}
		for i, p := range w.panes {
			if "g-"+p.id == m.PaneGroupID {
				w.deletes++
				w.panes = append(w.panes[:i], w.panes[i+1:]...)
				return &msg.MsgCLIResponse{OK: true}
			}
		}
		return &msg.MsgCLIResponse{OK: false, Error: "pane group not found"}
	case *msg.MsgCLIPaneProfile:
		w.mu.Lock()
		defer w.mu.Unlock()
		p := w.findLocked(m.PaneID)
		if p == nil {
			return &msg.MsgCLIPaneProfileReply{Error: "pane not found"}
		}
		switch m.Op {
		case "get":
			r := &msg.MsgCLIPaneProfileReply{OK: true, Tools: domain.ParseAgentTools(p.meta[domain.PaneMetaAgentTools])}
			if p.meta[domain.PaneMetaAgentPromptFile] != "" {
				r.HasPrompt = true
				r.Prompt = w.prompts[p.id]
			}
			return r
		case "set":
			w.profileSets++
			text := m.Prompt
			if w.badPrompt[p.id] {
				text = "something else"
			}
			w.prompts[p.id] = text
			p.meta[domain.PaneMetaAgentPromptFile] = "/ws/.rysh/pane-profiles/" + p.id + ".md"
			p.meta[domain.PaneMetaAgentTools] = strings.Join(m.Tools, ",")
			return &msg.MsgCLIPaneProfileReply{OK: true}
		case "clear":
			w.profileClears++
			delete(w.prompts, p.id)
			delete(p.meta, domain.PaneMetaAgentPromptFile)
			delete(p.meta, domain.PaneMetaAgentTools)
			return &msg.MsgCLIPaneProfileReply{OK: true}
		}
	}
	return &msg.MsgCLIResponse{OK: false, Error: "unexpected"}
}

func (w *world) writes() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.creates + w.deletes + w.metaWrites + w.nameWrites + w.profileSets + w.profileClears + w.fleetWrites
}

func (w *world) resetCounters() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.creates, w.deletes, w.metaWrites, w.nameWrites, w.profileSets, w.profileClears, w.fleetWrites = 0, 0, 0, 0, 0, 0, 0
}

func (w *world) pane(id string) fpane {
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.findLocked(id)
	if p == nil {
		return fpane{}
	}
	cp := fpane{id: p.id, given: p.given, meta: map[string]string{}}
	for k, v := range p.meta {
		cp.meta[k] = v
	}
	return cp
}

func (w *world) paneIDs() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, p := range w.panes {
		out = append(out, p.id)
	}
	return out
}

func (w *world) cfg() Config {
	return Config{NC: w.nc, RequestTimeout: 2 * time.Second, SettleTimeout: 2 * time.Second, Poll: 20 * time.Millisecond}
}

// run is the command after connect: manifest in, one JSON line out.
func (w *world) run(m Manifest) (*Result, string, error) {
	w.t.Helper()
	in, err := json.Marshal(m)
	if err != nil {
		w.t.Fatal(err)
	}
	var out bytes.Buffer
	err = Run(context.Background(), w.cfg(), bytes.NewReader(in), &out)
	if err != nil {
		return nil, out.String(), err
	}
	// stdout is exactly one JSON object and a newline.
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	dec.DisallowUnknownFields()
	var r Result
	if err := dec.Decode(&r); err != nil {
		w.t.Fatalf("stdout is not a Result: %v (%q)", err, out.String())
	}
	if _, err := dec.Token(); err != io.EOF {
		w.t.Fatalf("stdout has more than one JSON value: %q", out.String())
	}
	if !strings.HasSuffix(out.String(), "}\n") || strings.Count(out.String(), "\n") != 1 {
		w.t.Fatalf("stdout is not one line: %q", out.String())
	}
	return &r, out.String(), nil
}

const (
	fleetName = "family-0123456789ab"
	idAda     = "aaaaaaaa-1111-4111-8111-111111111111"
	idBo      = "bbbbbbbb-2222-4222-8222-222222222222"
	idCy      = "cccccccc-3333-4333-8333-333333333333"
	paneFirst = "f0000000-0000-4000-8000-000000000000"
	paneOther = "e0000000-0000-4000-8000-000000000000"
)

func family() Manifest {
	return Manifest{Fleet: fleetName, Avatars: []Avatar{
		{ID: idAda, Name: "Grandma Ada", Kind: "elder", Prompt: "You are Ada.\nBe warm."},
		{ID: idBo, Name: "Bo", Kind: "work", Prompt: "You are Bo."},
		{ID: idCy, Name: "Cy", Kind: "coach", Prompt: "You are Cy."},
	}}
}

func eq(a, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}

func (w *world) members() map[string]fleet.Member {
	f := w.reg.Get(fleetName)
	if f == nil {
		w.t.Fatal("fleet not registered")
	}
	out := map[string]fleet.Member{}
	for _, m := range f.Members {
		out[m.PaneID] = m
	}
	return out
}

func TestApply_FreshBuildsTheFamily(t *testing.T) {
	w := newWorld(t, paneFirst, paneOther)
	w.pane(paneOther) // a pane without an avatar, not the first: never touched
	r, _, err := w.run(family())
	if err != nil {
		t.Fatal(err)
	}
	if !eq(r.Created, []string{idAda, idBo, idCy}) || len(r.Updated)+len(r.Unchanged)+len(r.Retired) != 0 {
		t.Fatalf("result = %+v", r)
	}
	if r.Avatars[idAda] != paneFirst {
		t.Errorf("the first avatar should claim the session's first pane, got %s", r.Avatars[idAda])
	}
	if r.Avatars[idBo] == paneOther || r.Avatars[idCy] == paneOther {
		t.Error("an unclaimed pane that is not the first was reused")
	}
	if got := w.pane(paneOther); len(got.meta) != 0 || got.given != "" {
		t.Errorf("a non-avatar pane was touched: %+v", got)
	}
	if n := len(w.paneIDs()); n != 4 {
		t.Errorf("panes = %d, want 4 (first + other + 2 new)", n)
	}
	members := w.members()
	f := w.reg.Get(fleetName)
	if f.Watch.Resolved().Policy != fleet.PolicyOff {
		t.Error("check-ins are not off")
	}
	wantName := map[string]string{idAda: "GrandmaAda", idBo: "Bo", idCy: "Cy"}
	for _, av := range family().Avatars {
		pid := r.Avatars[av.ID]
		p := w.pane(pid)
		if p.meta[MetaAvatarID] != av.ID || p.meta[MetaFleetAgent] != "rysh" || p.given != wantName[av.ID] {
			t.Errorf("pane %s = %+v", pid, p)
		}
		if p.meta[domain.PaneMetaAgentTools] != "family_ask,family_reply,family_status" {
			t.Errorf("pane %s tools = %q", pid, p.meta[domain.PaneMetaAgentTools])
		}
		if w.prompt(pid) != av.Prompt {
			t.Errorf("pane %s prompt = %q", pid, w.prompt(pid))
		}
		m := members[pid]
		if m.Label != av.Name || m.Unit != av.Kind || m.Role != "worker" {
			t.Errorf("member %s = %+v", pid, m)
		}
	}
	if len(members) != 3 {
		t.Errorf("members = %d", len(members))
	}
}

func TestApply_SecondIdenticalApplyWritesNothing(t *testing.T) {
	w := newWorld(t, paneFirst)
	r1, _, err := w.run(family())
	if err != nil {
		t.Fatal(err)
	}
	w.resetCounters()
	r2, _, err := w.run(family())
	if err != nil {
		t.Fatal(err)
	}
	if !eq(r2.Unchanged, []string{idAda, idBo, idCy}) || len(r2.Created)+len(r2.Updated)+len(r2.Retired) != 0 {
		t.Fatalf("second result = %+v", r2)
	}
	for id, p := range r1.Avatars {
		if r2.Avatars[id] != p {
			t.Errorf("avatar %s moved from %s to %s", id, p, r2.Avatars[id])
		}
	}
	if n := w.writes(); n != 0 {
		t.Errorf("second apply made %d writes (profile sets %d)", n, w.profileSets)
	}
}

func TestApply_EditingOneAvatarUpdatesOnlyIt(t *testing.T) {
	w := newWorld(t, paneFirst)
	r0, _, err := w.run(family())
	if err != nil {
		t.Fatal(err)
	}

	m := family()
	m.Avatars[1].Prompt = "You are Bo, now retired."
	w.resetCounters()
	r, _, err := w.run(m)
	if err != nil {
		t.Fatal(err)
	}
	if !eq(r.Updated, []string{idBo}) || !eq(r.Unchanged, []string{idAda, idCy}) {
		t.Fatalf("prompt edit: %+v", r)
	}
	if w.profileSets != 1 || w.writes() != 1 {
		t.Errorf("prompt edit: profile sets %d, writes %d; want 1, 1", w.profileSets, w.writes())
	}
	if w.prompt(r0.Avatars[idBo]) != "You are Bo, now retired." {
		t.Error("Bo's prompt not updated")
	}

	m.Avatars[2].Name = "Coach Cy"
	m.Avatars[2].Kind = "sport"
	w.resetCounters()
	r, _, err = w.run(m)
	if err != nil {
		t.Fatal(err)
	}
	if !eq(r.Updated, []string{idCy}) || !eq(r.Unchanged, []string{idAda, idBo}) {
		t.Fatalf("name edit: %+v", r)
	}
	if w.nameWrites != 1 || w.fleetWrites != 1 || w.profileSets != 0 {
		t.Errorf("name edit: names %d fleet %d profiles %d", w.nameWrites, w.fleetWrites, w.profileSets)
	}
	pid := r0.Avatars[idCy]
	if w.pane(pid).given != "CoachCy" || w.members()[pid].Label != "Coach Cy" || w.members()[pid].Unit != "sport" {
		t.Errorf("Cy not renamed: %+v %+v", w.pane(pid), w.members()[pid])
	}
}

func TestApply_RemovingAnAvatarRetiresItsPane(t *testing.T) {
	w := newWorld(t, paneFirst)
	r0, _, err := w.run(family())
	if err != nil {
		t.Fatal(err)
	}
	boPane := r0.Avatars[idBo]
	m := family()
	m.Avatars = append(m.Avatars[:1], m.Avatars[2])
	w.resetCounters()
	r, _, err := w.run(m)
	if err != nil {
		t.Fatal(err)
	}
	if !eq(r.Retired, []string{idBo}) || !eq(r.Unchanged, []string{idAda, idCy}) || len(r.Created)+len(r.Updated) != 0 {
		t.Fatalf("result = %+v", r)
	}
	if _, ok := r.Avatars[idBo]; ok {
		t.Error("a retired avatar is still in the map")
	}
	for _, id := range w.paneIDs() {
		if id == boPane {
			t.Error("Bo's pane is still open")
		}
	}
	// N1: the profile is replaced by the inert retired one (persona gone,
	// family tools kept), never cleared.
	if w.profileClears != 0 || w.profileSets != 1 || w.deletes != 1 || w.refusedDeletes != 0 {
		t.Errorf("clears %d sets %d deletes %d refused %d", w.profileClears, w.profileSets, w.deletes, w.refusedDeletes)
	}
	if _, ok := w.members()[boPane]; ok || len(w.members()) != 2 {
		t.Errorf("fleet members = %+v", w.members())
	}

	// Again: nothing left to do.
	w.resetCounters()
	if r, _, err = w.run(m); err != nil || len(r.Retired) != 0 || w.writes() != 0 {
		t.Fatalf("re-apply after retire: %+v, %v, writes %d", r, err, w.writes())
	}
}

func TestApply_AKeptRetiredPaneIsUnclaimedAndReused(t *testing.T) {
	w := newWorld(t, paneFirst)
	m := family()
	m.Avatars = m.Avatars[:1]
	if _, _, err := w.run(m); err != nil {
		t.Fatal(err)
	}
	// Ada leaves, Bo arrives: the daemon will not close the session's only
	// pane, so it is unclaimed and Bo takes it.
	m.Avatars = []Avatar{family().Avatars[1]}
	r, _, err := w.run(m)
	if err != nil {
		t.Fatal(err)
	}
	if !eq(r.Retired, []string{idAda}) || !eq(r.Created, []string{idBo}) || r.Avatars[idBo] != paneFirst {
		t.Fatalf("result = %+v", r)
	}
	if p := w.pane(paneFirst); p.meta[MetaAvatarID] != idBo || p.meta[MetaRetired] != "" || w.prompt(paneFirst) != "You are Bo." {
		t.Errorf("first pane = %+v", p)
	}
	// Everyone leaves: the pane stays open and unclaimed, and keeps a
	// family-only profile with the inert prompt (N1).
	m.Avatars = nil
	r, _, err = w.run(m)
	if err != nil {
		t.Fatal(err)
	}
	if !eq(r.Retired, []string{idBo}) || len(r.Avatars) != 0 {
		t.Fatalf("result = %+v", r)
	}
	p := w.pane(paneFirst)
	if p.meta[MetaAvatarID] != "" || p.meta[MetaFleetAgent] != "" || p.meta[MetaRetired] != "1" {
		t.Errorf("kept pane still carries an avatar: %+v", p)
	}
	if p.meta[domain.PaneMetaAgentTools] != "family_ask,family_reply,family_status" || w.prompt(paneFirst) != RetiredPrompt {
		t.Errorf("a retired open pane must keep exactly the family tools and the inert prompt: tools %q prompt %q",
			p.meta[domain.PaneMetaAgentTools], w.prompt(paneFirst))
	}
	if len(w.members()) != 0 {
		t.Errorf("members = %+v", w.members())
	}
	// Idempotent, and the retired pane is claimed again by the next avatar.
	w.resetCounters()
	if r, _, err = w.run(m); err != nil || w.writes() != 0 {
		t.Fatalf("re-apply of an empty family: %+v %v writes %d", r, err, w.writes())
	}
	m.Avatars = []Avatar{family().Avatars[2]}
	if r, _, err = w.run(m); err != nil || r.Avatars[idCy] != paneFirst {
		t.Fatalf("the retired pane was not reclaimed: %+v %v", r, err)
	}
	if p := w.pane(paneFirst); p.meta[MetaRetired] != "" || w.prompt(paneFirst) != "You are Cy." {
		t.Errorf("reclaimed pane = %+v, prompt %q", p, w.prompt(paneFirst))
	}
}

// N1, with a pane the daemon will not close for another reason: it is left
// family-only, never on the default tools.
func TestApply_ARetiredPaneTheDaemonKeepsStaysFamilyOnly(t *testing.T) {
	w := newWorld(t, paneFirst)
	r0, _, err := w.run(family())
	if err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	w.keepPanes = true
	w.mu.Unlock()
	m := family()
	m.Avatars = m.Avatars[:2]
	r, _, err := w.run(m)
	if err != nil || !eq(r.Retired, []string{idCy}) {
		t.Fatalf("%+v %v", r, err)
	}
	p := w.pane(r0.Avatars[idCy])
	if p.id == "" || p.meta[domain.PaneMetaAgentTools] != "family_ask,family_reply,family_status" || w.prompt(p.id) != RetiredPrompt {
		t.Errorf("kept retired pane = %+v prompt %q", p, w.prompt(p.id))
	}
}

// N2: the session's first pane is claimed only when it is plainly unused.
func TestApply_ABusyOrProfiledFirstPaneIsNotClaimed(t *testing.T) {
	cases := map[string]func(p *fpane){
		"program running": func(p *fpane) { p.program = "claude" },
		"shell busy":      func(p *fpane) { p.status = "running shell" },
		"agent turn":      func(p *fpane) { p.status = "querying provider" },
		"profiled":        func(p *fpane) { p.meta[domain.PaneMetaAgentTools] = "bash,read_file" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, paneFirst)
			w.mu.Lock()
			mutate(w.findLocked(paneFirst))
			before := w.findLocked(paneFirst)
			want := map[string]string{}
			for k, v := range before.meta {
				want[k] = v
			}
			w.mu.Unlock()
			r, _, err := w.run(family())
			if err != nil {
				t.Fatal(err)
			}
			for id, pid := range r.Avatars {
				if pid == paneFirst {
					t.Fatalf("avatar %s claimed the busy/profiled first pane", id)
				}
			}
			if len(w.paneIDs()) != 4 {
				t.Errorf("panes = %d, want the first + 3 new", len(w.paneIDs()))
			}
			got := w.pane(paneFirst)
			if got.given != "" || len(got.meta) != len(want) {
				t.Errorf("the first pane was touched: %+v", got)
			}
		})
	}
}

func TestApply_ReadBackFailureFails(t *testing.T) {
	w := newWorld(t, paneFirst)
	w.badPrompt[paneFirst] = true
	r, out, err := w.run(family())
	if err == nil {
		t.Fatalf("apply succeeded with a wrong prompt on a pane: %+v", r)
	}
	if out != "" {
		t.Errorf("stdout on failure = %q, want empty", out)
	}
	if !strings.Contains(err.Error(), "read-back failed") || !strings.Contains(err.Error(), "different prompt") {
		t.Errorf("err = %v", err)
	}
}

func TestApply_ExtraToolOnAPaneIsReplaced(t *testing.T) {
	w := newWorld(t, paneFirst)
	r0, _, err := w.run(family())
	if err != nil {
		t.Fatal(err)
	}
	// Someone widened Bo's allow-list behind apply's back.
	w.mu.Lock()
	w.findLocked(r0.Avatars[idBo]).meta[domain.PaneMetaAgentTools] = "family_ask,family_reply,family_status,bash"
	w.mu.Unlock()
	r, _, err := w.run(family())
	if err != nil {
		t.Fatal(err)
	}
	if !eq(r.Updated, []string{idBo}) {
		t.Fatalf("result = %+v", r)
	}
	if got := w.pane(r0.Avatars[idBo]).meta[domain.PaneMetaAgentTools]; got != "family_ask,family_reply,family_status" {
		t.Errorf("tools = %q", got)
	}
}

func TestApply_DuplicateNamesGetDistinctPaneNames(t *testing.T) {
	w := newWorld(t, paneFirst)
	m := family()
	m.Avatars[1].Name = "Grandma Ada"
	r, _, err := w.run(m)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := w.pane(r.Avatars[idAda]).given, w.pane(r.Avatars[idBo]).given; a != "GrandmaAda" || b != "GrandmaAda-2" {
		t.Errorf("names = %q, %q", a, b)
	}
}

func TestRun_BadManifestTouchesNothing(t *testing.T) {
	w := newWorld(t, paneFirst)
	var out bytes.Buffer
	err := Run(context.Background(), w.cfg(), strings.NewReader(`{"fleet":"family-0123456789ab","avatars":[{"id":"x"}]}`), &out)
	if err == nil || out.Len() != 0 || w.writes() != 0 {
		t.Fatalf("err %v, stdout %q, writes %d", err, out.String(), w.writes())
	}
}

func TestParse_Validation(t *testing.T) {
	av := func(id, name, kind, prompt string) string {
		b, _ := json.Marshal(Avatar{ID: id, Name: name, Kind: kind, Prompt: prompt})
		return string(b)
	}
	ok := av(idAda, "Ada", "work", "hi")
	many := make([]string, 9)
	for i := range many {
		many[i] = av(fmt.Sprintf("aaaaaaaa-1111-4111-8111-%012d", i), "A", "work", "hi")
	}
	cases := map[string]string{
		"oversize":       `{"fleet":"family-0123456789ab","avatars":[],"pad":"` + strings.Repeat("x", MaxManifestBytes) + `"}`,
		"bad id":         `{"fleet":"family-0123456789ab","avatars":[` + av("not-a-uuid", "Ada", "work", "hi") + `]}`,
		"upper id":       `{"fleet":"family-0123456789ab","avatars":[` + av(strings.ToUpper(idAda), "Ada", "work", "hi") + `]}`,
		"nine avatars":   `{"fleet":"family-0123456789ab","avatars":[` + strings.Join(many, ",") + `]}`,
		"prompt too big": `{"fleet":"family-0123456789ab","avatars":[` + av(idAda, "Ada", "work", strings.Repeat("p", MaxPromptBytes+1)) + `]}`,
		"empty prompt":   `{"fleet":"family-0123456789ab","avatars":[` + av(idAda, "Ada", "work", "  ") + `]}`,
		"bad kind":       `{"fleet":"family-0123456789ab","avatars":[` + av(idAda, "Ada", "Work!", "hi") + `]}`,
		"long name":      `{"fleet":"family-0123456789ab","avatars":[` + av(idAda, strings.Repeat("n", 41), "work", "hi") + `]}`,
		"control name":   `{"fleet":"family-0123456789ab","avatars":[` + av(idAda, "A\nda", "work", "hi") + `]}`,
		"dup id":         `{"fleet":"family-0123456789ab","avatars":[` + ok + `,` + ok + `]}`,
		"bad fleet":      `{"fleet":"family-x","avatars":[` + ok + `]}`,
		"unknown field":  `{"fleet":"family-0123456789ab","avatars":[],"tools":["bash"]}`,
		"avatar tools":   `{"fleet":"family-0123456789ab","avatars":[{"id":"` + idAda + `","name":"A","kind":"work","prompt":"hi","tools":"bash"}]}`,
		"trailing":       `{"fleet":"family-0123456789ab","avatars":[]} {}`,
		"empty":          ``,
		"not json":       `fleet`,
	}
	for name, in := range cases {
		if _, err := Parse(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse(strings.NewReader(`{"fleet":"family-0123456789ab","avatars":[` + ok + `]}`)); err != nil {
		t.Errorf("valid manifest refused: %v", err)
	}
	eight := `{"fleet":"family-0123456789ab","avatars":[` + strings.Join(many[:8], ",") + `]}`
	if _, err := Parse(strings.NewReader(eight)); err != nil {
		t.Errorf("8 avatars refused: %v", err)
	}
	if _, err := Parse(strings.NewReader(`{"fleet":"family-0123456789ab","avatars":[` + av(idAda, "Ada", "work", strings.Repeat("p", MaxPromptBytes)) + `]}`)); err != nil {
		t.Errorf("a 64 KiB prompt refused: %v", err)
	}
}

func (w *world) prompt(pane string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.prompts[pane]
}
