package actors

import (
	"encoding/json"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// recordingKV captures the bytes persistToKV writes, so tests can assert on
// the exact record a restart would restore from. (fakeKV elsewhere in this
// package only counts puts.) Get serves those same bytes back, so a test can
// run a real restore over the record the previous "process" wrote.
type recordingKV struct {
	nats.KeyValue
	data map[string][]byte
}

func (r *recordingKV) Put(key string, value []byte) (uint64, error) {
	if r.data == nil {
		r.data = make(map[string][]byte)
	}
	r.data[key] = append([]byte(nil), value...)
	return 1, nil
}

func (r *recordingKV) Get(key string) (nats.KeyValueEntry, error) {
	value, ok := r.data[key]
	if !ok {
		return nil, nats.ErrKeyNotFound
	}
	return &recordedEntry{value: value}, nil
}

// recordedEntry is the minimal nats.KeyValueEntry a restore path reads: only
// Value() is called. The embedded nil interface satisfies the rest at compile
// time (same trick as recordingKV itself).
type recordedEntry struct {
	nats.KeyValueEntry
	value []byte
}

func (e *recordedEntry) Value() []byte { return e.value }

func persistedContacts(t *testing.T, kv *recordingKV, humanoid string) map[string]msg.ChannelConfig {
	t.Helper()
	raw, ok := kv.data["humanoids"]
	if !ok {
		t.Fatal("nothing was persisted to the humanoids KV key")
	}
	var entries map[string]humanoidKV
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("persisted KV record is not valid JSON: %v", err)
	}
	hkv, ok := entries[humanoid]
	if !ok {
		t.Fatalf("humanoid %q missing from persisted record", humanoid)
	}
	return hkv.Contacts
}

// TestGovernanceFlipPersistsToKV pins design 019 gap 2: a runtime
// `##humanoid governance` flip must reach the registry's KV record —
// restoreFromKV re-creates humanoids from these contacts, so an unpersisted
// flip silently reverts to the skill-file mode on restart.
func TestGovernanceFlipPersistsToKV(t *testing.T) {
	kv := &recordingKV{}
	r := &HumanoidRegistryActor{
		humanoids: map[string]*humanoidEntry{
			"mail-bot": {
				name: "mail-bot",
				contacts: map[string]msg.ChannelConfig{
					"email":    {Governance: "ai", EmailConfig: &msg.EmailChannelConfig{Governance: "ai"}},
					"slack":    {Governance: "ai"},
					"telegram": {Governance: "ai"},
				},
			},
		},
		kvStore: kv,
	}

	r.handleGovernanceChanged(&msg.MsgHumanoidGovernanceChanged{Name: "mail-bot", Mode: "human"})

	contacts := persistedContacts(t, kv, "mail-bot")
	for _, channel := range []string{"email", "slack", "telegram"} {
		if got := contacts[channel].Governance; got != "human" {
			t.Errorf("persisted %s governance = %q, want human", channel, got)
		}
	}
	// Email carries both spellings so either reader restores the same mode.
	if ec := contacts["email"].EmailConfig; ec == nil || ec.Governance != "human" {
		t.Error("persisted nested email config.governance must be flipped too")
	}
}

// TestGovernanceFlipRoundTripsThroughRestore closes the loop: the contacts
// exactly as persisted must re-initialise a HumanoidActor into the flipped
// mode. restoreFromKV feeds hkv.Contacts straight into NewHumanoidActor, so
// this is the restore path's only mode-carrying logic.
func TestGovernanceFlipRoundTripsThroughRestore(t *testing.T) {
	kv := &recordingKV{}
	r := &HumanoidRegistryActor{
		humanoids: map[string]*humanoidEntry{
			"mail-bot": {
				name: "mail-bot",
				contacts: map[string]msg.ChannelConfig{
					"email": {Governance: "ai"},
					"slack": {Governance: "ai"},
				},
			},
		},
		kvStore: kv,
	}
	r.handleGovernanceChanged(&msg.MsgHumanoidGovernanceChanged{Name: "mail-bot", Mode: "human"})

	restored := NewHumanoidActor("mail-bot", "sp",
		persistedContacts(t, kv, "mail-bot"), config.Config{}, nil, nil, nil, nil)
	if got := restored.govMode("email"); got != "human" {
		t.Errorf("restored govMode(email) = %q, want human", got)
	}
	if got := restored.govMode("slack"); got != "human" {
		t.Errorf("restored govMode(slack) = %q, want human", got)
	}
}

