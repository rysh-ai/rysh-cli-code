// SPDX-License-Identifier: Apache-2.0

package familyapply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Config is what Apply needs. NC must be connected to the session's NATS with
// the session prefix already set (msg.SetSessionPrefix).
type Config struct {
	NC     *nats.Conn
	Codecs *msg.CodecRegistry
	Log    *slog.Logger
	// RequestTimeout bounds one request to the daemon (5 s).
	RequestTimeout time.Duration
	// SettleTimeout bounds the wait for a new pane to appear and for the
	// read-back to show every write (10 s). Pane meta writes are delivered
	// to the pane asynchronously, so the read-back polls.
	SettleTimeout time.Duration
	// Poll is the read-back interval (100 ms).
	Poll time.Duration
}

func (c *Config) defaults() {
	if c.Codecs == nil {
		c.Codecs = msg.DefaultCodecRegistry()
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 5 * time.Second
	}
	if c.SettleTimeout <= 0 {
		c.SettleTimeout = 10 * time.Second
	}
	if c.Poll <= 0 {
		c.Poll = 100 * time.Millisecond
	}
}

// Result is the command's one stdout object. Every list is present (never
// null); Created/Updated/Unchanged keep manifest order, Retired is sorted.
type Result struct {
	Avatars   map[string]string `json:"avatars"` // avatar id -> pane id
	Created   []string          `json:"created"`
	Updated   []string          `json:"updated"`
	Retired   []string          `json:"retired"`
	Unchanged []string          `json:"unchanged"`
}

// FleetRole is every avatar's fleet role.
const FleetRole = "worker"

type pane struct {
	ID        string
	GivenName string
	Meta      map[string]string
	Program   string // foreground program, "" at the shell prompt
	Status    string // the pane's run status ("idle", "running shell", …)
	// Where it sits, for closing it: the daemon closes a pane only inside a
	// group of several, and a one-pane group only as a whole group.
	Group      string
	GroupPanes int
	LaneGroups int
}

type applier struct {
	ctx context.Context
	cfg Config
	pub *msg.NATSPublisher
	// writes counts every change sent to the daemon (tests read it).
	writes int
}

