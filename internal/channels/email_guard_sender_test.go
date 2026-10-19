// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 WO-11, root-0320 (rr-msg-089, the foreign-mail ruling): under the
// read-only guard a mail qualifies only if it carries the tag AND its From is
// on the allow-list (email.allow_from; default the account's own username),
// and the guard learns that from a BODY.PEEK[HEADER.FIELDS (FROM SUBJECT)]
// fetch BEFORE any body is fetched. A stranger's body never crosses the wire,
// so it can never reach the agent, the TUI or the watcher. The rule lives in
// guardedConn (the wire), so no caller can skip it.

const senderOwner = "m1-run@example.test"

// senderFake is a guard fake whose mailbox exercises the sender screen. UIDs:
//
//	51 owner, tagged              — the only mail that may be listed/read
//	52 stranger, tagged           — (i)
//	53 owner, OLD tag             — (ii)
//	54 spoofed display name       — (v)
//	61,63-65 ambiguous From headers — multi, folded, duplicate, group
//	62 owner, RFC 2047 encoded DISPLAY NAME — passes (decoded and ignored)
//	66 owner, address case differs — passes
//	67 owner with a display name   — passes
//	68 encoded addr-spec (local-part) — rejected
func senderFake(t *testing.T) *guardFakeServer {
	srv := newGuardFakeServer(t)
	tagged := func(s string) string { return "[" + guardTag + "] " + s }
	srv.mails = map[int]fakeMail{
		51: {from: senderOwner, subject: tagged("owner mail"), body: "OWNER-BODY-51"},
		52: {from: "stranger@example.org", subject: tagged("stranger mail"), body: "STRANGER-BODY-52"},
		53: {from: senderOwner, subject: "[RYSH-OLD-TAG-0001] owner old tag", body: "OLD-TAG-BODY-53"},
		54: {from: `"m1-run" <stranger@example.org>`, subject: tagged("spoofed name"), body: "SPOOF-BODY-54"},
		61: {from: senderOwner + ", stranger@example.org", subject: tagged("multi"), body: "MULTI-BODY-61"},
		62: {from: "=?UTF-8?B?bTEtcnVu?= <" + senderOwner + ">", subject: tagged("encoded"), body: "ENCODED-BODY-62"},
		63: {from: "\"m1-run\"\r\n <" + senderOwner + ">", subject: tagged("folded"), body: "FOLDED-BODY-63"},
		64: {from: senderOwner + "\r\nFrom: stranger@example.org", subject: tagged("duplicate"), body: "DUP-BODY-64"},
		65: {from: "owners: " + senderOwner + ";", subject: tagged("group"), body: "GROUP-BODY-65"},
		66: {from: "M1-Run@Example.TEST", subject: tagged("case"), body: "CASE-BODY-66"},
		67: {from: `"Some Name" <` + senderOwner + `>`, subject: tagged("named owner"), body: "NAMED-BODY-67"},
		68: {from: "=?UTF-8?B?bTEtcnVu?=@example.test", subject: tagged("encoded local-part"), body: "ENCLOCAL-BODY-68"},
	}
	return srv
}

// senderAdapter is guardedAdapter with the fake account senderOwner.
func senderAdapter(t *testing.T, srv *guardFakeServer) (*EmailAdapter, string) {
	t.Helper()
	e, audit := guardedAdapter(t, srv)
	e.config.EmailConfig.Username = senderOwner
	return e, audit
}

// fetchItems returns the item list of every UID FETCH sent for uid.
func fetchItems(cmds []string, uid int) []string {
	var out []string
	for _, c := range cmds {
		_, rest, _ := strings.Cut(c, " ")
		m := uidFetchPattern.FindStringSubmatch(rest)
		if m == nil {
			continue
		}
		if n := fmt.Sprint(uid); m[1] == n {
			out = append(out, m[2])
		}
	}
	return out
}

// isScreenFetch reports whether a FETCH item list is the header-fields screen
// only (UID / FLAGS / BODY.PEEK[HEADER.FIELDS (FROM SUBJECT)]).
func isScreenFetch(items string) bool {
	u := strings.ToUpper(items)
	u = strings.ReplaceAll(u, "BODY.PEEK[HEADER.FIELDS (FROM SUBJECT)]", "")
	u = strings.Trim(strings.TrimSpace(u), "()")
	for _, w := range strings.Fields(u) {
		if w != "UID" && w != "FLAGS" {
			return false
		}
	}
	return strings.Contains(strings.ToUpper(items), "HEADER.FIELDS (FROM SUBJECT)")
}