func TestGovernanceFlipIgnoresUnknownHumanoidAndBadMode(t *testing.T) {
	kv := &recordingKV{}
	r := &HumanoidRegistryActor{
		humanoids: map[string]*humanoidEntry{
			"mail-bot": {name: "mail-bot", contacts: map[string]msg.ChannelConfig{"slack": {Governance: "ai"}}},
		},
		kvStore: kv,
	}

	r.handleGovernanceChanged(&msg.MsgHumanoidGovernanceChanged{Name: "nobody", Mode: "human"})
	r.handleGovernanceChanged(&msg.MsgHumanoidGovernanceChanged{Name: "mail-bot", Mode: "root"})

	if len(kv.data) != 0 {
		t.Error("nothing valid changed, so nothing should have been persisted")
	}
	if got := r.humanoids["mail-bot"].contacts["slack"].Governance; got != "ai" {
		t.Errorf("an invalid mode must not mutate contacts, got %q", got)
	}
}

// TestReplyModeFlipPersistsToKV: same restart contract for `##humanoid
// reply-to` — adapters are rebuilt from the stored contacts on restore.
func TestReplyModeFlipPersistsToKV(t *testing.T) {
	kv := &recordingKV{}
	r := &HumanoidRegistryActor{
		humanoids: map[string]*humanoidEntry{
			"slack-bot": {
				name:     "slack-bot",
				contacts: map[string]msg.ChannelConfig{"slack": {ReplyMode: "messages"}},
			},
		},
		kvStore: kv,
	}

	r.handleReplyModeChanged(&msg.MsgHumanoidReplyModeChanged{
		Name: "slack-bot", ChannelType: "slack", Mode: "mentions",
	})

	contacts := persistedContacts(t, kv, "slack-bot")
	if got := contacts["slack"].ReplyMode; got != "mentions" {
		t.Errorf("persisted slack reply_mode = %q, want mentions", got)
	}
}

// TestCloneContactsIsolatesActorFromRegistry guards the reason cloneContacts
// exists: after a flip mutates the registry's copy (registry goroutine), the
// actor's copy — read on the actor goroutine — must be untouched shared
// state, EmailConfig pointer included.
func TestCloneContactsIsolatesActorFromRegistry(t *testing.T) {
	registryCopy := map[string]msg.ChannelConfig{
		"email": {Governance: "ai", EmailConfig: &msg.EmailChannelConfig{Governance: "ai"}},
	}
	actorCopy := cloneContacts(registryCopy)

	if actorCopy["email"].EmailConfig == registryCopy["email"].EmailConfig {
		t.Fatal("cloneContacts must not share the EmailConfig pointer across goroutine owners")
	}

	r := &HumanoidRegistryActor{
		humanoids: map[string]*humanoidEntry{
			"mail-bot": {name: "mail-bot", contacts: registryCopy},
		},
		kvStore: &recordingKV{},
	}
	r.handleGovernanceChanged(&msg.MsgHumanoidGovernanceChanged{Name: "mail-bot", Mode: "human"})

	if got := actorCopy["email"].Governance; got != "ai" {
		t.Errorf("registry-side flip leaked into the actor's copy: %q", got)
	}
	if got := actorCopy["email"].EmailConfig.Governance; got != "ai" {
		t.Errorf("registry-side flip leaked into the actor's nested EmailConfig: %q", got)
	}
}