// Apply makes the session match m and reports what it did. On any error the
// session may be partly applied; a re-run with the same manifest finishes
// the job (every step is a compare-then-write).
func Apply(ctx context.Context, cfg Config, m Manifest) (*Result, error) {
	if cfg.NC == nil {
		return nil, errors.New("familyapply: NC is required")
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	cfg.defaults()
	a := &applier{ctx: ctx, cfg: cfg, pub: msg.NewNATSPublisher(cfg.NC, cfg.Codecs)}
	return a.apply(m)
}

func (a *applier) apply(m Manifest) (*Result, error) {
	want := map[string]bool{}
	for _, av := range m.Avatars {
		want[av.ID] = true
	}

	// 1. Retire: panes of avatars that left the manifest, and any second
	// pane claiming the same avatar. Done first, so a retired pane that the
	// daemon will not close (the session's last) is free to be claimed below.
	panes, err := a.panes()
	if err != nil {
		return nil, err
	}
	fl, err := a.fleet(m.Fleet)
	if err != nil {
		return nil, err
	}
	claimedBy := map[string]string{}
	retiredSet := map[string]bool{}
	var retiredPanes []string
	closed := map[string]bool{}    // retired and closed
	unclaimed := map[string]bool{} // retired, kept open, claim removed
	for _, p := range panes {
		id := p.Meta[MetaAvatarID]
		if id == "" {
			continue
		}
		if want[id] && claimedBy[id] == "" {
			claimedBy[id] = p.ID
			continue
		}
		gone, err := a.retire(p, m.Fleet, fl)
		if err != nil {
			return nil, err
		}
		if gone {
			closed[p.ID] = true
		} else {
			unclaimed[p.ID] = true
		}
		retiredPanes = append(retiredPanes, p.ID)
		if !want[id] {
			retiredSet[id] = true
		}
	}
	if len(retiredPanes) > 0 {
		if panes, err = a.panes(); err != nil {
			return nil, err
		}
		// Meta writes and closes land asynchronously: do not let a stale
		// snapshot show a closed pane, or an old claim on a kept one.
		kept := panes[:0]
		for _, p := range panes {
			if closed[p.ID] {
				continue
			}
			if unclaimed[p.ID] {
				delete(p.Meta, MetaAvatarID)
				delete(p.Meta, MetaFleetAgent)
				p.Meta[MetaRetired] = "1"
			}
			kept = append(kept, p)
		}
		panes = kept
	}

	// 2. One pane per avatar: found by its meta, else the session's first
	// pane while unclaimed, else a new pane.
	byID := map[string]pane{}
	for _, p := range panes {
		byID[p.ID] = p
	}
	avatarPane := map[string]string{}
	used := map[string]bool{}
	for _, av := range m.Avatars {
		if pid := claimedBy[av.ID]; pid != "" {
			if _, ok := byID[pid]; ok {
				avatarPane[av.ID] = pid
				used[pid] = true
			}
		}
	}
	created := map[string]bool{}
	for _, av := range m.Avatars {
		if avatarPane[av.ID] != "" {
			continue
		}
		pid := ""
		if len(panes) > 0 && !used[panes[0].ID] && claimable(panes[0]) {
			pid = panes[0].ID
		} else {
			if pid, panes, err = a.newPane(panes); err != nil {
				return nil, err
			}
			for _, p := range panes {
				if _, ok := byID[p.ID]; !ok {
					byID[p.ID] = p
				}
			}
		}
		if err := a.setMeta(pid, MetaAvatarID, av.ID); err != nil {
			return nil, err
		}
		if byID[pid].Meta[MetaRetired] != "" {
			if err := a.setMeta(pid, MetaRetired, ""); err != nil {
				return nil, err
			}
		}
		avatarPane[av.ID] = pid
		used[pid] = true
		created[av.ID] = true
	}

	// 3. Names, the courier stamp and the profile, each only when it differs.
	names := givenNames(m, avatarPane, panes)
	touched := map[string]bool{}
	for _, av := range m.Avatars {
		pid := avatarPane[av.ID]
		p := byID[pid]
		if p.GivenName != names[av.ID] {
			if err := a.send(msg.T("pane", pid, "inbox"), &msg.MsgPaneSetGivenName{Name: names[av.ID]}); err != nil {
				return nil, err
			}
			touched[av.ID] = true
		}
		if p.Meta[MetaFleetAgent] != "rysh" {
			if err := a.setMeta(pid, MetaFleetAgent, "rysh"); err != nil {
				return nil, err
			}
			touched[av.ID] = true
		}
		prof, err := a.profile("get", pid, "")
		if err != nil {
			return nil, err
		}
		if !profileMatches(prof, av.Prompt) {
			if _, err := a.profile("set", pid, av.Prompt); err != nil {
				return nil, err
			}
			touched[av.ID] = true
		}
	}

	// 4. The fleet: registered, check-ins off (a check-in would cost a turn),
	// every avatar a member with its name and kind, nobody else.
	if fl, err = a.fleet(m.Fleet); err != nil {
		return nil, err
	}
	if fl == nil {
		r, err := a.fleetUpdate(fleet.Update{Op: fleet.OpRegister, Fleet: fleet.Fleet{Name: m.Fleet, Created: time.Now().UnixMilli()}})
		if err != nil {
			return nil, err
		}
		fl = r.Fleet
	}
	if fl == nil {
		return nil, fmt.Errorf("fleet %s: the registry did not return it after registering", m.Fleet)
	}
	if fl.Watch.Resolved().Policy != fleet.PolicyOff {
		if _, err := a.fleetUpdate(fleet.Update{Op: fleet.OpWatch, Name: m.Fleet, Watch: fleet.Watch{Policy: fleet.PolicyOff}}); err != nil {
			return nil, err
		}
	}
	members := map[string]fleet.Member{}
	for _, mem := range fl.Members {
		members[mem.PaneID] = mem
	}
	avatarPaneSet := map[string]bool{}
	for _, av := range m.Avatars {
		pid := avatarPane[av.ID]
		avatarPaneSet[pid] = true
		cur, ok := members[pid]
		if ok && memberMatches(cur, av) {
			continue
		}
		if _, err := a.fleetUpdate(fleet.Update{Op: fleet.OpMemberUpsert, Name: m.Fleet,
			Member: fleet.Member{PaneID: pid, Role: FleetRole, Label: av.Name, Unit: av.Kind}}); err != nil {
			return nil, err
		}
		touched[av.ID] = true
	}
	for _, mem := range fl.Members {
		if !avatarPaneSet[mem.PaneID] {
			if _, err := a.fleetUpdate(fleet.Update{Op: fleet.OpMemberRemove, Name: m.Fleet, PaneID: mem.PaneID}); err != nil {
				return nil, err
			}
		}
	}

	// 5. Read back; fail unless every avatar pane holds exactly the family
	// tools and its prompt (and its claim, name, stamp and membership).
	if err := a.verify(m, avatarPane, names, retiredPanes); err != nil {
		return nil, err
	}

	res := &Result{Avatars: avatarPane, Created: []string{}, Updated: []string{}, Retired: []string{}, Unchanged: []string{}}
	for _, av := range m.Avatars {
		switch {
		case created[av.ID]:
			res.Created = append(res.Created, av.ID)
		case touched[av.ID]:
			res.Updated = append(res.Updated, av.ID)
		default:
			res.Unchanged = append(res.Unchanged, av.ID)
		}
	}
	for id := range retiredSet {
		res.Retired = append(res.Retired, id)
	}
	sort.Strings(res.Retired)
	return res, nil
}

// retire takes a pane out of the family: profile cleared, out of the fleet,
// closed. A pane the daemon will not close (the session's last pane) loses
// its avatar claim and courier stamp instead, so nothing reaches it and the
// next avatar may claim it.
func (a *applier) retire(p pane, fleetName string, fl *fleet.Fleet) (closed bool, err error) {
	// The profile is never cleared (e20 H2 N1): clearing it would put a pane
	// that stays open back on the session's default tools, a shell among
	// them. It is replaced by the inert retired profile instead — the same
	// three family tools, and a prompt that carries no persona — so the
	// avatar's prompt text leaves the disk even when the pane is closed.
	prof, err := a.profile("get", p.ID, "")
	if err != nil {
		return false, err
	}
	if !profileMatches(prof, RetiredPrompt) {
		if _, err := a.profile("set", p.ID, RetiredPrompt); err != nil {
			return false, err
		}
	}
	if fl != nil {
		for _, mem := range fl.Members {
			if mem.PaneID == p.ID {
				if _, err := a.fleetUpdate(fleet.Update{Op: fleet.OpMemberRemove, Name: fleetName, PaneID: p.ID}); err != nil {
					return false, err
				}
				break
			}
		}
	}
	// The daemon's own rules: a pane closes inside a group of several; a
	// one-pane group closes as a group, but never the last group of its
	// lane (that would empty the lane, and in a family session the lane is
	// the session).
	var req interface{}
	switch {
	case p.GroupPanes > 1:
		req = &msg.MsgCLIDeletePane{PaneID: p.ID}
	case p.LaneGroups > 1 && p.Group != "":
		req = &msg.MsgCLIDeletePaneGroup{PaneGroupID: p.Group}
	}
	if req != nil {
		reply, err := a.request(msg.T("ws", "inbox"), req)
		if err != nil {
			return false, fmt.Errorf("close pane %.8s: %w", p.ID, err)
		}
		a.writes++
		if r, ok := reply.(*msg.MsgCLIResponse); ok && r.OK {
			return true, nil
		} else if ok {
			a.cfg.Log.Warn("family apply: the daemon kept a retired pane open; unclaiming it", "pane", p.ID, "why", r.Error)
		}
	}
	// Kept open: mark it as apply's own retired pane (so a later apply may
	// claim it again, N2), then drop the claim and the courier stamp. It
	// keeps the inert family-only profile.
	if err := a.setMeta(p.ID, MetaRetired, "1"); err != nil {
		return false, err
	}
	if err := a.setMeta(p.ID, MetaAvatarID, ""); err != nil {
		return false, err
	}
	if p.Meta[MetaFleetAgent] != "" {
		return false, a.setMeta(p.ID, MetaFleetAgent, "")
	}
	return false, nil
}

// claimable reports whether apply may claim the session's first pane for
// an avatar (e20 H2 N2). Never a pane another avatar holds. Otherwise only
// a pane apply itself retired and kept open (MetaRetired), or a pane that
// is plainly unused: at its shell prompt, no program in the foreground, no
// agent turn running, and no agent profile. A person's working pane — busy,
// or profiled for something else — is left alone; a new pane is opened
// instead.
func claimable(p pane) bool {
	if p.Meta[MetaAvatarID] != "" {
		return false
	}
	if p.Meta[MetaRetired] != "" {
		return true
	}
	if strings.TrimSpace(p.Meta[domain.PaneMetaAgentTools]) != "" || strings.TrimSpace(p.Meta[domain.PaneMetaAgentPromptFile]) != "" {
		return false
	}
	return p.Program == "" && (p.Status == "" || p.Status == "idle")
}

// givenNames picks each avatar pane's given-name: paneName(name), made
// unique against the other avatars and every other pane in the session
// (the daemon refuses a name already used in a lane).
func givenNames(m Manifest, avatarPane map[string]string, panes []pane) map[string]string {
	mine := map[string]bool{}
	for _, pid := range avatarPane {
		mine[pid] = true
	}
	taken := map[string]bool{}
	for _, p := range panes {
		if !mine[p.ID] && p.GivenName != "" {
			taken[p.GivenName] = true
		}
	}
	out := map[string]string{}
	for _, av := range m.Avatars {
		base := paneName(av.Name)
		n := base
		for i := 2; taken[n]; i++ {
			suffix := fmt.Sprintf("-%d", i)
			b := base
			if len(b)+len(suffix) > 24 {
				b = b[:24-len(suffix)]
			}
			n = b + suffix
		}
		taken[n] = true
		out[av.ID] = n
	}
	return out
}

func profileMatches(p *msg.MsgCLIPaneProfileReply, prompt string) bool {
	return p != nil && toolsExact(p.Tools) && p.HasPrompt && p.PromptErr == "" && p.Prompt == prompt
}

// toolsExact: the allow-list is exactly Tools (order free, no extras).
func toolsExact(tools []string) bool {
	if len(tools) != len(Tools) {
		return false
	}
	set := map[string]bool{}
	for _, t := range tools {
		set[t] = true
	}
	for _, t := range Tools {
		if !set[t] {
			return false
		}
	}
	return len(set) == len(Tools)
}

func memberMatches(m fleet.Member, av Avatar) bool {
	return m.Role == FleetRole && m.Label == av.Name && m.Unit == av.Kind
}

// verify polls the session until every avatar is fully applied, or fails
// after SettleTimeout naming what is still wrong.
func (a *applier) verify(m Manifest, avatarPane, names map[string]string, retired []string) error {
	deadline := time.Now().Add(a.cfg.SettleTimeout)
	for {
		problems, err := a.check(m, avatarPane, names, retired)
		if err != nil {
			return err
		}
		if len(problems) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("read-back failed: %s", strings.Join(problems, "; "))
		}
		if err := a.sleep(a.cfg.Poll); err != nil {
			return err
		}
	}
}

