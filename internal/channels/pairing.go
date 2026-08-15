// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// Contact pairing & allowlists (WS3, design 003). The PairingStore is the
// single source of truth for which external SenderIDs a humanoid's channels
// admit, which senders are waiting for human approval, and the opaque
// device-link credentials QR-based adapters (WhatsApp non-Cloud, Signal) need
// to resume a linked session.
//
// Backing storage is the JetStream KV bucket "rysh-pairings". Unlike the
// ephemeral session-suffixed buckets (rysh-usage-<session>, ...), this bucket
// is NOT session-suffixed: a linked device and an approved allowlist must
// outlive any one session daemon. When JetStream is unavailable the store
// degrades to the 0600 files under .rysh/channel-state/ (state.go); when BOTH
// are unavailable, Allowed returns false — fail-closed, never fail-open
// (design 003 G5 / §7).

const (
	// pairingBucket is the durable JetStream KV bucket for all pairing state.
	pairingBucket = "rysh-pairings"
	// pairingStateChannel is the .rysh/channel-state/<channel>/ directory used
	// by the file fallback (one "pairing-<humanoid>-<channel>"-style file per
	// contact channel via saveChannelState/loadChannelState).
	pairingStateChannel = "pairing"

	// pairCodeLen is the length of a pairing code. Codes are security-sensitive
	// and read aloud/typed, so they are longer than message short-IDs
	// (36^6 ≈ 2.2B combinations vs 36^4 for shortIDLen).
	pairCodeLen = 6

	// OpenClaw-parity defaults: codes expire in 1h, at most 3 pending requests.
	defaultPendingTTL = time.Hour
	defaultMaxPending = 3

	// pendingFirstMsgMax bounds the stored FirstMsg excerpt shown to approvers.
	pendingFirstMsgMax = 200
)

// PairingRecord is one (humanoid, channel) pairing state cell.
type PairingRecord struct {
	Allowlist  []string              `json:"allowlist"`   // approved SenderIDs
	Pending    map[string]PendingReq `json:"pending"`     // code -> request
	DeviceLink json.RawMessage       `json:"device_link"` // opaque adapter session creds (secret — never log)
	UpdatedAt  time.Time             `json:"updated_at"`
}

// PendingReq is one not-yet-approved contact request, keyed by its code.
type PendingReq struct {
	Code       string    `json:"code"` // base36, from newPairCode()
	SenderID   string    `json:"sender_id"`
	SenderName string    `json:"sender_name"`
	Channel    string    `json:"channel"`
	FirstMsg   string    `json:"first_msg"` // truncated, for the approver's context
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"` // CreatedAt + pending_ttl (default 1h)
}

// CredStore is the two-method view of PairingStore handed to QR adapters at
// construction so they can persist/resume their own device-link session
// without the actor (or the skill file) ever holding the credentials.
type CredStore interface {
	LoadDeviceLink(humanoid, channel string) (json.RawMessage, bool)
	SaveDeviceLink(humanoid, channel string, blob json.RawMessage) error
}

// PairingChannel is implemented only by adapters that establish a session via
// an out-of-band device link (WhatsApp non-Cloud, Signal). Credential-
// configured adapters (Slack, WhatsApp Cloud, email) do NOT implement it.
type PairingChannel interface {
	// PairingCh emits link-lifecycle events while Start() establishes the link.
	PairingCh() <-chan PairingEvent
}

// LinkableChannel is implemented by PairingChannel adapters whose link flow
// can also be started on demand (`##humanoid pair link`, X4 design 009 §3.4)
// rather than only at Start. TriggerLink returns immediately: the flow runs
// on its own goroutine and reports through PairingCh. Unless force is set,
// implementations must refuse when the daemon already holds a linked account
// (the re-link guard), reporting the refusal as an "error" pairing event.
type LinkableChannel interface {
	TriggerLink(force bool) error
}