type auditRec struct {
	Event    string   `json:"event"`
	UID      int      `json:"uid"`
	Verdict  string   `json:"verdict"`
	Line     string   `json:"line"`
	AllowTo  []string `json:"allow_from"`
	Tag      string   `json:"tag"`
	FromKey  string   `json:"from"`
	SubjKey  string   `json:"subject"`
	ConnID   string   `json:"conn"`
	Why      string   `json:"why"`
	Messages string   `json:"message_id"`
}

func readAuditRecs(t *testing.T, path string) []auditRec {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	var out []auditRec
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r auditRec
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("audit line %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

func screenVerdicts(recs []auditRec) map[int][]string {
	out := map[int][]string{}
	for _, r := range recs {
		if r.Event == "screen" {
			out[r.UID] = append(out[r.UID], r.Verdict)
		}
	}
	return out
}

// (i) A stranger's mail WITH the right tag: only header fields cross the
// wire, its body is never fetched, it is not listed, the agent's email_read
// and the reader and attachment save are refused, and the audit records a
// screen reject without the stranger's From or Subject.
func TestGuardSenderStrangerWithTagNeverBodyFetched(t *testing.T) {
	srv := senderFake(t)
	e, audit := senderAdapter(t, srv)

	got, err := e.ListEmailsIn("", 50, "")
	if err != nil {
		t.Fatalf("ListEmailsIn: %v", err)
	}
	for _, s := range got {
		if s.UID == 52 {
			t.Errorf("stranger mail 52 was listed: %+v", s)
		}
	}
	if _, err := e.ReadEmail(52); !errors.Is(err, ErrSafetyRefused) {
		t.Errorf("agent ReadEmail(52) = %v, want ErrSafetyRefused", err)
	}
	if _, err := e.ReadEmailIn("", 52); !errors.Is(err, ErrSafetyRefused) {
		t.Errorf("reader ReadEmailIn(52) = %v, want ErrSafetyRefused", err)
	}
	if _, err := e.SaveAttachment("", 52, 0, t.TempDir()); !errors.Is(err, ErrSafetyRefused) {
		t.Errorf("SaveAttachment(52) = %v, want ErrSafetyRefused", err)
	}

	items := fetchItems(srv.commands(), 52)
	if len(items) == 0 {
		t.Fatalf("UID 52 was never screened: %q", srv.commands())
	}
	for _, it := range items {
		if !isScreenFetch(it) {
			t.Errorf("a body item reached the wire for stranger UID 52: %q", it)
		}
	}
	assertReadOnlyWire(t, srv)

	recs := readAuditRecs(t, audit)
	if v := screenVerdicts(recs)[52]; len(v) == 0 || v[0] != "reject" {
		t.Errorf("screen verdicts for 52 = %v, want reject", v)
	}
	raw, _ := os.ReadFile(audit)
	for _, never := range []string{"stranger@example.org", "stranger mail", "STRANGER-BODY-52"} {
		if strings.Contains(string(raw), never) {
			t.Errorf("audit log contains %q", never)
		}
	}
	for _, r := range recs {
		if r.Event == "fetched" && r.UID == 52 {
			t.Errorf("audit records UID 52 as fetched")
		}
	}

	// The watcher publishes nothing of it either.
	srv2 := senderFake(t)
	e2, _ := senderAdapter(t, srv2)
	conn, err := e2.dialIMAP("fake:993")
	if err != nil {
		t.Fatalf("dialIMAP: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e2.watchInbox(ctx, newIMAPClient(conn), "fake:993", time.Minute, 10*time.Millisecond) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(e2.inbound) < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watchInbox: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watchInbox did not return")
	}
	var published []string
	for len(e2.inbound) > 0 {
		m := <-e2.inbound
		published = append(published, m.Metadata["subject"])
		if strings.Contains(m.Content, "STRANGER") || strings.Contains(m.Content, "SPOOF") {
			t.Errorf("watcher published a stranger's body: %q", m.Content)
		}
	}
	sort.Strings(published)
	want := []string{"[" + guardTag + "] case", "[" + guardTag + "] encoded", "[" + guardTag + "] named owner", "[" + guardTag + "] owner mail"}
	if fmt.Sprint(published) != fmt.Sprint(want) {
		t.Errorf("watcher published %q, want %q", published, want)
	}
	for _, it := range fetchItems(srv2.commands(), 52) {
		if !isScreenFetch(it) {
			t.Errorf("watcher fetched a body item for stranger UID 52: %q", it)
		}
	}
}

// (ii) The owner's own mail with an OLD tag is not returned by the tagged
// search and never has anything fetched — not even header fields.
func TestGuardSenderOwnerOldTagNeverFetched(t *testing.T) {
	srv := senderFake(t)
	e, _ := senderAdapter(t, srv)
	if _, err := e.ListEmailsIn("", 50, ""); err != nil {
		t.Fatalf("ListEmailsIn: %v", err)
	}
	if _, err := e.ReadEmail(53); !errors.Is(err, ErrSafetyRefused) {
		t.Errorf("ReadEmail(53) = %v, want ErrSafetyRefused", err)
	}
	if items := fetchItems(srv.commands(), 53); len(items) != 0 {
		t.Errorf("old-tag UID 53 was fetched: %q", items)
	}
}

// (iii) The owner's tagged mail: header fields first, then a body PEEK, in
// that order on the wire; listed; readable; never \Seen; screen pass.
func TestGuardSenderOwnerTaggedHeadersThenPeek(t *testing.T) {
	srv := senderFake(t)
	e, audit := senderAdapter(t, srv)
	got, err := e.ListEmailsIn("", 50, "")
	if err != nil {
		t.Fatalf("ListEmailsIn: %v", err)
	}
	var uids []int
	for _, s := range got {
		uids = append(uids, s.UID)
	}
	sort.Ints(uids)
	if fmt.Sprint(uids) != "[51 62 66 67]" {
		t.Errorf("listed UIDs %v, want [51 62 66 67] (owner, tagged)", uids)
	}
	if d, err := e.ReadEmail(51); err != nil || d.Body != "OWNER-BODY-51" {
		t.Errorf("ReadEmail(51) = %+v, %v", d, err)
	}
	items := fetchItems(srv.commands(), 51)
	if len(items) < 2 || !isScreenFetch(items[0]) || isScreenFetch(items[1]) {
		t.Errorf("UID 51 fetches %q, want the header-fields screen first, then a body PEEK", items)
	}
	if srv.isSeen(51) {
		t.Error("UID 51 is \\Seen")
	}
	assertReadOnlyWire(t, srv)
	if v := screenVerdicts(readAuditRecs(t, audit))[51]; len(v) == 0 || v[0] != "pass" {
		t.Errorf("screen verdicts for 51 = %v, want pass", v)
	}
}

// (iv) A caller that skips the client flow: a raw UID FETCH of a body item
// for a UID that a tagged search returned but the screen has not passed is
// refused at guardedConn.Write — the server never sees it.
func TestGuardSenderDirectBodyFetchRefused(t *testing.T) {
	srv := senderFake(t)
	e, audit := senderAdapter(t, srv)
	conn, err := e.dialIMAP("fake:993")
	if err != nil {
		t.Fatalf("dialIMAP: %v", err)
	}
	c := newIMAPClient(conn)
	if _, err := c.readResponse(""); err != nil {
		t.Fatal(err)
	}
	if err := c.command("EXAMINE", "INBOX"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.searchUIDs("ALL"); err != nil {
		t.Fatal(err)
	}
	before := len(srv.commands())
	for _, items := range []string{
		"(BODY.PEEK[TEXT])", "(BODY.PEEK[HEADER])", "(BODY.PEEK[])", "(BODY.PEEK[1])",
		"(ENVELOPE)", "(BODYSTRUCTURE)", "(RFC822.HEADER)", "(UID FLAGS BODY.PEEK[HEADER.FIELDS (FROM SUBJECT)] BODY.PEEK[TEXT])",
		"(BINARY.PEEK[1])", "(INTERNALDATE)",
	} {
		for _, uid := range []int{51, 52} {
			line := fmt.Sprintf("A0900 UID FETCH %d %s", uid, items)
			if err := c.writeLine(line); !errors.Is(err, ErrSafetyRefused) {
				t.Errorf("%q = %v, want ErrSafetyRefused (UID not screened)", line, err)
			}
		}
	}
	if got := srv.commands()[before:]; len(got) != 0 {
		t.Errorf("unscreened body fetches reached the server: %q", got)
	}
	refused := 0
	for _, r := range readAuditRecs(t, audit) {
		if r.Verdict == "refused" && strings.Contains(r.Line, "UID FETCH") {
			refused++
		}
	}
	if refused != 20 {
		t.Errorf("audit has %d refused FETCH lines, want 20", refused)
	}

	// Once the owner's mail has passed the screen on this connection, its body
	// may be PEEKed; the stranger's stays refused.
	if err := c.guardAdmit(51); err != nil {
		t.Fatalf("guardAdmit(51): %v", err)
	}
	if err := c.guardAdmit(52); !errors.Is(err, ErrSafetyRefused) {
		t.Errorf("guardAdmit(52) = %v, want ErrSafetyRefused", err)
	}
	if err := c.command("UID FETCH", "51 (BODY.PEEK[TEXT])"); err != nil {
		t.Errorf("screened UID 51 body PEEK: %v", err)
	}
	if err := c.writeLine("A0901 UID FETCH 52 (BODY.PEEK[TEXT])"); !errors.Is(err, ErrSafetyRefused) {
		t.Errorf("stranger UID 52 body after a failed screen = %v, want ErrSafetyRefused", err)
	}
	// A new EXAMINE starts over: nothing is verified on the reopened mailbox.
	if err := c.command("EXAMINE", "INBOX"); err != nil {
		t.Fatal(err)
	}
	if err := c.writeLine("A0902 UID FETCH 51 (BODY.PEEK[TEXT])"); !errors.Is(err, ErrSafetyRefused) {
		t.Errorf("UID 51 body after re-EXAMINE = %v, want ErrSafetyRefused", err)
	}
}

// (v) A spoofed display name is not an address: "m1-run" <stranger@…> fails.
func TestGuardSenderSpoofedDisplayNameRejected(t *testing.T) {
	srv := senderFake(t)
	e, _ := senderAdapter(t, srv)
	if _, err := e.ReadEmail(54); !errors.Is(err, ErrSafetyRefused) {
		t.Errorf("ReadEmail(54) = %v, want ErrSafetyRefused", err)
	}
	for _, it := range fetchItems(srv.commands(), 54) {
		if !isScreenFetch(it) {
			t.Errorf("spoofed UID 54 had a body item fetched: %q", it)
		}
	}
}

// Ambiguous From headers are rejected, never guessed at: multiple addresses,
// a folded line, a duplicate From, a group, an encoded addr-spec. The owner's
// address passes in another case, behind a display name, and behind an RFC
// 2047 ENCODED display name (decoded by net/mail and ignored; msg-0205 —
// Gmail may add one to what rysh's own SMTP path sent).
func TestGuardSenderAmbiguousFromRejected(t *testing.T) {
	srv := senderFake(t)
	e, audit := senderAdapter(t, srv)
	for _, uid := range []int{61, 63, 64, 65, 68} {
		if _, err := e.ReadEmail(uid); !errors.Is(err, ErrSafetyRefused) {
			t.Errorf("ReadEmail(%d) = %v, want ErrSafetyRefused", uid, err)
		}
		for _, it := range fetchItems(srv.commands(), uid) {
			if !isScreenFetch(it) {
				t.Errorf("ambiguous-From UID %d had a body item fetched: %q", uid, it)
			}
		}
	}
	for _, uid := range []int{62, 66, 67} {
		if _, err := e.ReadEmail(uid); err != nil {
			t.Errorf("ReadEmail(%d) owner address = %v, want it to pass", uid, err)
		}
	}
	v := screenVerdicts(readAuditRecs(t, audit))
	for _, uid := range []int{61, 63, 64, 65, 68} {
		if len(v[uid]) == 0 || v[uid][0] != "reject" {
			t.Errorf("screen %d = %v, want reject", uid, v[uid])
		}
	}
}

// (vi) No allow-list and no usable username: the guard is refused at
// construction and nothing is dialled. An allow_from entry that is not a bare
// address is refused too.
func TestGuardSenderNoAllowListNoUsernameRefused(t *testing.T) {
	for name, mut := range map[string]func(*msg.EmailChannelConfig){
		"no list, no username":     func(c *msg.EmailChannelConfig) { c.Username, c.AllowFrom = "", nil },
		"username not an address":  func(c *msg.EmailChannelConfig) { c.Username, c.AllowFrom = "m1run", nil },
		"entry with display name":  func(c *msg.EmailChannelConfig) { c.AllowFrom = []string{`"Me" <` + senderOwner + `>`} },
		"entry not an address":     func(c *msg.EmailChannelConfig) { c.AllowFrom = []string{"nobody"} },
		"disjoint layers sentinel": func(c *msg.EmailChannelConfig) { c.AllowFrom = []string{"<disjoint allow_from>"} },
	} {
		srv := senderFake(t)
		e, _ := senderAdapter(t, srv)
		mut(e.config.EmailConfig)
		if _, err := e.ListEmailsIn("", 10, ""); !errors.Is(err, ErrSafetyRefused) {
			t.Errorf("%s: ListEmailsIn = %v, want ErrSafetyRefused", name, err)
		}
		if srv.dials != 0 {
			t.Errorf("%s: dialled the server %d times", name, srv.dials)
		}
	}
}

// The allow-list defaults to the username; an explicit list replaces it.
// The guard event records the list (the tag stays in its existing field only).
func TestGuardSenderAllowListDefaultAndExplicit(t *testing.T) {
	srv := senderFake(t)
	e, audit := senderAdapter(t, srv)
	if _, err := e.ReadEmail(51); err != nil {
		t.Fatalf("default list: ReadEmail(51) = %v", err)
	}
	var guardEv *auditRec
	recs := readAuditRecs(t, audit)
	for i := range recs {
		if recs[i].Event == "guard" {
			guardEv = &recs[i]
			break
		}
	}
	if guardEv == nil || fmt.Sprint(guardEv.AllowTo) != "["+senderOwner+"]" {
		t.Errorf("guard event allow_from = %+v, want [%s]", guardEv, senderOwner)
	}
	raw, _ := os.ReadFile(audit)
	if n := strings.Count(string(raw), guardTag); n != strings.Count(string(raw), `"tag":"`+guardTag+`"`)+strings.Count(string(raw), `SUBJECT \"`+guardTag+`\"`) {
		t.Errorf("the tag appears in the audit log outside the guard event's tag field and the SEARCH lines")
	}

	srv2 := senderFake(t)
	e2, _ := senderAdapter(t, srv2)
	e2.config.EmailConfig.AllowFrom = []string{"other@example.test"}
	if _, err := e2.ReadEmail(51); !errors.Is(err, ErrSafetyRefused) {
		t.Errorf("explicit list without the owner: ReadEmail(51) = %v, want ErrSafetyRefused", err)
	}
}

// The latch carries the allow-list with the rest of the guard.
func TestGuardSenderLatchKeepsAllowFrom(t *testing.T) {
	old := msg.ChannelConfig{EmailConfig: &msg.EmailChannelConfig{Safety: "read_only", SubjectTag: guardTag, AuditLog: "/tmp/a.jsonl", AllowFrom: []string{senderOwner}}}
	next := latchSafety(old, msg.ChannelConfig{EmailConfig: &msg.EmailChannelConfig{Username: "x@example.test"}})
	if fmt.Sprint(next.EmailConfig.AllowFrom) != "["+senderOwner+"]" {
		t.Errorf("latched AllowFrom = %v, want [%s]", next.EmailConfig.AllowFrom, senderOwner)
	}
}

// Real servers do not all echo the section as it was asked for: the screen
// literal is recognised with quoted, re-cased or reordered field names
// (msg-0202). Only the response side is tolerant; what WE send stays exact.
func TestGuardSenderScreenToleratesEchoForms(t *testing.T) {
	for _, echo := range []string{
		`BODY[HEADER.FIELDS ("FROM" "SUBJECT")]`,
		`BODY[HEADER.FIELDS (From Subject)]`,
		`BODY[HEADER.FIELDS ("Subject" "From")]`,
		`body[header.fields (subject from)]`,
	} {
		srv := senderFake(t)
		srv.hfEcho = echo
		e, _ := senderAdapter(t, srv)
		if d, err := e.ReadEmail(51); err != nil || d.Body != "OWNER-BODY-51" {
			t.Errorf("echo %s: ReadEmail(51) = %+v, %v; want the owner's mail", echo, d, err)
		}
		if _, err := e.ReadEmail(52); !errors.Is(err, ErrSafetyRefused) {
			t.Errorf("echo %s: ReadEmail(52) = %v, want ErrSafetyRefused", echo, err)
		}
	}
}

// A screen response that names a different UID than the one armed verifies
// nothing: the owner's mail is refused rather than admitted on another
// message's headers.
func TestGuardSenderScreenWrongUIDVerifiesNothing(t *testing.T) {
	srv := senderFake(t)
	srv.hfWrongUID = true
	e, audit := senderAdapter(t, srv)
	if _, err := e.ReadEmail(51); !errors.Is(err, ErrSafetyRefused) {
		t.Errorf("ReadEmail(51) with a wrong-UID screen response = %v, want ErrSafetyRefused", err)
	}
	for _, it := range fetchItems(srv.commands(), 51) {
		if !isScreenFetch(it) {
			t.Errorf("UID 51 body fetched although its screen never verified: %q", it)
		}
	}
	if v := screenVerdicts(readAuditRecs(t, audit))[51]; len(v) == 0 || v[0] != "reject" {
		t.Errorf("screen verdicts for 51 = %v, want reject", v)
	}
}