func (a *applier) check(m Manifest, avatarPane, names map[string]string, retired []string) ([]string, error) {
	panes, err := a.panes()
	if err != nil {
		return nil, err
	}
	byID := map[string]pane{}
	for _, p := range panes {
		byID[p.ID] = p
	}
	var problems []string
	for _, av := range m.Avatars {
		pid := avatarPane[av.ID]
		p, ok := byID[pid]
		if !ok {
			problems = append(problems, fmt.Sprintf("avatar %s: pane %.8s is gone", av.ID, pid))
			continue
		}
		if p.Meta[MetaAvatarID] != av.ID {
			problems = append(problems, fmt.Sprintf("avatar %s: pane %.8s is not marked as its pane", av.ID, pid))
		}
		if p.Meta[MetaFleetAgent] != "rysh" {
			problems = append(problems, fmt.Sprintf("avatar %s: pane %.8s has no courier stamp", av.ID, pid))
		}
		if p.GivenName != names[av.ID] {
			problems = append(problems, fmt.Sprintf("avatar %s: pane %.8s is named %q, want %q", av.ID, pid, p.GivenName, names[av.ID]))
		}
		prof, err := a.profile("get", pid, "")
		if err != nil {
			return nil, err
		}
		switch {
		case !toolsExact(prof.Tools):
			problems = append(problems, fmt.Sprintf("avatar %s: pane %.8s allows %v, want exactly %v", av.ID, pid, prof.Tools, Tools))
		case !prof.HasPrompt || prof.PromptErr != "":
			problems = append(problems, fmt.Sprintf("avatar %s: pane %.8s has no readable prompt (%s)", av.ID, pid, prof.PromptErr))
		case prof.Prompt != av.Prompt:
			problems = append(problems, fmt.Sprintf("avatar %s: pane %.8s holds a different prompt", av.ID, pid))
		}
	}
	for _, pid := range retired {
		p, ok := byID[pid]
		if !ok {
			continue // closed
		}
		if id := p.Meta[MetaAvatarID]; id != "" && avatarPane[id] == pid {
			continue // claimed again by a current avatar, checked above
		}
		if p.Meta[MetaAvatarID] != "" {
			problems = append(problems, fmt.Sprintf("retired pane %.8s still claims avatar %s", pid, p.Meta[MetaAvatarID]))
		}
		// N1: a retired pane left open stays family-only.
		prof, err := a.profile("get", pid, "")
		if err != nil {
			return nil, err
		}
		if !toolsExact(prof.Tools) {
			problems = append(problems, fmt.Sprintf("retired pane %.8s allows %v, want exactly %v", pid, prof.Tools, Tools))
		}
	}
	fl, err := a.fleet(m.Fleet)
	if err != nil {
		return nil, err
	}
	if fl == nil {
		return append(problems, fmt.Sprintf("fleet %s is not registered", m.Fleet)), nil
	}
	if fl.Watch.Resolved().Policy != fleet.PolicyOff {
		problems = append(problems, fmt.Sprintf("fleet %s: check-ins are not off", m.Fleet))
	}
	members := map[string]fleet.Member{}
	for _, mem := range fl.Members {
		members[mem.PaneID] = mem
	}
	for _, av := range m.Avatars {
		if mem, ok := members[avatarPane[av.ID]]; !ok || !memberMatches(mem, av) {
			problems = append(problems, fmt.Sprintf("avatar %s: not a member of fleet %s as %q/%s", av.ID, m.Fleet, av.Name, av.Kind))
		}
	}
	if len(fl.Members) != len(m.Avatars) {
		problems = append(problems, fmt.Sprintf("fleet %s has %d members, want %d", m.Fleet, len(fl.Members), len(m.Avatars)))
	}
	return problems, nil
}

