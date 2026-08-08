package channels

import (
	"fmt"
	"sync"
	"time"
)

// Attachment holds a file attachment for an email draft.
type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// Draft represents an unsent draft.
type Draft struct {
	ID string
	// Channel is the channel type this draft belongs to ("slack", "email",
	// "whatsapp"). A humanoid's channels share ONE store, so an owner's bare
	// "send" must only ever approve a draft on the channel they are looking
	// at — ApproveLatest filters on this field.
	Channel     string
	To          string
	Subject     string
	Body        string
	InReplyTo   string
	Attachments []Attachment
	CreatedAt   time.Time

	// ApprovedAt is set when the OWNER confirms the draft (typing "send" in
	// the humanoid's pane). Zero means pending.
	//
	// This is the enforcement state behind human-governed channels. Without it
	// the "draft-and-confirm" guarantee is only prompt text — the model is
	// told not to send unbidden, and nothing stops it if it does. A send tool
	// under human governance must refuse a draft whose ApprovedAt is zero.
	ApprovedAt time.Time
}

// Approved reports whether the owner has confirmed this draft.
func (d *Draft) Approved() bool { return !d.ApprovedAt.IsZero() }

// DraftStore provides thread-safe in-memory draft storage.
type DraftStore struct {
	mu     sync.Mutex
	drafts map[string]*Draft
	seq    int
}

// NewDraftStore creates a new empty DraftStore.
func NewDraftStore() *DraftStore {
	return &DraftStore{drafts: make(map[string]*Draft)}
}

// Create adds a new draft for the given channel type and returns its ID.
func (ds *DraftStore) Create(channel, to, subject, body, inReplyTo string) string {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	ds.seq++
	id := fmt.Sprintf("draft-%d", ds.seq)
	ds.drafts[id] = &Draft{
		ID:        id,
		Channel:   channel,
		To:        to,
		Subject:   subject,
		Body:      body,
		InReplyTo: inReplyTo,
		CreatedAt: time.Now(),
	}
	return id
}

// Get returns a draft by ID.
func (ds *DraftStore) Get(id string) (*Draft, bool) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	d, ok := ds.drafts[id]
	return d, ok
}

// Approve marks a draft as owner-confirmed. Returns false if the ID is unknown.
func (ds *DraftStore) Approve(id string) bool {
	if ds == nil {
		return false
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	d, ok := ds.drafts[id]
	if !ok {
		return false
	}
	if d.ApprovedAt.IsZero() {
		d.ApprovedAt = time.Now()
	}
	return true
}

// ApproveLatest confirms the most recently created pending draft on the given
// channel and returns its ID. An empty channel matches any draft.
//
// This backs the bare "send" confirmation, where the owner names no ID: the
// draft they are looking at is the one just created. Only PENDING drafts are
// considered, so repeating "send" cannot silently re-approve an older draft
// that was already sent and deleted, nor resurrect one the owner ignored. The
// channel filter exists because the store is shared by ALL of a humanoid's
// channels: an owner's "send" on the email flow must never flip a pending
// Slack draft (or vice versa) — the channel send tools verify approval in
// code, so a cross-channel approval would authorise the wrong payload.
func (ds *DraftStore) ApproveLatest(channel string) (string, bool) {
	if ds == nil {
		return "", false
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	var newest *Draft
	for _, d := range ds.drafts {
		if !d.ApprovedAt.IsZero() {
			continue
		}
		if channel != "" && d.Channel != channel {
			continue
		}
		if newest == nil || d.CreatedAt.After(newest.CreatedAt) {
			newest = d
		}
	}
	if newest == nil {
		return "", false
	}
	newest.ApprovedAt = time.Now()
	return newest.ID, true
}

// Update replaces the body of an existing draft.
func (ds *DraftStore) Update(id, body string) error {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	d, ok := ds.drafts[id]
	if !ok {
		return fmt.Errorf("draft %q not found", id)
	}
	d.Body = body
	return nil
}

// AddAttachment appends an attachment to an existing draft.
func (ds *DraftStore) AddAttachment(id string, att Attachment) error {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	d, ok := ds.drafts[id]
	if !ok {
		return fmt.Errorf("draft %q not found", id)
	}
	d.Attachments = append(d.Attachments, att)
	return nil
}

// Delete removes a draft by ID.
func (ds *DraftStore) Delete(id string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	delete(ds.drafts, id)
}

// List returns all current drafts.
func (ds *DraftStore) List() []*Draft {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	out := make([]*Draft, 0, len(ds.drafts))
	for _, d := range ds.drafts {
		out = append(out, d)
	}
	return out
}