// PairingEvent is one device-link lifecycle event from a PairingChannel.
type PairingEvent struct {
	Kind    string          // "qr" | "linked" | "error"
	QR      string          // raw QR payload to render (Kind=="qr")
	Session json.RawMessage // device-link creds to persist (Kind=="linked"; secret — never log)
	Detail  string          // human message (Kind=="error")
}

// PairingStore persists allowlists, pending pairing requests, and device-link
// creds. It lives in the channels package (not the actor) because it is shared
// between the actor's Receive goroutine and adapter/pairingLoop goroutines, so
// — per the project convention that only adapters may hold locks — it guards
// its read-modify-write cycles with a mutex here rather than in actor state.
type PairingStore struct {
	mu         sync.Mutex
	kv         nats.KeyValue // nil when JetStream is unavailable → file fallback
	maxPending int
	pendingTTL time.Duration
	now        func() time.Time // time source (time.Now); injectable so TTL tests need no real sleeps
}

var _ CredStore = (*PairingStore)(nil)

// PairingOption customises a PairingStore at construction.
type PairingOption func(*PairingStore)

// WithMaxPending overrides the pending-request cap (default 3).
func WithMaxPending(n int) PairingOption {
	return func(s *PairingStore) {
		if n > 0 {
			s.maxPending = n
		}
	}
}

// WithPendingTTL overrides the pending-request lifetime (default 1h).
func WithPendingTTL(d time.Duration) PairingOption {
	return func(s *PairingStore) {
		if d > 0 {
			s.pendingTTL = d
		}
	}
}

// WithNow overrides the store's time source. Test seam: TTL expiry tests
// advance a fake clock instead of racing real sleeps against wall-clock
// TTLs (which flaked under -race on loaded CI runners).
func WithNow(now func() time.Time) PairingOption {
	return func(s *PairingStore) {
		if now != nil {
			s.now = now
		}
	}
}

// NewPairingStore opens (or creates) the durable "rysh-pairings" KV bucket on
// nc's JetStream context, mirroring UsageActor.openKV. A nil connection or an
// unavailable JetStream leaves kv nil and every operation degrades to the
// file-backed channel-state path — the store never fails to construct.
func NewPairingStore(nc *nats.Conn, opts ...PairingOption) *PairingStore {
	s := &PairingStore{
		maxPending: defaultMaxPending,
		pendingTTL: defaultPendingTTL,
		now:        time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	if nc == nil {
		return s
	}
	js, err := nc.JetStream()
	if err != nil {
		slog.Warn("pairing: JetStream unavailable, using file fallback", "err", err)
		return s
	}
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:  pairingBucket,
		Storage: nats.FileStorage,
	})
	if err != nil {
		if kv, err = js.KeyValue(pairingBucket); err != nil {
			slog.Warn("pairing: KV unavailable, using file fallback",
				"bucket", pairingBucket, "err", err)
			return s
		}
	}
	s.kv = kv
	return s
}

// pairingKVKey builds the "state/{humanoid}/{channel}" bucket key, sanitising
// each segment to the KV-safe character set.
func pairingKVKey(humanoid, channel string) string {
	return "state/" + pairingSafeSegment(humanoid) + "/" + pairingSafeSegment(channel)
}

// pairingFileAccount is the account key for the file fallback — it yields a
// "pairing-<humanoid>-<channel>"-style file under .rysh/channel-state/pairing/.
func pairingFileAccount(humanoid, channel string) string {
	return pairingSafeSegment(humanoid) + "-" + pairingSafeSegment(channel)
}

func pairingSafeSegment(s string) string {
	safe := stateKeyUnsafe.ReplaceAllString(s, "_")
	if safe == "" {
		safe = "default"
	}
	return safe
}

// load reads the (humanoid, channel) record: KV first, falling back to the
// 0600 channel-state file when KV is nil or errors. A missing record on both
// paths returns an empty record — which Allowed treats as "deny", so the
// KV-and-file-both-unavailable case is fail-closed by construction.
// Callers must hold s.mu.
func (s *PairingStore) load(humanoid, channel string) PairingRecord {
	var rec PairingRecord
	if s.kv != nil {
		entry, err := s.kv.Get(pairingKVKey(humanoid, channel))
		switch {
		case err == nil:
			if json.Unmarshal(entry.Value(), &rec) == nil {
				return ensurePairingMaps(rec)
			}
		case errors.Is(err, nats.ErrKeyNotFound):
			return ensurePairingMaps(rec)
		}
		// Any other KV error (JetStream down mid-session): try the file.
	}
	loadChannelState(pairingStateChannel, pairingFileAccount(humanoid, channel), &rec)
	return ensurePairingMaps(rec)
}