// ---------------------------------------------------------------------------
// the daemon
// ---------------------------------------------------------------------------

func (a *applier) sleep(d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-a.ctx.Done():
		return a.ctx.Err()
	case <-t.C:
		return nil
	}
}

func (a *applier) request(subject string, m interface{}) (interface{}, error) {
	if err := a.ctx.Err(); err != nil {
		return nil, err
	}
	return a.pub.Request(subject, m, a.cfg.RequestTimeout)
}

func (a *applier) send(subject string, m interface{}) error {
	if err := a.ctx.Err(); err != nil {
		return err
	}
	if err := a.pub.Send(subject, m); err != nil {
		return fmt.Errorf("send to %s: %w", subject, err)
	}
	a.writes++
	return a.pub.Flush()
}

func (a *applier) setMeta(pane, key, value string) error {
	return a.send(msg.T("pane", pane, "inbox"), &msg.MsgPaneSetMeta{Key: key, Value: value})
}

// panes lists the session's panes in layout order.
func (a *applier) panes() ([]pane, error) {
	reply, err := a.request(msg.T("ws", "snapshot"), &msg.MsgGetWorkspaceSnapshot{LayoutOnly: true, NoHistories: true})
	if err != nil {
		return nil, fmt.Errorf("the session did not answer (is it running?): %w", err)
	}
	snap, ok := reply.(*msg.MsgWorkspaceSnapshotReply)
	if !ok {
		return nil, fmt.Errorf("unexpected snapshot reply %T", reply)
	}
	var out []pane
	for _, tab := range snap.Snapshot.Tabs {
		for _, lane := range tab.Lanes {
			for _, g := range lane.PaneGroups {
				for _, p := range g.Panes {
					meta := map[string]string{}
					for k, v := range p.Meta {
						meta[k] = v
					}
					out = append(out, pane{ID: p.ID, GivenName: p.GivenName, Meta: meta, Program: p.Program, Status: p.Status,
						Group: g.ID, GroupPanes: len(g.Panes), LaneGroups: len(lane.PaneGroups)})
				}
			}
		}
	}
	return out, nil
}

// newPane opens a pane and returns its id, found as the pane that was not in
// before (the daemon's create reply carries no id).
func (a *applier) newPane(before []pane) (string, []pane, error) {
	had := map[string]bool{}
	for _, p := range before {
		had[p.ID] = true
	}
	reply, err := a.request(msg.T("ws", "inbox"), &msg.MsgCLICreatePaneGroup{})
	if err != nil {
		return "", nil, fmt.Errorf("new pane: %w", err)
	}
	a.writes++
	if r, ok := reply.(*msg.MsgCLIResponse); !ok || !r.OK {
		why := "unexpected reply"
		if ok {
			why = r.Error
		}
		return "", nil, fmt.Errorf("new pane: %s", why)
	}
	deadline := time.Now().Add(a.cfg.SettleTimeout)
	for {
		panes, err := a.panes()
		if err != nil {
			return "", nil, err
		}
		for _, p := range panes {
			if !had[p.ID] {
				return p.ID, panes, nil
			}
		}
		if time.Now().After(deadline) {
			return "", nil, errors.New("new pane: it did not appear in the session")
		}
		if err := a.sleep(a.cfg.Poll); err != nil {
			return "", nil, err
		}
	}
}

// profile runs one MsgCLIPaneProfile op ("get", "set" with prompt, "clear").
func (a *applier) profile(op, pane, prompt string) (*msg.MsgCLIPaneProfileReply, error) {
	req := &msg.MsgCLIPaneProfile{Op: op, PaneID: pane}
	if op == "set" {
		req.Tools = append([]string(nil), Tools...)
		req.Prompt = prompt
	}
	reply, err := a.request(msg.T("ws", "inbox"), req)
	if err != nil {
		return nil, fmt.Errorf("profile %s on pane %.8s: %w", op, pane, err)
	}
	r, ok := reply.(*msg.MsgCLIPaneProfileReply)
	if !ok {
		// A daemon older than this command answers the unknown message with
		// nothing useful: say so instead of guessing.
		return nil, fmt.Errorf("profile %s on pane %.8s: the daemon does not support family apply (reply %T)", op, pane, reply)
	}
	if !r.OK {
		return nil, fmt.Errorf("profile %s on pane %.8s: %s", op, pane, r.Error)
	}
	if op != "get" {
		a.writes++
	}
	return r, nil
}