// save writes the record to KV, degrading to the channel-state file when KV is
// nil or the put fails. Callers must hold s.mu.
func (s *PairingStore) save(humanoid, channel string, rec PairingRecord) error {
	rec.UpdatedAt = s.now()
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if s.kv != nil {
		if _, err := s.kv.Put(pairingKVKey(humanoid, channel), data); err == nil {
			return nil
		}
		// KV write failed — degrade to the file so state is not lost.
	}
	return savePairingFile(humanoid, channel, data)
}

// savePairingFile is the file-fallback writer: the same 0600 temp-file+rename
// scheme and .rysh/channel-state/ layout as saveChannelState (state.go), but
// with COMPACT json.Marshal output — MarshalIndent would reformat the opaque
// DeviceLink json.RawMessage, so the blob would no longer round-trip
// byte-for-byte for the adapter that has to resume from it.
func savePairingFile(humanoid, channel string, data []byte) error {
	path := channelStatePath(pairingStateChannel, pairingFileAccount(humanoid, channel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func ensurePairingMaps(rec PairingRecord) PairingRecord {
	if rec.Pending == nil {
		rec.Pending = make(map[string]PendingReq)
	}
	return rec
}

// Allowed reports whether sender is on the (humanoid, channel) allowlist.
// An empty sender, a missing record, or unavailable storage all yield false —
// the admission gate is fail-closed (design 003 G5).
func (s *PairingStore) Allowed(humanoid, channel, sender string) bool {
	if sender == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.load(humanoid, channel)
	for _, id := range rec.Allowlist {
		if id == sender {
			return true
		}
	}
	return false
}

// AddPending registers a pairing request for an unknown sender and returns the
// request (with its approval code). Expired entries are swept first; a sender
// that already has a live pending request gets the SAME request back (so
// repeated messages do not mint new codes or eat pending slots). When the
// post-sweep count is at maxPending, the request is refused with an error the
// caller can surface as a rate-note.
func (s *PairingStore) AddPending(humanoid, channel string, im InboundMessage) (PendingReq, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec := s.load(humanoid, channel)
	swept := sweepExpired(rec.Pending, s.now())

	// Dedupe: one live request per sender.
	for _, req := range rec.Pending {
		if req.SenderID == im.SenderID {
			if swept > 0 {
				_ = s.save(humanoid, channel, rec) // persist the sweep, best-effort
			}
			return req, nil
		}
	}

	if len(rec.Pending) >= s.maxPending {
		if swept > 0 {
			_ = s.save(humanoid, channel, rec)
		}
		return PendingReq{}, fmt.Errorf(
			"pairing: pending limit reached for %s/%s (max %d) — approve or let codes expire",
			humanoid, channel, s.maxPending)
	}

	now := s.now()
	req := PendingReq{
		Code:       uniquePairCode(rec.Pending),
		SenderID:   im.SenderID,
		SenderName: im.SenderName,
		Channel:    channel,
		FirstMsg:   truncateRunes(im.Content, pendingFirstMsgMax),
		CreatedAt:  now,
		ExpiresAt:  now.Add(s.pendingTTL),
	}
	rec.Pending[req.Code] = req
	if err := s.save(humanoid, channel, rec); err != nil {
		return PendingReq{}, err
	}
	return req, nil
}

// Approve consumes a pending code: the sender is promoted to the allowlist and
// the code is removed (single-use). An unknown or expired code is a no-op
// returning ok=false with no error, so the caller can print a notice.
func (s *PairingStore) Approve(humanoid, channel, code string) (PendingReq, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec := s.load(humanoid, channel)
	swept := sweepExpired(rec.Pending, s.now())

	req, ok := rec.Pending[code]
	if !ok {
		if swept > 0 {
			_ = s.save(humanoid, channel, rec)
		}
		return PendingReq{}, false, nil
	}

	delete(rec.Pending, code)
	rec.Allowlist = appendUnique(rec.Allowlist, req.SenderID)
	if err := s.save(humanoid, channel, rec); err != nil {
		return PendingReq{}, false, err
	}
	return req, true, nil
}

// Allow adds sender directly to the allowlist, skipping the code flow.
// Additive and idempotent — used both by the ##humanoid allow command and by
// the spawn-time merge of a channel's declared allowlist.
func (s *PairingStore) Allow(humanoid, channel, sender string) error {
	if sender == "" {
		return errors.New("pairing: empty sender")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rec := s.load(humanoid, channel)
	for _, id := range rec.Allowlist {
		if id == sender {
			return nil // already allowed — no write needed
		}
	}
	rec.Allowlist = append(rec.Allowlist, sender)
	return s.save(humanoid, channel, rec)
}

// List returns the current record for approver display, sweeping expired
// pendings first. The DeviceLink blob is blanked in the returned copy: it is a
// secret and List feeds rendering paths (pane / dashboard) that must never see
// it — use LoadDeviceLink for the credential itself.
func (s *PairingStore) List(humanoid, channel string) (PairingRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec := s.load(humanoid, channel)
	if sweepExpired(rec.Pending, s.now()) > 0 {
		_ = s.save(humanoid, channel, rec) // persist the sweep, best-effort
	}
	rec.DeviceLink = nil
	return rec, nil
}

// SaveDeviceLink persists an adapter's opaque device-link session blob.
// The blob is a secret: it is stored (0600 file / FileStorage KV under .rysh/)
// and never logged or rendered.
func (s *PairingStore) SaveDeviceLink(humanoid, channel string, blob json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec := s.load(humanoid, channel)
	rec.DeviceLink = blob
	return s.save(humanoid, channel, rec)
}

// LoadDeviceLink returns the persisted device-link blob, if any.
func (s *PairingStore) LoadDeviceLink(humanoid, channel string) (json.RawMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec := s.load(humanoid, channel)
	return rec.DeviceLink, len(rec.DeviceLink) > 0
}

// sweepExpired removes entries whose ExpiresAt has passed, returning how many
// were dropped. Zero-ExpiresAt entries (from hand-built records) never expire.
func sweepExpired(pending map[string]PendingReq, now time.Time) int {
	removed := 0
	for code, req := range pending {
		if !req.ExpiresAt.IsZero() && now.After(req.ExpiresAt) {
			delete(pending, code)
			removed++
		}
	}
	return removed
}

// appendUnique appends v when absent (allowlists stay duplicate-free).
func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

// newPairCode returns a random pairCodeLen-character base36 code (e.g.
// "k7m2q9"). It mirrors newShortID (shortid.go) — same crypto/rand base36
// draw, same fallback — but at length 6 because a pairing code is
// security-sensitive. The caller collision-checks against the live Pending
// set (uniquePairCode), as uniqueShortIDLocked does for short IDs.
func newPairCode() string {
	b := make([]byte, pairCodeLen)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(shortIDAlphabet))))
		if err != nil {
			// crypto/rand failure is effectively impossible; fall back to 'a'.
			b[i] = shortIDAlphabet[0]
			continue
		}
		b[i] = shortIDAlphabet[n.Int64()]
	}
	return string(b)
}

// uniquePairCode draws codes until one is not already a pending key. With
// 36^6 combinations and max_pending≈3 collisions are effectively impossible,
// but the loop makes uniqueness a guarantee rather than a probability.
func uniquePairCode(pending map[string]PendingReq) string {
	for {
		code := newPairCode()
		if _, taken := pending[code]; !taken {
			return code
		}
	}
}

// truncateRunes shortens s to at most n runes (UTF-8 safe) with an ellipsis.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