func (a *applier) fleet(name string) (*fleet.Fleet, error) {
	if err := a.ctx.Err(); err != nil {
		return nil, err
	}
	r, err := fleet.Ask(a.cfg.NC, fleet.Query{Name: name}, a.cfg.RequestTimeout)
	if err != nil {
		return nil, err
	}
	for i := range r.Fleets {
		if r.Fleets[i].Name == name {
			return &r.Fleets[i], nil
		}
	}
	return nil, nil
}

func (a *applier) fleetUpdate(u fleet.Update) (*fleet.UpdateReply, error) {
	if err := a.ctx.Err(); err != nil {
		return nil, err
	}
	r, err := fleet.Send(a.cfg.NC, u, a.cfg.RequestTimeout)
	if err != nil {
		return nil, fmt.Errorf("fleet %s: %w", u.Op, err)
	}
	a.writes++
	return r, nil
}

// Run is the whole command after the session is connected: parse the
// manifest from in, apply it, and write exactly one JSON line to out. On any
// error nothing is written to out.
func Run(ctx context.Context, cfg Config, in io.Reader, out io.Writer) error {
	m, err := Parse(in)
	if err != nil {
		return err
	}
	res, err := Apply(ctx, cfg, m)
	if err != nil {
		return err
	}
	return WriteResult(out, res)
}

// WriteResult writes res as one JSON line.
func WriteResult(out io.Writer, res *Result) error {
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	_, err = out.Write(append(b, '\n'))
	return err
}
